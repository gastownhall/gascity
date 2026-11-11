package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// readyCountingStore counts the Ready reads that reach it.
type readyCountingStore struct {
	*beads.MemStore
	ready int
}

func (s *readyCountingStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.ready++
	return s.MemStore.Ready(q...)
}

// TestControllerDemandReadyThroughAHandleAsksThePhysicalStore pins both
// controller-demand ready reads (readyForControllerDemand and
// readyDemandCache.controllerDemandReady) through a typed handle, which
// forwards Handles whatever it wraps (T7a). Over a plain store the store has
// no cached tier, so demand reads it once, as it did before the forwarding.
// Over a CachingStore it reads the cache, then pays the one live
// freshness read of the backing store: what the cache itself gets.
func TestControllerDemandReadyThroughAHandleAsksThePhysicalStore(t *testing.T) {
	reads := map[string]func(beads.Store) error{
		"readyForControllerDemand": func(s beads.Store) error { _, err := readyForControllerDemand(s); return err },
		"controllerDemandReady": func(s beads.Store) error {
			_, err := newReadyDemandCache().controllerDemandReady(s, "work")
			return err
		},
	}
	for name, read := range reads {
		plain := &readyCountingStore{MemStore: beads.NewMemStore()}
		if err := read(beadstest.Sessions(plain)); err != nil {
			t.Fatalf("%s over a plain store: %v", name, err)
		}
		if plain.ready != 1 {
			t.Errorf("%s through a handle over a plain store: %d Ready reads, want 1", name, plain.ready)
		}

		backing := &readyCountingStore{MemStore: beads.NewMemStore()}
		cache := beads.NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatal(err)
		}
		backing.ready = 0
		for via, s := range map[string]beads.Store{"the cache": cache, "a handle over the cache": beadstest.Sessions(cache)} {
			before := backing.ready
			if err := read(s); err != nil {
				t.Fatalf("%s through %s: %v", name, via, err)
			}
			if got := backing.ready - before; got != 1 {
				t.Errorf("%s through %s: %d backing Ready reads, want 1 (the cache, then one live read)", name, via, got)
			}
		}
	}
}
