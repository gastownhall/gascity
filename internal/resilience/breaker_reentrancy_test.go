package resilience

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// The regressions in this file come from gastownhall/gascity#4839 (Karel
// Bourgois), which reported that the state-change callback deadlocks on any
// breaker-state read. They are kept against the lock-free State() fix that
// supersedes that PR's post-unlock dispatch.

// runWithinDeadline runs fn and fails if it does not return. A regression here
// is a deadlock, not a wrong value, so the subject runs on its own goroutine
// and the test fails instead of hanging the package. The goroutine is leaked
// on timeout: it is parked on b.mu and can never be woken.
func runWithinDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s deadlocked: a state-change callback reading breaker state blocks on b.mu", what)
	}
}

func TestBreakerCallbackCanReadStateWithoutDeadlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger func(*Breaker, *testClock)
		want    State
	}{
		{"RecordFailure trips closed->open", func(b *Breaker, _ *testClock) { b.RecordFailure() }, StateOpen},
		{"Trip forces closed->open", func(b *Breaker, _ *testClock) { b.Trip() }, StateOpen},
		{"RecordSuccess closes a half-open breaker", func(b *Breaker, c *testClock) {
			b.Trip()
			c.Advance(time.Minute)
			b.Allow()
			b.RecordSuccess()
		}, StateClosed},
		{"Allow admits the half-open probe", func(b *Breaker, c *testClock) {
			b.Trip()
			c.Advance(time.Minute)
			b.Allow()
		}, StateHalfOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newTestClock()
			reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1, Now: clock.Now})
			var last Transition
			var observed State
			reg.SetOnStateChange(func(tr Transition) {
				last = tr
				observed = reg.Breaker(tr.Scope, tr.OpClass).State()
			})
			b := reg.Breaker("/city/rig", OpClassBd)

			runWithinDeadline(t, tc.name, func() { tc.trigger(b, clock) })

			if last.To != tc.want {
				t.Fatalf("last transition To = %v, want %v", last.To, tc.want)
			}
			if observed != last.To {
				t.Fatalf("State() during callback = %v, want the committed state %v", observed, last.To)
			}
		})
	}
}

func TestBreakerCallbackCanReadRegistryStatesWithoutDeadlock(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	reg.Breaker("/city/other", OpClassBd)
	b := reg.Breaker("/city/rig", OpClassBd)

	var snapshot map[Key]State
	reg.SetOnStateChange(func(Transition) { snapshot = reg.States() })

	runWithinDeadline(t, "Registry.States() from the callback", func() { b.Trip() })

	if got := snapshot[Key{Scope: "/city/rig", OpClass: OpClassBd}]; got != StateOpen {
		t.Fatalf("States() from the callback reported %v for the transitioning breaker, want %v", got, StateOpen)
	}
}
