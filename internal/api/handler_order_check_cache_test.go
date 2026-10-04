package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderCheckTestOrders returns two cooldown orders — enough to make the
// per-order fan-out visible in the store's call count, and deliberately free
// of any condition trigger, which would disable the response cache outright.
//
// Each names a rig, because that is what gives it a store to read history
// from: orderStoreInfosForState resolves a rig-scoped store first, and an
// order naming neither a rig nor a city store yields no store info at all, so
// the history read is skipped and the fan-out this file measures never
// happens.
func orderCheckTestOrders() []orders.Order {
	enabled := true
	return []orders.Order{
		{Name: "dolt-health", Exec: "dolt status", Trigger: "cooldown", Interval: "5m", Rig: "myrig", Enabled: &enabled},
		{Name: "mail-sweep", Exec: "gc mail sweep", Trigger: "cooldown", Interval: "5m", Rig: "myrig", Enabled: &enabled},
	}
}

// TestHandleOrderCheckCachesAcrossIndexChanges pins the fix:
// /orders/check keys its response cache on a wall-clock time bucket, not on
// the event sequence.
//
// Keyed on the sequence, the cache was unreachable for the callers that need
// it most. Building this body costs one labeled bead List per order per
// store, and the endpoint's readers are pollers: the SBF observability
// exporter reads it once a minute. On the two production cities the sequence
// advanced about six times a minute, so an entry stored under one index was
// never looked up under that index again, and every poll paid the full
// rebuild — 5.8-11.1s on a 23-order city, 14.2-22.4s on a 26-order one,
// against the exporter's 5s per-call timeout. Its collector marks a city down
// when any single endpoint fails, so both cities reported unreachable while
// every other endpoint answered normally.
func TestHandleOrderCheckCachesAcrossIndexChanges(t *testing.T) {
	// A wide bucket puts every request in this test in the same bucket, which
	// isolates "sequence churn must not bust the cache" from bucket-boundary
	// timing. The floor is pinned off so the bucket lookup alone carries the
	// assertion.
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	oldFloor := orderCheckResponseTTLFloor
	orderCheckResponseTTLFloor = 0
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		orderCheckResponseTTLFloor = oldFloor
	})

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	state.autos = orderCheckTestOrders()
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/orders/check"), nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first orders/check = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	built := store.listByLabelCalls
	if built == 0 {
		t.Fatalf("labeled List calls after the first build = 0, want the per-order history reads; the test is not exercising the fan-out it means to")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second orders/check = %d, want 200", rec.Code)
	}
	if store.listByLabelCalls != built {
		t.Fatalf("labeled List calls after a cached repeat = %d, want %d", store.listByLabelCalls, built)
	}

	// The busy-city scenario: a moving event sequence must keep hitting the
	// time-bucketed cache rather than forcing a rebuild.
	for i := 0; i < 5; i++ {
		state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("orders/check after event %d = %d, want 200", i, rec.Code)
		}
	}
	if store.listByLabelCalls != built {
		t.Fatalf("labeled List calls after 5 sequence advances = %d, want %d (a time-bucketed cache must survive sequence churn)", store.listByLabelCalls, built)
	}
}

// TestHandleOrderCheckServesStaleAndRefreshesInBackground pins the path that
// actually keeps a fixed-interval poller off the cold build.
//
// A poller whose interval exceeds both the bucket TTL and the floor misses
// both fast lookups every time, which is the exporter's exact shape: it reads
// once a minute, and no wall-clock window short enough to be useful to
// interactive callers is long enough to survive that gap. Serving the stale
// entry and refreshing behind it is what makes that request cheap, and it is
// the same treatment /status received (ra-4u2eqc).
func TestHandleOrderCheckServesStaleAndRefreshesInBackground(t *testing.T) {
	// Both fast lookups off: a zero bucket TTL gives every request its own
	// bucket, and a zero floor admits nothing. Every request after the first
	// therefore reaches the stale path.
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = 0
	oldFloor := orderCheckResponseTTLFloor
	orderCheckResponseTTLFloor = 0
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		orderCheckResponseTTLFloor = oldFloor
	})

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	state.autos = orderCheckTestOrders()
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/orders/check"), nil)

	// Prime the cache, so the request below has a stale entry to be served
	// rather than falling through to the synchronous cold-start build.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("priming orders/check = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	primed := rec.Body.String()
	builtOnce := store.listByLabelCalls

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("stale-served orders/check = %d, want 200", rec2.Code)
	}
	if rec2.Body.String() != primed {
		t.Fatalf("stale-served body differs from the primed body\n got: %s\nwant: %s", rec2.Body.String(), primed)
	}

	srv.waitForBackground()

	if store.listByLabelCalls <= builtOnce {
		t.Fatalf("labeled List calls after the background refresh = %d, want more than %d (the refresh never rebuilt)", store.listByLabelCalls, builtOnce)
	}
	// Asserted over the whole map rather than against a reconstructed key.
	// OrderCheckInput embeds CityScope, so the handler's key carries the city
	// path parameter and a literal built here would not match it — the lookup
	// would be false whether or not a guard leaked. "No guard is held for any
	// key once the refresh has returned" is also the property that matters: a
	// leaked one wedges every future refresh for that key.
	if len(srv.responseRefreshing) != 0 {
		t.Fatalf("responseRefreshing = %v after the background refresh returned, want empty (a leaked guard wedges every future refresh)", srv.responseRefreshing)
	}
}

// TestHandleOrderCheckFreshBypassesCache holds the escape hatch open: a caller
// that cannot tolerate a body built up to one poll interval ago asks for
// ?fresh=true and is never served a cached one. Without this the staleness the
// two tests above introduce would have no bound a caller can opt out of.
func TestHandleOrderCheckFreshBypassesCache(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	state.autos = orderCheckTestOrders()
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/orders/check"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("priming orders/check = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	built := store.listByLabelCalls

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/orders/check?fresh=true"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh orders/check = %d, want 200", rec.Code)
	}
	if store.listByLabelCalls <= built {
		t.Fatalf("labeled List calls after ?fresh=true = %d, want more than %d (fresh must rebuild)", store.listByLabelCalls, built)
	}
}
