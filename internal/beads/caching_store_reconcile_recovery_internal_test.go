package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// droppingListStore wraps a Store and silently omits selected bead IDs from
// List results — simulating a tolerant parser that drops malformed entries
// or a partial List under backend stress.
type droppingListStore struct {
	Store
	dropFromList map[string]struct{}
	getErr       map[string]error
}

func (s *droppingListStore) List(query ListQuery) ([]Bead, error) {
	all, err := s.Store.List(query)
	if err != nil || len(s.dropFromList) == 0 {
		return all, err
	}
	filtered := make([]Bead, 0, len(all))
	for _, b := range all {
		if _, drop := s.dropFromList[b.ID]; drop {
			continue
		}
		filtered = append(filtered, b)
	}
	return filtered, nil
}

func (s *droppingListStore) Get(id string) (Bead, error) {
	if err, ok := s.getErr[id]; ok {
		return Bead{}, err
	}
	return s.Store.Get(id)
}

// TestReconcileSkipsCloseWhenListDropsAliveBead reproduces the cache-thrash
// scenario where bd's tolerant JSON parser silently drops an alive bead from
// a List result. Before the fix, the reconciler would synthesize bead.closed
// every cycle and re-introduction via other paths would re-trigger it.
func TestReconcileSkipsCloseWhenListDropsAliveBead(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	survivor, err := mem.Create(Bead{Title: "Survivor"})
	if err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	dropped, err := mem.Create(Bead{Title: "Dropped by tolerant parser"})
	if err != nil {
		t.Fatalf("Create dropped: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.dropFromList = map[string]struct{}{dropped.ID: {}}
	events = events[:0]

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+dropped.ID {
			t.Fatalf("emitted bead.closed for an alive bead dropped by List; events = %v", events)
		}
	}

	got, err := cache.Get(dropped.ID)
	if err != nil {
		t.Fatalf("Get(dropped) after reconcile: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("Get(dropped) returned status=closed; cache should still see it as alive")
	}
	if _, err := cache.Get(survivor.ID); err != nil {
		t.Fatalf("Get(survivor) after reconcile: %v", err)
	}
}

// TestReconcileEmitsCloseWhenBackingConfirmsNotFound verifies that a genuine
// closure (List omits the bead AND backing.Get reports ErrNotFound) still
// produces a bead.closed event.
func TestReconcileEmitsCloseWhenBackingConfirmsNotFound(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	gone, err := mem.Create(Bead{Title: "Truly gone"})
	if err != nil {
		t.Fatalf("Create gone: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.dropFromList = map[string]struct{}{gone.ID: {}}
	backing.getErr = map[string]error{
		gone.ID: fmt.Errorf("getting bead %q: %w", gone.ID, ErrNotFound),
	}
	events = events[:0]

	cache.runReconciliation()

	want := "bead.closed:" + gone.ID
	for _, e := range events {
		if e == want {
			return
		}
	}
	t.Fatalf("events = %v, want %s when backing confirmed not-found", events, want)
}

// TestReconcileDefersCloseOnBackingError verifies that a transient backing
// failure (List omits the bead, Get returns a non-NotFound error) does NOT
// produce a bead.closed event — the close is deferred until a later scan.
func TestReconcileDefersCloseOnBackingError(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	uncertain, err := mem.Create(Bead{Title: "Uncertain"})
	if err != nil {
		t.Fatalf("Create uncertain: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.dropFromList = map[string]struct{}{uncertain.ID: {}}
	backing.getErr = map[string]error{uncertain.ID: errors.New("dolt: connection reset")}
	events = events[:0]

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+uncertain.ID {
			t.Fatalf("emitted bead.closed despite backing.Get error; events = %v", events)
		}
	}
	if _, err := cache.Get(uncertain.ID); err != nil {
		t.Fatalf("Get(uncertain) after reconcile: %v", err)
	}
}
