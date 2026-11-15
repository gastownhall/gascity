package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// errRetentionReadTooLarge stands in for a store whose read budget a listing
// of the whole retained tracking history does not fit.
var errRetentionReadTooLarge = errors.New("tracking listing larger than one retention page")

// retentionReadBudgetStore fails any order-tracking listing that returns more
// rows than one retention page, the way a native Dolt read of a large
// retained history runs out of its read budget. It records the largest
// listing it answered. It embeds the Store interface, not *MemStore, so that
// the live handle a retention read takes cannot step past its List.
type retentionReadBudgetStore struct {
	beads.Store
	mem         *beads.MemStore
	maxRows     int
	largestRead int
}

func newRetentionReadBudgetStore(seed []beads.Bead) *retentionReadBudgetStore {
	mem := beads.NewMemStoreFrom(1000, seed, nil)
	return &retentionReadBudgetStore{Store: mem, mem: mem, maxRows: orderTrackingRetentionPageSize}
}

func (s *retentionReadBudgetStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	list, err := s.Store.List(q)
	if err != nil {
		return nil, err
	}
	if q.Label == labelOrderTracking {
		if len(list) > s.largestRead {
			s.largestRead = len(list)
		}
		if len(list) > s.maxRows {
			return nil, fmt.Errorf("%w: %d rows", errRetentionReadTooLarge, len(list))
		}
	}
	return list, nil
}

func retentionRun(order string, i int, createdAt time.Time) beads.Bead {
	return beads.Bead{
		ID:        fmt.Sprintf("%s-%04d", order, i),
		Title:     "order:" + order,
		Status:    "closed",
		Type:      "task",
		CreatedAt: createdAt,
		Labels:    []string{"order-run:" + order, labelOrderTracking},
		Ephemeral: i%2 == 0,
	}
}

func retentionStoreIDs(t *testing.T, store beads.Store) map[string]bool {
	t.Helper()
	list, err := store.List(beads.ListQuery{Label: labelOrderTracking, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("listing remaining tracking beads: %v", err)
	}
	ids := make(map[string]bool, len(list))
	for _, b := range list {
		ids[b.ID] = true
	}
	return ids
}

// drainRetention runs the retention prune until a run deletes nothing,
// checking before each run that the confirm count equals what the run then
// deletes. It returns the number of runs that deleted something.
func drainRetention(t *testing.T, store beads.Store, now time.Time, policy orderTrackingRetentionPolicy) int {
	t.Helper()
	runs := 0
	for range 100 {
		eligible, err := countClosedOrderTrackingRetentionEligible([]beads.Store{store}, now, policy, nil)
		if err != nil {
			t.Fatalf("run %d: count: %v", runs+1, err)
		}
		result, err := sweepClosedOrderTrackingRetentionAcrossStores([]beads.Store{store}, now, policy, nil)
		if err != nil {
			t.Fatalf("run %d: sweep: %v", runs+1, err)
		}
		if result.deleted != eligible {
			t.Fatalf("run %d: deleted %d, but the confirm count was %d", runs+1, result.deleted, eligible)
		}
		if result.deleted > orderTrackingRetentionRunBudget {
			t.Fatalf("run %d: deleted %d, over the run budget %d", runs+1, result.deleted, orderTrackingRetentionRunBudget)
		}
		if result.deleted == 0 {
			return runs
		}
		runs++
	}
	t.Fatal("retention did not settle in 100 runs")
	return runs
}

// A retained history larger than one read drains over successive runs, each
// listing at most one page and deleting at most the run budget, and the
// newest ten runs of every order survive.
func TestOrderTrackingRetentionDrainsBacklogOverRunsWithinOnePageRead(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	policy := orderTrackingRetentionPolicy{deleteAfterClose: 24 * time.Hour}
	const perOrder = 260
	var seed []beads.Bead
	for _, order := range []string{"alpha", "beta", "gamma"} {
		for i := range perOrder {
			seed = append(seed, retentionRun(order, i, now.Add(-72*time.Hour+time.Duration(i)*time.Minute)))
		}
	}
	store := newRetentionReadBudgetStore(seed)

	runs := drainRetention(t, store, now, policy)

	eligible := 3 * (perOrder - minClosedOrderTrackingRetained)
	if want := (eligible + orderTrackingRetentionRunBudget - 1) / orderTrackingRetentionRunBudget; runs != want {
		t.Fatalf("drained in %d runs, want %d (%d eligible, budget %d)", runs, want, eligible, orderTrackingRetentionRunBudget)
	}
	if store.largestRead > orderTrackingRetentionPageSize {
		t.Fatalf("largest tracking listing = %d rows, want at most %d", store.largestRead, orderTrackingRetentionPageSize)
	}
	left := retentionStoreIDs(t, store.mem)
	if len(left) != 3*minClosedOrderTrackingRetained {
		t.Fatalf("%d tracking beads left, want %d", len(left), 3*minClosedOrderTrackingRetained)
	}
	for _, order := range []string{"alpha", "beta", "gamma"} {
		for i := perOrder - minClosedOrderTrackingRetained; i < perOrder; i++ {
			if id := fmt.Sprintf("%s-%04d", order, i); !left[id] {
				t.Fatalf("%s was deleted, want the newest %d of %s kept", id, minClosedOrderTrackingRetained, order)
			}
		}
	}
}

// An order with fewer than ten runs inside the TTL keeps its newest expired
// runs up to the floor, although the walk never reads its runs inside the TTL
// and has to page through another order's history to reach its expired ones.
func TestOrderTrackingRetentionKeepsFloorOfOrderWithFewRunsInsideTTL(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	policy := orderTrackingRetentionPolicy{deleteAfterClose: 24 * time.Hour}
	var seed []beads.Bead
	// busy: 700 expired runs, all newer than sparse's expired runs, so the
	// walk reaches sparse's only on its second page.
	for i := range 700 {
		seed = append(seed, retentionRun("busy", i, now.Add(-30*time.Hour+time.Duration(i)*time.Second)))
	}
	for i := range minClosedOrderTrackingRetained {
		seed = append(seed, retentionRun("busy", 700+i, now.Add(-time.Hour+time.Duration(i)*time.Minute)))
	}
	// sparse: 20 expired runs and 4 inside the TTL.
	for i := range 20 {
		seed = append(seed, retentionRun("sparse", i, now.Add(-96*time.Hour+time.Duration(i)*time.Minute)))
	}
	for i := range 4 {
		seed = append(seed, retentionRun("sparse", 20+i, now.Add(-2*time.Hour+time.Duration(i)*time.Minute)))
	}
	store := newRetentionReadBudgetStore(seed)

	drainRetention(t, store, now, policy)

	left := retentionStoreIDs(t, store.mem)
	for i := range 24 {
		id := fmt.Sprintf("sparse-%04d", i)
		if keep := i >= 24-minClosedOrderTrackingRetained; left[id] != keep {
			t.Fatalf("%s kept = %t, want %t: sparse keeps its 4 runs inside the TTL and its 6 newest expired ones", id, left[id], keep)
		}
	}
	for i := range 710 {
		id := fmt.Sprintf("busy-%04d", i)
		if keep := i >= 700; left[id] != keep {
			t.Fatalf("%s kept = %t, want %t", id, left[id], keep)
		}
	}
}

// More floor-kept runs than one page sit at the newest end of the expired
// history; the walk steps past them to the deletable runs below.
func TestOrderTrackingRetentionWalksPastAPageOfKeptRuns(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	policy := orderTrackingRetentionPolicy{deleteAfterClose: 24 * time.Hour}
	var seed []beads.Bead
	// 60 retired orders, ten expired runs each: every one is on its order's
	// floor, and together they fill more than a page.
	for o := range 60 {
		order := fmt.Sprintf("retired%02d", o)
		for i := range minClosedOrderTrackingRetained {
			seed = append(seed, retentionRun(order, i, now.Add(-48*time.Hour+time.Duration(o*minClosedOrderTrackingRetained+i)*time.Second)))
		}
	}
	// old: 30 runs, all older than the retired orders' runs.
	for i := range 30 {
		seed = append(seed, retentionRun("old", i, now.Add(-200*time.Hour+time.Duration(i)*time.Minute)))
	}
	store := newRetentionReadBudgetStore(seed)

	deleted, err := sweepClosedOrderTrackingRetention(store, now, policy, nil)
	if err != nil {
		t.Fatalf("sweepClosedOrderTrackingRetention: %v", err)
	}
	if deleted != 30-minClosedOrderTrackingRetained {
		t.Fatalf("deleted = %d, want %d", deleted, 30-minClosedOrderTrackingRetained)
	}
	left := retentionStoreIDs(t, store.mem)
	if len(left) != 61*minClosedOrderTrackingRetained {
		t.Fatalf("%d tracking beads left, want %d", len(left), 61*minClosedOrderTrackingRetained)
	}
}
