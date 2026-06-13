package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
)

const (
	// maintenanceHistorySize bounds the in-memory ring buffer of run
	// outcomes. Operators see these via the status API (bead 8).
	maintenanceHistorySize = 16

	// maintenanceJitterFraction is the ±fraction applied to the scheduled
	// interval so multiple cities sharing one host do not fire together.
	// 0.1 → interval ∈ [0.9·I, 1.1·I].
	maintenanceJitterFraction = 0.1

	// maintenanceStaleMultiplier defines how far past the due time the
	// scheduler waits before treating lastRunAt as stale and firing
	// immediately to catch up.
	maintenanceStaleMultiplier = 1.5

	// maintenanceActor identifies the supervisor subsystem as the
	// originator of maintenance events.
	maintenanceActor = "supervisor"

	// maintenanceSmokeTable names the bd-managed table the post-gc smoke
	// test reads from. bd's Dolt schema names it "issues"; see
	// internal/api/convoy_sql.go for the same literal on the read path.
	maintenanceSmokeTable = "issues"
)

// maintenanceSmokeTimeout caps the post-gc SELECT COUNT(*) probe. It is
// a var (not const) so tests can shorten it; production keeps the 5 s
// value mandated by design D5.
var maintenanceSmokeTimeout = 5 * time.Second

// MaintenanceRun summarizes one completed (or failed) maintenance run.
// Stage is "done" for successful runs and names the failing phase
// ("backup" | "gc" | "smoke-test" | "prune") for failed runs. Err is
// empty on success. BeforeBytes / AfterBytes / SnapshotPath are
// populated by the stages that can measure them; they remain zero when
// a dependency is not wired or a stage has no size metric.
type MaintenanceRun struct {
	StartedAt    time.Time
	FinishedAt   time.Time
	Stage        string
	Err          string
	BeforeBytes  int64
	AfterBytes   int64
	SnapshotPath string
}

// DoltOps is the minimal SQL surface the maintenance loop needs to sweep
// CALL DOLT_GC() across every managed database and run the post-gc smoke
// test. Each Dolt database is a separate repository with its own chunk
// store, so reclaiming the whole store requires one GC per database — see
// runDoltGC. Production wraps *sql.DB via NewSQLDoltOps; tests supply
// fakes. Close must release the underlying connection exactly once per
// cycle.
type DoltOps interface {
	// ListDatabases returns the database names visible to the connection
	// (SHOW DATABASES), including system schemas. The caller filters those
	// out via gcTargetDatabases.
	ListDatabases(ctx context.Context) ([]string, error)
	// ExecGCFor runs CALL DOLT_GC() against database db with the supplied
	// context's deadline. Implementations must pin the USE + CALL to a
	// single connection so a pool cannot split them across sessions and
	// GC the wrong database.
	ExecGCFor(ctx context.Context, db string) error
	// SmokeCountFor runs SELECT COUNT(*) FROM issues against database db
	// and returns the scalar result. A SQL error means the store is not
	// queryable after GC (the smoke-test failure signal); the count itself
	// is informational — an empty database is a healthy post-gc state for a
	// freshly-created rig, so a per-database zero is not a failure.
	SmokeCountFor(ctx context.Context, db string) (int, error)
	// Close releases the underlying connection.
	Close() error
}

// DoltOpsFactory opens a DoltOps for one maintenance cycle. Returning a
// non-nil error surfaces as a stage="gc" MaintenanceError from
// runDoltGC — the scheduler classifies "cannot reach Dolt" alongside
// "CALL DOLT_GC() failed" because the operator remediation is the
// same.
type DoltOpsFactory func(ctx context.Context) (DoltOps, error)

// MaintenanceError classifies a failed maintenance stage. Stage names
// the phase ("backup" | "gc" | "smoke-test" | "prune"); Err carries the
// underlying cause and is unwrappable via errors.Is / errors.As so
// context.DeadlineExceeded propagates across stage boundaries.
type MaintenanceError struct {
	Stage string
	Err   error
}

// Error renders the classified failure as "<stage>: <cause>".
func (e *MaintenanceError) Error() string {
	if e == nil {
		return "<nil maintenance error>"
	}
	if e.Err == nil {
		return e.Stage + ": <nil cause>"
	}
	return e.Stage + ": " + e.Err.Error()
}

// Unwrap exposes the underlying error for errors.Is / errors.As.
func (e *MaintenanceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// StoreMaintenanceLoopDeps bundles the runtime dependencies for the
// loop. Unset optional fields are replaced with sensible defaults.
type StoreMaintenanceLoopDeps struct {
	Cfg       config.DoltMaintenance
	Store     beads.Store     // city Dolt store; future beads exercise it
	CityPath  string          // absolute path for backup layout + logs
	Recorder  events.Recorder // defaults to events.Discard when nil
	Stderr    io.Writer       // defaults to io.Discard when nil
	Clock     func() time.Time
	Rand      func() float64 // returns [0,1); defaults to math/rand
	LastRunAt time.Time      // seeded from the event log by the caller

	// OpenDoltOps opens a SQL connection to the managed Dolt store for
	// one maintenance cycle. Nil leaves runDoltGC a no-op so deployments
	// can wire maintenance dependencies incrementally. Production wires
	// this to NewSQLDoltOps.
	OpenDoltOps DoltOpsFactory

	// OpenDoltBackup opens a DoltBackupRunner for one snapshot cycle.
	// Nil leaves runSnapshot a no-op. Production wires this to
	// NewExecDoltBackupRunner rooted at the managed Dolt DB dir.
	OpenDoltBackup DoltBackupRunnerFactory

	// Mail sends operator alert mail on failed runs when Cfg.AlertTo is
	// set. Nil disables alerts; tests that do not exercise the alert
	// path can leave it unset.
	Mail mail.Provider

	// DiskFreeBytes probes the free bytes available in path's filesystem.
	// Nil disables the disk pre-flight for this loop. Production wires
	// this to the OS statvfs call; tests supply a fake reader.
	DiskFreeBytes func(path string) (int64, error)

	// DiskMinFreeBytes is the critical floor. When free space falls below
	// this threshold CALL DOLT_GC is skipped and StoreDiskCritical is
	// emitted. Zero disables the check entirely.
	DiskMinFreeBytes int64

	// DiskWarnFreeBytes is the soft floor. When free space falls below this
	// threshold (but is still above DiskMinFreeBytes) StoreDiskWarn is
	// emitted and the GC proceeds.
	DiskWarnFreeBytes int64

	// StoreSizeBytes returns the current on-disk size of the managed Dolt
	// store in bytes. It gates CALL DOLT_GC on Cfg.MinStoreMB: when the
	// store is smaller than the floor the sweep is skipped (dolt_gc on a
	// small store reclaims little and is not worth the maintenance lease).
	// It also supplies the before/after byte deltas recorded on each run.
	// Nil disables the size gate (GC always runs) and leaves
	// BeforeBytes/AfterBytes zero. Production wires this to
	// storehealth.WalkSize(storehealth.StorePath(cityPath)).
	StoreSizeBytes func() int64
}

// StoreMaintenanceLoop runs periodic Dolt store maintenance inside the
// supervisor process. It is a goroutine sibling to
// CachingStore.reconcileLoop; see docs/adr/0002-dolt-store-maintenance-runbook.md
// and the ga-d5y design document for the full state machine.
//
// The zero value is not usable — construct with NewStoreMaintenanceLoop.
type StoreMaintenanceLoop struct {
	cfg               config.DoltMaintenance
	store             beads.Store
	cityPath          string
	recorder          events.Recorder
	stderr            io.Writer
	clock             func() time.Time
	rand              func() float64
	openDoltOps       DoltOpsFactory
	openDoltBackup    DoltBackupRunnerFactory
	mail              mail.Provider
	diskFreeBytes     func(path string) (int64, error)
	diskMinFreeBytes  int64
	diskWarnFreeBytes int64
	storeSizeBytes    func() int64

	// mu is the in-process maintenance lease. runOnce and TriggerNow hold
	// it for the duration of a single maintenance cycle; each contends on
	// the same mutex so the manual-override API returns 409 when the
	// scheduler (or a prior manual trigger) is mid-cycle.
	mu sync.Mutex

	// runStartedAt reports the start time of the currently in-flight run
	// so callers contending for the lease can surface started_at in a 409
	// Conflict body without having to acquire mu (which would block for
	// the remainder of the cycle). Set before a cycle begins and cleared
	// in the cycle's defer; nil means "no run in flight."
	runStartedAt atomic.Pointer[time.Time]

	lastRunAt time.Time
	history   []MaintenanceRun
}

// MaintenanceInProgressError is returned by TriggerNow when the maintenance
// lease is already held (either by the scheduled loop or a prior manual
// trigger). The StartedAt field is the timestamp of the in-flight run and
// is surfaced in the HTTP 409 Conflict body so operators can tell whether
// the existing run is fresh or stuck.
type MaintenanceInProgressError struct {
	StartedAt time.Time
}

// Error implements the error interface. The message shape is stable so the
// CLI's --wait stderr message remains grep-able from tests.
func (e *MaintenanceInProgressError) Error() string {
	if e == nil {
		return "<nil maintenance-in-progress>"
	}
	if e.StartedAt.IsZero() {
		return "maintenance already in progress"
	}
	return fmt.Sprintf("maintenance already in progress (started %s)", e.StartedAt.UTC().Format(time.RFC3339))
}

// NewStoreMaintenanceLoop constructs a loop from the given dependencies,
// filling in defaults (Clock=time.Now, Rand=rand.Float64,
// Recorder=events.Discard, Stderr=io.Discard) when unset.
func NewStoreMaintenanceLoop(deps StoreMaintenanceLoopDeps) *StoreMaintenanceLoop {
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.Rand == nil {
		deps.Rand = rand.Float64
	}
	if deps.Recorder == nil {
		deps.Recorder = events.Discard
	}
	if deps.Stderr == nil {
		deps.Stderr = io.Discard
	}
	return &StoreMaintenanceLoop{
		cfg:               deps.Cfg,
		store:             deps.Store,
		cityPath:          deps.CityPath,
		recorder:          deps.Recorder,
		stderr:            deps.Stderr,
		clock:             deps.Clock,
		rand:              deps.Rand,
		openDoltOps:       deps.OpenDoltOps,
		openDoltBackup:    deps.OpenDoltBackup,
		mail:              deps.Mail,
		diskFreeBytes:     deps.DiskFreeBytes,
		diskMinFreeBytes:  deps.DiskMinFreeBytes,
		diskWarnFreeBytes: deps.DiskWarnFreeBytes,
		storeSizeBytes:    deps.StoreSizeBytes,
		lastRunAt:         deps.LastRunAt,
		history:           make([]MaintenanceRun, 0, maintenanceHistorySize),
	}
}

// Run drives the maintenance schedule until ctx is canceled. When the
// loop is configured with Enabled=false it returns immediately so the
// caller can safely invoke it unconditionally during startup.
func (m *StoreMaintenanceLoop) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		return
	}
	timer := time.NewTimer(m.nextDelay(m.clock()))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// Timer firing is the run signal; do not re-sample jitter here.
		// A second nextDelay call would draw a new random value and could
		// return non-zero even though the due time has passed, silently
		// skipping the run.
		m.runOnce(ctx)
		timer.Reset(m.nextDelay(m.clock()))
	}
}

// LastRunAt returns the start time of the most recent maintenance run,
// or the zero value if the loop has not run (and no prior run was
// seeded from the event log).
func (m *StoreMaintenanceLoop) LastRunAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRunAt
}

// History returns a copy of the bounded run history in chronological
// order (oldest first).
func (m *StoreMaintenanceLoop) History() []MaintenanceRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MaintenanceRun, len(m.history))
	copy(out, m.history)
	return out
}

// nextDelay returns the duration until the next maintenance run should
// fire. A zero value means "fire now". Callers must not hold m.mu.
//
// Scheduling rules (see design D10 under bead ga-d5y):
//   - lastRunAt is the zero value → fire immediately (fresh install).
//   - lastRunAt is older than 1.5× interval → fire immediately
//     (catch-up after a long downtime).
//   - otherwise → due = lastRunAt + jittered(interval); delay = due-now.
func (m *StoreMaintenanceLoop) nextDelay(now time.Time) time.Duration {
	interval := m.cfg.IntervalOrDefault()
	if interval <= 0 {
		return 0
	}
	m.mu.Lock()
	last := m.lastRunAt
	m.mu.Unlock()
	if last.IsZero() {
		return 0
	}
	staleCutoff := time.Duration(float64(interval) * maintenanceStaleMultiplier)
	if now.Sub(last) >= staleCutoff {
		return 0
	}
	due := last.Add(m.applyJitter(interval))
	delay := due.Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

// applyJitter returns interval scaled by (1 ± maintenanceJitterFraction)
// using rand as the source. Pure function of the injected rand so tests
// can drive it deterministically.
func (m *StoreMaintenanceLoop) applyJitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	factor := (1 - maintenanceJitterFraction) + 2*maintenanceJitterFraction*m.rand()
	return time.Duration(float64(interval) * factor)
}

// runOnce executes one maintenance cycle, holding the lease for its
// duration. If the lease is already held (manual override in flight, or
// a previous tick has not finished), it returns without doing work —
// lease contention is a normal, silent condition.
func (m *StoreMaintenanceLoop) runOnce(ctx context.Context) {
	if !m.mu.TryLock() {
		return
	}
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	m.executeCycleLocked(ctx)
}

// TriggerNow runs one maintenance cycle synchronously, returning the
// MaintenanceRun summary on success. When the lease is held by another
// goroutine (the scheduler or a prior manual trigger), TriggerNow returns
// a *MaintenanceInProgressError whose StartedAt is the in-flight run's
// start time — this is what the POST
// /v0/city/{city}/maintenance/dolt-gc handler turns into a 409 Conflict.
//
// The returned run is a copy of the entry appended to history; callers may
// mutate it freely.
func (m *StoreMaintenanceLoop) TriggerNow(ctx context.Context) (MaintenanceRun, error) {
	if !m.mu.TryLock() {
		started := time.Time{}
		if p := m.runStartedAt.Load(); p != nil {
			started = *p
		}
		return MaintenanceRun{}, &MaintenanceInProgressError{StartedAt: started}
	}
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return MaintenanceRun{}, err
	}
	return m.executeCycleLocked(ctx), nil
}

// InFlightStart reports the start time of the currently-in-flight
// maintenance cycle and whether one is running. Non-blocking: it never
// acquires m.mu, so it is safe to call from HTTP handlers while a real
// cycle holds the lease for minutes.
func (m *StoreMaintenanceLoop) InFlightStart() (time.Time, bool) {
	p := m.runStartedAt.Load()
	if p == nil {
		return time.Time{}, false
	}
	return *p, true
}

// executeCycleLocked performs one maintenance cycle with m.mu already
// held. Callers are responsible for acquiring/releasing the lease and for
// the context-cancellation pre-check; this method focuses on the cycle
// body so runOnce and TriggerNow share exactly one code path.
func (m *StoreMaintenanceLoop) executeCycleLocked(ctx context.Context) MaintenanceRun {
	started := m.clock()
	m.runStartedAt.Store(&started)
	defer m.runStartedAt.Store(nil)

	snapshotPath, err := m.runSnapshot(ctx)
	if err != nil {
		return m.finishCycleLocked(started, snapshotPath, gcSweepResult{}, err)
	}
	if m.checkDiskPreflight() {
		// Disk is critically low — skip CALL DOLT_GC to avoid growing the
		// store further. The StoreDiskCritical event informs operators.
		// C1 (hold-on-store-unreachable) handles downstream safety.
		return m.finishCycleLocked(started, snapshotPath, gcSweepResult{}, nil)
	}
	sweep, err := m.runDoltGC(ctx)
	return m.finishCycleLocked(started, snapshotPath, sweep, err)
}

func (m *StoreMaintenanceLoop) finishCycleLocked(started time.Time, snapshotPath string, sweep gcSweepResult, err error) MaintenanceRun {
	run := MaintenanceRun{
		StartedAt:    started,
		FinishedAt:   m.clock(),
		SnapshotPath: snapshotPath,
		BeforeBytes:  sweep.BeforeBytes,
		AfterBytes:   sweep.AfterBytes,
	}
	if err != nil {
		run.Stage = "maintenance"
		var maintenanceErr *MaintenanceError
		if errors.As(err, &maintenanceErr) {
			run.Stage = maintenanceErr.Stage
		}
		run.Err = err.Error()
	} else {
		run.Stage = "done"
	}
	m.lastRunAt = started
	m.appendHistoryLocked(run)
	m.emitRunEvent(run)
	return run
}

// emitRunEvent records the typed gc.store.maintenance.done or
// gc.store.maintenance.failed event for a completed run. The failed
// variant fires when run.Err is non-empty; the done variant otherwise.
// Emission failures are swallowed (the recorder itself is best-effort).
func (m *StoreMaintenanceLoop) emitRunEvent(run MaintenanceRun) {
	if m.recorder == nil {
		return
	}
	duration := run.FinishedAt.Sub(run.StartedAt).Seconds()
	if duration < 0 {
		duration = 0
	}
	var (
		eventType string
		payload   events.Payload
	)
	if run.Err == "" {
		eventType = events.StoreMaintenanceDone
		payload = events.StoreMaintenanceDonePayload{
			DurationSeconds: duration,
			BeforeBytes:     run.BeforeBytes,
			AfterBytes:      run.AfterBytes,
			SnapshotPath:    run.SnapshotPath,
		}
	} else {
		eventType = events.StoreMaintenanceFailed
		payload = events.StoreMaintenanceFailedPayload{
			Stage:           run.Stage,
			ErrorMsg:        run.Err,
			SnapshotPath:    run.SnapshotPath,
			DurationSeconds: duration,
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	m.recorder.Record(events.Event{
		Type:    eventType,
		Actor:   maintenanceActor,
		Subject: m.cityPath,
		Ts:      run.FinishedAt,
		Payload: raw,
	})
	if run.Err != "" {
		m.sendFailureAlert(run)
	}
}

// checkDiskPreflight checks free space in cityPath's filesystem before a
// disk-growing operation (CALL DOLT_GC). Returns true when the GC should be
// skipped (CRITICAL), false when it may proceed. Side-effects: emits
// StoreDiskWarn or StoreDiskCritical events and logs to stderr.
// Fails open: a probe error or a nil DiskFreeBytes always returns false.
func (m *StoreMaintenanceLoop) checkDiskPreflight() bool {
	if m.diskFreeBytes == nil || m.diskMinFreeBytes == 0 {
		return false
	}
	free, err := m.diskFreeBytes(m.cityPath)
	if err != nil {
		fmt.Fprintf(m.stderr, "store-maintenance: disk pre-flight probe failed (fail-open): %v\n", err) //nolint:errcheck
		return false
	}
	const gib = float64(1 << 30)
	if free < m.diskMinFreeBytes {
		m.emitDiskEvent(events.StoreDiskCritical, free)
		fmt.Fprintf(m.stderr, //nolint:errcheck
			"store-maintenance: disk CRITICAL — %.1f GiB free (floor %.1f GiB) on %s; skipping CALL DOLT_GC\n",
			float64(free)/gib, float64(m.diskMinFreeBytes)/gib, m.cityPath)
		return true
	}
	if m.diskWarnFreeBytes > 0 && free < m.diskWarnFreeBytes {
		m.emitDiskEvent(events.StoreDiskWarn, free)
		fmt.Fprintf(m.stderr, //nolint:errcheck
			"store-maintenance: disk WARN — %.1f GiB free (warn %.1f GiB) on %s; proceeding\n",
			float64(free)/gib, float64(m.diskWarnFreeBytes)/gib, m.cityPath)
	}
	return false
}

// emitDiskEvent records a StoreDiskWarn or StoreDiskCritical event.
// Best-effort: JSON marshal errors are silently dropped.
func (m *StoreMaintenanceLoop) emitDiskEvent(eventType string, free int64) {
	if m.recorder == nil {
		return
	}
	var payload events.Payload
	switch eventType {
	case events.StoreDiskWarn:
		payload = events.StoreDiskWarnPayload{
			FreeBytes:  free,
			WarnBytes:  m.diskWarnFreeBytes,
			FloorBytes: m.diskMinFreeBytes,
			DataDir:    m.cityPath,
		}
	case events.StoreDiskCritical:
		payload = events.StoreDiskCriticalPayload{
			FreeBytes:  free,
			FloorBytes: m.diskMinFreeBytes,
			DataDir:    m.cityPath,
		}
	default:
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	m.recorder.Record(events.Event{
		Type:    eventType,
		Actor:   maintenanceActor,
		Subject: m.cityPath,
		Ts:      m.clock(),
		Payload: raw,
	})
}

// sendFailureAlert posts one best-effort operator alert mail for a
// failed maintenance run. It is a no-op when Mail is unset or AlertTo
// is empty; Send errors are logged to stderr but never propagate. The
// subject and body shape is stable and documented in the runbook
// (ga-d5y / ga-sec).
func (m *StoreMaintenanceLoop) sendFailureAlert(run MaintenanceRun) {
	if m.mail == nil || m.cfg.AlertTo == "" {
		return
	}
	duration := run.FinishedAt.Sub(run.StartedAt).Seconds()
	if duration < 0 {
		duration = 0
	}
	nextRetry := run.StartedAt.Add(m.cfg.IntervalOrDefault()).UTC().Format(time.RFC3339)

	subject := fmt.Sprintf("[ALERT] Dolt store maintenance failed: %s", run.Stage)
	var body strings.Builder
	fmt.Fprintf(&body, "Dolt store maintenance run failed.\n\n")
	fmt.Fprintf(&body, "Stage:         %s\n", run.Stage)
	fmt.Fprintf(&body, "Error:         %s\n", run.Err)
	fmt.Fprintf(&body, "Duration:      %.3fs\n", duration)
	if run.SnapshotPath != "" {
		fmt.Fprintf(&body, "Snapshot path: %s\n", run.SnapshotPath)
	}
	fmt.Fprintf(&body, "City:          %s\n", m.cityPath)
	fmt.Fprintf(&body, "Next retry:    %s (approximate; actual time subject to jitter)\n", nextRetry)

	if _, err := m.mail.Send(maintenanceActor, m.cfg.AlertTo, subject, body.String()); err != nil {
		fmt.Fprintf(m.stderr, "store-maintenance: alert mail send failed: %v\n", err) //nolint:errcheck // best-effort stderr
	}
}

// appendHistoryLocked appends r to the history ring buffer, dropping
// the oldest entry when the buffer is full. Caller must hold m.mu.
func (m *StoreMaintenanceLoop) appendHistoryLocked(r MaintenanceRun) {
	m.history = append(m.history, r)
	if len(m.history) > maintenanceHistorySize {
		m.history = m.history[len(m.history)-maintenanceHistorySize:]
	}
}

// gcSweepResult reports the measured outcome of a runDoltGC sweep so the
// cycle can populate MaintenanceRun.BeforeBytes/AfterBytes. Skipped is
// true when the store-size gate declined to run any GC because the store
// was below the configured floor; in that case BeforeBytes == AfterBytes.
type gcSweepResult struct {
	BeforeBytes int64
	AfterBytes  int64
	Skipped     bool
}

// gcSystemDatabases are the Dolt/MySQL system schemas SHOW DATABASES
// surfaces that must never be GC-swept: they hold no bead chunk data and
// CALL DOLT_GC against them is meaningless. Mirrors the system-DB sets in
// cmd/gc (dolt_sql_health.go, dolt_cleanup_drop_planner.go); kept as a
// local copy because internal/supervisor must not import the cmd/gc top
// layer. Keys are lowercase; gcTargetDatabases lowercases before lookup.
var gcSystemDatabases = map[string]struct{}{
	"information_schema": {},
	"mysql":              {},
	"performance_schema": {},
	"sys":                {},
	"dolt":               {},
	"dolt_cluster":       {},
	"__gc_probe":         {},
}

// gcTargetDatabases returns the subset of all that should be swept by
// CALL DOLT_GC: every database minus the well-known system schemas and
// blank entries. Input order is preserved and original casing is retained
// so the names round-trip back into USE statements. Pure function; no I/O.
func gcTargetDatabases(all []string) []string {
	out := make([]string, 0, len(all))
	for _, db := range all {
		name := strings.TrimSpace(db)
		if name == "" {
			continue
		}
		if _, sys := gcSystemDatabases[strings.ToLower(name)]; sys {
			continue
		}
		out = append(out, name)
	}
	return out
}

// measureStoreBytes returns the current on-disk store size, or 0 when no
// StoreSizeBytes probe is wired. A negative reading is clamped to 0 so a
// misbehaving probe cannot poison the size gate or the byte deltas.
func (m *StoreMaintenanceLoop) measureStoreBytes() int64 {
	if m.storeSizeBytes == nil {
		return 0
	}
	if n := m.storeSizeBytes(); n > 0 {
		return n
	}
	return 0
}

// runDoltGC sweeps CALL DOLT_GC() across every managed database and runs
// the per-database SELECT COUNT(*) smoke test. Design D4 + D5 from ga-d5y,
// extended to a multi-database sweep for ga-uvq: each Dolt database is a
// separate repository with its own chunk store, so reclaiming the whole
// store requires one GC per database.
//
// The sweep is gated on store size: when StoreSizeBytes is wired and the
// store is below Cfg.MinStoreBytesOrDefault(), GC is skipped (dolt_gc on a
// small store reclaims little and is not worth the maintenance lease) and
// the returned result has Skipped=true with BeforeBytes == AfterBytes.
//
// The returned gcSweepResult carries the before/after byte readings even
// on failure (BeforeBytes is measured before any GC runs). A non-nil error
// aggregates every per-database failure via errors.Join; finishCycleLocked
// classifies the run Stage from the first *MaintenanceError in the chain:
//
//   - "gc": factory error, SHOW DATABASES error, a SQL error from CALL
//     DOLT_GC(), or the configured GCTimeout elapsing for some database.
//   - "smoke-test": a SQL error on a post-gc SELECT COUNT(*) or the 5 s
//     smoke deadline elapsing for some database. A per-database zero count
//     is healthy (an empty rig) and is not a failure.
//
// When openDoltOps is nil, runDoltGC returns a zero result and nil error.
func (m *StoreMaintenanceLoop) runDoltGC(ctx context.Context) (gcSweepResult, error) {
	var res gcSweepResult
	if m.openDoltOps == nil {
		return res, nil
	}

	res.BeforeBytes = m.measureStoreBytes()
	if floor := m.cfg.MinStoreBytesOrDefault(); floor > 0 && res.BeforeBytes > 0 && res.BeforeBytes < floor {
		res.Skipped = true
		res.AfterBytes = res.BeforeBytes
		fmt.Fprintf(m.stderr, //nolint:errcheck // best-effort stderr
			"store-maintenance: store %s below floor %s; skipping CALL DOLT_GC\n",
			formatMiB(res.BeforeBytes), formatMiB(floor))
		return res, nil
	}

	ops, err := m.openDoltOps(ctx)
	if err != nil {
		return res, &MaintenanceError{Stage: "gc", Err: fmt.Errorf("open dolt conn: %w", err)}
	}
	defer ops.Close() //nolint:errcheck // best-effort cleanup; underlying pool manages lifecycle

	listCtx, cancelList := context.WithTimeout(ctx, maintenanceSmokeTimeout)
	defer cancelList()
	all, err := ops.ListDatabases(listCtx)
	if err != nil {
		return res, &MaintenanceError{Stage: "gc", Err: fmt.Errorf("list databases: %w", err)}
	}
	targets := gcTargetDatabases(all)
	if len(targets) == 0 {
		fmt.Fprintf(m.stderr, "store-maintenance: no user databases to GC (saw %d system schemas)\n", len(all)) //nolint:errcheck
		res.AfterBytes = res.BeforeBytes
		return res, nil
	}

	var errs []error
	for _, db := range targets {
		if err := m.gcOneDatabase(ctx, ops, db); err != nil {
			errs = append(errs, err)
		}
	}
	res.AfterBytes = m.measureStoreBytes()
	return res, errors.Join(errs...)
}

// gcOneDatabase runs CALL DOLT_GC() then the post-gc smoke test against a
// single database, each under its own deadline (GCTimeoutOrDefault for the
// GC, maintenanceSmokeTimeout for the smoke test). The returned error, when
// non-nil, is a *MaintenanceError whose Stage names the failing phase and
// whose cause is annotated with db so an aggregated sweep error identifies
// which database failed.
func (m *StoreMaintenanceLoop) gcOneDatabase(ctx context.Context, ops DoltOps, db string) error {
	gcCtx, cancelGC := context.WithTimeout(ctx, m.cfg.GCTimeoutOrDefault())
	defer cancelGC()
	if err := ops.ExecGCFor(gcCtx, db); err != nil {
		return &MaintenanceError{Stage: "gc", Err: fmt.Errorf("db %q: %w", db, err)}
	}

	smokeCtx, cancelSmoke := context.WithTimeout(ctx, maintenanceSmokeTimeout)
	defer cancelSmoke()
	// A SQL error means the database is not queryable after GC. The count
	// itself is informational: an empty database is a healthy post-gc state.
	if _, err := ops.SmokeCountFor(smokeCtx, db); err != nil {
		return &MaintenanceError{Stage: "smoke-test", Err: fmt.Errorf("db %q: %w", db, err)}
	}
	return nil
}

// formatMiB renders a byte count as a whole-MiB string for operator log
// lines (e.g. "896 MiB"). Used only for human-readable stderr output.
func formatMiB(b int64) string {
	return fmt.Sprintf("%d MiB", b>>20)
}

// NewSQLDoltOps adapts a *sql.DB opener to the DoltOps interface. The
// returned factory is safe to assign to StoreMaintenanceLoopDeps.OpenDoltOps.
//
// open is called once per maintenance cycle and receives the per-cycle
// context; the returned *sql.DB is closed by the DoltOps' Close method
// when the cycle ends.
func NewSQLDoltOps(open func(ctx context.Context) (*sql.DB, error)) DoltOpsFactory {
	return func(ctx context.Context) (DoltOps, error) {
		db, err := open(ctx)
		if err != nil {
			return nil, err
		}
		return &sqlDoltOps{db: db}, nil
	}
}

// sqlDoltOps implements DoltOps against a *sql.DB pool. ExecGCFor pins its
// USE + CALL DOLT_GC to a single connection so the pool cannot split them
// across sessions; SmokeCountFor uses a qualified table name and so is
// pool-safe without pinning. Close closes the pool.
type sqlDoltOps struct {
	db *sql.DB
}

func (s *sqlDoltOps) ListDatabases(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only iteration; row error surfaced via rows.Err
	var dbs []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		dbs = append(dbs, name)
	}
	return dbs, rows.Err()
}

func (s *sqlDoltOps) ExecGCFor(ctx context.Context, db string) error {
	// USE sets per-session state, so it and CALL DOLT_GC must run on the
	// same connection — a pooled *sql.DB could otherwise route them to
	// different sessions and GC the wrong database.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // returns the connection to the pool
	if _, err := conn.ExecContext(ctx, "USE "+quoteDoltIdent(db)); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "CALL DOLT_GC()")
	return err
}

func (s *sqlDoltOps) SmokeCountFor(ctx context.Context, db string) (int, error) {
	var n int
	// Qualified table reference (db.issues) keeps this pool-safe — no USE,
	// so it does not need a pinned connection.
	q := "SELECT COUNT(*) FROM " + quoteDoltIdent(db) + "." + quoteDoltIdent(maintenanceSmokeTable)
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *sqlDoltOps) Close() error {
	return s.db.Close()
}

// quoteDoltIdent backtick-quotes a SQL identifier and escapes embedded
// backticks by doubling them (MySQL convention). Dolt derives database
// names from repository directory names, which may start with a digit or
// contain characters that require quoting.
func quoteDoltIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// SeedLastRunAt returns the timestamp of the most recent
// gc.store.maintenance.done event recorded by provider, or the zero
// value when no such event exists or the query fails. A zero return
// is the fresh-install signal — the scheduler fires immediately so a
// newly-enabled maintenance loop does not wait a full interval before
// its first run.
//
// Query failures are swallowed by design: maintenance scheduling is
// best-effort and must tolerate a missing or unreadable event log.
func SeedLastRunAt(provider events.Provider) time.Time {
	if provider == nil {
		return time.Time{}
	}
	evts, err := provider.List(events.Filter{Type: events.StoreMaintenanceDone})
	if err != nil {
		return time.Time{}
	}
	var latest time.Time
	for _, e := range evts {
		if e.Ts.After(latest) {
			latest = e.Ts
		}
	}
	return latest
}
