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
	// covered separately by TestHandleStatusCacheExpiresOnTTL.
	oldTTL := statusResponseCacheTTL
	statusResponseCacheTTL = time.Hour
	t.Cleanup(func() { statusResponseCacheTTL = oldTTL })

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
// time bucket rolls over, the next /status rebuilds. Drives statusCacheBucket
// directly by collapsing the TTL so the test stays fast and deterministic.
func TestHandleStatusCacheExpiresOnTTL(t *testing.T) {
	oldTTL := statusResponseCacheTTL
	statusResponseCacheTTL = time.Nanosecond // every request lands in a new bucket
	t.Cleanup(func() { statusResponseCacheTTL = oldTTL })

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
