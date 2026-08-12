package session

import (
	"testing"
	"time"
)

// TestPendingCreateClaimIsLoadBearingOnObservedRuntime is the Observed:true twin
// of the Observed:false subtests that previously concluded PendingCreateClaim
// does not affect the projection. On the Observed:false path the
// !input.Runtime.Observed bail returns before the PendingCreateClaim branch is
// ever reached, so identical projections there prove nothing about the branch.
//
// Observed:true is the only value either production RuntimeFacts construction
// site uses (cmd/gc/session_reconcile.go:887, cmd/gc/session_sleep.go:144), so
// this is the path that actually runs. Here the claim IS load-bearing: it
// selects a start-requested projection that never consults creatingStateIsStale.
func TestPendingCreateClaimIsLoadBearingOnObservedRuntime(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	newInput := func(claim bool) LifecycleInput {
		return LifecycleInput{
			StoredState:        string(StateCreating),
			PendingCreateClaim: claim,
			LastWokeAt:         "",
			Runtime:            RuntimeFacts{Observed: true, Alive: false},
			// Ancient create against a one-minute staleness budget: any path
			// that reaches creatingStateIsStale must classify this as stale.
			CreatedAt:          now.Add(-24 * time.Hour),
			StaleCreatingAfter: time.Minute,
			Now:                now,
		}
	}

	claimed := ProjectLifecycle(newInput(true))
	unclaimed := ProjectLifecycle(newInput(false))

	if claimed.RuntimeProjection == unclaimed.RuntimeProjection {
		t.Fatalf("PendingCreateClaim did not change the projection on the Observed:true path: both = %q", claimed.RuntimeProjection)
	}
	if got, want := unclaimed.RuntimeProjection, RuntimeProjectionStaleCreating; got != want {
		t.Errorf("unclaimed RuntimeProjection = %q, want %q (ancient create must age out)", got, want)
	}
	if got, want := unclaimed.ReconciledState, StateAsleep; got != want {
		t.Errorf("unclaimed ReconciledState = %q, want %q", got, want)
	}
	if got, want := claimed.RuntimeProjection, RuntimeProjectionStartRequested; got != want {
		t.Errorf("claimed RuntimeProjection = %q, want %q", got, want)
	}
	if got, want := claimed.ReconciledState, StateStartPending; got != want {
		t.Errorf("claimed ReconciledState = %q, want %q", got, want)
	}
	if !claimed.CountsAgainstCap {
		t.Error("claimed CountsAgainstCap = false, want true (a start-requested creating bead holds a capacity slot)")
	}
}

// TestPendingCreateClaimStartRequestedHasNoAgeBound pins the absence of an age
// bound on the claim-gated branch: no matter how old the create is, the
// projection keeps reporting start-requested and keeps counting against
// capacity. Age is varied across four orders of magnitude while every other
// fact is held fixed, so a staleness check added to that branch fails here.
func TestPendingCreateClaimStartRequestedHasNoAgeBound(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
		view := ProjectLifecycle(LifecycleInput{
			StoredState:        string(StateCreating),
			PendingCreateClaim: true,
			LastWokeAt:         "",
			Runtime:            RuntimeFacts{Observed: true, Alive: false},
			CreatedAt:          now.Add(-age),
			StaleCreatingAfter: time.Minute,
			Now:                now,
		})
		if got, want := view.RuntimeProjection, RuntimeProjectionStartRequested; got != want {
			t.Errorf("age %s: RuntimeProjection = %q, want %q", age, got, want)
		}
		if !view.CountsAgainstCap {
			t.Errorf("age %s: CountsAgainstCap = false, want true", age)
		}
	}
}

// TestStartPendingProjectionHasNoAgeBound covers the state the claim-gated
// branch heals a creating bead INTO. CreateOptions{BeadOnly:true} mints session
// intents directly in start-pending with pending_create_claim=true and no
// last_woke_at, so this is also the shape of every fresh never-started create.
// BaseStateStartPending returns start-requested with no staleness input at all.
func TestStartPendingProjectionHasNoAgeBound(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	view := ProjectLifecycle(LifecycleInput{
		StoredState:        string(StateStartPending),
		PendingCreateClaim: true,
		LastWokeAt:         "",
		Runtime:            RuntimeFacts{Observed: true, Alive: false},
		CreatedAt:          now.Add(-30 * 24 * time.Hour),
		StaleCreatingAfter: time.Minute,
		Now:                now,
	})
	if got, want := view.RuntimeProjection, RuntimeProjectionStartRequested; got != want {
		t.Errorf("RuntimeProjection = %q, want %q", got, want)
	}
	if got, want := view.ReconciledState, StateStartPending; got != want {
		t.Errorf("ReconciledState = %q, want %q", got, want)
	}
	if !view.CountsAgainstCap {
		t.Error("CountsAgainstCap = false, want true")
	}
}
