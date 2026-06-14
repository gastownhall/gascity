package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

type countingStore struct {
	beads.Store

	listCalls           int
	listByLabelCalls    int
	listByAssigneeCalls int
}

func (s *countingStore) ListOpen(status ...string) ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.ListOpen(status...)
}

func (s *countingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	switch {
	case query.Assignee != "":
		s.listByAssigneeCalls++
	case query.Label != "":
		s.listByLabelCalls++
	case query.Status != "" || query.AllowScan:
		s.listCalls++
	}
	return s.Store.List(query)
}

func (s *countingStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.listByLabelCalls++
	return s.Store.ListByLabel(label, limit, opts...)
}

func (s *countingStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	s.listByAssigneeCalls++
	return s.Store.ListByAssignee(assignee, status, limit)
}

// TestHandleStatusCachesAcrossIndexChanges pins the gascity#3186 fix: /status
// keys its response cache on a wall-clock TTL bucket, not the event sequence,
// so a busy city (whose sequence advances every poll) still hits the cache
// instead of rebuilding the O(store-size) body on every request. Recording an
// event must NOT bust the /status cache within the TTL window — unlike the
// index-keyed endpoints (see TestHandleAgentListCachesUntilIndexChanges).
func TestHandleStatusCachesAcrossIndexChanges(t *testing.T) {
	// Pin a wide TTL so every request in this test lands in the same time
	// bucket; this isolates the "index churn must not bust the cache" property
	// from wall-clock bucket-boundary timing. The TTL-expiry/staleness bound is
	// covered separately by TestHandleStatusCacheExpiresOnTTL. The TTL floor is
	// pinned off so the bucket cache alone carries the assertion; the floor's
	// own behavior is covered by
	// TestHandleStatusServesRecentResponseDespiteIndexAdvance.
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	oldFloor := statusResponseTTLFloor
	statusResponseTTLFloor = 0
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
	})

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", rec.Code)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after cached repeat = %d, want 1", store.listCalls)
	}

	// A moving event sequence — the busy-city scenario — must keep hitting the
	// time-bucketed cache, not force a rebuild.
	for i := 0; i < 5; i++ {
		state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status after event %d = %d, want 200", i, rec.Code)
		}
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after %d index changes = %d, want 1 (time-bucketed cache must survive sequence churn)", 5, store.listCalls)
	}

	// The X-GC-Index header still reflects the live sequence even on a cache
	// hit, so blocking/long-poll consumers see fresh index values.
	if got := rec.Header().Get("X-GC-Index"); got == "" || got == "0" {
		t.Fatalf("X-GC-Index = %q, want live sequence on cache hit", got)
	}
}

// TestHandleStatusCacheExpiresOnTTL verifies the staleness bound: once the
// time bucket rolls over, the next /status rebuilds. Drives responseCacheTimeBucket
// directly by collapsing the TTL so the test stays fast and deterministic.
func TestHandleStatusCacheExpiresOnTTL(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // every request lands in a new bucket
	oldFloor := statusResponseTTLFloor
	statusResponseTTLFloor = 0 // floor off: each request must reach the rebuild
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
	})

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil)
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status #%d = %d, want 200", i, rec.Code)
		}
	}
	if store.listCalls < 2 {
		t.Fatalf("List calls with expiring TTL = %d, want >= 2 (each request should rebuild)", store.listCalls)
	}
}

// TestHandleStatusBlockingBypassesTimeCache verifies the preserved
// strict-freshness path: a blocking ?index=&wait= request must rebuild the
// body (reflecting the event it waited for) instead of being served a
// time-bucketed cache entry built before that event (gascity#3186).
func TestHandleStatusBlockingBypassesTimeCache(t *testing.T) {
	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	// Prime the time-bucketed cache with a non-blocking request.
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("priming status = %d, want 200", rec.Code)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after priming = %d, want 1", store.listCalls)
	}

	// A blocking request (index=0 returns immediately since the sequence is
	// already ahead) must bypass the time cache and rebuild.
	blockReq := httptest.NewRequest(http.MethodGet, cityURL(state, "/status?index=0&wait=1s"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, blockReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("blocking status = %d, want 200", rec.Code)
	}
	if store.listCalls != 2 {
		t.Fatalf("List calls after blocking request = %d, want 2 (blocking must bypass time cache)", store.listCalls)
	}
}

func TestHandleAgentListCachesUntilIndexChanges(t *testing.T) {
	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/agents"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first agents = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second agents = %d, want 200", rec.Code)
	}

	if store.listByAssigneeCalls != 2 {
		t.Fatalf("ListByAssignee calls after cached repeat = %d, want 2", store.listByAssigneeCalls)
	}

	state.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("third agents = %d, want 200", rec.Code)
	}
	if store.listByAssigneeCalls != 4 {
		t.Fatalf("ListByAssignee calls after index change = %d, want 4", store.listByAssigneeCalls)
	}
}

func TestHandleOrdersFeedCachesUntilIndexChanges(t *testing.T) {
	state := newFakeState(t)
	rigStore := &countingStore{Store: beads.NewMemStore()}
	cityStore := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = rigStore
	state.cityBeadStore = cityStore

	_, err := rigStore.Create(beads.Bead{
		Title: "Adopt PR",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-123",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/orders/feed?scope_kind=rig&scope_ref=myrig"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first feed = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal first feed: %v", err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second feed = %d, want 200", rec.Code)
	}
	if rigStore.listCalls != 1 {
		t.Fatalf("rig List calls after cached repeat = %d, want 1", rigStore.listCalls)
	}
	if cityStore.listByLabelCalls != 1 {
		t.Fatalf("city ListByLabel calls after cached repeat = %d, want 1", cityStore.listByLabelCalls)
	}

	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("third feed = %d, want 200", rec.Code)
	}
	if rigStore.listCalls != 2 {
		t.Fatalf("rig List calls after index change = %d, want 2", rigStore.listCalls)
	}
	if cityStore.listByLabelCalls != 2 {
		t.Fatalf("city ListByLabel calls after index change = %d, want 2", cityStore.listByLabelCalls)
	}
}

// newFormulaFeedCacheFixture seeds a rig store with one graph.v2 workflow
// root so /formulas/feed has a body to build, and returns the wrapped store
// whose listCalls counts feed rebuilds.
func newFormulaFeedCacheFixture(t *testing.T) (*fakeState, *countingStore, http.Handler) {
	t.Helper()
	state := newFakeState(t)
	rigStore := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = rigStore
	if _, err := rigStore.Create(beads.Bead{
		Title: "Adopt PR",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-123",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	}); err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	return state, rigStore, newTestCityHandler(t, state)
}

// TestHandleFormulaFeedCachesAcrossIndexChanges pins the #3208 feed-latency
// fix: /formulas/feed keys its response cache on a wall-clock TTL bucket, not
// the event sequence, so a busy city (whose sequence advances every poll) no
// longer rebuilds the O(store-history) feed body on every request.
func TestHandleFormulaFeedCachesAcrossIndexChanges(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })

	state, rigStore, h := newFormulaFeedCacheFixture(t)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/feed?scope_kind=rig&scope_ref=myrig"), nil)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed #%d = %d, want 200", i, rec.Code)
		}
	}
	if rigStore.listCalls != 1 {
		t.Fatalf("rig List calls after cached repeat = %d, want 1", rigStore.listCalls)
	}

	// A moving event sequence — the busy-city scenario from #3208 — must
	// keep hitting the time-bucketed cache, not force a rebuild per poll.
	for i := 0; i < 5; i++ {
		state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed after event %d = %d, want 200", i, rec.Code)
		}
	}
	if rigStore.listCalls != 1 {
		t.Fatalf("rig List calls across index churn = %d, want 1 (feed must key on time bucket)", rigStore.listCalls)
	}
}

// TestHandleFormulaFeedCacheExpiresOnTTL verifies the feed's staleness bound:
// once the time bucket rolls over, the next request rebuilds.
func TestHandleFormulaFeedCacheExpiresOnTTL(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // every request lands in a new bucket
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })

	state, rigStore, h := newFormulaFeedCacheFixture(t)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/feed?scope_kind=rig&scope_ref=myrig"), nil)
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed #%d = %d, want 200", i, rec.Code)
		}
	}
	if rigStore.listCalls < 2 {
		t.Fatalf("rig List calls with expiring TTL = %d, want >= 2", rigStore.listCalls)
	}
}

// TestHandleBeadListAllCachesAcrossIndexChanges pins the #3208 large-read
// lever: all=true /beads reads (which bypass the CachingStore and scan full
// history per rig) key their response cache on a time bucket, so concurrent
// pollers share one rebuild per TTL window. Open-only reads stay uncached.
func TestHandleBeadListAllCachesAcrossIndexChanges(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	if _, err := store.Create(beads.Bead{Title: "task one", Type: "task"}); err != nil {
		t.Fatalf("create bead: %v", err)
	}
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads?all=true"), nil)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("beads all #%d = %d, want 200", i, rec.Code)
		}
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after cached all=true repeat = %d, want 1", store.listCalls)
	}

	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("beads all after event = %d, want 200", rec.Code)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls across index churn = %d, want 1 (all=true must key on time bucket)", store.listCalls)
	}

	// Open-only reads are served from the store every time — they hit the
	// in-memory CachingStore in production and must stay fresh.
	openReq := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads"), nil)
	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, openReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("open beads #%d = %d, want 200", i, rec.Code)
		}
	}
	if store.listCalls != 3 {
		t.Fatalf("List calls after open-only reads = %d, want 3 (open reads must not be response-cached)", store.listCalls)
	}
}

// TestHandleBeadListAllBlockingBypassesTimeCache verifies the preserved
// strict-freshness path on /beads: a blocking ?index=&wait= all=true request
// must rebuild the body rather than be served an entry built before the
// event it waited for.
func TestHandleBeadListAllBlockingBypassesTimeCache(t *testing.T) {
	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	if _, err := store.Create(beads.Bead{Title: "task one", Type: "task"}); err != nil {
		t.Fatalf("create bead: %v", err)
	}
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads?all=true"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("priming beads all = %d, want 200", rec.Code)
	}
	if store.listCalls != 1 {
		t.Fatalf("List calls after priming = %d, want 1", store.listCalls)
	}

	blockReq := httptest.NewRequest(http.MethodGet, cityURL(state, "/beads?all=true&index=0&wait=1s"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, blockReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("blocking beads all = %d, want 200", rec.Code)
	}
	if store.listCalls != 2 {
		t.Fatalf("List calls after blocking request = %d, want 2 (blocking must bypass time cache)", store.listCalls)
	}
}
