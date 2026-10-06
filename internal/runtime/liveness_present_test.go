package runtime

import (
	"context"
	"testing"
	"time"
)

// v5 O1: Present is "the name is listed, a corpse included". It is derived
// from Running and Corpse, so acp, subprocess and every other provider that
// reports Running reports Present without setting anything.
// Kills: Present reading Running only (a corpse read as gone) or Corpse only
// (a live session read as gone).
func TestLivenessPresentImpliedByRunning(t *testing.T) {
	for _, tc := range []struct {
		obs  Liveness
		want bool
	}{
		{Liveness{}, false},
		{Liveness{Running: true}, true},
		{Liveness{Running: true, Alive: true}, true},
		{Liveness{Corpse: true, ObjectID: "$7"}, true},
		{Liveness{ObjectID: "$7"}, false},
	} {
		if got := tc.obs.Present(); got != tc.want {
			t.Errorf("%+v.Present() = %v, want %v", tc.obs, got, tc.want)
		}
	}

	// The bare-bool fallback (a provider with no liveness observer).
	sp := NewFake()
	if err := sp.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for name, want := range map[string]bool{"worker": true, "missing": false} {
		got, status, err := ObserveLivenessBounded(context.Background(), sp, name, nil, time.Minute)
		if err != nil || status != ObservationComplete {
			t.Fatalf("ObserveLivenessBounded(%s) = %v, %v", name, status, err)
		}
		if got.Present() != want || got.Present() != got.Running || got.Corpse || got.ObjectID != "" {
			t.Errorf("ObserveLivenessBounded(%s) = %+v, want Present = Running = %v and no corpse or object id", name, got, want)
		}
	}
}
