package main

import (
	"sync"
	"time"
)

// WakeReason describes why a session should be awake.
// Computed fresh each reconciler tick — never stored.
type WakeReason string

const (
	// WakeConfig means a pool slot is within the config-driven desired count.
	WakeConfig WakeReason = "config"
	// WakeCreate means the session has an explicit create/start claim that the
	// reconciler still needs to satisfy.
	WakeCreate WakeReason = "create"
	// WakeSession keeps an active interactive session running when idle sleep
	// is disabled for that session.
	WakeSession WakeReason = "session"
	// WakeKeepWarm keeps an interactive session warm for its post-detach
	// grace window before it becomes eligible for idle sleep.
	WakeKeepWarm WakeReason = "keep-warm"
	// WakeAttached means a user terminal is connected to the session.
	WakeAttached WakeReason = "attached"
	// WakeWait means a durable wait is ready for this session continuation.
	WakeWait WakeReason = "wait"
	// WakeWork means the session has hooked/open beads (Phase 4).
	WakeWork WakeReason = "work"
	// WakePending means the session is blocked on a structured interaction.
	WakePending WakeReason = "pending"
	// WakePin means pin_awake is set as a durable explicit wake reason.
	WakePin WakeReason = "pin"
	// WakeDependency means another awake session depends on this template.
	WakeDependency WakeReason = "dependency"
)

// ExecSpec defines a validated command for process creation.
// Command is NEVER a shell string — always structured argv.
type ExecSpec struct {
	// Path is the absolute path to the executable.
	Path string
	// Args are the command arguments (no shell interpolation).
	Args []string
	// Env are environment variables for the process.
	Env map[string]string
	// WorkDir is the validated working directory.
	WorkDir string
}

// drainState tracks an in-progress async drain. Ephemeral (in-memory only).
// Lost on controller crash — safe because NDI reconverges.
type drainState struct {
	startedAt  time.Time
	deadline   time.Time
	reason     string // "idle", "pool-excess", "config-drift", "user"
	generation int    // generation at drain start — fence for Stop
	ackSet     bool   // true after GC_DRAIN_ACK has been set by the reconciler
	followUp   bool   // true when the controller should trigger one more immediate tick
}

// idleProbeState tracks an async WaitForIdle probe for interactive idle sleep.
// It stays in-memory only and is consumed on a later reconciler tick.
type idleProbeState struct {
	ready       bool
	success     bool
	completedAt time.Time
}

// drainTracker manages in-memory drain states for all sessions.
type drainTracker struct {
	mu              sync.Mutex
	drains          map[string]*drainState     // session bead ID -> drain state
	idleProbes      map[string]*idleProbeState // session bead ID -> async idle probe
	idleProbeCursor int
}

func newDrainTracker() *drainTracker {
	return &drainTracker{
		drains:     make(map[string]*drainState),
		idleProbes: make(map[string]*idleProbeState),
	}
}

func (dt *drainTracker) get(beadID string) *drainState {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.drains[beadID]
}

func (dt *drainTracker) set(beadID string, ds *drainState) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.drains[beadID] = ds
}

func (dt *drainTracker) remove(beadID string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	delete(dt.drains, beadID)
}

func (dt *drainTracker) all() map[string]*drainState {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	cp := make(map[string]*drainState, len(dt.drains))
	for k, v := range dt.drains {
		cp[k] = v
	}
	return cp
}

func (dt *drainTracker) consumeFollowUpTick() bool {
	if dt == nil {
		return false
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()

	needed := false
	for _, ds := range dt.drains {
		if ds == nil || !ds.followUp {
			continue
		}
		ds.followUp = false
		needed = true
	}
	return needed
}

func (dt *drainTracker) idleProbe(beadID string) (idleProbeState, bool) {
	if dt == nil {
		return idleProbeState{}, false
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	probe, ok := dt.idleProbes[beadID]
	if !ok || probe == nil {
		return idleProbeState{}, false
	}
	return *probe, true
}

func (dt *drainTracker) startIdleProbe(beadID string) *idleProbeState {
	if dt == nil {
		return nil
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if _, exists := dt.idleProbes[beadID]; exists {
		return nil
	}
	probe := &idleProbeState{}
	dt.idleProbes[beadID] = probe
	return probe
}

func (dt *drainTracker) finishIdleProbe(beadID string, probe *idleProbeState, success bool, completedAt time.Time) {
	if dt == nil || probe == nil {
		return
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	current, ok := dt.idleProbes[beadID]
	if !ok || current == nil || current != probe {
		return
	}
	current.ready = true
	current.success = success
	current.completedAt = completedAt
}

func (dt *drainTracker) clearIdleProbe(beadID string) {
	if dt == nil {
		return
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	delete(dt.idleProbes, beadID)
}

// Reconciler tuning defaults.
const (
	// stabilityThreshold is how long a session must survive after wake
	// before it's considered stable (not a rapid exit / crash).
	stabilityThreshold = 30 * time.Second

	// maxWakesPerTick limits how many sessions can be woken per reconciler
	// tick to prevent thundering herd after controller restart.
	defaultMaxWakesPerTick = 5

	// defaultTickBudget is the wall-clock budget per reconciler tick.
	// Remaining work is deferred to the next tick.
	defaultTickBudget = 5 * time.Second

	// orphanGraceTicks is how many ticks an unmatched running session
	// survives before being killed. Prevents killing sessions that are
	// slow to register their beads.
	orphanGraceTicks = 3

	// defaultDrainTimeout is the default time allowed for graceful drain
	// before force-stopping a session.
	defaultDrainTimeout = 5 * time.Minute

	// defaultQuarantineDuration is how long a session is quarantined
	// after exceeding max wake failures.
	defaultQuarantineDuration = 5 * time.Minute

	// defaultMaxWakeAttempts is how many consecutive wake failures before
	// quarantine.
	defaultMaxWakeAttempts = 5

	// churnProductivityThreshold is how long a session must run to be
	// considered productive. Sessions that survive past stabilityThreshold
	// but die before this threshold are "churning" — alive long enough to
	// not count as a rapid crash, but too short to do useful work. This
	// catches the context exhaustion death spiral where gc prime gets
	// re-injected every ~60-90s.
	churnProductivityThreshold = 5 * time.Minute

	// defaultMaxChurnCycles is how many consecutive non-productive
	// wake→die cycles before quarantine. Three cycles means the session
	// failed to be productive three times in a row.
	defaultMaxChurnCycles = 3
)
