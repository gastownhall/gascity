package resilience

import (
	"testing"
	"time"
)

// runWithinDeadline runs fn and reports whether it finished in time. A
// regression here is a DEADLOCK, not a wrong value, so every test in this file
// runs the subject on its own goroutine and fails on timeout instead of hanging
// the package until the go test binary is killed.
//
// The goroutine is deliberately leaked on timeout: it is parked on b.mu and can
// never be woken, so there is nothing to clean up and the test binary is about
// to fail anyway.
func runWithinDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s deadlocked: the state-change callback is being invoked while b.mu is held, "+
			"so a callback reading breaker state blocks forever on the same non-reentrant mutex", what)
	}
}

// TestBreakerCallbackCanReadStateWithoutDeadlock is the regression for
// dispatching the transition callback under b.mu.
//
// Event and diagnostics wiring wants to read breaker state from the callback,
// which is the whole point of exposing State() and Registry.States(). Both take
// b.mu, and Go mutexes are not reentrant, so invoking the callback while the
// lock is held wedges the calling goroutine permanently on its own lock.
func TestBreakerCallbackCanReadStateWithoutDeadlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		// trigger drives a state change through one of the four public entry
		// points that can transition. Each must dispatch after unlocking.
		trigger func(*Breaker)
	}{
		{"RecordFailure trips closed->open", func(b *Breaker) { b.RecordFailure() }},
		{"Trip forces closed->open", func(b *Breaker) { b.Trip() }},
		{"RecordSuccess closes a tripped breaker", func(b *Breaker) { b.Trip(); b.RecordSuccess() }},
		{"Allow admits the half-open probe", func(b *Breaker) {
			b.Trip()
			// Move past the backoff deadline so Allow performs the
			// open -> half-open transition rather than rejecting.
			b.mu.Lock()
			b.deadline = b.now().Add(-time.Second)
			b.mu.Unlock()
			b.Allow()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
			var seen []State
			reg.SetOnStateChange(func(tr Transition) {
				// The deadlocking read: this is what a diagnostics or event
				// emitter does with a transition.
				seen = append(seen, reg.Breaker(tr.Scope, tr.OpClass).State())
			})
			b := reg.Breaker("/city/rig", "bd")

			runWithinDeadline(t, tc.name, func() { tc.trigger(b) })

			if len(seen) == 0 {
				t.Fatal("callback never fired; the test is not exercising a transition")
			}
		})
	}
}

// TestBreakerCallbackCanReadRegistryStatesWithoutDeadlock covers the other read
// the wiring performs: a fleet-wide snapshot. Registry.States() locks the
// registry and then every breaker, so a callback dispatched under b.mu deadlocks
// on the breaker that is mid-transition.
func TestBreakerCallbackCanReadRegistryStatesWithoutDeadlock(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	// More than one breaker, so States() must walk past the transitioning one.
	reg.Breaker("/city/other", "bd")
	b := reg.Breaker("/city/rig", "bd")

	var snapshot map[Key]State
	reg.SetOnStateChange(func(Transition) { snapshot = reg.States() })

	runWithinDeadline(t, "Registry.States() from the callback", func() { b.Trip() })

	if len(snapshot) == 0 {
		t.Fatal("callback never fired, or States() returned nothing")
	}
}

// TestBreakerCallbackObservesTheCompletedTransition pins the ordering the fix
// depends on: the callback runs AFTER the new state is committed, so a reader
// sees the state the transition describes rather than the one it replaced.
//
// Dispatching post-unlock would be worthless if it exposed a stale read.
func TestBreakerCallbackObservesTheCompletedTransition(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	var reported, observed State
	reg.SetOnStateChange(func(tr Transition) {
		reported = tr.To
		observed = reg.Breaker(tr.Scope, tr.OpClass).State()
	})
	b := reg.Breaker("/city/rig", "bd")

	runWithinDeadline(t, "transition ordering", func() { b.Trip() })

	if reported != StateOpen {
		t.Errorf("Transition.To = %v, want %v", reported, StateOpen)
	}
	if observed != reported {
		t.Errorf("State() during callback = %v, want %v — the callback must see the committed state, not the pre-transition one", observed, reported)
	}
}

// TestBreakerCallbackRewiredMidTransitionDoesNotFireLate guards the capture
// semantics. The notification carries the callback it was captured with, so a
// callback swapped out after the transition is not invoked for a change it was
// never installed for.
func TestBreakerCallbackRewiredMidTransitionDoesNotFireLate(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1})
	firstCalls, secondCalls := 0, 0
	reg.SetOnStateChange(func(Transition) {
		firstCalls++
		// Rewire from inside the callback — legal now that the lock is free.
		reg.SetOnStateChange(func(Transition) { secondCalls++ })
	})
	b := reg.Breaker("/city/rig", "bd")

	runWithinDeadline(t, "rewire from callback", func() { b.Trip() })

	if firstCalls != 1 {
		t.Errorf("original callback fired %d times, want 1", firstCalls)
	}
	if secondCalls != 0 {
		t.Errorf("replacement callback fired %d times for a transition that predates it, want 0", secondCalls)
	}
}
