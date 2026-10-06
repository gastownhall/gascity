package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	result, err := buildOrderRunFeedItems(st, beadmeta.ScopeKindCity, workflowCityScopeRef(st.CityName()), 0)
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

	result, err := buildOrderRunFeedItems(st, beadmeta.ScopeKindCity, workflowCityScopeRef(st.CityName()), 0)
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

// seedClosedOrderRunBead writes one closed order-tracking bead carrying captured
// exec output, the shape the history endpoints read.
func seedClosedOrderRunBead(t *testing.T, store beads.Store, scoped, output string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title:    "order:" + scoped,
		Status:   "closed",
		Labels:   []string{"order-run:" + scoped, "order-tracking"},
		Metadata: map[string]string{"convergence.gate_stdout": output},
	})
	if err != nil {
		t.Fatalf("seeding the closed tracking bead: %v", err)
	}
	return bead
}

// orderHistoryListStoreRef drives GET /orders/history and returns the single
// entry's bead ID and store_ref — the handle the endpoint tells a client to use.
func orderHistoryListStoreRef(t *testing.T, h http.Handler, fs *fakeState, scoped string) (string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/orders/history?scoped_name="+scoped), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	var listResp struct {
		Entries []struct {
			BeadID   string `json:"bead_id"`
			StoreRef string `json:"store_ref"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Entries) != 1 {
		t.Fatalf("list entries = %+v, want exactly 1", listResp.Entries)
	}
	return listResp.Entries[0].BeadID, listResp.Entries[0].StoreRef
}

type orderHistoryDetailBody struct {
	BeadID   string `json:"bead_id"`
	StoreRef string `json:"store_ref"`
	Output   string `json:"output"`
}

// orderHistoryDetail drives GET /order/history/{bead_id}, appending store_ref
// when one is supplied, and returns the status and decoded body.
func orderHistoryDetail(t *testing.T, h http.Handler, fs *fakeState, beadID, storeRef string) (int, orderHistoryDetailBody) {
	t.Helper()
	path := "/order/history/" + beadID
	if storeRef != "" {
		path += "?store_ref=" + url.QueryEscape(storeRef)
	}
	req := httptest.NewRequest(http.MethodGet, cityURL(fs, path), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body orderHistoryDetailBody
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal detail: %v", err)
		}
	}
	return w.Code, body
}

// splitOrdersFakeState builds the production split shape: the work ledger holds
// the city's own beads and ONE binding serves both the graph and orders classes,
// which is the only split a city boots with (storageSplitWhole assigns the same
// store value to every relocated class).
func splitOrdersFakeState(t *testing.T) (*fakeState, beads.Store) {
	t.Helper()
	binding := beads.NewMemStore()

	st := newFakeState(t)
	st.cityBeadStore = beads.NewMemStore()
	st.graphBeadStore = binding
	st.ordersBeadStore = binding
	st.stores = nil
	st.cfg.Rigs = nil
	return st, binding
}

// TestOrderHistoryListStoreRefRoundTripsToDetail is the list->detail contract:
// the store_ref the list hands a client is the handle the detail endpoint takes
// back. An endpoint that mints a ref its sibling rejects answers 404 for every
// order run on a split city — every run after cutover.
func TestOrderHistoryListStoreRefRoundTripsToDetail(t *testing.T) {
	st, binding := splitOrdersFakeState(t)
	seedClosedOrderRunBead(t, binding, "nightly-review", "binding output")

	h := newTestCityHandler(t, st)
	beadID, listRef := orderHistoryListStoreRef(t, h, st, "nightly-review")

	code, body := orderHistoryDetail(t, h, st, beadID, listRef)
	if code != http.StatusOK {
		t.Fatalf("detail status for the list's own store_ref %q = %d, want %d", listRef, code, http.StatusOK)
	}
	if body.BeadID != beadID {
		t.Fatalf("detail bead_id = %q, want %q", body.BeadID, beadID)
	}
	if body.Output != "binding output" {
		t.Fatalf("detail output = %q, want %q", body.Output, "binding output")
	}
	if body.StoreRef != listRef {
		t.Fatalf("detail store_ref = %q, want the list's %q; one bead must not carry two names", body.StoreRef, listRef)
	}
}

// TestOrderHistoryDetailReachesTheOrdersBindingDirectly pins the second half of
// the fix: the detail read reaches the orders class itself rather than through
// the graph binding it happens to share a store with today.
func TestOrderHistoryDetailReachesTheOrdersBindingDirectly(t *testing.T) {
	work := beads.NewMemStore()
	binding := beads.NewMemStore()

	st := newFakeState(t)
	st.cityBeadStore = work
	st.ordersBeadStore = binding // orders relocated, graph left on the work store
	st.stores = nil
	st.cfg.Rigs = nil

	tracking := seedClosedOrderRunBead(t, binding, "dolt-health", "binding output")

	h := newTestCityHandler(t, st)
	code, body := orderHistoryDetail(t, h, st, tracking.ID, "")
	if code != http.StatusOK {
		t.Fatalf("detail status without store_ref = %d, want %d; the read never opened the orders binding", code, http.StatusOK)
	}
	if body.Output != "binding output" {
		t.Fatalf("detail output = %q, want %q", body.Output, "binding output")
	}
}

// TestOrderHistoryStoreRefsAllResolveToTheSameStore holds the invariant the 404
// broke: every store_ref the order endpoints publish for one bead resolves, and
// resolves to the database that bead is actually in.
func TestOrderHistoryStoreRefsAllResolveToTheSameStore(t *testing.T) {
	st, binding := splitOrdersFakeState(t)
	tracking := seedClosedOrderRunBead(t, binding, "nightly-review", "binding output")

	h := newTestCityHandler(t, st)
	_, listRef := orderHistoryListStoreRef(t, h, st, "nightly-review")
	_, detailBody := orderHistoryDetail(t, h, st, tracking.ID, "")

	feed, err := buildOrderRunFeedItems(st, beadmeta.ScopeKindCity, workflowCityScopeRef(st.CityName()), 0)
	if err != nil {
		t.Fatalf("buildOrderRunFeedItems: %v", err)
	}
	feedRef := ""
	for _, item := range feed.Items {
		if item.BeadID == tracking.ID {
			feedRef = item.StoreRef
		}
	}
	if feedRef == "" {
		t.Fatalf("feed items = %+v, want the binding-resident tracking bead %s", feed.Items, tracking.ID)
	}

	for _, ref := range []string{listRef, detailBody.StoreRef, feedRef} {
		info, ok := workflowStoreByRef(st, ref)
		if !ok {
			t.Fatalf("workflowStoreByRef(%q) = false; an order endpoint published a ref no reader resolves", ref)
		}
		if info.store != binding {
			t.Fatalf("workflowStoreByRef(%q) resolved to a store other than the bead's own binding", ref)
		}
	}
}

// TestOrderHistoryStoreRefUnchangedOnSingleStoreCity is the compatibility half:
// a city that relocates nothing publishes the same city:<name> ref it always
// did, and that ref still round-trips.
func TestOrderHistoryStoreRefUnchangedOnSingleStoreCity(t *testing.T) {
	city := beads.NewMemStore()

	st := newFakeState(t)
	st.cityBeadStore = city
	st.stores = nil
	st.cfg.Rigs = nil

	seedClosedOrderRunBead(t, city, "weekly-audit", "city output")

	h := newTestCityHandler(t, st)
	beadID, listRef := orderHistoryListStoreRef(t, h, st, "weekly-audit")
	if listRef != "city:test-city" {
		t.Fatalf("list store_ref = %q, want city:test-city", listRef)
	}

	code, body := orderHistoryDetail(t, h, st, beadID, listRef)
	if code != http.StatusOK {
		t.Fatalf("detail status = %d, want %d", code, http.StatusOK)
	}
	if body.StoreRef != "city:test-city" || body.Output != "city output" {
		t.Fatalf("detail = %+v, want city:test-city / city output", body)
	}

	code, body = orderHistoryDetail(t, h, st, beadID, "")
	if code != http.StatusOK {
		t.Fatalf("detail status without store_ref = %d, want %d", code, http.StatusOK)
	}
	if body.StoreRef != "city:test-city" {
		t.Fatalf("detail store_ref without a hint = %q, want city:test-city", body.StoreRef)
	}
}
