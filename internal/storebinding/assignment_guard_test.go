package storebinding

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// guardedBeadsStore is a canonical Beads store that carries the status+assignee
// guard, the way BdStore and CachingStore do. The guard body only records the
// call: these rows resolve the CAPABILITY, not the guard's own semantics.
type guardedBeadsStore struct {
	beads.Store
	called bool
}

func (s *guardedBeadsStore) UpdateIfAssignment(_, _, _ string, _ beads.UpdateOpts) (bool, error) {
	s.called = true
	return true, nil
}

// wrappingGraphFrontDoor is a GraphStore implementation that is NOT the beads
// projection — an engine's own closed front door — standing in for every
// non-beads binding the resolver can be handed.
type wrappingGraphFrontDoor struct{ GraphStore }

// TestAssignmentGuardedUpdaterFor resolves the status+assignee guard from a
// graph front door exactly when the canonical Beads projection's backing
// store carries it: a guarded store resolves to ITS guard (the write must
// reach the store the route resolved, not an adapter copy), a store without
// the guard answers false, and a non-beads front door answers false so its
// exact claims stay revision-fenced or fail closed rather than reaching for
// a write the engine cannot fence.
func TestAssignmentGuardedUpdaterFor(t *testing.T) {
	t.Run("a guarded backing store resolves to its own guard", func(t *testing.T) {
		backing := &guardedBeadsStore{Store: beads.NewMemStore()}
		graph, err := NewBeadsGraphStore(backing)
		if err != nil {
			t.Fatalf("NewBeadsGraphStore: %v", err)
		}
		guard, ok := AssignmentGuardedUpdaterFor(graph)
		if !ok || guard == nil {
			t.Fatalf("AssignmentGuardedUpdaterFor = (%v, %v), want the guard", guard, ok)
		}
		landed, err := guard.UpdateIfAssignment("gcg-1", "open", "", beads.UpdateOpts{})
		if err != nil || !landed {
			t.Fatalf("UpdateIfAssignment = (landed=%v err=%v), want the delegated call", landed, err)
		}
		if !backing.called {
			t.Fatalf("the guard did not reach the backing store")
		}
	})

	t.Run("a store without the guard answers false", func(t *testing.T) {
		graph, err := NewBeadsGraphStore(beads.NewMemStore())
		if err != nil {
			t.Fatalf("NewBeadsGraphStore: %v", err)
		}
		guard, ok := AssignmentGuardedUpdaterFor(graph)
		if ok || guard != nil {
			t.Fatalf("AssignmentGuardedUpdaterFor = (%v, %v), want false over a MemStore", guard, ok)
		}
	})

	t.Run("a non-beads front door answers false", func(t *testing.T) {
		guard, ok := AssignmentGuardedUpdaterFor(wrappingGraphFrontDoor{})
		if ok || guard != nil {
			t.Fatalf("AssignmentGuardedUpdaterFor = (%v, %v), want false over a non-beads front door", guard, ok)
		}
	})
}
