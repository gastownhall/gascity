package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

type policyReadyCountingStore struct {
	*beads.MemStore
	readyCalls int
}

func (s *policyReadyCountingStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.readyCalls++
	return s.MemStore.Ready(query...)
}

// TestBeadPolicyStoreForwardsTheReadyCache pins the production chain the API
// server holds (policy -> cache -> store): the cache-preferred Ready read
// survives the policy wrapper, and the read
// keeps the policy's expanded tier, so wisps are answered from the cache.
func TestBeadPolicyStoreForwardsTheReadyCache(t *testing.T) {
	backing := &policyReadyCountingStore{MemStore: beads.NewMemStore()}
	wisp, err := backing.Create(beads.Bead{Title: "wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	wrapped := wrapStoreWithBeadPolicies(cache, &config.City{})
	backing.readyCalls = 0

	rows, err := beads.ReadyPreferCache(wrapped, beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("ReadyPreferCache: %v", err)
	}
	if backing.readyCalls != 0 {
		t.Fatalf("backing Ready scans = %d, want the cache to answer through the policy layer", backing.readyCalls)
	}
	if len(rows) != 1 || rows[0].ID != wisp.ID {
		t.Fatalf("ready = %+v, want the wisp (policy tier TierBoth)", rows)
	}
}

// TestBeadPolicyStoreReadyPreferCacheReadsACachelessStoreLive: with no cache
// underneath, the read is the policy's live Ready.
func TestBeadPolicyStoreReadyPreferCacheReadsACachelessStoreLive(t *testing.T) {
	backing := &policyReadyCountingStore{MemStore: beads.NewMemStore()}
	if _, err := backing.Create(beads.Bead{Title: "wisp", Ephemeral: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	wrapped := wrapStoreWithBeadPolicies(backing, &config.City{})
	rows, err := beads.ReadyPreferCache(wrapped, beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("ReadyPreferCache: %v", err)
	}
	if backing.readyCalls != 1 || len(rows) != 1 {
		t.Fatalf("live reads = %d, rows = %d, want one live read returning the wisp", backing.readyCalls, len(rows))
	}
}
