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

// DoltOps is the minimal SQL surface the maintenance loop needs to run
// CALL DOLT_GC() and the post-gc smoke test. Production wraps *sql.DB
// via NewSQLDoltOps; tests supply fakes. Close must release the
// underlying connection exactly once per cycle.
type DoltOps interface {
	// ExecGC runs CALL DOLT_GC() with the supplied context's deadline.
	ExecGC(ctx context.Context) error
	// SmokeCount runs SELECT COUNT(*) FROM issues against the current
	// database and returns the scalar result.
	SmokeCount(ctx context.Context) (int, error)
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

	// Alert dedup state (guarded by mu — written only inside a cycle).
	// alertFingerprint is the stage+error of the failure last alerted; empty
	// means the loop is in the healthy state (or alerts were never sent).
	// Alerts fire on fingerprint CHANGE — a new failure, a different failure,
	// or recovery — never per interval, so a persistently failing store mails
	// the operator once, not every run (plus once on recovery). State is
	// in-memory: a supervisor restart re-alerts a still-failing store once,
	// which doubles as a liveness reminder.
	// alertDelivered says whether alertFingerprint's alert reached the
	// operator; an undelivered one is retried by the next identical run.
	// alertSince is when the current failing streak began, not when its
	// latest shape appeared, so the recovery notice can report the outage.
	// alertSuppressed counts repeats of the latest delivered alert.
	// recoveryOwed means the store came back but the notice for
	// alertFingerprint did not go out, so the retained failure is over and
	// must not suppress a later one.
	alertFingerprint string
	alertDelivered   bool
	recoveryOwed     bool
	alertSince       time.Time
	alertSuppressed  int
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

	if m.checkDiskPreflight() {
		// Disk is critically low — skip both the snapshot and CALL DOLT_GC.
		// Snapshotting the store needs roughly as much free space as the
		// store itself, so a critically-low store would fail the backup and
		// still leave no room for GC. Running the pre-flight before the
		// snapshot means a CRITICAL disk skips both stages rather than
		// attempting a doomed backup that only consumes the last of the disk.
		// The StoreDiskCritical event informs operators; C1
		// (hold-on-store-unreachable) handles downstream safety.
		return m.finishSkippedCycleLocked(started)
	}
	snapshotPath, err := m.runSnapshot(ctx)
	if err != nil {
		return m.finishCycleLocked(started, snapshotPath, err)
	}
	if err := m.runDoltGC(ctx); err != nil {
		return m.finishCycleLocked(started, snapshotPath, err)
	}
	return m.finishCycleLocked(started, snapshotPath, nil)
}

func (m *StoreMaintenanceLoop) finishCycleLocked(started time.Time, snapshotPath string, err error) MaintenanceRun {
	run := MaintenanceRun{
		StartedAt:    started,
		FinishedAt:   m.clock(),
		SnapshotPath: snapshotPath,
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
	return m.recordRunLocked(run)
}

// finishSkippedCycleLocked completes a cycle the disk pre-flight declined to
// start. The stage stays "done" because the loop did not fail, but the
// operator-alert state machine is not consulted at all: a cycle that attempted
// nothing is evidence of nothing, and reading it as a success would close an
// open alert with a recovery notice for a store nobody has touched. This is the
// only completion path that skips the alert machinery, which is why it does not
// go through recordRunLocked. The gc.store.maintenance.done event a skipped
// cycle emits is long-standing behavior and unchanged here; StoreDiskCritical is
// what tells operators why the cycle skipped. Caller must hold m.mu.
func (m *StoreMaintenanceLoop) finishSkippedCycleLocked(started time.Time) MaintenanceRun {
	run := MaintenanceRun{StartedAt: started, FinishedAt: m.clock(), Stage: "done"}
	m.lastRunAt = run.StartedAt
	m.appendHistoryLocked(run)
	m.recordRunEventLocked(run)
	return run
}

// recordRunLocked is the single run-completion point: it advances lastRunAt,
// appends to the history ring and fires the completion side effects. Caller must
// hold m.mu.
func (m *StoreMaintenanceLoop) recordRunLocked(run MaintenanceRun) MaintenanceRun {
	m.lastRunAt = run.StartedAt
	m.appendHistoryLocked(run)
	m.emitRunEventLocked(run)
	return run
}

// emitRunEventLocked is the single run-completion side-effect point: it
// drives the operator-alert state machine and records the typed
// gc.store.maintenance.done or gc.store.maintenance.failed event. The
// failed variant fires when run.Err is non-empty; the done variant
// otherwise. Emission failures are swallowed (the recorder itself is
// best-effort). Caller must hold m.mu, which guards the alert-dedup state
// this mutates. That is the cycle lease, held for the whole maintenance
// run (finishCycleLocked), so the alert mail send under it adds negligible
// hold time to a lease that already spans the multi-minute cycle.
func (m *StoreMaintenanceLoop) emitRunEventLocked(run MaintenanceRun) {
	m.maybeSendAlertLocked(run)
	m.recordRunEventLocked(run)
}

// recordRunEventLocked records the typed gc.store.maintenance.done or
// gc.store.maintenance.failed event for a completed run, without touching the
// operator-alert state machine. Caller must hold m.mu.
func (m *StoreMaintenanceLoop) recordRunEventLocked(run MaintenanceRun) {
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
}

// maybeSendAlertLocked drives the operator-alert state machine after each
// run. Mail fires on failure-fingerprint CHANGE only: entering a failure
// state (or the failure changing shape) sends one alert; identical repeats
// are counted but suppressed; the first success after a failure sends one
// recovery notice.
//
// What the loop OBSERVES and what the operator KNOWS are tracked separately,
// because a mail backend that is down must not cost the streak. alertFingerprint
// is the failure the store is currently in; alertDelivered says whether its
// alert reached the operator. Suppression needs both, so an undelivered alert is
// retried by the next identical run rather than read as a repeat of something
// never received, and a failure whose alert never went out is still the failure
// the recovery notice reports when the store comes back.
//
// The fingerprint is the stage plus the rendered error, which makes the error
// text part of the contract: a failure message that carries a per-run value (a
// clock-derived path, an attempt counter, a temp directory) reads as a new
// condition every cycle and the suppression is worth nothing. The loop must not
// put such a value into its own failure text, and does not. Text that an
// external backup or SQL backend varies per attempt is outside the loop's
// control and will re-alert. Caller must hold m.mu.
func (m *StoreMaintenanceLoop) maybeSendAlertLocked(run MaintenanceRun) {
	fingerprint := ""
	if run.Err != "" {
		fingerprint = run.Stage + "\x00" + run.Err
	}
	if fingerprint == "" {
		if m.alertFingerprint == "" {
			// Healthy→healthy: nothing owed.
			return
		}
		// Failure→success: recovery notice, then back to the healthy state. A
		// notice that cannot be sent is owed, not dropped: the state stays so
		// the next successful run retries it.
		if !m.sendRecoveryNotice(run) {
			m.recoveryOwed = true
			return
		}
		m.clearAlertStateLocked()
		return
	}
	if m.recoveryOwed {
		// The store came back and broke again before the recovery notice could
		// be sent. That notice is stale now, and the retained failure is over,
		// so neither may suppress what follows: this run opens a new outage.
		m.clearAlertStateLocked()
	}
	switch fingerprint {
	case m.alertFingerprint:
		// The same failure again. Suppress it only if the operator actually got
		// the alert; otherwise this run is the retry.
		if m.alertDelivered {
			m.alertSuppressed++
			return
		}
		if m.sendFailureAlert(run) {
			m.alertDelivered = true
			m.alertSuppressed = 0
		}
	default:
		// A new failure, or the failure changed shape mid-streak.
		if m.alertFingerprint == "" {
			// Healthy to failing: this run opens the outage. A shape change
			// later in the same streak does not move the start, so the recovery
			// notice reports how long the store was actually broken rather than
			// how long its most recent symptom lasted.
			m.alertSince = run.StartedAt
		}
		m.alertFingerprint = fingerprint
		m.alertSuppressed = 0
		m.alertDelivered = m.sendFailureAlert(run)
	}
}

// clearAlertStateLocked returns the loop to the healthy state, forgetting the
// failure it was tracking along with anything still owed about it. Caller must
// hold m.mu.
func (m *StoreMaintenanceLoop) clearAlertStateLocked() {
	m.alertFingerprint = ""
	m.alertDelivered = false
	m.recoveryOwed = false
	m.alertSince = time.Time{}
	m.alertSuppressed = 0
}

// checkDiskPreflight checks free space in cityPath's filesystem before the
// disk-growing stages of a maintenance cycle (snapshot, then CALL DOLT_GC).
// Returns true when both stages should be skipped (CRITICAL), false when the
// cycle may proceed. Side-effects: emits
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
			"store-maintenance: disk CRITICAL — %.1f GiB free (floor %.1f GiB) on %s; skipping snapshot and CALL DOLT_GC\n",
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
// failed maintenance run. Callers gate it on fingerprint change (see
// maybeSendAlertLocked) so it fires once per distinct failure, not per
// interval. It reports whether this failure is now accounted for: true when
// the mail went out, and also when there is nothing to send because Mail is
// unset or AlertTo is empty, since no later run can do better. A Send error
// returns false, and is logged to stderr rather than propagated. The subject
// and body shape is stable and documented in the runbook (ga-d5y / ga-sec).
func (m *StoreMaintenanceLoop) sendFailureAlert(run MaintenanceRun) bool {
	if m.mail == nil || m.cfg.AlertTo == "" {
		return true
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
		return false
	}
	return true
}

// sendRecoveryNotice posts one best-effort operator mail when a run
// succeeds after a failure streak, closing the loop opened by
// sendFailureAlert. Caller must hold m.mu (it reads the alert dedup
// state). Like sendFailureAlert it reports whether the notice is
// accounted for: true when it went out and when there is nothing to send
// (Mail unset, AlertTo empty, or there was no failure to close because
// alertFingerprint is empty), false when Send errored, so the next
// successful run retries the notice instead of dropping it. When the
// failure's own alert never went out, the body says so: this notice is the
// operator's first sight of that failure.
func (m *StoreMaintenanceLoop) sendRecoveryNotice(run MaintenanceRun) bool {
	if m.mail == nil || m.cfg.AlertTo == "" || m.alertFingerprint == "" {
		return true
	}
	stage, errMsg, _ := strings.Cut(m.alertFingerprint, "\x00")

	subject := fmt.Sprintf("[RECOVERED] Dolt store maintenance succeeded after failing: %s", stage)
	var body strings.Builder
	fmt.Fprintf(&body, "Dolt store maintenance recovered.\n\n")
	fmt.Fprintf(&body, "Failed stage:  %s\n", stage)
	fmt.Fprintf(&body, "Last error:    %s\n", errMsg)
	if !m.alertSince.IsZero() {
		fmt.Fprintf(&body, "Failing since: %s\n", m.alertSince.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&body, "Repeats:       %d suppressed after the latest alert\n", m.alertSuppressed)
	if !m.alertDelivered {
		fmt.Fprintf(&body, "Note:          the alert for this failure could not be delivered, so this notice is the first you have seen of it.\n")
	}
	fmt.Fprintf(&body, "Recovered at:  %s\n", run.FinishedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&body, "City:          %s\n", m.cityPath)

	if _, err := m.mail.Send(maintenanceActor, m.cfg.AlertTo, subject, body.String()); err != nil {
		fmt.Fprintf(m.stderr, "store-maintenance: recovery mail send failed: %v\n", err) //nolint:errcheck // best-effort stderr
		return false
	}
	return true
}

// appendHistoryLocked appends r to the history ring buffer, dropping
// the oldest entry when the buffer is full. Caller must hold m.mu.
func (m *StoreMaintenanceLoop) appendHistoryLocked(r MaintenanceRun) {
	m.history = append(m.history, r)
	if len(m.history) > maintenanceHistorySize {
		m.history = m.history[len(m.history)-maintenanceHistorySize:]
	}
}

// runDoltGC runs CALL DOLT_GC() followed by the SELECT COUNT(*) smoke
// test against the managed Dolt store. Design D4 + D5 from ga-d5y.
//
// Returns nil on success. A non-nil return is a *MaintenanceError
// whose Stage classifies the failing phase:
//
//   - "gc": factory error, SQL error from CALL DOLT_GC(), or the
//     configured GCTimeout elapsed.
//   - "smoke-test": SQL error on SELECT COUNT(*), the 5 s smoke
//     deadline elapsed, or the query returned 0 rows (which indicates
//     either a corrupted schema or a wiped table and is never a
//     healthy post-gc state for a running city).
//
// When openDoltOps is nil, runDoltGC returns nil.
func (m *StoreMaintenanceLoop) runDoltGC(ctx context.Context) error {
	if m.openDoltOps == nil {
		return nil
	}
	ops, err := m.openDoltOps(ctx)
	if err != nil {
		return &MaintenanceError{Stage: "gc", Err: fmt.Errorf("open dolt conn: %w", err)}
	}
	defer ops.Close() //nolint:errcheck // best-effort cleanup; underlying pool manages lifecycle

	gcCtx, cancelGC := context.WithTimeout(ctx, m.cfg.GCTimeoutOrDefault())
	defer cancelGC()
	if err := ops.ExecGC(gcCtx); err != nil {
		return &MaintenanceError{Stage: "gc", Err: err}
	}

	smokeCtx, cancelSmoke := context.WithTimeout(ctx, maintenanceSmokeTimeout)
	defer cancelSmoke()
	count, err := ops.SmokeCount(smokeCtx)
	if err != nil {
		return &MaintenanceError{Stage: "smoke-test", Err: err}
	}
	if count == 0 {
		return &MaintenanceError{Stage: "smoke-test", Err: errors.New("SELECT COUNT(*) returned 0 rows")}
	}
	return nil
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

// sqlDoltOps implements DoltOps against a *sql.DB pool. ExecGC and
// SmokeCount each take one connection from the pool and return it;
// Close closes the pool.
type sqlDoltOps struct {
	db *sql.DB
}

func (s *sqlDoltOps) ExecGC(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "CALL DOLT_GC()")
	return err
}

func (s *sqlDoltOps) SmokeCount(ctx context.Context) (int, error) {
	var n int
	// LIMIT 1 is redundant on a COUNT(*) aggregate but matches the
	// design-doc literal so the runbook and the code stay in lockstep.
	row := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+maintenanceSmokeTable+"` LIMIT 1")
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *sqlDoltOps) Close() error {
	return s.db.Close()
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
