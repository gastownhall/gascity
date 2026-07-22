package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// storeWithoutUpdateTransition deliberately exposes only the base Store
// contract so CachingStore must use its compatibility update path. That path
// still recognizes a definite Update(status=closed) as a real close and emits a
// genuine bead.closed carrying the authoritative closed snapshot.
type storeWithoutUpdateTransition struct {
	beads.Store
}

func TestCachingStoreUpdateCloseEventReachesControllerAutoclose(t *testing.T) {
	previousDispatch := beadCloseAutocloseDispatch
	dispatches := 0
	beadCloseAutocloseDispatch = func(fn func()) { dispatches++; fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = previousDispatch })

	backing := beads.NewMemStore()
	convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create convoy: %v", err)
	}
	closedChild, err := backing.Create(beads.Bead{Title: "already closed", ParentID: convoy.ID})
	if err != nil {
		t.Fatalf("Create closed child: %v", err)
	}
	lastChild, err := backing.Create(beads.Bead{Title: "last open", ParentID: convoy.ID})
	if err != nil {
		t.Fatalf("Create last child: %v", err)
	}
	if err := backing.Close(closedChild.ID); err != nil {
		t.Fatalf("Close first child: %v", err)
	}

	var emitted []events.Event
	cache := beads.NewCachingStoreForTest(storeWithoutUpdateTransition{Store: backing}, func(eventType, beadID string, payload json.RawMessage) {
		emitted = append(emitted, events.Event{
			Type:    eventType,
			Actor:   "cache-write",
			Subject: beadID,
			Payload: payload,
		})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed := "closed"
	if err := cache.Update(lastChild.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("Update last child closed: %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted events = %+v, want one cache write event", emitted)
	}
	// The capability-less update path now emits a genuine bead.closed for this
	// definite close (red-team P1 #8), not the old bead.updated-with-closed-payload
	// compensation.
	if emitted[0].Type != events.BeadClosed {
		t.Fatalf("emitted event type = %q, want %q from a definite capability-less close", emitted[0].Type, events.BeadClosed)
	}

	cs := &controllerState{
		beadStores: map[string]beads.Store{"test": cache},
		pokeCh:     make(chan struct{}, 1),
	}
	cs.applyBeadEventToStores(emitted[0])

	got, err := backing.Get(convoy.ID)
	if err != nil {
		t.Fatalf("Get convoy: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("convoy status = %q, want closed after the cache event reached controller autoclose", got.Status)
	}
	// beadEventAutocloseID routes bead.closed through its early return, never the
	// bead.updated-with-closed-payload branch, so a real close triggers autoclose
	// exactly once — no double-trigger now that the event is a genuine close.
	if dispatches != 1 {
		t.Fatalf("autoclose dispatched %d times for one bead.closed, want exactly one", dispatches)
	}
}
