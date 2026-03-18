package main

import (
	"regexp"
	"sync"
	"time"
)

// WakeReason describes why a session should be awake.
// Computed fresh each reconciler tick — never stored.
type WakeReason string

const (
	// WakeConfig means an [[agent]] entry exists for this session.
	// For pools, only if the slot is within the desired count.
	WakeConfig WakeReason = "config"
	// WakeAttached means a user terminal is connected to the session.
	WakeAttached WakeReason = "attached"
	// WakeWait means a durable wait is ready for this session continuation.
	WakeWait WakeReason = "wait"
	// WakeWork means the session has hooked/open beads (Phase 4).
	WakeWork WakeReason = "work"
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
}

// drainTracker manages in-memory drain states for all sessions.
type drainTracker struct {
	mu     sync.Mutex
	drains map[string]*drainState // session bead ID -> drain state
}

func newDrainTracker() *drainTracker {
	return &drainTracker{drains: make(map[string]*drainState)}
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

// sessionNamePattern validates session names. Only alphanumerics, hyphens,
// and underscores are allowed. This prevents tmux injection and shell
// escaping issues.
var sessionNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

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

)
