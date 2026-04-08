package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// failingStore wraps a MemStore and forces Ready/List to fail. Used to
// exercise the per-rig fallback semantics in precomputeBulkRoutedCounts.
type failingStore struct {
	beads.Store
}

func (failingStore) Ready() ([]beads.Bead, error) {
	return nil, errors.New("simulated dolt failure")
}

func (failingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("simulated dolt failure")
}

func mustSeed(t *testing.T, store beads.Store, in beads.Bead) beads.Bead {
	t.Helper()
	out, err := store.Create(in)
	if err != nil {
		t.Fatalf("seed bead: %v", err)
	}
	if in.Status != "" && in.Status != out.Status {
		status := in.Status
		if err := store.Update(out.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("seed bead status: %v", err)
		}
		out.Status = status
	}
	return out
}

func TestPrecomputeBulkRoutedCounts_GroupsByRoutedTo(t *testing.T) {
	store := beads.NewMemStore()
	// Routed to alpha: one ready unassigned, one ready assigned (excluded),
	// one in-progress unassigned, one in-progress assigned (excluded).
	mustSeed(t, store, beads.Bead{Title: "r1", Metadata: map[string]string{"gc.routed_to": "alpha"}})
	mustSeed(t, store, beads.Bead{Title: "r2", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "alpha"}})
	mustSeed(t, store, beads.Bead{Title: "p1", Status: "in_progress", Metadata: map[string]string{"gc.routed_to": "alpha"}})
	mustSeed(t, store, beads.Bead{Title: "p2", Status: "in_progress", Assignee: "worker-2", Metadata: map[string]string{"gc.routed_to": "alpha"}})
	// Routed to beta: two ready unassigned.
	mustSeed(t, store, beads.Bead{Title: "b1", Metadata: map[string]string{"gc.routed_to": "beta"}})
	mustSeed(t, store, beads.Bead{Title: "b2", Metadata: map[string]string{"gc.routed_to": "beta"}})
	// Unrouted: excluded from all counts.
	mustSeed(t, store, beads.Bead{Title: "u1"})

	cfg := &config.City{}
	bulk := precomputeBulkRoutedCounts(map[string]beads.Store{"rig1": store}, cfg)
	if bulk == nil {
		t.Fatal("bulk is nil")
	}
	if !bulk.Covers("rig1") {
		t.Error("rig1 should be covered")
	}
	if got := bulk.Ready["alpha"]; got != 1 {
		t.Errorf("Ready[alpha] = %d, want 1", got)
	}
	if got := bulk.InProgress["alpha"]; got != 1 {
		t.Errorf("InProgress[alpha] = %d, want 1", got)
	}
	if got := bulk.Total("alpha"); got != 2 {
		t.Errorf("Total(alpha) = %d, want 2", got)
	}
	if got := bulk.Total("beta"); got != 2 {
		t.Errorf("Total(beta) = %d, want 2", got)
	}
	if !bulk.Has("alpha") || !bulk.Has("beta") {
		t.Error("Has should be true for both templates")
	}
	if bulk.Has("gamma") {
		t.Error("Has(gamma) should be false")
	}
}

func TestPrecomputeBulkRoutedCounts_PartialRigFailure(t *testing.T) {
	ok := beads.NewMemStore()
	mustSeed(t, ok, beads.Bead{Title: "x", Metadata: map[string]string{"gc.routed_to": "alpha"}})

	stores := map[string]beads.Store{
		"healthy": ok,
		"broken":  failingStore{Store: beads.NewMemStore()},
	}
	bulk := precomputeBulkRoutedCounts(stores, &config.City{})
	if bulk == nil {
		t.Fatal("bulk is nil")
	}
	if !bulk.Covers("healthy") {
		t.Error("healthy rig should be covered")
	}
	if bulk.Covers("broken") {
		t.Error("broken rig must NOT be covered — caller should fall back per-rig")
	}
	if got := bulk.Ready["alpha"]; got != 1 {
		t.Errorf("Ready[alpha] = %d, want 1 (broken rig must not erase healthy counts)", got)
	}
}

func TestBulkRoutedCounts_NilSafe(t *testing.T) {
	var b *BulkRoutedCounts
	if b.Covers("anything") {
		t.Error("nil Covers must return false")
	}
	if b.Has("anything") {
		t.Error("nil Has must return false")
	}
	if b.Total("anything") != 0 {
		t.Error("nil Total must return 0")
	}
}

func TestPrecomputeBulkRoutedCounts_EmptyInputs(t *testing.T) {
	if got := precomputeBulkRoutedCounts(nil, &config.City{}); got != nil {
		t.Error("nil stores should return nil")
	}
	if got := precomputeBulkRoutedCounts(map[string]beads.Store{}, &config.City{}); got != nil {
		t.Error("empty stores should return nil")
	}
	if got := precomputeBulkRoutedCounts(map[string]beads.Store{"r": beads.NewMemStore()}, nil); got != nil {
		t.Error("nil cfg should return nil")
	}
}
