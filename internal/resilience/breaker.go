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
	"sync/atomic"
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
	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
	// TripDecay keeps the backoff exponent across a quick recovery: a
	// success no longer resets trips, and a trip within TripDecay of the
	// breaker closing doubles the previous cap. Trips reset once the breaker
	// has stayed closed for TripDecay. Zero keeps the default, where any
	// success resets trips.
	TripDecay time.Duration
	// IgnoreSuccessWhileOpen makes a success recorded while open (a
	// straggler admitted before the trip) leave the breaker open. Only a
	// half-open success closes it. False keeps the default, where any
	// success closes the breaker.
	IgnoreSuccessWhileOpen bool
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
// diagnostics. Delivered synchronously from the state-changing call, while
// the breaker's lock is held, so deliveries for one breaker are ordered and
// never concurrent. A callback may call State, Available, Registry.States and
// Registry.Breaker; it must not call any other Breaker or Registry method.
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
	// state is written under mu and read without it, so a transition
	// callback, which runs under mu, can read State (gastownhall/gascity#4839).
	state atomic.Int32
	// failures counts consecutive transport-class failures while closed.
	failures int
	// trips counts consecutive open episodes without an intervening
	// success; it drives the backoff exponent.
	trips int
	// deadline is the earliest probe admission time while open.
	deadline time.Time
	// lastProbeAt is when the most recent half-open probe was admitted;
	// zero once ReleaseProbe resolves it.
	lastProbeAt time.Time
	// closedAt is when the breaker last closed after an open episode.
	closedAt time.Time
}

// Status is a consistent copy of one breaker's mutable state.
type Status struct {
	State    State
	Failures int
	// Trips counts consecutive open episodes; it drives the backoff exponent.
	Trips int
	// Deadline is the earliest probe admission while open.
	Deadline time.Time
	// BackoffCap is the full-jitter cap for the current trip count, zero
	// when Trips is zero.
	BackoffCap  time.Duration
	LastProbeAt time.Time
	// ClosedAt is when the breaker last closed after an open episode, zero
	// if it never opened.
	ClosedAt time.Time
}

func newBreaker(scope, opClass string, settings Settings, onChange func(Transition)) *Breaker {
	settings = settings.withDefaults()
	now := settings.Now
	if now == nil {
		now = time.Now
	}
	return &Breaker{
		scope:    scope,
		opClass:  opClass,
		settings: settings,
		now:      now,
		jitter:   fullJitter,
		onChange: onChange,
	}
}

func (b *Breaker) loadState() State { return State(b.state.Load()) }

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
	allowed, _ := b.AllowProbe()
	return allowed
}

// AllowProbe is Allow that also reports whether the admission is the
// half-open probe, decided under the same lock so exactly one concurrent
// caller learns it holds the probe.
func (b *Breaker) AllowProbe() (allowed, probe bool) {
	if !b.settings.Enabled {
		return true, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch b.loadState() {
	case StateOpen:
		if now.Before(b.deadline) {
			return false, false
		}
		b.transitionLocked(StateHalfOpen, 0, now)
		b.lastProbeAt = now
		return true, true
	case StateHalfOpen:
		if now.Sub(b.lastProbeAt) < b.settings.HalfOpenInterval {
			return false, false
		}
		b.lastProbeAt = now
		return true, true
	default:
		return true, false
	}
}

// ReleaseProbe resolves an inconclusive half-open probe so the next Allow
// admits a new one without waiting out HalfOpenInterval. It is a no-op in
// any other state.
func (b *Breaker) ReleaseProbe() {
	if !b.settings.Enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.loadState() == StateHalfOpen {
		b.lastProbeAt = time.Time{}
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
	return b.loadState() == StateClosed
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
	switch b.loadState() {
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
// direct evidence the store is reachable). IgnoreSuccessWhileOpen and
// TripDecay narrow this; see Settings.
func (b *Breaker) RecordSuccess() {
	if !b.settings.Enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.loadState()
	if state == StateOpen && b.settings.IgnoreSuccessWhileOpen {
		return
	}
	// trips is kept: closedTripsLocked discards it at the next trip unless
	// TripDecay keeps the exponent.
	b.failures = 0
	if state != StateClosed {
		now := b.now()
		b.closedAt = now
		b.transitionLocked(StateClosed, 0, now)
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
	switch b.loadState() {
	case StateOpen:
		// Straggler while open: the episode is already counted.
		return
	case StateHalfOpen:
		b.trips++
		b.openLocked(now)
	default: // closed
		b.failures++
		if b.failures >= b.settings.ConsecutiveFailures {
			b.trips = b.closedTripsLocked(now) + 1
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
	now := b.now()
	switch b.loadState() {
	case StateOpen:
		return
	case StateHalfOpen:
		b.trips++
	default: // closed
		b.trips = b.closedTripsLocked(now) + 1
	}
	b.openLocked(now)
}

// State returns the current breaker state without mutating it. It takes no
// lock, so it is safe to call from a transition callback.
func (b *Breaker) State() State {
	if !b.settings.Enabled {
		return StateClosed
	}
	return b.loadState()
}

// Status returns a consistent copy of the breaker's state. A disabled
// breaker reports a zero closed Status. It takes b.mu, so it must not be
// called from a transition callback.
func (b *Breaker) Status() Status {
	if !b.settings.Enabled {
		return Status{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := Status{
		State:       b.loadState(),
		Failures:    b.failures,
		Trips:       b.trips,
		Deadline:    b.deadline,
		LastProbeAt: b.lastProbeAt,
		ClosedAt:    b.closedAt,
	}
	if st.State == StateClosed {
		st.Trips = b.closedTripsLocked(b.now())
	}
	if st.Trips > 0 {
		st.BackoffCap = backoffCap(b.settings, st.Trips)
	}
	return st
}

// closedTripsLocked returns the trip count a closed breaker carries into
// its next trip: the kept exponent while inside TripDecay of closing,
// otherwise zero. Caller must hold b.mu.
func (b *Breaker) closedTripsLocked(now time.Time) int {
	if b.settings.TripDecay <= 0 || b.closedAt.IsZero() || now.Sub(b.closedAt) >= b.settings.TripDecay {
		return 0
	}
	return b.trips
}

// openLocked moves to StateOpen with a full-jitter backoff deadline.
// Caller must hold b.mu and have set b.trips.
func (b *Breaker) openLocked(now time.Time) {
	backoff := b.jitter(b.backoffCapLocked())
	b.deadline = now.Add(backoff)
	b.transitionLocked(StateOpen, backoff, now)
}

// backoffCapLocked returns the backoff cap for the current trip count.
// Caller must hold b.mu.
func (b *Breaker) backoffCapLocked() time.Duration {
	return backoffCap(b.settings, b.trips)
}

// backoffCap returns min(OpenMax, OpenBase << (trips-1)) with overflow
// protection.
func backoffCap(settings Settings, trips int) time.Duration {
	capDur := settings.OpenBase
	for i := 1; i < trips; i++ {
		capDur *= 2
		if capDur >= settings.OpenMax || capDur <= 0 {
			return settings.OpenMax
		}
	}
	if capDur > settings.OpenMax {
		return settings.OpenMax
	}
	return capDur
}

// transitionLocked changes state and notifies the callback. Caller must
// hold b.mu. The callback runs under b.mu, which keeps deliveries ordered,
// serialized, and fenced by SetOnStateChange; State takes no lock, so a
// callback can still read it.
func (b *Breaker) transitionLocked(to State, backoff time.Duration, now time.Time) {
	from := b.loadState()
	if from == to {
		return
	}
	b.state.Store(int32(to))
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
	return breakerSnapshot{state: b.loadState(), failures: b.failures, trips: b.trips, deadline: b.deadline}
}
