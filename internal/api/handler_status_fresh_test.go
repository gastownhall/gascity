package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// pinStatusBucket lands every /status read of one test in a single time
// bucket with the TTL floor off, so any read that uses the response cache is a
// certain bucket hit on the entry the test primed.
func pinStatusBucket(t *testing.T) {
	t.Helper()
	oldTTL, oldFloor := timeBucketResponseCacheTTL, statusResponseTTLFloor
	timeBucketResponseCacheTTL, statusResponseTTLFloor = time.Hour, 0
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
	})
}

// getStatusBody GETs path from state's city through h and decodes the body.
func getStatusBody(t *testing.T, h http.Handler, state State, path string) (StatusBody, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, path), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
	}
	var body StatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode GET %s: %v", path, err)
	}
	return body, rec
}

// savedStatusAt reports when srv saved its response under key.
func savedStatusAt(t *testing.T, srv *Server, key string) time.Time {
	t.Helper()
	srv.responseCacheMu.Lock()
	defer srv.responseCacheMu.Unlock()
	entry, ok := srv.responseCacheEntries[key]
	if !ok {
		t.Fatalf("no response saved under %q", key)
	}
	return entry.storedAt
}

// A ?fresh=true read is built for the request even when a saved body would be
// a certain time-bucket hit, and it neither replaces nor saves an entry, so
// pollers keep the body they were sharing (ga-v1vyh8.17).
func TestHandleStatusFreshBuildsInsteadOfServingTheSavedBody(t *testing.T) {
	pinStatusBucket(t)

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	primed, _ := getStatusBody(t, h, state, "/status")
	if primed.Work.Open != 0 || store.listCalls != 1 {
		t.Fatalf("primed Work.Open = %d, List calls = %d; want 0 and 1", primed.Work.Open, store.listCalls)
	}
	primedAt := savedStatusAt(t, srv, "status")

	if _, err := store.Create(beads.Bead{Title: "new work", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	head, err := state.eventProv.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}

	fresh, rec := getStatusBody(t, h, state, "/status?fresh=true")
	if fresh.Work.Open != 1 {
		t.Errorf("fresh Work.Open = %d, want 1: the read must reflect the task created after the saved body", fresh.Work.Open)
	}
	if store.listCalls != 2 {
		t.Errorf("List calls after the fresh read = %d, want 2 (one build for the read)", store.listCalls)
	}
	if idx := responseIndex(t, rec); idx != head {
		t.Errorf("fresh X-GC-Index = %d, want the head %d captured at the request", idx, head)
	}
	if got := rec.Header().Get("X-GC-Cache-Refresh"); got == "rebuilding" {
		t.Error("fresh X-GC-Cache-Refresh = rebuilding, want never on a body built for the read")
	}

	again, _ := getStatusBody(t, h, state, "/status")
	if again.Work.Open != 0 {
		t.Errorf("plain read after fresh Work.Open = %d, want 0: the fresh read must not replace the saved body", again.Work.Open)
	}
	if got := savedStatusAt(t, srv, "status"); !got.Equal(primedAt) {
		t.Errorf("saved entry storedAt = %v, want the primed %v: the fresh read must not save its body", got, primedAt)
	}
}

// A ?fresh=true read on a cold cache builds the body without saving it.
func TestHandleStatusFreshColdCacheSavesNothing(t *testing.T) {
	pinStatusBucket(t)

	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	getStatusBody(t, h, state, "/status?fresh=true")
	if _, _, ok := staleResponseAs[indexedResponse[StatusBody]](srv, "status"); ok {
		t.Fatal("a fresh read on a cold cache saved a /status entry; want none")
	}
}

// A ?fresh=true read reports the X-GC-Index captured before its build, not the
// saved body's and not one that passes an event recorded during the build
// (#7661).
func TestHandleStatusFreshReportsTheIndexCapturedBeforeItsBuild(t *testing.T) {
	pinStatusBucket(t)

	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	_, rec := getStatusBody(t, h, state, "/status")
	primedIndex := responseIndex(t, rec)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	head, err := ep.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}
	if head <= primedIndex {
		t.Fatalf("head %d did not advance past the primed index %d", head, primedIndex)
	}
	late := &lateEventProvider{Provider: state.sp, ep: ep}
	state.sessionProvider = late

	_, rec = getStatusBody(t, h, state, "/status?fresh=true")
	if late.lateSeq == 0 {
		t.Fatal("the mid-build event did not fire; the fresh read no longer asks the session provider")
	}
	if idx := responseIndex(t, rec); idx < head || idx >= late.lateSeq {
		t.Fatalf("fresh X-GC-Index = %d, want at least %d (the head at the request) and below %d (the event recorded during its build)", idx, head, late.lateSeq)
	}
}

// isRunningCounter counts IsRunning calls, which every /status build makes
// while counting agents.
type isRunningCounter struct {
	runtime.Provider
	calls atomic.Int64
}

func (p *isRunningCounter) IsRunning(name string) bool {
	p.calls.Add(1)
	return p.Provider.IsRunning(name)
}

// ?lite=true&fresh=true builds the lite body for the read and leaves both the
// full and the lite saved entries alone.
func TestHandleStatusFreshLiteBuildsTheLiteBody(t *testing.T) {
	pinStatusBucket(t)

	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	getStatusBody(t, h, state, "/status")
	getStatusBody(t, h, state, "/status?lite=true")
	fullAt := savedStatusAt(t, srv, "status")
	liteAt := savedStatusAt(t, srv, "status?lite")

	counter := &isRunningCounter{Provider: state.sp}
	state.sessionProvider = counter
	body, _ := getStatusBody(t, h, state, "/status?lite=true&fresh=true")
	if counter.calls.Load() == 0 {
		t.Error("the fresh lite read asked no session state: it was answered from the saved body, not built")
	}
	if body.StoreHealth != nil {
		t.Errorf("fresh lite StoreHealth = %+v, want nil", body.StoreHealth)
	}
	if body.Work != (StatusWorkCounts{}) {
		t.Errorf("fresh lite Work = %+v, want zero", body.Work)
	}
	if got := savedStatusAt(t, srv, "status"); !got.Equal(fullAt) {
		t.Error("the fresh lite read replaced the saved full body")
	}
	if got := savedStatusAt(t, srv, "status?lite"); !got.Equal(liteAt) {
		t.Error("the fresh lite read replaced the saved lite body")
	}
}

// ?fresh=false is the default: the read is answered from the response cache.
func TestHandleStatusFreshFalseKeepsTheCache(t *testing.T) {
	pinStatusBucket(t)

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	h := newTestCityHandler(t, state)

	getStatusBody(t, h, state, "/status")
	getStatusBody(t, h, state, "/status?fresh=false")
	if store.listCalls != 1 {
		t.Fatalf("List calls after ?fresh=false = %d, want 1 (a bucket hit)", store.listCalls)
	}
}
