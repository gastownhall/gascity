package main

import (
	"math/rand"
	"sync"
	"time"
)

// maxSessionAgeTracker records per-agent preemptive-restart thresholds and
// decides whether a session's current runtime instance has lived past its
// configured max age. Follows the same nil-guard pattern as idleTracker:
// a nil tracker disables preemptive restarts entirely.
//
// The tracker layers a per-session randomized jitter on top of the base
// duration so fleets of identically-configured agents don't synchronize
// restarts after a controller start. Jitter is recomputed per (sessionName,
// creationCompleteAt) pair so a freshly-restarted session gets a new
// target — without that, every restart would inherit the same offset and
// the cycle would re-synchronize over time.
type maxSessionAgeTracker interface {
	// shouldRestart reports whether the session identified by sessionName
	// should be preemptively restarted given its current runtime-start
	// anchor (creationCompleteAt, typically session.Metadata["creation_complete_at"])
	// and the current wall clock. Returns false whenever the session is
	// not registered, the anchor is zero, or the elapsed age has not yet
	// reached the configured threshold.
	shouldRestart(sessionName string, creationCompleteAt, now time.Time) bool

	// setConfig configures a session's preemptive-restart bounds. A zero
	// maxAge removes the session from the tracker. jitter ≤ 0 disables
	// jitter (deterministic threshold). Safe to call repeatedly — the
	// tracker will re-roll the jitter window if the configuration changes.
	setConfig(sessionName string, maxAge, jitter time.Duration)
}

// memoryMaxSessionAgeTracker is the production implementation. The jitter
// source is injected so tests can make the per-session offset deterministic.
type memoryMaxSessionAgeTracker struct {
	mu      sync.Mutex
	configs map[string]maxSessionAgeConfig
	offsets map[string]time.Duration // sessionName -> current per-session offset
	// rng backs setConfig's random offset roll. Wrapped in a mutex so
	// concurrent setConfig calls remain safe on the shared generator.
	rngMu sync.Mutex
	rng   *rand.Rand
}

type maxSessionAgeConfig struct {
	maxAge time.Duration
	jitter time.Duration
}

// newMaxSessionAgeTracker creates a tracker with a time-seeded jitter RNG.
// Returns a non-nil tracker; callers pass nil explicitly when the feature
// is disabled for the entire config.
func newMaxSessionAgeTracker() *memoryMaxSessionAgeTracker {
	return &memoryMaxSessionAgeTracker{
		configs: make(map[string]maxSessionAgeConfig),
		offsets: make(map[string]time.Duration),
		//nolint:gosec // jitter only needs uniform distribution, not crypto randomness
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (m *memoryMaxSessionAgeTracker) setConfig(sessionName string, maxAge, jitter time.Duration) {
	if sessionName == "" {
		return
	}
	if maxAge <= 0 {
		m.mu.Lock()
		delete(m.configs, sessionName)
		delete(m.offsets, sessionName)
		m.mu.Unlock()
		return
	}
	var offset time.Duration
	if jitter > 0 {
		m.rngMu.Lock()
		// Uniform in [0, jitter).
		offset = time.Duration(m.rng.Int63n(int64(jitter)))
		m.rngMu.Unlock()
	}
	m.mu.Lock()
	m.configs[sessionName] = maxSessionAgeConfig{maxAge: maxAge, jitter: jitter}
	m.offsets[sessionName] = offset
	m.mu.Unlock()
}

func (m *memoryMaxSessionAgeTracker) shouldRestart(sessionName string, creationCompleteAt, now time.Time) bool {
	if sessionName == "" || creationCompleteAt.IsZero() || now.IsZero() {
		return false
	}
	m.mu.Lock()
	cfg, ok := m.configs[sessionName]
	offset := m.offsets[sessionName]
	m.mu.Unlock()
	if !ok || cfg.maxAge <= 0 {
		return false
	}
	threshold := cfg.maxAge + offset
	return now.Sub(creationCompleteAt) >= threshold
}
