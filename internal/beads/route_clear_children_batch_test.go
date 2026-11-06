package beads_test

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// batchingStore is a store that serves ChildrenOfAny itself and records the
// parents it was asked for, so a test can tell the batched read from the
// per-parent one.
type batchingStore struct {
	beads.Store
	batches [][]string
}

func (s *batchingStore) ChildrenOfAny(parentIDs []string, _ ...beads.QueryOpt) ([]beads.Bead, error) {
	s.batches = append(s.batches, slices.Clone(parentIDs))
	return []beads.Bead{{ID: "batched-child"}}, nil
}

// The route-clearing decorator forwards the backing's batched children read: a
// cache above it asserts ChildrenBatchLister on its backing, and a decorator
// that hid it sent a subtree walk back to one children read per member.
func TestRouteClearForwardsChildrenOfAnyToABatchingBacking(t *testing.T) {
	backing := &batchingStore{Store: beads.NewMemStore()}
	wrapped := beads.WithRouteChangeClearing(backing, nil)

	batch, ok := wrapped.(beads.ChildrenBatchLister)
	if !ok {
		t.Fatalf("%T does not advertise ChildrenOfAny", wrapped)
	}
	got, err := batch.ChildrenOfAny([]string{"p1", "p2"})
	if err != nil {
		t.Fatalf("ChildrenOfAny: %v", err)
	}
	if len(got) != 1 || got[0].ID != "batched-child" {
		t.Errorf("ChildrenOfAny = %v, want the backing's batched answer", got)
	}
	if len(backing.batches) != 1 || !slices.Equal(backing.batches[0], []string{"p1", "p2"}) {
		t.Errorf("backing batches = %v, want one batch of [p1 p2]", backing.batches)
	}
}

// A backing with no batched read is answered per parent, closed children
// included and each child once, so the forward never claims a read the
// backing cannot serve.
func TestRouteClearChildrenOfAnyWithoutABackingBatchReadsPerParent(t *testing.T) {
	mem := beads.NewMemStore()
	p1, err := mem.Create(beads.Bead{Title: "p1"})
	if err != nil {
		t.Fatalf("Create p1: %v", err)
	}
	p2, err := mem.Create(beads.Bead{Title: "p2"})
	if err != nil {
		t.Fatalf("Create p2: %v", err)
	}
	open, err := mem.Create(beads.Bead{Title: "open", ParentID: p1.ID})
	if err != nil {
		t.Fatalf("Create open: %v", err)
	}
	closed, err := mem.Create(beads.Bead{Title: "closed", ParentID: p2.ID})
	if err != nil {
		t.Fatalf("Create closed: %v", err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := beads.ChildrenOfAny(beads.WithRouteChangeClearing(mem, nil), []string{p1.ID, p2.ID})
	if err != nil {
		t.Fatalf("ChildrenOfAny: %v", err)
	}
	var ids []string
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	slices.Sort(ids)
	want := []string{open.ID, closed.ID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("ChildrenOfAny ids = %v, want %v", ids, want)
	}
}
