package main

import (
	"sync"
	"time"
)

// SpawnStormSafetyNet is the defense-in-depth detector for pool spawn
// storms. It observes worker drain outcomes; when too many workers drain
// without claiming any bead within a sliding window, it declares a storm
// for that template and gates further spawn decisions until the storm
// subsides.
//
// The signal — "worker drained without ever claiming a bead" — is the
// unambiguous trace of the contract mismatch this layer exists to catch:
// when the reconciler's spawn-decision predicate disagrees with what a
// worker can actually claim, the worker has no choice but to drain
// without work. The L0 fix (symmetric ReadyQuery) eliminates the
// asymmetry in the supported case; this safety net catches any future
// asymmetry, regardless of where it originates.
//
// The safety net is a backstop, not the primary mechanism. In healthy
// operation it should never fire.
//
// Effect scope: this layer ONLY suppresses NEW spawn decisions for the
// affected template. It has no API for stopping or draining in-flight
// workers. Workers already mid-execution complete their work normally
// and close their beads; the throttle's blast radius is limited to
// future spawn admissions.
type SpawnStormSafetyNet struct {
	cfg SpawnStormConfig

	mu          sync.Mutex
	perTemplate map[string]*templateStormState
}

// SpawnStormConfig tunes detection sensitivity and throttle behavior.
// Zero values fall through to safe defaults (see effectiveConfig).
type SpawnStormConfig struct {
	// Window over which drain-without-claim outcomes accumulate. Outcomes
	// older than now-Window are evicted before each detection check.
	Window time.Duration

	// DrainThreshold is the number of drain-without-claim outcomes within
	// Window required to declare a storm. Set high enough that legitimate
	// transients (a single failed worker, a quick test) don't trip it.
	DrainThreshold int

	// InitialBackoff is the throttle duration applied on the first
	// detection of a storm episode. Subsequent drain-without-claim
	// outcomes during the same episode double the backoff (capped at
	// MaxBackoff).
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential growth so a persistent
	// misconfiguration doesn't lock out a pool indefinitely. When the
	// backoff window passes with no further drain-without-claim
	// outcomes, the episode resets and the next storm starts from
	// InitialBackoff again.
	MaxBackoff time.Duration
}

func (c SpawnStormConfig) effective() SpawnStormConfig {
	if c.Window <= 0 {
		c.Window = 5 * time.Minute
	}
	if c.DrainThreshold <= 0 {
		c.DrainThreshold = 5
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = 10 * time.Minute
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 60 * time.Minute
	}
	if c.MaxBackoff < c.InitialBackoff {
		c.MaxBackoff = c.InitialBackoff
	}
	return c
}

type templateStormState struct {
	// drains is the sliding-window history of drain-without-claim
	// timestamps, kept in arrival order. Pruned on every observation
	// and every IsThrottled call.
	drains []time.Time

	inStorm        bool
	throttledUntil time.Time
	currentBackoff time.Duration
}

// NewSpawnStormSafetyNet returns a safety net configured with cfg.
// Zero-valued fields use built-in defaults.
func NewSpawnStormSafetyNet(cfg SpawnStormConfig) *SpawnStormSafetyNet {
	return &SpawnStormSafetyNet{
		cfg:         cfg.effective(),
		perTemplate: make(map[string]*templateStormState),
	}
}

// RecordDrainOutcome reports that a worker session for the given template
// has drained. claimed indicates whether the session ever claimed a bead
// during its lifetime. Returns true exactly once per storm-episode
// transition — i.e., when this outcome causes the safety net to declare
// a NEW storm (so the caller can send a one-shot notification).
//
// A claimed=true outcome is healthy and does not contribute to the
// sliding window. A claimed=false outcome is the storm signal.
func (s *SpawnStormSafetyNet) RecordDrainOutcome(template, sessionName string, claimed bool, now time.Time) bool {
	if s == nil {
		return false
	}
	if claimed {
		return false
	}
	template = trimTemplate(template)
	if template == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.perTemplate[template]
	if st == nil {
		st = &templateStormState{}
		s.perTemplate[template] = st
	}

	// Slide the window forward before counting.
	st.drains = pruneBefore(st.drains, now.Add(-s.cfg.Window))
	st.drains = append(st.drains, now)

	if st.inStorm {
		// Extend the throttle exponentially. Each additional
		// drain-without-claim during an active episode signals the
		// underlying condition is still firing.
		st.currentBackoff = doublingCappedAt(st.currentBackoff, s.cfg.MaxBackoff)
		st.throttledUntil = now.Add(st.currentBackoff)
		return false
	}

	if len(st.drains) < s.cfg.DrainThreshold {
		return false
	}

	// Threshold crossed: declare a new storm episode.
	st.inStorm = true
	st.currentBackoff = s.cfg.InitialBackoff
	st.throttledUntil = now.Add(st.currentBackoff)
	return true
}

// IsThrottled reports whether spawn decisions for template should be
// suppressed at time now. As a side effect, it clears expired episodes
// — once an episode's throttle window passes without further
// drain-without-claim outcomes, the sliding window is reset so the next
// storm starts from InitialBackoff rather than the elevated backoff of
// the previous episode.
func (s *SpawnStormSafetyNet) IsThrottled(template string, now time.Time) bool {
	if s == nil {
		return false
	}
	template = trimTemplate(template)
	if template == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.perTemplate[template]
	if st == nil {
		return false
	}

	if !st.throttledUntil.IsZero() && now.Before(st.throttledUntil) {
		return true
	}

	// Throttle window has elapsed. If the episode produced no new
	// drain-without-claim outcomes during the window, transition out:
	// the storm has subsided and the next episode (if any) starts
	// fresh.
	if st.inStorm {
		st.inStorm = false
		st.currentBackoff = 0
		st.throttledUntil = time.Time{}
		st.drains = nil
	}
	return false
}

// trimTemplate normalizes the template name. Templates passed through
// untrusted call paths (CLI, config) can carry whitespace.
func trimTemplate(t string) string {
	// Inline strings.TrimSpace to avoid importing strings just for this
	// — see also other minimal helpers in this package.
	for len(t) > 0 && isSpace(t[0]) {
		t = t[1:]
	}
	for len(t) > 0 && isSpace(t[len(t)-1]) {
		t = t[:len(t)-1]
	}
	return t
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	idx := 0
	for idx < len(ts) && ts[idx].Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return ts
	}
	return append(ts[:0], ts[idx:]...)
}

func doublingCappedAt(current, max time.Duration) time.Duration {
	doubled := current * 2
	if doubled <= 0 || doubled > max {
		return max
	}
	return doubled
}

// applyThrottleToScaleCheckCounts zeroes out demand for any template the
// safety net is currently throttling. Called from buildDesiredState
// directly after defaultScaleCheckCounts populates the map so the
// gate runs before ComputePoolDesiredStates sizes the pool. Returns
// the set of templates whose demand was suppressed so the caller can
// surface diagnostics on a tick that actually gated something.
//
// Safe to call with sn==nil (returns nil set, leaves the map alone) so
// test paths that don't construct a safety net stay on the original
// behavior.
func applyThrottleToScaleCheckCounts(sn *SpawnStormSafetyNet, counts map[string]int, now time.Time) map[string]bool {
	if sn == nil || len(counts) == 0 {
		return nil
	}
	var throttled map[string]bool
	for template, count := range counts {
		if count == 0 {
			continue
		}
		if sn.IsThrottled(template, now) {
			counts[template] = 0
			if throttled == nil {
				throttled = make(map[string]bool)
			}
			throttled[template] = true
		}
	}
	return throttled
}

// activeSpawnStormSafetyNet is the controller-scoped safety net registered
// at city startup. buildDesiredState consults it when gating demand; the
// session reconciler observes drain outcomes through it. Tests that don't
// register one (the vast majority) operate as if the gate were absent.
//
// The pattern: at most one controller per process registers, via
// RegisterSpawnStormSafetyNetForCurrentController; the deferred restore is
// always run on shutdown so subsequent processes start clean. Read paths
// hold the lock only long enough to copy the pointer.
var (
	activeSafetyNetMu sync.RWMutex
	activeSafetyNet   *SpawnStormSafetyNet
)

// RegisterSpawnStormSafetyNetForCurrentController installs sn as the
// active safety net for the lifetime of the returned restore function.
// CityRuntime calls this at startup and defers the restore at shutdown.
// Tests use it to inject a pre-loaded detector for gate-level assertions.
func RegisterSpawnStormSafetyNetForCurrentController(sn *SpawnStormSafetyNet) (restore func()) {
	activeSafetyNetMu.Lock()
	prev := activeSafetyNet
	activeSafetyNet = sn
	activeSafetyNetMu.Unlock()
	return func() {
		activeSafetyNetMu.Lock()
		activeSafetyNet = prev
		activeSafetyNetMu.Unlock()
	}
}

// safetyNetForGating returns the currently registered safety net, or nil
// when no controller is running (tests, one-shot CLI invocations).
func safetyNetForGating() *SpawnStormSafetyNet {
	activeSafetyNetMu.RLock()
	defer activeSafetyNetMu.RUnlock()
	return activeSafetyNet
}

// resolveSpawnStormSafetyNet returns p when non-nil, otherwise a fresh
// detector with default tuning. Production controllers (newCityRuntime)
// call this so every controller has a working safety net without forcing
// every callsite to construct one. Tests that want the gate disabled can
// disable detection by passing a detector configured with a very high
// threshold; the cleanest path is just to never register, which is
// already handled by run() being a no-op when this returns nil — but for
// production correctness we always want one.
func resolveSpawnStormSafetyNet(p *SpawnStormSafetyNet) *SpawnStormSafetyNet {
	if p != nil {
		return p
	}
	return NewSpawnStormSafetyNet(SpawnStormConfig{})
}
