package resilience

import (
	"sync"
	"time"
)

// OpClassBd is the operation class for bd CLI transport operations
// (subprocess invocations against the managed Dolt backend). All bd
// subprocess traffic for a scope shares one breaker so any chokepoint's
// transport failures protect every other chokepoint.
const OpClassBd = "bd"

// OpClassSessionStart is the operation class for session starts gated by a
// shared serving endpoint's capacity.
const OpClassSessionStart = "session-start"

// Key identifies a breaker: a store scope (canonical scope root path)
// plus an operation class.
type Key struct {
	Scope   string
	OpClass string
}

// Registry hands out shared breakers keyed by (scope, opClass). All
// breakers in a registry share Settings and the state-change callback.
// Safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	settings Settings
	onChange func(Transition)
	// jitter, when set, replaces full jitter on every breaker (tests only).
	jitter   func(capDur time.Duration) time.Duration
	breakers map[Key]*Breaker
}

// NewRegistry creates a registry with the given settings. Zero-valued
// settings fields fall back to package defaults.
func NewRegistry(settings Settings) *Registry {
	return &Registry{
		settings: settings.withDefaults(),
		breakers: make(map[Key]*Breaker),
	}
}

// SetOnStateChange installs the transition callback on the registry and
// every existing breaker. New breakers inherit it. This is the wiring
// point for typed breaker.state_changed event emission.
func (r *Registry) SetOnStateChange(fn func(Transition)) {
	r.mu.Lock()
	r.onChange = fn
	existing := make([]*Breaker, 0, len(r.breakers))
	for _, b := range r.breakers {
		existing = append(existing, b)
	}
	r.mu.Unlock()
	for _, b := range existing {
		b.setOnChangeForRegistry(fn)
	}
}

// Breaker returns the shared breaker for (scope, opClass), creating it on
// first use.
func (r *Registry) Breaker(scope, opClass string) *Breaker {
	key := Key{Scope: scope, OpClass: opClass}
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.breakers[key]; ok {
		return b
	}
	b := newBreaker(scope, opClass, r.settings, r.onChange)
	if r.jitter != nil {
		b.jitter = r.jitter
	}
	r.breakers[key] = b
	return b
}

// Remove forgets the breaker for (scope, opClass). A later Breaker call
// creates a fresh closed one. Callers use it to drop scopes that no longer
// exist; it never fires the transition callback.
func (r *Registry) Remove(scope, opClass string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.breakers, Key{Scope: scope, OpClass: opClass})
}

// SetJitterForTest replaces full jitter with fn on the registry's existing
// and future breakers, so tests can pin or seed backoff deadlines.
func (r *Registry) SetJitterForTest(fn func(capDur time.Duration) time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jitter = fn
	for _, b := range r.breakers {
		b.mu.Lock()
		b.jitter = fn
		b.mu.Unlock()
	}
}

// States returns a snapshot of every breaker's current state, for
// diagnostics surfaces.
func (r *Registry) States() map[Key]State {
	r.mu.Lock()
	breakers := make(map[Key]*Breaker, len(r.breakers))
	for k, b := range r.breakers {
		breakers[k] = b
	}
	r.mu.Unlock()
	out := make(map[Key]State, len(breakers))
	for k, b := range breakers {
		out[k] = b.State()
	}
	return out
}

// Statuses returns a snapshot of every breaker's Status, for traces and
// diagnostics surfaces. Like Breaker.Status, it must not be called from a
// transition callback.
func (r *Registry) Statuses() map[Key]Status {
	r.mu.Lock()
	breakers := make(map[Key]*Breaker, len(r.breakers))
	for k, b := range r.breakers {
		breakers[k] = b
	}
	r.mu.Unlock()
	out := make(map[Key]Status, len(breakers))
	for k, b := range breakers {
		out[k] = b.Status()
	}
	return out
}
