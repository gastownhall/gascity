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

type freshLivenessStub struct {
	*Fake
	obs   Liveness
	since time.Time
}

func (s *freshLivenessStub) ObserveLivenessSince(_ string, _ []string, since time.Time) (Liveness, error) {
	s.since = since
	return s.obs, nil
}

// A fresh read prefers the capability and passes since through; without it,
// it answers from the error-bearing read. A promoted Running clears Corpse.
// Kills: the capability ignored or since dropped; a corpse that is running.
func TestObserveLivenessSinceUsesCapabilityAndNormalizes(t *testing.T) {
	since := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	stub := &freshLivenessStub{Fake: NewFake(), obs: Liveness{Alive: true, Corpse: true, ObjectID: "$1"}}
	got, err := ObserveLivenessSince(stub, "worker", nil, since)
	if err != nil || got != (Liveness{Running: true, Alive: true, ObjectID: "$1"}) || !stub.since.Equal(since) {
		t.Fatalf("ObserveLivenessSince = (%+v, %v), since %v; want normalized running and since passed", got, err, stub.since)
	}
	plain := NewFake()
	if err := plain.Start(context.Background(), "worker", Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, err := ObserveLivenessSince(plain, "worker", nil, since); err != nil || !got.Running {
		t.Fatalf("ObserveLivenessSince fallback = (%+v, %v), want running", got, err)
	}
}
