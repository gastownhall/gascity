package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// A scope whose store is unavailable must FREEZE its prior desired state —
// partial-marked templates retain their open session counts — and must never
// silently flatten to zero demand. Other scopes proceed unaffected (scope
// quarantine). Any Ready error marks the scope partial; ErrStoreUnavailable is
// the error these tests use to express the outage.

// unavailableReadyStore wraps a Store and fails Ready with the typed
// ErrStoreUnavailable, simulating a breaker-open scope.
type unavailableReadyStore struct {
	beads.Store
}

func (s *unavailableReadyStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("listing ready beads: %w", beads.ErrStoreUnavailable)
}

func TestDefaultScaleCheckCountsStoreUnavailableQuarantinesOneScope(t *testing.T) {
	healthy := beads.NewMemStore()
	if _, err := healthy.Create(beads.Bead{
		Title:    "routed work",
		Metadata: map[string]string{"gc.routed_to": "hq/worker"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	targets := []defaultScaleCheckTarget{
		{template: "vr/worker", storeKey: "vr", store: &unavailableReadyStore{Store: beads.NewMemStore()}},
		{template: "hq/worker", storeKey: "hq", store: healthy},
	}

	counts, partials, errs := defaultScaleCheckCounts(targets)

	if !partials["vr/worker"] {
		t.Fatalf("partials = %v, want vr/worker marked partial when its store is unavailable", partials)
	}
	if partials["hq/worker"] {
		t.Fatalf("partials = %v — a healthy scope must not be quarantined by another scope's outage", partials)
	}
	if got := counts["hq/worker"]; got != 1 {
		t.Fatalf("counts[hq/worker] = %d, want 1 (healthy scopes proceed)", got)
	}
	if len(errs) == 0 {
		t.Fatal("errs is empty; the unavailable scope must surface a diagnostic, not vanish")
	}
}

func TestRetainScaleCheckPartialPoolDesiredFreezesUnavailableScope(t *testing.T) {
	// The unavailable scope reported zero new demand (counts has no
	// entry); retention must freeze the desired count at the number of
	// currently-running sessions so the reconciler cannot drain the pool
	// on a store outage.
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{
		{
			ID:     "gm-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"template":     "vr/worker",
				"pool_managed": "true",
				"pool_slot":    "1",
				"state":        "awake",
			},
		},
		{
			ID:     "gm-2",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"template":     "vr/worker",
				"pool_managed": "true",
				"pool_slot":    "2",
				"state":        "active",
			},
		},
	})

	// The partial set comes from counting, not from a hand-built map, so the
	// mark is carried from the store failure through to retention.
	counts, partials, _ := defaultScaleCheckCounts([]defaultScaleCheckTarget{
		{template: "vr/worker", storeKey: "vr", store: &unavailableReadyStore{Store: beads.NewMemStore()}},
	})
	if got := counts["vr/worker"]; got != 0 {
		t.Fatalf("counts[vr/worker] = %d, want 0 from the unavailable store", got)
	}
	got := retainScaleCheckPartialPoolDesired(nil, counts, sessionBeads, partials)
	if got["vr/worker"] != 2 {
		t.Fatalf("retained pool desired = %v, want vr/worker frozen at 2 running sessions", got)
	}

	// Without partial marking the same zero-count input drains: the partial
	// marking is what arms the freeze.
	cases := []struct {
		name     string
		partials map[string]bool
	}{
		{name: "no partial templates", partials: nil},
		{name: "another template partial", partials: map[string]bool{"hq/worker": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if drained := retainScaleCheckPartialPoolDesired(nil, nil, sessionBeads, tc.partials); drained["vr/worker"] != 0 {
				t.Fatalf("retention with partials %v = %v, want vr/worker not retained", tc.partials, drained)
			}
		})
	}
}
