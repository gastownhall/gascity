package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// The order-tracking bead is orders class, and on a city whose infrastructure
// classes are served by their own binding the controller creates it there. Every
// API read of those beads therefore has to reach the binding: the check/history
// edge through orderStoreInfosForState, and the monitor feed through its own
// store list. Reading only the work stores reports a city whose orders fire every
// few minutes as having no runs at all.

// seedOrdersTrackingBead writes one open order-tracking bead into store.
func seedOrdersTrackingBead(t *testing.T, store beads.Store, scoped string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title:  "order:" + scoped,
		Labels: []string{"order-run:" + scoped, "order-tracking"},
	})
	if err != nil {
		t.Fatalf("seeding the tracking bead: %v", err)
	}
	return bead
}

// TestOrderStoreInfosReachTheOrdersBinding pins the check/history read path.
func TestOrderStoreInfosReachTheOrdersBinding(t *testing.T) {
	work := beads.NewMemStore()
	binding := beads.NewMemStore()

	st := newFakeState(t)
	st.cityBeadStore = work
	st.ordersBeadStore = binding
	st.stores = nil
	st.cfg.Rigs = nil

	infos, err := orderStoreInfosForState(st, orders.Order{Name: "dolt-health"})
	if err != nil {
		t.Fatalf("orderStoreInfosForState: %v", err)
	}
	var reachesBinding bool
	for _, info := range infos {
		if info.store == beads.Store(binding) {
			reachesBinding = true
		}
	}
	if !reachesBinding {
		t.Fatalf("order store infos = %+v, none of them the orders binding; the API reports a split city's orders as never run and calls a just-fired order due", infos)
	}
}

// TestOrderStoreInfosStayOnTheOneStoreOnSingleStoreCity is the compatibility
// half: with OrdersBeadStore() == CityBeadStore() — every city that relocates
// nothing — the list is exactly the one it always was, so the read does not scan
// one database twice.
func TestOrderStoreInfosStayOnTheOneStoreOnSingleStoreCity(t *testing.T) {
	city := beads.NewMemStore()

	st := newFakeState(t)
	st.cityBeadStore = city
	st.stores = nil
	st.cfg.Rigs = nil

	infos, err := orderStoreInfosForState(st, orders.Order{Name: "dolt-health"})
	if err != nil {
		t.Fatalf("orderStoreInfosForState: %v", err)
	}
	if len(infos) != 1 || infos[0].store != beads.Store(city) {
		t.Fatalf("order store infos = %+v, want exactly the city store", infos)
	}
}

// TestOrderFeedListsTrackingBeadsFromTheOrdersBinding pins the monitor feed.
// It builds its store list from the workflow scan (which leads with the GRAPH
// binding), so the orders leg is a distinct decision and a distinct revert.
func TestOrderFeedListsTrackingBeadsFromTheOrdersBinding(t *testing.T) {
	work := beads.NewMemStore()
	binding := beads.NewMemStore()

	st := newFakeState(t)
	st.cityBeadStore = work
	st.ordersBeadStore = binding
	st.stores = nil
	st.cfg.Rigs = nil

	tracking := seedOrdersTrackingBead(t, binding, "dolt-health")

	result, err := buildOrderRunFeedItems(st, beadmeta.ScopeKindCity, workflowCityScopeRef(st.CityName()))
	if err != nil {
		t.Fatalf("buildOrderRunFeedItems: %v", err)
	}
	var found bool
	for _, item := range result.Items {
		if item.BeadID == tracking.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("order feed items = %+v, want the binding-resident tracking bead %s; the dashboard shows no order activity at all on a split city", result.Items, tracking.ID)
	}
}

// TestOrderFeedCountsAStoreServingBothClassesOnce is the byte-identity half:
// a city that relocates nothing lists each tracking bead exactly once, rather
// than once per store list entry that happens to resolve to the same database.
func TestOrderFeedCountsAStoreServingBothClassesOnce(t *testing.T) {
	city := beads.NewMemStore()

	st := newFakeState(t)
	st.cityBeadStore = city
	st.stores = nil
	st.cfg.Rigs = nil

	tracking := seedOrdersTrackingBead(t, city, "dolt-health")

	result, err := buildOrderRunFeedItems(st, beadmeta.ScopeKindCity, workflowCityScopeRef(st.CityName()))
	if err != nil {
		t.Fatalf("buildOrderRunFeedItems: %v", err)
	}
	var seen int
	for _, item := range result.Items {
		if item.BeadID == tracking.ID {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("tracking bead %s appears %d times in the feed, want 1", tracking.ID, seen)
	}
}
