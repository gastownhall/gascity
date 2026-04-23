package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
)

// MaintenanceRun summarizes one completed (or failed) maintenance run.
// Stage is "done" for successful runs and names the failing phase
// ("backup" | "gc" | "smoke-test" | "prune") for failed runs. Err is
// empty on success. BeforeBytes / AfterBytes / SnapshotPath are
// populated by the work stages (beads ga-74d, ga-zoj); they are zero
// when the placeholder path runs without real maintenance.
type MaintenanceRun struct {
	StartedAt    time.Time
	FinishedAt   time.Time
	Stage        string
	Err          string
	BeforeBytes  int64
	AfterBytes   int64
	SnapshotPath string
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
}

// StoreMaintenanceLoop runs periodic Dolt store maintenance inside the
// supervisor process. It is a goroutine sibling to
// CachingStore.reconcileLoop; see docs/adr/0002-dolt-store-maintenance-runbook.md
// and the ga-d5y design document for the full state machine.
//
// The zero value is not usable — construct with NewStoreMaintenanceLoop.
type StoreMaintenanceLoop struct {
	cfg      config.DoltMaintenance
	store    beads.Store
	cityPath string
	recorder events.Recorder
	stderr   io.Writer
	clock    func() time.Time
	rand     func() float64

	// mu is the in-process maintenance lease. runOnce holds it for the
	// duration of a single maintenance cycle; the future manual-override
	// API (bead ga-zn8) contends on the same mutex and returns 409 when
	// it cannot acquire it.
	mu        sync.Mutex
	lastRunAt time.Time
	history   []MaintenanceRun
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
		cfg:       deps.Cfg,
		store:     deps.Store,
		cityPath:  deps.CityPath,
		recorder:  deps.Recorder,
		stderr:    deps.Stderr,
		clock:     deps.Clock,
		rand:      deps.Rand,
		lastRunAt: deps.LastRunAt,
		history:   make([]MaintenanceRun, 0, maintenanceHistorySize),
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
		if m.nextDelay(m.clock()) == 0 {
			m.runOnce(ctx)
		}
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
//
// The current body is a placeholder: it logs, records a success run,
// and emits a gc.store.maintenance.done event so SeedLastRunAt recovers
// the schedule across restarts. Real work (dolt backup, CALL DOLT_GC(),
// smoke test) lands with beads ga-74d, ga-zoj.
func (m *StoreMaintenanceLoop) runOnce(ctx context.Context) {
	if !m.mu.TryLock() {
		return
	}
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	started := m.clock()
	fmt.Fprintf(m.stderr, "store-maintenance: would run maintenance for %q\n", m.cityPath) //nolint:errcheck // best-effort stderr
	finished := m.clock()
	run := MaintenanceRun{
		StartedAt:  started,
		FinishedAt: finished,
		Stage:      "done",
	}
	m.lastRunAt = started
	m.appendHistoryLocked(run)
	m.emitRunEvent(run)
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
}

// appendHistoryLocked appends r to the history ring buffer, dropping
// the oldest entry when the buffer is full. Caller must hold m.mu.
func (m *StoreMaintenanceLoop) appendHistoryLocked(r MaintenanceRun) {
	m.history = append(m.history, r)
	if len(m.history) > maintenanceHistorySize {
		m.history = m.history[len(m.history)-maintenanceHistorySize:]
	}
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
