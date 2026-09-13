// Package resilience provides in-process circuit breakers for
// transport-class store failures, keyed by (scope, opClass).
//
// The breaker exists so a wedged backend (managed Dolt server, db-proxy,
// or bd CLI transport) degrades into a cheap, typed
// beads.ErrStoreUnavailable instead of an unbounded pile-up of
// subprocesses and dial timeouts. Semantics are aligned with the
// beads-lib breaker (beads internal/storage/dolt/circuit.go): only
// transport-class failures count, success resets, and recovery happens
// through probing — but this breaker is purely in-memory (no status
// files; the process table and live probes are the source of truth) and
// uses full-jitter exponential backoff for the open state.
//
// The breaker holds no judgment calls: callers classify failures
// mechanically (string/exit-code tables) and the thresholds come from
// configuration.
package resilience

import (
	"math/rand/v2"
	"sync"
	"time"
)

// State is the circuit breaker state.
type State int

// Breaker states. A closed breaker admits everything; an open breaker
// rejects until its backoff deadline; a half-open breaker admits a single
// probe per HalfOpenInterval.
const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

// String returns the lowercase state name.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Default breaker settings, used when the corresponding Settings field is
// zero. The trip threshold of 3 and the 1s→60s open backoff come from the
// city-scale architecture plan (item 1.2).
const (
	DefaultConsecutiveFailures = 3
	DefaultOpenBase            = time.Second
	DefaultOpenMax             = 60 * time.Second
	DefaultHalfOpenInterval    = 15 * time.Second
)

// Settings configures breaker behavior. Zero-valued fields fall back to
// the package defaults; Enabled=false disables tripping entirely (the
// breaker stays closed and admits everything — today's behavior).
type Settings struct {
	// Enabled gates the breaker. Disabled breakers never trip.
	Enabled bool
	// ConsecutiveFailures is the number of consecutive transport-class
	// failures that trips a closed breaker.
	ConsecutiveFailures int
	// OpenBase is the initial open-state backoff cap. Each consecutive
	// re-trip doubles the cap up to OpenMax; the actual wait is drawn
	// with full jitter from (0, cap].
	OpenBase time.Duration
	// OpenMax caps the open-state backoff.
	OpenMax time.Duration
	// HalfOpenInterval is the minimum spacing between probe admissions
	// while half-open, so an unresolved probe (crashed caller) cannot
	// wedge the breaker.
	HalfOpenInterval time.Duration
}

// withDefaults returns a copy with zero fields replaced by defaults and
// OpenMax raised to at least OpenBase.
func (s Settings) withDefaults() Settings {
	if s.ConsecutiveFailures <= 0 {
		s.ConsecutiveFailures = DefaultConsecutiveFailures
	}
	if s.OpenBase <= 0 {
		s.OpenBase = DefaultOpenBase
	}
	if s.OpenMax <= 0 {
		s.OpenMax = DefaultOpenMax
	}
	if s.OpenMax < s.OpenBase {
		s.OpenMax = s.OpenBase
	}
	if s.HalfOpenInterval <= 0 {
		s.HalfOpenInterval = DefaultHalfOpenInterval
	}
	return s
}

// DefaultSettings returns the enabled default breaker configuration.
func DefaultSettings() Settings {
	return Settings{Enabled: true}.withDefaults()
}

// Transition describes a breaker state change, for event emission and
// diagnostics. Delivered synchronously from the state-changing call.
type Transition struct {
	// Scope identifies the store scope (canonical scope root path).
	Scope string
	// OpClass identifies the operation class (e.g. OpClassBd).
	OpClass string
	// From and To are the states on either side of the change.
	From State
	To   State
	// Failures is the consecutive transport-failure count at the change.
	Failures int
	// Backoff is the open-state wait chosen for this episode (zero when
	// transitioning to closed or half-open).
	Backoff time.Duration
	// At is when the transition happened.
	At time.Time
}

// Breaker is a thread-safe circuit breaker for one (scope, opClass).
// Construct via Registry.Breaker.
type Breaker struct {
	scope    string
	opClass  string
	settings Settings

	// now and jitter are injectable for deterministic tests.
	now    func() time.Time
	jitter func(capDur time.Duration) time.Duration

	mu sync.Mutex
	// onChange receives state transitions; guarded by mu so registry
	// rewiring is race-free with state changes.
	onChange func(Transition)
	state    State
	// failures counts consecutive transport-class failures while closed.
	failures int
	// trips counts consecutive open episodes without an intervening
	// success; it drives the backoff exponent.
	trips int
	// deadline is the earliest probe admission time while open.
	deadline time.Time
	// lastProbeAt is when the most recent half-open probe was admitted.
	lastProbeAt time.Time
}

func newBreaker(scope, opClass string, settings Settings, onChange func(Transition)) *Breaker {
	return &Breaker{
		scope:    scope,
		opClass:  opClass,
		settings: settings.withDefaults(),
		now:      time.Now,
		jitter:   fullJitter,
		onChange: onChange,
		state:    StateClosed,
	}
}

// fullJitter draws a wait uniformly from (0, capDur]. Zero or negative caps
// return zero.
func fullJitter(capDur time.Duration) time.Duration {
	if capDur <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(capDur))) + 1
}

// Allow reports whether an operation may proceed. Closed: always true.
// Open: false until the backoff deadline, then the caller is admitted as
// the half-open probe. Half-open: false until HalfOpenInterval has passed
// since the last probe admission, then one more probe is admitted.
//
// Callers admitted while non-closed are probes: their RecordSuccess /
// RecordFailure resolves the half-open state.
func (b *Breaker) Allow() bool {
	if !b.settings.Enabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch b.state {
	case StateOpen:
		if now.Before(b.deadline) {
			return false
		}
		b.transitionLocked(StateHalfOpen, 0, now)
		b.lastProbeAt = now
		return true
	case StateHalfOpen:
		if now.Sub(b.lastProbeAt) < b.settings.HalfOpenInterval {
			return false
		}
		b.lastProbeAt = now
		return true
	default:
		return true
	}
}

// Available reports whether the breaker currently believes the store is
// reachable (state closed). It never mutates state, making it safe for
// read paths that should serve degraded data without consuming the
// half-open probe slot.
func (b *Breaker) Available() bool {
	if !b.settings.Enabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == StateClosed
}

// ProbeDue reports whether a non-closed breaker would currently admit a
// probe, without mutating state. Periodic loops (cache reconcile) use it
// to skip cycles cheaply while open but still run the cycle that performs
// the recovery probe.
func (b *Breaker) ProbeDue() bool {
	if !b.settings.Enabled {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch b.state {
	case StateOpen:
		return !now.Before(b.deadline)
	case StateHalfOpen:
		return now.Sub(b.lastProbeAt) >= b.settings.HalfOpenInterval
	default:
		return false
	}
}

// RecordSuccess records a successful operation. Any success closes the
// breaker and resets the failure count and backoff — including successes
// observed while open (a straggling in-flight operation succeeding is
// direct evidence the store is reachable).
func (b *Breaker) RecordSuccess() {
	if !b.settings.Enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.trips = 0
	if b.state != StateClosed {
		b.transitionLocked(StateClosed, 0, b.now())
	}
}

// RecordFailure records a transport-class failure. Callers must classify
// before calling: application-level errors (bad query, missing bead) must
// NOT be recorded. Trips the breaker after ConsecutiveFailures consecutive
// failures; re-trips a half-open breaker with doubled backoff; is a no-op
// while already open (stragglers don't extend the episode).
func (b *Breaker) RecordFailure() {
	if !b.settings.Enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch b.state {
	case StateOpen:
		// Straggler while open: the episode is already counted.
		return
	case StateHalfOpen:
		b.trips++
		b.openLocked(now)
	default: // closed
		b.failures++
		if b.failures >= b.settings.ConsecutiveFailures {
			b.trips = 1
			b.openLocked(now)
		}
	}
}

// Trip forces the breaker open immediately, without waiting for the failure
// threshold, and arms the standard backoff. Out-of-band health signals (e.g. a
// store-health probe that has already determined the backing store is
// unavailable) use this instead of synthesizing repeated RecordFailure calls,
// so the caller needs no knowledge of the configured threshold. It is a no-op
// while disabled or already open, mirroring RecordFailure.
func (b *Breaker) Trip() {
	if !b.settings.Enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateOpen:
		return
	case StateHalfOpen:
		b.trips++
	default: // closed
		b.trips = 1
	}
	b.openLocked(b.now())
}

// State returns the current breaker state without mutating it.
func (b *Breaker) State() State {
	if !b.settings.Enabled {
		return StateClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// openLocked moves to StateOpen with a full-jitter backoff deadline.
// Caller must hold b.mu and have set b.trips.
func (b *Breaker) openLocked(now time.Time) {
	backoff := b.jitter(b.backoffCapLocked())
	b.deadline = now.Add(backoff)
	b.transitionLocked(StateOpen, backoff, now)
}

// backoffCapLocked returns min(OpenMax, OpenBase << (trips-1)) with
// overflow protection. Caller must hold b.mu.
func (b *Breaker) backoffCapLocked() time.Duration {
	capDur := b.settings.OpenBase
	for i := 1; i < b.trips; i++ {
		capDur *= 2
		if capDur >= b.settings.OpenMax || capDur <= 0 {
			return b.settings.OpenMax
		}
	}
	if capDur > b.settings.OpenMax {
		return b.settings.OpenMax
	}
	return capDur
}

// transitionLocked changes state and notifies the callback. Caller must
// hold b.mu.
func (b *Breaker) transitionLocked(to State, backoff time.Duration, now time.Time) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	if b.onChange != nil {
		b.onChange(Transition{
			Scope:    b.scope,
			OpClass:  b.opClass,
			From:     from,
			To:       to,
			Failures: b.failures,
			Backoff:  backoff,
			At:       now,
		})
	}
}

// setOnChangeForRegistry rewires the transition callback; used by
// Registry.SetOnStateChange so late wiring reaches existing breakers.
func (b *Breaker) setOnChangeForRegistry(fn func(Transition)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onChange = fn
}

// breakerSnapshot is a test-visibility copy of mutable breaker state.
type breakerSnapshot struct {
	state    State
	failures int
	trips    int
	deadline time.Time
}

// snapshot returns a copy of the mutable state for tests.
func (b *Breaker) snapshot() breakerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return breakerSnapshot{state: b.state, failures: b.failures, trips: b.trips, deadline: b.deadline}
}
