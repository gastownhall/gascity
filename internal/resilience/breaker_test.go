package resilience

import (
	"sync"
	"testing"
	"time"
)

// testClock is a manually advanced clock for deterministic breaker tests.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// maxJitter pins full jitter to its upper bound so open deadlines are
// deterministic in tests.
func maxJitter(capDur time.Duration) time.Duration { return capDur }

func newTestBreaker(t *testing.T, settings Settings, clock *testClock, onChange func(Transition)) *Breaker {
	t.Helper()
	b := newBreaker("scope-a", "bd", settings.withDefaults(), onChange)
	b.now = clock.Now
	b.jitter = maxJitter
	return b
}

func TestBreakerStartsClosed(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true}, clock, nil)
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() = %v, want %v", got, StateClosed)
	}
	if !b.Allow() {
		t.Fatal("Allow() = false for a closed breaker, want true")
	}
	if !b.Available() {
		t.Fatal("Available() = false for a closed breaker, want true")
	}
}

func TestBreakerTripsAfterConsecutiveFailures(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 3}, clock, nil)

	b.RecordFailure()
	b.RecordFailure()
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() after 2 failures = %v, want %v", got, StateClosed)
	}
	b.RecordFailure()
	if got := b.State(); got != StateOpen {
		t.Fatalf("State() after 3 failures = %v, want %v", got, StateOpen)
	}
	if b.Allow() {
		t.Fatal("Allow() = true immediately after trip, want false")
	}
	if b.Available() {
		t.Fatal("Available() = true for an open breaker, want false")
	}
}

func TestBreakerTripOpensImmediately(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 3}, clock, nil)

	// Trip opens without crossing the failure threshold.
	b.Trip()
	if got := b.State(); got != StateOpen {
		t.Fatalf("State() after Trip = %v, want %v", got, StateOpen)
	}
	if b.Allow() {
		t.Fatal("Allow() = true immediately after Trip, want false")
	}

	// A success closes it, and a single Trip reopens it.
	b.RecordSuccess()
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() after success = %v, want %v", got, StateClosed)
	}
	b.Trip()
	if got := b.State(); got != StateOpen {
		t.Fatalf("State() after second Trip = %v, want %v", got, StateOpen)
	}
	// Trip while already open is a no-op (does not extend the deadline).
	deadline := b.deadline
	b.Trip()
	if b.deadline != deadline {
		t.Fatalf("Trip while open changed deadline %v -> %v, want no-op", deadline, b.deadline)
	}
}

func TestBreakerTripDisabledIsNoOp(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: false}, clock, nil)
	b.Trip()
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() after Trip on disabled breaker = %v, want %v", got, StateClosed)
	}
}

func TestBreakerSuccessResetsConsecutiveCount(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 3}, clock, nil)

	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess()
	b.RecordFailure()
	b.RecordFailure()
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() = %v, want %v (success must reset the consecutive counter)", got, StateClosed)
	}
	b.RecordFailure()
	if got := b.State(); got != StateOpen {
		t.Fatalf("State() = %v, want %v", got, StateOpen)
	}
}

func tripBreaker(t *testing.T, b *Breaker) {
	t.Helper()
	for i := 0; i < b.settings.ConsecutiveFailures; i++ {
		b.RecordFailure()
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("State() after trip = %v, want %v", got, StateOpen)
	}
}

func TestBreakerOpenAdmitsSingleProbeAfterBackoff(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 3, OpenBase: time.Second, OpenMax: time.Minute}, clock, nil)
	tripBreaker(t, b)

	// First trip backoff cap is OpenBase (1s) and jitter is pinned to max.
	clock.Advance(500 * time.Millisecond)
	if b.Allow() {
		t.Fatal("Allow() = true before the open deadline, want false")
	}
	clock.Advance(600 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("Allow() = false after the open deadline, want one admitted probe")
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("State() after probe admission = %v, want %v", got, StateHalfOpen)
	}
	// Second caller inside the half-open interval is rejected.
	if b.Allow() {
		t.Fatal("Allow() = true for a second caller during half-open, want false")
	}
}

func TestBreakerHalfOpenSuccessCloses(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 3}, clock, nil)
	tripBreaker(t, b)
	clock.Advance(2 * time.Second)
	if !b.Allow() {
		t.Fatal("Allow() = false after backoff, want probe admission")
	}
	b.RecordSuccess()
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() after half-open success = %v, want %v", got, StateClosed)
	}
	if !b.Allow() {
		t.Fatal("Allow() = false after recovery, want true")
	}
}

func TestBreakerHalfOpenFailureReopensWithDoubledBackoff(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 3, OpenBase: time.Second, OpenMax: time.Minute}, clock, nil)
	tripBreaker(t, b)

	clock.Advance(time.Second) // first backoff: 1s
	if !b.Allow() {
		t.Fatal("Allow() = false after first backoff, want probe admission")
	}
	b.RecordFailure()
	if got := b.State(); got != StateOpen {
		t.Fatalf("State() after failed probe = %v, want %v", got, StateOpen)
	}

	// Second backoff cap doubles to 2s.
	clock.Advance(time.Second)
	if b.Allow() {
		t.Fatal("Allow() = true 1s into a 2s backoff, want false")
	}
	clock.Advance(time.Second + time.Millisecond)
	if !b.Allow() {
		t.Fatal("Allow() = false after the doubled backoff elapsed, want probe admission")
	}
}

func TestBreakerBackoffCapsAtOpenMax(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 1, OpenBase: time.Second, OpenMax: 4 * time.Second}, clock, nil)

	b.RecordFailure() // trip 1: cap 1s
	for i := 0; i < 10; i++ {
		clock.Advance(5 * time.Second) // beyond any cap
		if !b.Allow() {
			t.Fatalf("Allow() = false on probe admission %d, want true", i)
		}
		b.RecordFailure() // re-trip, doubling toward the cap
	}
	// After many re-trips the cap must still be OpenMax: deadline is
	// now + 4s (jitter pinned to the cap), so just before it: rejected.
	clock.Advance(4*time.Second - time.Millisecond)
	if b.Allow() {
		t.Fatal("Allow() = true just before the capped deadline, want false")
	}
	clock.Advance(2 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("Allow() = false after the capped (OpenMax) backoff elapsed, want true")
	}
}

func TestBreakerHalfOpenReadmitsProbeAfterInterval(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 1, HalfOpenInterval: 15 * time.Second}, clock, nil)
	b.RecordFailure()
	clock.Advance(2 * time.Second)
	if !b.Allow() {
		t.Fatal("Allow() = false after backoff, want probe admission")
	}
	// Probe never resolved (caller crashed). Within the interval: reject.
	clock.Advance(10 * time.Second)
	if b.Allow() {
		t.Fatal("Allow() = true 10s into the 15s half-open interval, want false")
	}
	clock.Advance(5*time.Second + time.Millisecond)
	if !b.Allow() {
		t.Fatal("Allow() = false after the half-open interval elapsed, want a fresh probe admission")
	}
}

func TestBreakerSuccessWhileOpenCloses(t *testing.T) {
	// A straggling in-flight operation that succeeds while the breaker is
	// open is direct evidence the store is reachable; mirror the beads-lib
	// breaker and reset to closed.
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 1}, clock, nil)
	b.RecordFailure()
	if got := b.State(); got != StateOpen {
		t.Fatalf("State() = %v, want %v", got, StateOpen)
	}
	b.RecordSuccess()
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() after success-while-open = %v, want %v", got, StateClosed)
	}
}

func TestBreakerFailureWhileOpenKeepsState(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 1, OpenBase: time.Second}, clock, nil)
	b.RecordFailure()
	deadlineBefore := b.snapshot().deadline
	b.RecordFailure() // straggler failure while open: no state change, no backoff growth
	if got := b.State(); got != StateOpen {
		t.Fatalf("State() = %v, want %v", got, StateOpen)
	}
	if got := b.snapshot().deadline; !got.Equal(deadlineBefore) {
		t.Fatalf("deadline moved on straggler failure: %v -> %v", deadlineBefore, got)
	}
}

func TestBreakerDisabledIsAlwaysClosed(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: false, ConsecutiveFailures: 1}, clock, nil)
	for i := 0; i < 5; i++ {
		b.RecordFailure()
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("State() = %v, want %v (disabled breaker never trips)", got, StateClosed)
	}
	if !b.Allow() || !b.Available() {
		t.Fatal("disabled breaker must always allow")
	}
}

func TestBreakerProbeDue(t *testing.T) {
	clock := newTestClock()
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 1, OpenBase: time.Second, HalfOpenInterval: 15 * time.Second}, clock, nil)

	if b.ProbeDue() {
		t.Fatal("ProbeDue() = true for a closed breaker, want false")
	}
	b.RecordFailure()
	if b.ProbeDue() {
		t.Fatal("ProbeDue() = true before the open deadline, want false")
	}
	clock.Advance(time.Second + time.Millisecond)
	if !b.ProbeDue() {
		t.Fatal("ProbeDue() = false after the open deadline, want true")
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("ProbeDue must not mutate state: State() = %v, want %v", got, StateOpen)
	}
	if !b.Allow() {
		t.Fatal("Allow() = false when a probe is due, want admission")
	}
	if b.ProbeDue() {
		t.Fatal("ProbeDue() = true right after a probe admission, want false")
	}
	clock.Advance(15*time.Second + time.Millisecond)
	if !b.ProbeDue() {
		t.Fatal("ProbeDue() = false after the half-open interval, want true")
	}
}

func TestBreakerStateChangeCallback(t *testing.T) {
	clock := newTestClock()
	var transitions []Transition
	b := newTestBreaker(t, Settings{Enabled: true, ConsecutiveFailures: 2, OpenBase: time.Second}, clock, func(tr Transition) {
		transitions = append(transitions, tr)
	})

	b.RecordFailure()
	b.RecordFailure() // closed -> open
	clock.Advance(2 * time.Second)
	b.Allow()         // open -> half-open
	b.RecordFailure() // half-open -> open
	clock.Advance(3 * time.Second)
	b.Allow()         // open -> half-open
	b.RecordSuccess() // half-open -> closed

	want := []struct{ from, to State }{
		{StateClosed, StateOpen},
		{StateOpen, StateHalfOpen},
		{StateHalfOpen, StateOpen},
		{StateOpen, StateHalfOpen},
		{StateHalfOpen, StateClosed},
	}
	if len(transitions) != len(want) {
		t.Fatalf("got %d transitions %+v, want %d", len(transitions), transitions, len(want))
	}
	for i, w := range want {
		if transitions[i].From != w.from || transitions[i].To != w.to {
			t.Errorf("transition[%d] = %v->%v, want %v->%v", i, transitions[i].From, transitions[i].To, w.from, w.to)
		}
		if transitions[i].Scope != "scope-a" || transitions[i].OpClass != "bd" {
			t.Errorf("transition[%d] key = (%q,%q), want (scope-a,bd)", i, transitions[i].Scope, transitions[i].OpClass)
		}
		if transitions[i].At.IsZero() {
			t.Errorf("transition[%d].At is zero", i)
		}
	}
	if transitions[0].Failures != 2 {
		t.Errorf("trip transition Failures = %d, want 2", transitions[0].Failures)
	}
	if transitions[0].Backoff <= 0 {
		t.Errorf("trip transition Backoff = %v, want > 0", transitions[0].Backoff)
	}
}

func TestBreakerFullJitterStaysWithinCap(t *testing.T) {
	// The default jitter must return a duration in (0, cap].
	for i := 0; i < 1000; i++ {
		d := fullJitter(time.Second)
		if d <= 0 || d > time.Second {
			t.Fatalf("fullJitter(1s) = %v, want in (0, 1s]", d)
		}
	}
	if d := fullJitter(0); d != 0 {
		t.Fatalf("fullJitter(0) = %v, want 0", d)
	}
}

func TestBreakerConcurrentAccess(_ *testing.T) {
	b := newBreaker("scope-a", "bd", DefaultSettings(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				switch (n + j) % 4 {
				case 0:
					b.RecordFailure()
				case 1:
					b.RecordSuccess()
				case 2:
					b.Allow()
				default:
					_ = b.State()
					_ = b.Available()
					_ = b.ProbeDue()
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestSettingsWithDefaults(t *testing.T) {
	got := Settings{Enabled: true}.withDefaults()
	if got.ConsecutiveFailures != DefaultConsecutiveFailures {
		t.Errorf("ConsecutiveFailures = %d, want %d", got.ConsecutiveFailures, DefaultConsecutiveFailures)
	}
	if got.OpenBase != DefaultOpenBase {
		t.Errorf("OpenBase = %v, want %v", got.OpenBase, DefaultOpenBase)
	}
	if got.OpenMax != DefaultOpenMax {
		t.Errorf("OpenMax = %v, want %v", got.OpenMax, DefaultOpenMax)
	}
	if got.HalfOpenInterval != DefaultHalfOpenInterval {
		t.Errorf("HalfOpenInterval = %v, want %v", got.HalfOpenInterval, DefaultHalfOpenInterval)
	}

	// Explicit values are preserved.
	explicit := Settings{
		Enabled:             true,
		ConsecutiveFailures: 7,
		OpenBase:            2 * time.Second,
		OpenMax:             30 * time.Second,
		HalfOpenInterval:    5 * time.Second,
	}.withDefaults()
	if explicit.ConsecutiveFailures != 7 || explicit.OpenBase != 2*time.Second ||
		explicit.OpenMax != 30*time.Second || explicit.HalfOpenInterval != 5*time.Second {
		t.Errorf("withDefaults() clobbered explicit values: %+v", explicit)
	}

	// OpenMax can never be below OpenBase.
	swapped := Settings{Enabled: true, OpenBase: time.Minute, OpenMax: time.Second}.withDefaults()
	if swapped.OpenMax < swapped.OpenBase {
		t.Errorf("withDefaults() left OpenMax %v < OpenBase %v", swapped.OpenMax, swapped.OpenBase)
	}
}

func TestStateString(t *testing.T) {
	cases := map[State]string{
		StateClosed:   "closed",
		StateOpen:     "open",
		StateHalfOpen: "half-open",
		State(99):     "unknown",
	}
	for state, want := range cases {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}
