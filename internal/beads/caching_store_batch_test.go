package beads

import (
	"encoding/json"
	"errors"
	"testing"
)

// batchBackingSpy is a MemStore that also advertises BatchDeleter, recording the
// batched calls the CachingStore forwards to it. It models the corrected
// `bd delete <ids...> --force` semantics: it deletes exactly the given ids and
// leaves every other bead (including external dependents) alive, so a survivor
// outside the deleted set is expected to remain.
type batchBackingSpy struct {
	*MemStore
	batchCalls [][]string
}

//nolint:unparam // error return satisfies BatchDeleter; the test spy never fails.
func (s *batchBackingSpy) DeleteBatch(ids []string) error {
	s.batchCalls = append(s.batchCalls, append([]string(nil), ids...))
	for _, id := range ids {
		_ = s.Delete(id)
	}
	return nil
}

var _ BatchDeleter = (*batchBackingSpy)(nil)

func mustBatchCreate(t *testing.T, cs *CachingStore, b Bead) Bead {
	t.Helper()
	created, err := cs.Create(b)
	if err != nil {
		t.Fatalf("Create(%+v): %v", b, err)
	}
	return created
}

func TestCachingStoreDeleteBatchForwardsBatchAndEvicts(t *testing.T) {
	backing := &batchBackingSpy{MemStore: NewMemStore()}
	var deletedEvents []string
	cs := NewCachingStoreForTest(backing, func(evtType, beadID string, _ json.RawMessage) {
		if evtType == "bead.deleted" {
			deletedEvents = append(deletedEvents, beadID)
		}
	})

	root := mustBatchCreate(t, cs, Bead{Type: "molecule", Status: "closed"})
	child := mustBatchCreate(t, cs, Bead{Type: "task", Status: "closed"})
	survivor := mustBatchCreate(t, cs, Bead{Type: "task", Status: "open"})
	if err := cs.DepAdd(child.ID, root.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd child->root: %v", err)
	}
	// survivor depends on child: an incoming edge into the deleted closure from a
	// bead OUTSIDE that closure. The batch delete must orphan it, not delete it.
	if err := cs.DepAdd(survivor.ID, child.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd survivor->child: %v", err)
	}

	if err := cs.DeleteBatch([]string{root.ID, child.ID}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}

	// Backing forwarded exactly one batched call carrying both ids.
	if len(backing.batchCalls) != 1 || len(backing.batchCalls[0]) != 2 {
		t.Fatalf("backing batch calls = %v, want one batched call of 2 ids", backing.batchCalls)
	}

	// Deleted beads are evicted and fenced in the cache.
	for _, id := range []string{root.ID, child.ID} {
		if _, err := cs.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v, want ErrNotFound after batch delete", id, err)
		}
		cs.mu.RLock()
		_, stillCached := cs.beads[id]
		_, fenced := cs.deletedSeq[id]
		cs.mu.RUnlock()
		if stillCached {
			t.Errorf("%s still resident in cache after batch delete", id)
		}
		if !fenced {
			t.Errorf("%s missing deletion fence after batch delete", id)
		}
	}

	// Survivor is kept (external dependent orphaned, not deleted), and its stale
	// incoming edge to the deleted child is scrubbed.
	if _, err := cs.Get(survivor.ID); err != nil {
		t.Errorf("survivor Get: %v, want present", err)
	}
	cs.mu.RLock()
	survivorDeps := append([]Dep(nil), cs.deps[survivor.ID]...)
	cs.mu.RUnlock()
	for _, d := range survivorDeps {
		if d.DependsOnID == child.ID {
			t.Errorf("survivor retains cached dep to deleted child: %+v", survivorDeps)
		}
	}

	// bead.deleted fired for both removed beads.
	if !batchContainsAll(deletedEvents, root.ID, child.ID) {
		t.Errorf("deleted events = %v, want %s and %s", deletedEvents, root.ID, child.ID)
	}
}

// A backing store without BatchDeleter must still delete every id, per bead.
func TestCachingStoreDeleteBatchFallsBackPerBead(t *testing.T) {
	cs := NewCachingStoreForTest(NewMemStore(), nil) // MemStore does not implement BatchDeleter
	a := mustBatchCreate(t, cs, Bead{Type: "task", Status: "closed"})
	b := mustBatchCreate(t, cs, Bead{Type: "task", Status: "closed"})

	if err := cs.DeleteBatch([]string{a.ID, b.ID}); err != nil {
		t.Fatalf("DeleteBatch fallback: %v", err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if _, err := cs.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v, want ErrNotFound after fallback delete", id, err)
		}
	}
}

// The per-bead fallback (backing store without BatchDeleter) must honor the same
// orphaning contract as the batched path: an external bead that depends on a
// deleted closure member survives, and its now-dangling edge is scrubbed from
// both the cache and the backing store's dependency table. A bare backing Delete
// (MemStore) drops only the bead row, so DeleteBatch has to strip the edge rows.
func TestCachingStoreDeleteBatchFallbackOrphansExternalDependents(t *testing.T) {
	backing := NewMemStore() // MemStore does not implement BatchDeleter
	cs := NewCachingStoreForTest(backing, nil)

	root := mustBatchCreate(t, cs, Bead{Type: "molecule", Status: "closed"})
	child := mustBatchCreate(t, cs, Bead{Type: "task", Status: "closed"})
	survivor := mustBatchCreate(t, cs, Bead{Type: "task", Status: "open"})
	if err := cs.DepAdd(child.ID, root.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd child->root: %v", err)
	}
	// survivor depends on child from OUTSIDE the deleted closure. The fallback
	// must orphan it (drop the edge), not delete it.
	if err := cs.DepAdd(survivor.ID, child.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd survivor->child: %v", err)
	}

	if err := cs.DeleteBatch([]string{root.ID, child.ID}); err != nil {
		t.Fatalf("DeleteBatch fallback: %v", err)
	}

	// Closure members are gone from cache and backing.
	for _, id := range []string{root.ID, child.ID} {
		if _, err := cs.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v, want ErrNotFound after fallback delete", id, err)
		}
	}

	// Survivor is kept (external dependent orphaned, not deleted).
	if _, err := cs.Get(survivor.ID); err != nil {
		t.Errorf("survivor Get: %v, want present", err)
	}

	// The survivor's stale edge to the deleted child is scrubbed from the cache.
	cs.mu.RLock()
	survivorDeps := append([]Dep(nil), cs.deps[survivor.ID]...)
	cs.mu.RUnlock()
	for _, d := range survivorDeps {
		if d.DependsOnID == child.ID {
			t.Errorf("survivor retains cached dep to deleted child: %+v", survivorDeps)
		}
	}

	// ...and from the backing store's dependency table, so no dangling edge row
	// survives a cache reprime.
	backingDeps, err := backing.DepList(survivor.ID, "down")
	if err != nil {
		t.Fatalf("backing DepList(survivor, down): %v", err)
	}
	for _, d := range backingDeps {
		if d.DependsOnID == child.ID {
			t.Errorf("backing retains dep to deleted child: %+v", backingDeps)
		}
	}
}

// partialBatchBackingSpy models a chunk-committing backend that durably deletes
// only the first `commit` ids and then reports the rest failed via
// *BatchDeleteError — the shape BdStore returns when a later chunk fails after
// earlier chunks committed. It lets a test assert the CachingStore reconciles
// exactly the committed ids on a mid-batch failure.
type partialBatchBackingSpy struct {
	*MemStore
	commit int
}

func (s *partialBatchBackingSpy) DeleteBatch(ids []string) error {
	committed := make([]string, 0, len(ids))
	for i, id := range ids {
		if i >= s.commit {
			return &BatchDeleteError{
				Committed: committed,
				Err:       errors.New("later chunk failed"),
			}
		}
		_ = s.Delete(id)
		committed = append(committed, id)
	}
	return nil
}

var _ BatchDeleter = (*partialBatchBackingSpy)(nil)

// A partial backing failure must not leave the cache divergent from the backing:
// ids the backend durably removed are evicted and fenced, while ids it never
// touched stay resident (evicting them would phantom-delete live beads).
func TestCachingStoreDeleteBatchReconcilesCommittedOnPartialFailure(t *testing.T) {
	backing := &partialBatchBackingSpy{MemStore: NewMemStore(), commit: 1}
	var deletedEvents []string
	cs := NewCachingStoreForTest(backing, func(evtType, beadID string, _ json.RawMessage) {
		if evtType == "bead.deleted" {
			deletedEvents = append(deletedEvents, beadID)
		}
	})

	committedBead := mustBatchCreate(t, cs, Bead{Type: "task", Status: "closed"})
	pendingBead := mustBatchCreate(t, cs, Bead{Type: "task", Status: "closed"})

	err := cs.DeleteBatch([]string{committedBead.ID, pendingBead.ID})
	if err == nil {
		t.Fatalf("DeleteBatch: want error on partial backing failure")
	}

	// The id the backend durably removed is evicted and fenced in the cache, so
	// it never reads as present-but-stale before the next GC tick.
	if _, gErr := cs.Get(committedBead.ID); !errors.Is(gErr, ErrNotFound) {
		t.Errorf("Get(committed) = %v, want ErrNotFound (reconciled)", gErr)
	}
	cs.mu.RLock()
	_, committedCached := cs.beads[committedBead.ID]
	_, committedFenced := cs.deletedSeq[committedBead.ID]
	_, pendingCached := cs.beads[pendingBead.ID]
	cs.mu.RUnlock()
	if committedCached {
		t.Errorf("committed bead still resident after partial failure")
	}
	if !committedFenced {
		t.Errorf("committed bead missing deletion fence after partial failure")
	}

	// The id the backend never removed must stay resident: evicting it would be
	// a phantom delete of a bead that still exists in the backing store.
	if !pendingCached {
		t.Errorf("uncommitted bead wrongly evicted after partial failure")
	}
	if _, gErr := cs.Get(pendingBead.ID); gErr != nil {
		t.Errorf("Get(uncommitted) = %v, want present", gErr)
	}

	// bead.deleted fires only for the committed id.
	if !batchContainsAll(deletedEvents, committedBead.ID) {
		t.Errorf("deleted events = %v, want committed %s", deletedEvents, committedBead.ID)
	}
	for _, id := range deletedEvents {
		if id == pendingBead.ID {
			t.Errorf("deleted event fired for uncommitted bead %s", pendingBead.ID)
		}
	}
}

func batchContainsAll(haystack []string, needles ...string) bool {
	set := make(map[string]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; !ok {
			return false
		}
	}
	return true
}

var _ BatchDeleter = (*CachingStore)(nil)
