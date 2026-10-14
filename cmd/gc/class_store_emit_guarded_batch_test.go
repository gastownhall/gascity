package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// The emitter's guarded batches are discovered only over a backing that has
// them, resolve to the emitter itself so the batch still emits, and record one
// event per item once the batch lands: bead.created for a create, bead.closed
// for an update that closes.
func TestEmittingClassStoreGuardedBatchEmitsEachItem(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	row, err := leaf.Create(beads.Bead{Title: "row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &emittingClassStore{Store: leaf, cityPath: cityPath}
	batcher, ok := beads.GuardedBatcherFor(store)
	if !ok {
		t.Fatal("GuardedBatcherFor(emitter over MemStore) = false")
	}
	if batcher != beads.GuardedBatcher(store) {
		t.Fatalf("GuardedBatcherFor resolved %T, want the emitter so the batch emits", batcher)
	}
	closed := "closed"
	if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbemitted", Title: "emitted"}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Status: &closed}}},
	}}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	got := map[string]string{}
	for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
		got[evt.Subject] = evt.Type
	}
	if got["gc-gbemitted"] != events.BeadCreated || got[row.ID] != events.BeadClosed {
		t.Fatalf("events = %v, want bead.created for gc-gbemitted and bead.closed for %s", got, row.ID)
	}

	stale := row.Revision
	if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbrefused", Title: "refused"}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &stale, Opts: beads.UpdateOpts{Title: &closed}}},
	}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("ApplyGuarded on a stale revision = %v, want a precondition failure", err)
	}
	for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
		if evt.Subject == "gc-gbrefused" {
			t.Fatalf("a refused batch emitted %s for gc-gbrefused", evt.Type)
		}
	}

	if b, ok := beads.GuardedBatcherFor(&emittingClassStore{Store: storeWithoutBatchedDepRead{Store: beads.NewMemStore()}, cityPath: cityPath}); ok {
		t.Fatalf("GuardedBatcherFor(emitter over an incapable backing) = (%T, true), want no capability", b)
	}
}
