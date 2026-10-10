package closeorder_test

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/closeorder"
)

// perIDStore hides MemStore's batched dep read and counts per-id reads.
type perIDStore struct {
	beads.Store
	depLists int
}

func (s *perIDStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depLists++
	return s.Store.DepList(id, direction)
}

// batchCountingStore counts both read shapes on a store that has the batch.
type batchCountingStore struct {
	*beads.MemStore
	batches, depLists int
	unsupported       bool
}

func (s *batchCountingStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	s.batches++
	if s.unsupported {
		return nil, beads.ErrDepListBatchUnsupported
	}
	return s.MemStore.DepListBatch(ids)
}

func (s *batchCountingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depLists++
	return s.MemStore.DepList(id, direction)
}

// chain builds c blocked-by b blocked-by a, plus an unrelated d, and returns
// the ids in reverse dependency order so ordering has work to do.
func chain(t *testing.T, store *beads.MemStore) (ids []string, a, b, c, d string) {
	t.Helper()
	mk := func(title string) string {
		bead, err := store.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		return bead.ID
	}
	a, b, c, d = mk("a"), mk("b"), mk("c"), mk("d")
	if err := store.DepAdd(c, b, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(b, a, "blocks"); err != nil {
		t.Fatal(err)
	}
	return []string{c, d, b, a}, a, b, c, d
}

func TestOrderPutsBlockersFirstOnEveryReadPath(t *testing.T) {
	mem := beads.NewMemStore()
	ids, a, b, c, d := chain(t, mem)
	want := []string{d, a, b, c}

	perID := &perIDStore{Store: mem}
	got, err := closeorder.Order(perID, ids)
	if err != nil {
		t.Fatalf("Order (per id): %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Order (per id) = %v, want %v", got, want)
	}
	if perID.depLists != len(ids) {
		t.Fatalf("per-id path made %d DepList calls, want %d", perID.depLists, len(ids))
	}

	batched := &batchCountingStore{MemStore: mem}
	got, err = closeorder.Order(batched, ids)
	if err != nil {
		t.Fatalf("Order (batch): %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Order (batch) = %v, want %v (same order as the per-id path)", got, want)
	}
	if batched.batches != 1 || batched.depLists != 0 {
		t.Fatalf("batch path made %d batch and %d per-id reads, want 1 and 0", batched.batches, batched.depLists)
	}

	unsupported := &batchCountingStore{MemStore: mem, unsupported: true}
	got, err = closeorder.Order(unsupported, ids)
	if err != nil {
		t.Fatalf("Order (unsupported batch): %v", err)
	}
	if !slices.Equal(got, want) || unsupported.depLists != len(ids) {
		t.Fatalf("Order (unsupported batch) = %v with %d per-id reads, want %v with %d", got, unsupported.depLists, want, len(ids))
	}
}
