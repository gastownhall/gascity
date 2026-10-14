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

// getCountingGuardedStore counts the row reads the emitter makes around a
// guarded batch.
type getCountingGuardedStore struct {
	*beads.MemStore
	gets int
}

func (s *getCountingGuardedStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.MemStore.Get(id)
}

func (s *getCountingGuardedStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return s.MemStore, true
}

// The emitter reads each row once after the batch, and before it only the
// rows whose status the batch writes (an update that leaves the status alone
// cannot open or close the row), and every item still emits.
func TestEmittingClassStoreGuardedBatchReadsEachRowOnce(t *testing.T) {
	cityPath := t.TempDir()
	leaf := &getCountingGuardedStore{MemStore: beads.NewMemStore()}
	tagged, err := leaf.Create(beads.Bead{Title: "tagged"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closing, err := leaf.Create(beads.Bead{Title: "closing"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &emittingClassStore{Store: leaf, cityPath: cityPath}
	closed := "closed"
	if _, err := store.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbone", Title: "one"}},
		{Create: &beads.Bead{ID: "gc-gbtwo", Title: "two"}},
		{Update: &beads.GuardedUpdate{ID: tagged.ID, ExpectedRevision: &tagged.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "v"}}}},
		{Update: &beads.GuardedUpdate{ID: closing.ID, ExpectedRevision: &closing.Revision, Opts: beads.UpdateOpts{Status: &closed}}},
	}}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	if want := 4 + 1; leaf.gets != want {
		t.Errorf("emitter read rows %d times, want %d: one snapshot per item and one pre-read for the status write", leaf.gets, want)
	}
	got := map[string]string{}
	for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
		got[evt.Subject] = evt.Type
	}
	want := map[string]string{
		"gc-gbone": events.BeadCreated, "gc-gbtwo": events.BeadCreated,
		tagged.ID: events.BeadUpdated, closing.ID: events.BeadClosed,
	}
	for id, typ := range want {
		if got[id] != typ {
			t.Errorf("event for %s = %q, want %q (all events %v)", id, got[id], typ, got)
		}
	}
}
