package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// retentionListingStore parks the closed order-tracking listing the retention
// sweep reads (orders.Store.ClosedRunsForRetention) until release closes,
// signaling entered on the first one, and counts the store calls the sweep
// makes. A nil release never parks. Embedding the beads.Store interface (not
// *MemStore) hides DeleteBatch, so a sweep deletes bead by bead.
type retentionListingStore struct {
	beads.Store
	release   <-chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
	// listings counts retention listings started; listed counts those that
	// returned.
	listings atomic.Int32
	listed   atomic.Int32
	// calls counts every List, Get, Delete and DepList the store serves.
	calls atomic.Int32
}

func newRetentionListingStore(inner beads.Store, release <-chan struct{}) *retentionListingStore {
	return &retentionListingStore{Store: inner, release: release, entered: make(chan struct{})}
}

// isRetentionListing reports whether q is the retention sweep's listing: every
// closed order-tracking bead, newest first, unbounded. The startup backlog
// advisory's listing carries a limit and no sort, so it never matches.
func isRetentionListing(q beads.ListQuery) bool {
	return q.Status == "closed" && q.Label == labelOrderTracking &&
		q.Sort == beads.SortCreatedDesc && q.Limit == 0
}

func (s *retentionListingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.calls.Add(1)
	if !isRetentionListing(q) {
		return s.Store.List(q)
	}
	s.listings.Add(1)
	s.enterOnce.Do(func() { close(s.entered) })
	if s.release != nil {
		<-s.release
	}
	defer s.listed.Add(1)
	return s.Store.List(q)
}

func (s *retentionListingStore) Get(id string) (beads.Bead, error) {
	s.calls.Add(1)
	return s.Store.Get(id)
}

func (s *retentionListingStore) Delete(id string) error {
	s.calls.Add(1)
	return s.Store.Delete(id)
}

func (s *retentionListingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.calls.Add(1)
	return s.Store.DepList(id, direction)
}

// The startup order pass runs before the cold-start reconcile and readiness,
// so it must not run the retention sweep: with prunable beads, a fresh backup
// and a zero stamp, it neither lists nor deletes closed tracking runs and
// leaves the stamp for the retention lane (#6429).
func TestStartupOrderPassDoesNotRunRetentionSweep(t *testing.T) {
	now := time.Now()
	cr, store, _ := seedRetentionWatchdogCity(t, now, time.Hour)
	listing := newRetentionListingStore(store, nil)
	cr.standaloneCityStore = listing
	od := &recordingOrderDispatcher{}
	cr.od = od

	cr.dispatchOrders(context.Background(), cr.cityPath)

	if !od.called.Load() {
		t.Fatal("the startup order pass did not reach the order dispatcher")
	}
	if n := listing.listings.Load(); n != 0 {
		t.Fatalf("closed order-tracking retention listings = %d, want 0 in the order pass", n)
	}
	for _, id := range []string{"guard-00", "guard-01"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("Get(%s) = %v: the order pass pruned a closed tracking bead", id, err)
		}
	}
	if !cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Fatalf("orderTrackingRetentionWatchdogLast = %v, want zero after the order pass", cr.orderTrackingRetentionWatchdogLast)
	}
}
