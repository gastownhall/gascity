package beads

import (
	"context"
	"errors"
	"testing"
)

type flakyReadyStore struct {
	*MemStore
	failReady error
}

func (s *flakyReadyStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	if s.failReady != nil {
		return nil, s.failReady
	}
	return s.MemStore.Ready(query...)
}

func TestReadyLiveBypassesCachingStore(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	ready, err := backing.Create(Bead{Title: "ready"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	if err := backing.DepAdd(ready.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := backing.Close(blocker.ID); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}

	stale, err := cache.Ready()
	if err != nil {
		t.Fatalf("cache.Ready(): %v", err)
	}
	for _, bead := range stale {
		if bead.ID == ready.ID {
			t.Fatalf("cache.Ready() = %v, want stale result without %s before reconcile", stale, ready.ID)
		}
	}

	live, err := ReadyLive(cache)
	if err != nil {
		t.Fatalf("ReadyLive(): %v", err)
	}
	if len(live) != 1 || live[0].ID != ready.ID {
		t.Fatalf("ReadyLive() = %v, want only %s", live, ready.ID)
	}
}

func TestReadyLiveReturnsBackingErrors(t *testing.T) {
	t.Parallel()

	backing := &flakyReadyStore{MemStore: NewMemStore()}
	if _, err := backing.Create(Bead{Title: "ready"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.failReady = errors.New("backing ready failed")

	if cached, err := cache.Ready(); err != nil {
		t.Fatalf("cache.Ready(): %v", err)
	} else if len(cached) != 1 {
		t.Fatalf("cache.Ready() len = %d, want 1 cached bead", len(cached))
	}

	if _, err := ReadyLive(cache); err == nil || err.Error() != "backing ready failed" {
		t.Fatalf("ReadyLive() err = %v, want backing error", err)
	}
}
