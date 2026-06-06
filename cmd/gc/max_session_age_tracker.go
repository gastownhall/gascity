package main

import (
	"hash/fnv"
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
// restarts after a controller start. Template fallback jitter is derived from
// (template, sessionName, creationCompleteAt), so freshly-restarted pool
// sessions get a new target without keeping per-lifecycle cache state.
type maxSessionAgeTracker interface {
	// shouldRestart reports whether the session identified by sessionName
	// should be preemptively restarted given its current runtime-start
	// anchor (creationCompleteAt, typically session.Metadata["creation_complete_at"])
	// and the current wall clock. Returns false whenever the session is
	// not registered, the anchor is zero, or the elapsed age has not yet
	// reached the configured threshold. template is the agent's qualified
	// template name and is used as a fallback lookup when the session name
	// is not registered directly.
	shouldRestart(sessionName, template string, creationCompleteAt, now time.Time) bool

	// setConfig configures a session's preemptive-restart bounds. A zero
	// maxAge removes the session from the tracker. jitter ≤ 0 disables
	// jitter (deterministic threshold). Safe to call repeatedly — the
	// tracker will re-roll the jitter window if the configuration changes.
	setConfig(sessionName string, maxAge, jitter time.Duration)

	// setConfigForTemplate configures preemptive-restart bounds for every
	// session belonging to an agent template whose concrete runtime names
	// are minted after controller startup.
	setConfigForTemplate(template string, maxAge, jitter time.Duration)

	// exemptTemplateFallbackForSession prevents one stable session from
	// inheriting the template config. Used for mode="always" named sessions
	// that share a template with pool siblings.
	exemptTemplateFallbackForSession(sessionName string)
}

// memoryMaxSessionAgeTracker is the production implementation. The jitter
// source is injected so tests can make the per-session offset deterministic.
type memoryMaxSessionAgeTracker struct {
	mu                         sync.Mutex
	configs                    map[string]maxSessionAgeConfig
	templateConfigs            map[string]maxSessionAgeConfig
	offsets                    map[string]time.Duration // sessionName -> current per-session offset
	templateFallbackExemptions map[string]bool          // session name -> skip template fallback
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
		configs:                    make(map[string]maxSessionAgeConfig),
		templateConfigs:            make(map[string]maxSessionAgeConfig),
		offsets:                    make(map[string]time.Duration),
		templateFallbackExemptions: make(map[string]bool),
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

func (m *memoryMaxSessionAgeTracker) setConfigForTemplate(template string, maxAge, jitter time.Duration) {
	if template == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if maxAge <= 0 {
		delete(m.templateConfigs, template)
		return
	}
	m.templateConfigs[template] = maxSessionAgeConfig{maxAge: maxAge, jitter: jitter}
}

func (m *memoryMaxSessionAgeTracker) exemptTemplateFallbackForSession(sessionName string) {
	if sessionName == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateFallbackExemptions[sessionName] = true
}

func (m *memoryMaxSessionAgeTracker) shouldRestart(sessionName, template string, creationCompleteAt, now time.Time) bool {
	if sessionName == "" || creationCompleteAt.IsZero() || now.IsZero() {
		return false
	}
	cfg, offset, ok := m.configFor(sessionName, template, creationCompleteAt)
	if !ok || cfg.maxAge <= 0 {
		return false
	}
	threshold := cfg.maxAge + offset
	return now.Sub(creationCompleteAt) >= threshold
}

func (m *memoryMaxSessionAgeTracker) configFor(sessionName, template string, creationCompleteAt time.Time) (maxSessionAgeConfig, time.Duration, bool) {
	m.mu.Lock()
	cfg, ok := m.configs[sessionName]
	if ok {
		offset := m.offsets[sessionName]
		m.mu.Unlock()
		return cfg, offset, true
	}
	if m.templateFallbackExemptions[sessionName] || template == "" {
		m.mu.Unlock()
		return maxSessionAgeConfig{}, 0, false
	}
	cfg, ok = m.templateConfigs[template]
	m.mu.Unlock()
	if !ok {
		return maxSessionAgeConfig{}, 0, false
	}
	return cfg, deterministicMaxSessionAgeOffset(template, sessionName, creationCompleteAt, cfg.jitter), true
}

func deterministicMaxSessionAgeOffset(template, sessionName string, creationCompleteAt time.Time, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(template))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(sessionName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(creationCompleteAt.UTC().Format(time.RFC3339Nano)))
	return time.Duration(h.Sum64() % uint64(jitter))
}
