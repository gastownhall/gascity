package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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
// time bucket rolls over AND no entry has ever been cached, the next
// /status rebuilds synchronously (there is nothing stale to serve). Drives
// responseCacheTimeBucket directly by collapsing the TTL so the test stays
// fast and deterministic.
//
// Once an entry DOES exist, a bucket/floor miss is now served that stale
// entry immediately and refreshed in the background instead of rebuilding
// inline (ra-4u2eqc, stale-while-revalidate) — see
// TestHandleStatusServesStaleAndRefreshesInBackground for that path. This
// test only pins the true-cold-start case, which is unaffected by SWR.
func TestHandleStatusCacheExpiresOnTTL(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // every request lands in a new bucket
	oldFloor := statusResponseTTLFloor
	statusResponseTTLFloor = 0 // floor off: only the bucket cache can hit
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
	if store.listCalls != 1 {
		t.Fatalf("List calls after cold first request = %d, want 1", store.listCalls)
	}
}

// TestHandleStatusServesStaleAndRefreshesInBackground is the ra-4u2eqc
// regression test. On a cache-miss with a previously cached body available,
// /status must serve that stale body immediately — never blocking the
// request on a slow rebuild — and refresh in the background so the next
// poll gets a fresh body. Uses a store whose List blocks until released to
// prove the second (SWR) request does not wait on it.
func TestHandleStatusServesStaleAndRefreshesInBackground(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // every request lands in a new bucket
	oldFloor := statusResponseTTLFloor
	statusResponseTTLFloor = 0 // floor off: force every request past both fast-path caches
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
	})

	// Pin the liveness clock so the city store's snapshot age is exact, and
	// back the city scope with a CachingStore-like reporter whose last fresh
	// observation is staleSnapshotAgeS old. That is the lagging-reconciler
	// case: the response entry below is milliseconds old, the snapshot it was
	// built from is minutes old, and X-GC-Cache-Age-S must report the worse
	// of the two so `gc status`'s staleness banner still fires.
	const staleSnapshotAgeS = 300.0
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(SetLivenessClockForTest(&clock.Fake{Time: now}))

	state := newFakeState(t)
	fastStore := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = fastStore
	state.cityBeadStore = stubLivenessReporter{
		Store:     beads.NewMemStore(),
		live:      true,
		lastFresh: now.Add(-time.Duration(staleSnapshotAgeS) * time.Second),
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil)

	// Prime the cache with a fast, unblocked build.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("priming status = %d, want 200", rec.Code)
	}
	var primed statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &primed); err != nil {
		t.Fatalf("decode priming response: %v", err)
	}

	// Swap in a store that blocks List until released, then issue the
	// cache-miss request. If the handler still blocks on a rebuild, this
	// request hangs until the test's hang-guard fires; SWR must return
	// promptly instead, serving the primed (stale) body.
	release := make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(closeRelease) // safety net if the test fails before the explicit close below
	state.stores["myrig"] = &blockingListStore{Store: fastStore, release: release}

	rec2 := httptest.NewRecorder()
	served := make(chan struct{})
	go func() { h.ServeHTTP(rec2, req); close(served) }()
	select {
	case <-served:
	case <-time.After(5 * time.Second): // hang guard, not a timing bound
		t.Fatal("status handler blocked on rebuild instead of serving the stale cached body (SWR not honored)")
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("stale-served status = %d, want 200", rec2.Code)
	}
	var stale statusResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &stale); err != nil {
		t.Fatalf("decode stale response: %v", err)
	}
	if stale.UptimeSec != primed.UptimeSec || stale.Name != primed.Name {
		t.Fatalf("stale-served body = %+v, want a copy of the primed body %+v", stale, primed)
	}

	// The stale-served response still carries the existing X-GC-Cache-Age-S
	// staleness marker (ra-4u2eqc's "if the API shape allows" — it already
	// does, so no new field was added), letting the CLI's existing >30s
	// staleness banner logic (which reads this same header via the CLI's
	// api.Client) apply to an SWR-served body the same way it does to any
	// other cache hit. Its VALUE matters, not just its presence: the header
	// must report the greater of the response-entry age (milliseconds here)
	// and the city store's snapshot age (staleSnapshotAgeS). Reporting only
	// the entry age would silently suppress the CLI banner on exactly the
	// lagging-reconciler city this endpoint exists to survive.
	raw := rec2.Header().Get("X-GC-Cache-Age-S")
	if raw == "" {
		t.Fatal("stale-served response missing X-GC-Cache-Age-S header")
	}
	age, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("X-GC-Cache-Age-S = %q, want a float: %v", raw, err)
	}
	if age < 0 {
		t.Fatalf("X-GC-Cache-Age-S = %v, want >= 0", age)
	}
	if age != staleSnapshotAgeS {
		t.Fatalf("X-GC-Cache-Age-S = %v, want %v (the greater of the response-entry age and the lagging store snapshot age)", age, staleSnapshotAgeS)
	}

	// Concurrent duplicate misses must coalesce onto a single background
	// rebuild rather than each launching their own. Issue them while the
	// blocking store is still held, so every one of them races the same
	// in-flight guard; the assertion is on the List count measured after a
	// barrier, never on timing.
	const concurrentMisses = 4
	var wg sync.WaitGroup
	concurrentRecs := make([]*httptest.ResponseRecorder, concurrentMisses)
	for i := range concurrentRecs {
		concurrentRecs[i] = httptest.NewRecorder()
		wg.Add(1)
		go func(rec *httptest.ResponseRecorder) {
			defer wg.Done()
			// A per-goroutine request: http.Request is not safe to share
			// across concurrent ServeHTTP calls.
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil))
		}(concurrentRecs[i])
	}
	allServed := make(chan struct{})
	go func() { wg.Wait(); close(allServed) }()
	select {
	case <-allServed:
	case <-time.After(5 * time.Second): // hang guard, not a timing bound
		t.Fatal("concurrent /status requests blocked on rebuild instead of being served the stale cached body (SWR not honored)")
	}
	for i, rec := range concurrentRecs {
		if rec.Code != http.StatusOK {
			t.Fatalf("concurrent stale-served status #%d = %d, want 200", i, rec.Code)
		}
	}

	// Release the blocked store and let the background refresh finish, then
	// confirm it actually ran (not skipped) by observing a fresh List call —
	// and that all the misses above produced exactly ONE rebuild between
	// them, not one apiece.
	listCallsBeforeRefresh := fastStore.listCalls
	closeRelease()
	srv.waitForBackground()
	if got := fastStore.listCalls - listCallsBeforeRefresh; got != 1 {
		t.Fatalf("rig List calls during background refresh = %d, want exactly 1 (%d concurrent misses must coalesce onto one rebuild)", got, concurrentMisses+1)
	}

	// The in-flight guard must clear after completion; a leaked guard would
	// wedge every future refresh for this key.
	if srv.responseRefreshing["status"] {
		t.Fatal("responseRefreshing[\"status\"] still true after background refresh completed (leaked guard)")
	}
}

// panicOnIsRunningProvider panics from IsRunning, which buildStatusBody calls
// synchronously on its own goroutine while counting agents. That makes it an
// injection point for a panic raised on the SWR background-refresh goroutine
// itself — the blast radius this endpoint's move to a detached rebuild
// created. A rig store that panics would NOT exercise the same path: the
// work-count fan-out reads stores from child goroutines
// (statusWorkCounts/statusListStoreWithTimeout), whose panics no recover in
// the refresh closure can catch, and which already crashed the process on the
// pre-SWR request path too.
type panicOnIsRunningProvider struct {
	runtime.Provider
}

func (p *panicOnIsRunningProvider) IsRunning(string) bool {
	panic("injected panic from the status background refresh")
}

// TestHandleStatusBackgroundRefreshPanicDoesNotCrashServer pins the blast
// radius of the ra-4u2eqc background rebuild. Before SWR, buildStatusBody ran
// on the request path, where withRecovery (middleware.go) turned a panic into
// a single 500. runBackground spawns its task with no recover of its own, so
// the detached rebuild must recover for itself or one panic takes the whole
// controller process down. The panicking refresh must also release the
// in-flight guard, or every future refresh for this key is wedged.
func TestHandleStatusBackgroundRefreshPanicDoesNotCrashServer(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // every request lands in a new bucket
	oldFloor := statusResponseTTLFloor
	statusResponseTTLFloor = 0 // floor off: force the second request past both fast-path caches
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
	})

	state := newFakeState(t)
	fastStore := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = fastStore
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	// Prime the cache with a healthy build, so the request below has a stale
	// entry to be served and takes the SWR branch rather than falling through
	// to the synchronous cold-start build.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("priming status = %d, want 200", rec.Code)
	}
	var primed statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &primed); err != nil {
		t.Fatalf("decode priming response: %v", err)
	}

	// Arm the panic only after priming, so it can fire in the background
	// rebuild and nowhere else.
	state.sessionProvider = &panicOnIsRunningProvider{Provider: state.sp}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("stale-served status = %d, want 200", rec2.Code)
	}
	var stale statusResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &stale); err != nil {
		t.Fatalf("decode stale response: %v", err)
	}
	if stale.UptimeSec != primed.UptimeSec || stale.Name != primed.Name {
		t.Fatalf("stale-served body = %+v, want a copy of the primed body %+v", stale, primed)
	}

	// If the refresh did not recover, the process is already gone and this
	// line never runs; reaching it at all is half the assertion.
	srv.waitForBackground()

	if srv.responseRefreshing["status"] {
		t.Fatal("responseRefreshing[\"status\"] still true after a panicking background refresh (leaked guard wedges every future refresh)")
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

// listCallsPerFeedBuild is how many store List calls one workflow-projection
// build costs: listActiveWorkflowProjectionBeads issues one Live,
// status-scoped read per active status, because bd is the only reader that can
// filter on the raw status (gc-4zb). These tests assert how often the feed
// rebuilds, so they count builds in reads rather than pinning a literal.
var listCallsPerFeedBuild = len(activeWorkflowProjectionStatuses)

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
	if rigStore.listCalls != listCallsPerFeedBuild {
		t.Fatalf("rig List calls after cached repeat = %d, want %d (one build)", rigStore.listCalls, listCallsPerFeedBuild)
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
	if want := 2 * listCallsPerFeedBuild; rigStore.listCalls != want {
		t.Fatalf("rig List calls after index change = %d, want %d (two builds)", rigStore.listCalls, want)
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
	if rigStore.listCalls != listCallsPerFeedBuild {
		t.Fatalf("rig List calls after cached repeat = %d, want %d (one build)", rigStore.listCalls, listCallsPerFeedBuild)
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
	if rigStore.listCalls != listCallsPerFeedBuild {
		t.Fatalf("rig List calls across index churn = %d, want %d (one build; feed must key on time bucket)", rigStore.listCalls, listCallsPerFeedBuild)
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
