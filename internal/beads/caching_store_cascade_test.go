package beads

import (
	"encoding/json"
	"errors"
	"testing"
)

// cascadeBackingSpy is a MemStore that also advertises CascadeDeleter, recording
// the batched calls the CachingStore forwards to it.
type cascadeBackingSpy struct {
	*MemStore
	cascadeCalls [][]string
}

//nolint:unparam // error return satisfies CascadeDeleter; the test spy never fails.
func (s *cascadeBackingSpy) DeleteCascade(ids []string) error {
	s.cascadeCalls = append(s.cascadeCalls, append([]string(nil), ids...))
	for _, id := range ids {
		_ = s.Delete(id)
	}
	return nil
}

var _ CascadeDeleter = (*cascadeBackingSpy)(nil)

func mustCascadeCreate(t *testing.T, cs *CachingStore, b Bead) Bead {
	t.Helper()
	created, err := cs.Create(b)
	if err != nil {
		t.Fatalf("Create(%+v): %v", b, err)
	}
	return created
}

func TestCachingStoreDeleteCascadeForwardsBatchAndEvicts(t *testing.T) {
	backing := &cascadeBackingSpy{MemStore: NewMemStore()}
	var deletedEvents []string
	cs := NewCachingStoreForTest(backing, func(evtType, beadID string, _ json.RawMessage) {
		if evtType == "bead.deleted" {
			deletedEvents = append(deletedEvents, beadID)
		}
	})

	root := mustCascadeCreate(t, cs, Bead{Type: "molecule", Status: "closed"})
	child := mustCascadeCreate(t, cs, Bead{Type: "task", Status: "closed"})
	survivor := mustCascadeCreate(t, cs, Bead{Type: "task", Status: "open"})
	if err := cs.DepAdd(child.ID, root.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd child->root: %v", err)
	}
	// survivor depends on child: an incoming edge into the deleted closure.
	if err := cs.DepAdd(survivor.ID, child.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd survivor->child: %v", err)
	}

	if err := cs.DeleteCascade([]string{root.ID, child.ID}); err != nil {
		t.Fatalf("DeleteCascade: %v", err)
	}

	// Backing forwarded exactly one batched call carrying both ids.
	if len(backing.cascadeCalls) != 1 || len(backing.cascadeCalls[0]) != 2 {
		t.Fatalf("backing cascade calls = %v, want one batched call of 2 ids", backing.cascadeCalls)
	}

	// Deleted beads are evicted and fenced in the cache.
	for _, id := range []string{root.ID, child.ID} {
		if _, err := cs.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v, want ErrNotFound after cascade", id, err)
		}
		cs.mu.RLock()
		_, stillCached := cs.beads[id]
		_, fenced := cs.deletedSeq[id]
		cs.mu.RUnlock()
		if stillCached {
			t.Errorf("%s still resident in cache after cascade", id)
		}
		if !fenced {
			t.Errorf("%s missing deletion fence after cascade", id)
		}
	}

	// Survivor is kept, and its stale incoming edge to the deleted child is gone.
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
	if !cascadeContainsAll(deletedEvents, root.ID, child.ID) {
		t.Errorf("deleted events = %v, want %s and %s", deletedEvents, root.ID, child.ID)
	}
}

// A backing store without CascadeDeleter must still delete every id, per bead.
func TestCachingStoreDeleteCascadeFallsBackPerBead(t *testing.T) {
	cs := NewCachingStoreForTest(NewMemStore(), nil) // MemStore does not implement CascadeDeleter
	a := mustCascadeCreate(t, cs, Bead{Type: "task", Status: "closed"})
	b := mustCascadeCreate(t, cs, Bead{Type: "task", Status: "closed"})

	if err := cs.DeleteCascade([]string{a.ID, b.ID}); err != nil {
		t.Fatalf("DeleteCascade fallback: %v", err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if _, err := cs.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v, want ErrNotFound after fallback delete", id, err)
		}
	}
}

func cascadeContainsAll(haystack []string, needles ...string) bool {
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

var _ CascadeDeleter = (*CachingStore)(nil)
