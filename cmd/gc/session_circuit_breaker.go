// session_circuit_breaker.go implements a respawn circuit breaker for named
// sessions. The supervisor reconciler will otherwise restart a named session
// indefinitely with zero awareness of loop conditions. When a named session
// is stuck in a respawn loop with no observable progress, this breaker trips
// and blocks further respawn attempts until an operator intervenes (or the
// automatic silence-based reset fires).
//
// Background: in one production incident the "mayor" named session kept being
// auto-respawned because it was assigned to beads it could never actually
// reach. Each respawn produced heavy dolt writes, starving btrfs I/O for
// hours. The breaker here is the minimal infrastructure to interrupt such a
// loop. See also the instructions logged in the ERROR path below for the
// manual reset knob.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// sessionCircuitBreakerConfig controls the breaker thresholds. Zero values
// fall back to package defaults so callers can construct with only the
// fields they want to override.
type sessionCircuitBreakerConfig struct {
	// Window is the rolling window over which restart timestamps are
	// counted. Default: 30 minutes.
	Window time.Duration
	// MaxRestarts is the number of restarts allowed within Window before
	// the breaker considers tripping. Default: 5.
	MaxRestarts int
	// ResetAfter is the idle interval after which an OPEN breaker
	// automatically resets back to CLOSED. Default: 2 * Window.
	ResetAfter time.Duration
}

const (
	defaultCircuitBreakerWindow      = 30 * time.Minute
	defaultCircuitBreakerMaxRestarts = 5
)

func (c sessionCircuitBreakerConfig) withDefaults() sessionCircuitBreakerConfig {
	if c.Window <= 0 {
		c.Window = defaultCircuitBreakerWindow
	}
	if c.MaxRestarts <= 0 {
		c.MaxRestarts = defaultCircuitBreakerMaxRestarts
	}
	if c.ResetAfter <= 0 {
		c.ResetAfter = 2 * c.Window
	}
	return c
}

// circuitBreakerStateKind is the logical state of a single identity's
// breaker entry. CLOSED is the normal case (respawns allowed). OPEN means
// the supervisor MUST NOT materialize or spawn this session.
type circuitBreakerStateKind int

const (
	circuitClosed circuitBreakerStateKind = iota
	circuitOpen
)

func (k circuitBreakerStateKind) String() string {
	switch k {
	case circuitOpen:
		return "CIRCUIT_OPEN"
	default:
		return "CIRCUIT_CLOSED"
	}
}

// circuitBreakerEntry is the in-memory state tracked for a single named
// session identity. All fields are owned by the parent breaker and are only
// read/written with the breaker's mutex held.
type circuitBreakerEntry struct {
	restarts       []time.Time // timestamps within the rolling window
	lastRestart    time.Time
	lastProgress   time.Time
	progressSig    string // last observed assigned-bead status signature
	state          circuitBreakerStateKind
	openedAt       time.Time
	openRestartCnt int // snapshot of restart count at the moment the breaker opened
	loggedOpenOnce bool
}

// CircuitBreakerSnapshot is a point-in-time view of a single identity's
// breaker state. Exposed to the status hook so operators can see who is
// tripped without reaching into breaker internals.
type CircuitBreakerSnapshot struct {
	Identity     string    `json:"identity"`
	State        string    `json:"state"`
	RestartCount int       `json:"restart_count"`
	WindowStart  time.Time `json:"window_start,omitempty"`
	LastRestart  time.Time `json:"last_restart,omitempty"`
	LastProgress time.Time `json:"last_progress,omitempty"`
	OpenedAt     time.Time `json:"opened_at,omitempty"`
	ResetAfter   time.Time `json:"reset_after,omitempty"`
}

// sessionCircuitBreaker tracks restart attempts for named sessions and
// enforces a rolling-window circuit-breaker policy. It is safe for
// concurrent use by multiple reconciler ticks.
type sessionCircuitBreaker struct {
	cfg     sessionCircuitBreakerConfig
	mu      sync.Mutex
	entries map[string]*circuitBreakerEntry
}

// newSessionCircuitBreaker constructs a breaker with the given config.
// Zero-valued config fields fall back to defaults.
func newSessionCircuitBreaker(cfg sessionCircuitBreakerConfig) *sessionCircuitBreaker {
	return &sessionCircuitBreaker{
		cfg:     cfg.withDefaults(),
		entries: make(map[string]*circuitBreakerEntry),
	}
}

// trimLocked discards restart timestamps older than the rolling window. The
// caller must hold b.mu.
func (b *sessionCircuitBreaker) trimLocked(e *circuitBreakerEntry, now time.Time) {
	cutoff := now.Add(-b.cfg.Window)
	i := 0
	for ; i < len(e.restarts); i++ {
		if !e.restarts[i].Before(cutoff) {
			break
		}
	}
	if i > 0 {
		e.restarts = append(e.restarts[:0], e.restarts[i:]...)
	}
}

// maybeAutoResetLocked resets an OPEN entry to CLOSED when no restart has
// been observed for ResetAfter. The caller must hold b.mu.
func (b *sessionCircuitBreaker) maybeAutoResetLocked(e *circuitBreakerEntry, now time.Time) {
	if e.state != circuitOpen {
		return
	}
	// Silence since the last restart attempt. If the supervisor has not
	// even tried to respawn the identity for ResetAfter, we assume the
	// operator (or upstream demand) has cleared whatever caused the loop.
	if e.lastRestart.IsZero() {
		return
	}
	if now.Sub(e.lastRestart) >= b.cfg.ResetAfter {
		e.state = circuitClosed
		e.restarts = nil
		e.openedAt = time.Time{}
		e.openRestartCnt = 0
		e.loggedOpenOnce = false
	}
}

// RecordRestart records a restart attempt for the given identity at time
// `now`. If the rolling-window restart count exceeds the configured max AND
// there is no progress signal inside the window, the entry transitions to
// CIRCUIT_OPEN. Returns the post-record state kind.
func (b *sessionCircuitBreaker) RecordRestart(identity string, now time.Time) circuitBreakerStateKind {
	if identity == "" {
		return circuitClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[identity]
	if e == nil {
		e = &circuitBreakerEntry{}
		b.entries[identity] = e
	}
	b.maybeAutoResetLocked(e, now)
	e.restarts = append(e.restarts, now)
	e.lastRestart = now
	b.trimLocked(e, now)

	if e.state == circuitOpen {
		return e.state
	}
	if len(e.restarts) > b.cfg.MaxRestarts {
		// No progress signal inside the window = trip the breaker. A
		// progress event that landed inside the window keeps us CLOSED.
		if !progressWithinWindow(e, now, b.cfg.Window) {
			e.state = circuitOpen
			e.openedAt = now
			e.openRestartCnt = len(e.restarts)
		}
	}
	return e.state
}

// RecordProgress records an observable progress signal (a bead state
// transition attributable to the identity) at time `now`. Progress events
// do NOT clear an already-OPEN breaker — only automatic reset or the manual
// reset knob can do that — but they do keep a CLOSED breaker from tripping
// even if restarts accumulate.
func (b *sessionCircuitBreaker) RecordProgress(identity string, now time.Time) {
	if identity == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[identity]
	if e == nil {
		e = &circuitBreakerEntry{}
		b.entries[identity] = e
	}
	e.lastProgress = now
}

// ObserveProgressSignature records an arbitrary opaque signature
// describing what the reconciler sees for `identity` (typically a digest of
// its assigned beads' statuses). If the signature has changed since the
// last observation, that counts as a progress event. The first observation
// is NOT counted as progress (there is nothing to compare against yet);
// the reconciler's very first tick after process start should not magically
// reset a breaker that is already OPEN.
func (b *sessionCircuitBreaker) ObserveProgressSignature(identity, sig string, now time.Time) {
	if identity == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[identity]
	if e == nil {
		e = &circuitBreakerEntry{progressSig: sig}
		b.entries[identity] = e
		return
	}
	if e.progressSig == "" {
		e.progressSig = sig
		return
	}
	if e.progressSig != sig {
		e.progressSig = sig
		e.lastProgress = now
	}
}

// IsOpen returns true if the breaker for `identity` is currently OPEN and
// the reconciler MUST NOT materialize or spawn the session. The call may
// transition the entry to CLOSED if the auto-reset window has elapsed.
func (b *sessionCircuitBreaker) IsOpen(identity string, now time.Time) bool {
	if identity == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[identity]
	if e == nil {
		return false
	}
	b.maybeAutoResetLocked(e, now)
	return e.state == circuitOpen
}

// LogOpenOnce writes a loud ERROR-level message the first time a given
// OPEN breaker is observed during respawn suppression. The message tells
// operators exactly how to clear the state. Subsequent calls for the same
// OPEN incident are suppressed to avoid log floods (the supervisor may
// re-check the breaker on every tick).
func (b *sessionCircuitBreaker) LogOpenOnce(identity string, w io.Writer) {
	if identity == "" || w == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[identity]
	if e == nil || e.state != circuitOpen || e.loggedOpenOnce {
		return
	}
	e.loggedOpenOnce = true
	fmt.Fprintf(w, //nolint:errcheck // best-effort stderr
		"ERROR session-circuit-breaker: CIRCUIT_OPEN for named session %q (restarts=%d in last %s, no progress). "+
			"Supervisor will NOT respawn. Run `gc session reset %s` to clear.\n",
		identity, e.openRestartCnt, b.cfg.Window, identity)
}

// Reset forces the entry for `identity` back to CLOSED and discards any
// accumulated restart history. This is invoked from the `gc session reset`
// CLI (cmd_session_reset.go) when an operator clears a tripped breaker.
// Calling Reset on an unknown identity is a no-op.
func (b *sessionCircuitBreaker) Reset(identity string) {
	if identity == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, identity)
}

// Snapshot returns a stable-ordered point-in-time view of all tracked
// identities. Used by status output and by tests.
func (b *sessionCircuitBreaker) Snapshot(now time.Time) []CircuitBreakerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]CircuitBreakerSnapshot, 0, len(b.entries))
	for id, e := range b.entries {
		b.maybeAutoResetLocked(e, now)
		snap := CircuitBreakerSnapshot{
			Identity:     id,
			State:        e.state.String(),
			RestartCount: len(e.restarts),
			LastRestart:  e.lastRestart,
			LastProgress: e.lastProgress,
		}
		if len(e.restarts) > 0 {
			snap.WindowStart = e.restarts[0]
		}
		if e.state == circuitOpen {
			snap.OpenedAt = e.openedAt
			if !e.lastRestart.IsZero() {
				snap.ResetAfter = e.lastRestart.Add(b.cfg.ResetAfter)
			}
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out
}

// progressWithinWindow reports whether a progress event is recent enough
// to keep the breaker CLOSED. "Recent enough" means "no earlier than the
// start of the current restart rolling window", which is `now - window`.
func progressWithinWindow(e *circuitBreakerEntry, now time.Time, window time.Duration) bool {
	if e.lastProgress.IsZero() {
		return false
	}
	return !e.lastProgress.Before(now.Add(-window))
}

// -----------------------------------------------------------------------------
// Package-level singleton used by the reconciler. Kept as an indirection so
// tests can swap it out without threading a new parameter through every
// reconcileSessionBeads call site.
// -----------------------------------------------------------------------------

var (
	sessionCircuitBreakerMu        sync.Mutex
	sessionCircuitBreakerSingleton *sessionCircuitBreaker
)

// defaultSessionCircuitBreaker returns the process-wide breaker, lazily
// constructing it with defaults on first use.
func defaultSessionCircuitBreaker() *sessionCircuitBreaker {
	sessionCircuitBreakerMu.Lock()
	defer sessionCircuitBreakerMu.Unlock()
	if sessionCircuitBreakerSingleton == nil {
		sessionCircuitBreakerSingleton = newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	}
	return sessionCircuitBreakerSingleton
}

// setSessionCircuitBreakerForTest swaps the singleton, returning a cleanup
// function that restores the previous value. Tests call this to inject a
// fake-clocked breaker without touching production wiring.
func setSessionCircuitBreakerForTest(b *sessionCircuitBreaker) func() {
	sessionCircuitBreakerMu.Lock()
	prev := sessionCircuitBreakerSingleton
	sessionCircuitBreakerSingleton = b
	sessionCircuitBreakerMu.Unlock()
	return func() {
		sessionCircuitBreakerMu.Lock()
		sessionCircuitBreakerSingleton = prev
		sessionCircuitBreakerMu.Unlock()
	}
}

// computeNamedSessionProgressSignatures returns a signature per named
// session identity derived from the identities of its assigned work beads
// and their statuses. A signature change between reconciler ticks means a
// bead changed status (open -> in_progress, in_progress -> closed, a new
// bead was routed, an old one dropped, etc.), which is treated as a
// progress signal by the circuit breaker.
//
// Assignee on a work bead may be a bead ID, a session name, or an alias;
// we resolve to the named-session identity via session bead metadata the
// same way the rest of the reconciler does.
func computeNamedSessionProgressSignatures(
	sessionBeads []beads.Bead,
	assignedWorkBeads []beads.Bead,
) map[string]string {
	if len(sessionBeads) == 0 {
		return nil
	}
	// Build: resolver key -> identity. An identity is only relevant to the
	// breaker if it belongs to a configured named session (i.e., the
	// session bead carries the configured_named_identity metadata).
	resolve := make(map[string]string, len(sessionBeads)*3)
	knownIdentities := make(map[string]bool)
	for _, sb := range sessionBeads {
		identity := strings.TrimSpace(sb.Metadata[namedSessionIdentityMetadata])
		if identity == "" {
			continue
		}
		knownIdentities[identity] = true
		resolve[identity] = identity
		if id := strings.TrimSpace(sb.ID); id != "" {
			resolve[id] = identity
		}
		if sn := strings.TrimSpace(sb.Metadata["session_name"]); sn != "" {
			resolve[sn] = identity
		}
		if alias := strings.TrimSpace(sb.Metadata["alias"]); alias != "" {
			resolve[alias] = identity
		}
	}
	if len(knownIdentities) == 0 {
		return nil
	}

	// Gather per-identity (beadID, status) pairs.
	perIdentity := make(map[string][]string, len(knownIdentities))
	for _, wb := range assignedWorkBeads {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			continue
		}
		identity, ok := resolve[assignee]
		if !ok {
			continue
		}
		perIdentity[identity] = append(perIdentity[identity],
			wb.ID+"="+wb.Status)
	}

	out := make(map[string]string, len(knownIdentities))
	for identity := range knownIdentities {
		pairs := perIdentity[identity]
		sort.Strings(pairs)
		h := sha1.Sum([]byte(strings.Join(pairs, "|")))
		out[identity] = hex.EncodeToString(h[:])
	}
	return out
}

// SessionCircuitBreakerSnapshot is the exported status hook: it returns the
// current breaker state for all tracked named-session identities. The
// "gc status" command and any future dashboard can call this to surface
// tripped breakers without reaching into package internals.
func SessionCircuitBreakerSnapshot(now time.Time) []CircuitBreakerSnapshot {
	return defaultSessionCircuitBreaker().Snapshot(now)
}
