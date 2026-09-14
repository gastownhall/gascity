package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestApplyBeadEventRoutesCacheReconcileEventsAsSnapshots pins the controller
// half of the cache-reconcile flood fix (ga-yoix1).
//
// The reconcile emitter marshals a whole absorbed bead, but Dependencies and
// Needs are omitempty, so a bead with no dependencies produces a payload that
// looks exactly like a bd on_update patch. Fed back through the plain ApplyEvent
// path, the cache treats dependency coverage as unknown and clears the
// is_blocked verdict reconciliation just installed — the divergence that makes
// every later pass re-emit. Routing on the actor is what tells the cache the
// payload is its own snapshot.
func TestApplyBeadEventRoutesCacheReconcileEventsAsSnapshots(t *testing.T) {
	backing := beads.NewMemStore()
	unblocked := false
	created, err := backing.Create(beads.Bead{Title: "backlog issue", IsBlocked: &unblocked})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	payload, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if raw := string(payload); strings.Contains(raw, `"dependencies"`) {
		t.Fatalf("payload %s carries a dependencies key; the omitempty premise no longer holds", raw)
	}

	cs := &controllerState{
		beadStores: map[string]beads.Store{"alpha": cached},
		pokeCh:     make(chan struct{}, 1),
	}
	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadUpdated,
		Actor:   cacheReconcileActor,
		Subject: created.ID,
		Payload: payload,
	})

	got, err := cached.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.IsBlocked == nil {
		t.Fatalf("re-absorbing a cache-reconcile snapshot nilled %s's is_blocked verdict", created.ID)
	}
}
