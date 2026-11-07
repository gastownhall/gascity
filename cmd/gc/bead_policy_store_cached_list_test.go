package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// refreshFailingStore is a backing whose Get fails while fail is set, so a
// cache's refresh after a write it forwards fails and leaves the written row
// dirty. It counts its Gets.
type refreshFailingStore struct {
	beads.Store
	fail bool
	gets int
}

func (s *refreshFailingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	if s.fail {
		return beads.Bead{}, errors.New("bd show: connection refused")
	}
	return s.Store.Get(id)
}

// TestBeadPolicyStoreForwardsRefreshingCachedList: the API's list active-work
// lookup reaches the controller's city and rig caches through this layer and
// the work handle over it, where the embedded Store interface hides the
// cache's own reads. The layer answers at its own read tier, as its List
// does, so a wisp-tier claim answers the lookup's default-tier query, and it
// hands the caller's context to the cache, which waits that long for its
// refresh of a dirty row: none once the context has ended. Over a store with
// no cache it answers false, and CachedList stays hidden.
func TestBeadPolicyStoreForwardsRefreshingCachedList(t *testing.T) {
	mem := beads.NewMemStore()
	wisp, err := mem.Create(beads.Bead{Title: "step", Status: "open", Ephemeral: true})
	if err != nil {
		t.Fatalf("create wisp: %v", err)
	}
	backing := &refreshFailingStore{Store: mem, fail: true}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	status, assignee := "in_progress", "worker"
	if err := cache.Update(wisp.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("claim through the cache: %v", err)
	}
	query := beads.ListQuery{Assignee: assignee, Status: status, Limit: 1, Sort: beads.SortCreatedDesc}
	handle := beads.WorkStore{Store: wrapStoreWithBeadPolicies(cache, &config.City{})}
	lister, ok := beads.As[beads.RefreshingCachedLister](handle)
	if !ok {
		t.Fatal("a work handle over the policy layer has no RefreshingCachedLister")
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if rows, ok := lister.CachedListRefreshing(ended, query); ok {
		t.Fatalf("dirty cache with an ended context = %v, answered; want declined", rows)
	}
	backing.fail = false
	gets := backing.gets

	rows, ok := lister.CachedListRefreshing(context.Background(), query)

	if !ok || len(rows) != 1 || rows[0].ID != wisp.ID {
		t.Fatalf("CachedListRefreshing = %v, %v; want the claimed wisp %s", rows, ok, wisp.ID)
	}
	if n := backing.gets - gets; n != 1 {
		t.Fatalf("backing Gets during the refresh = %d, want the dirty row's 1", n)
	}
	if rows, ok := lister.CachedListRefreshing(ended, query); !ok || len(rows) != 1 {
		t.Fatalf("after the refresh = %v, %v; want the clean cache's answer", rows, ok)
	}
	if rows, ok := cache.CachedListRefreshing(ended, query); !ok || len(rows) != 0 {
		t.Fatalf("the cache at its own tier = %v, %v; want a miss: the layer's tier finds the wisp", rows, ok)
	}
	if _, ok := beads.As[beads.CachedLister](handle); ok {
		t.Fatal("the policy layer forwards CachedList")
	}

	uncached, ok := beads.As[beads.RefreshingCachedLister](beads.WorkStore{Store: wrapStoreWithBeadPolicies(mem, &config.City{})})
	if !ok {
		t.Fatal("a work handle over the policy layer has no RefreshingCachedLister")
	}
	if rows, ok := uncached.CachedListRefreshing(context.Background(), query); ok {
		t.Fatalf("a policy layer over a store with no cache answered %v", rows)
	}
}
