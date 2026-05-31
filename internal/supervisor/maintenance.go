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
}

// StoreMaintenanceLoop runs periodic Dolt store maintenance inside the
// supervisor process. It is a goroutine sibling to
// CachingStore.reconcileLoop; see docs/adr/0002-dolt-store-maintenance-runbook.md
// and the ga-d5y design document for the full state machine.
//
// The zero value is not usable — construct with NewStoreMaintenanceLoop.
type StoreMaintenanceLoop struct {
	cfg            config.DoltMaintenance
	store          beads.Store
	cityPath       string
	recorder       events.Recorder
	stderr         io.Writer
	clock          func() time.Time
	rand           func() float64
	openDoltOps    DoltOpsFactory
	openDoltBackup DoltBackupRunnerFactory
	mail           mail.Provider

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
		cfg:            deps.Cfg,
		store:          deps.Store,
		cityPath:       deps.CityPath,
		recorder:       deps.Recorder,
		stderr:         deps.Stderr,
		clock:          deps.Clock,
		rand:           deps.Rand,
		openDoltOps:    deps.OpenDoltOps,
		openDoltBackup: deps.OpenDoltBackup,
		mail:           deps.Mail,
		lastRunAt:      deps.LastRunAt,
		history:        make([]MaintenanceRun, 0, maintenanceHistorySize),
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
