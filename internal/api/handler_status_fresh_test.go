package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// pinStatusBucket lands every /status read of one test in a single time
// bucket, keeps saved entries from expiring and turns the TTL floor off, so
// any read that uses the response cache is a bucket hit on the entry the test
// primed, even after the test stalls past responseCacheTTL's 2s.
func pinStatusBucket(t *testing.T) {
	t.Helper()
	oldTTL, oldFloor, oldEntryTTL := timeBucketResponseCacheTTL, statusResponseTTLFloor, responseCacheTTL
	timeBucketResponseCacheTTL, statusResponseTTLFloor, responseCacheTTL = time.Hour, 0, time.Hour
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
		responseCacheTTL = oldEntryTTL
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
// a certain time-bucket hit, and its build replaces the saved body, so the
// pollers that share it see the same city (ga-v1vyh8.17).
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

	srv.waitForBackground()
	if got := savedStatusAt(t, srv, "status"); !got.After(primedAt) {
		t.Errorf("saved entry storedAt = %v, want after the primed %v: the fresh read's build saves its body", got, primedAt)
	}
	again, _ := getStatusBody(t, h, state, "/status")
	if again.Work.Open != 1 {
		t.Errorf("plain read after fresh Work.Open = %d, want 1: pollers get the body the fresh read's build saved", again.Work.Open)
	}
	if store.listCalls != 2 {
		t.Errorf("List calls after the plain read = %d, want 2 (a bucket hit on the saved build)", store.listCalls)
	}
}

// stalledRuntime holds every IsRunning call, which each /status build makes
// while counting agents, until release closes: a runtime that accepts calls
// but stops answering, as a wedged tmux server does. entered closes on the
// first call, once a build has reached the runtime.
type stalledRuntime struct {
	runtime.Provider
	release <-chan struct{}
	entered chan struct{}
	once    sync.Once
}

// newStalledRuntime returns a stalledRuntime over sp, held until release
// closes.
func newStalledRuntime(sp runtime.Provider, release <-chan struct{}) *stalledRuntime {
	return &stalledRuntime{Provider: sp, release: release, entered: make(chan struct{})}
}

func (p *stalledRuntime) IsRunning(name string) bool {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return p.Provider.IsRunning(name)
}

// shortenStatusFreshBuildWait sets statusFreshBuildWait to d for one test.
func shortenStatusFreshBuildWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := statusFreshBuildWait
	statusFreshBuildWait = d
	t.Cleanup(func() { statusFreshBuildWait = old })
}

// serveInBackground serves req through h on its own goroutine and returns the
// recorder and a channel that closes once req is answered.
func serveInBackground(h http.Handler, req *http.Request) (*httptest.ResponseRecorder, <-chan struct{}) {
	rec := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		h.ServeHTTP(rec, req)
	}()
	return rec, served
}

// freshStatusRequest is a GET of state's city's /status?fresh=true.
func freshStatusRequest(state State) *http.Request {
	return httptest.NewRequest(http.MethodGet, cityURL(state, "/status?fresh=true"), nil)
}

// awaitSignal fails t unless ch closes before a hang guard, which bounds a
// broken test, not the code's timing.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second): // hang guard, not a timing bound
		t.Fatalf("timed out waiting: %s", what)
	}
}

// A ?fresh=true read whose build is not done statusFreshBuildWait after the
// read arrived, on a runtime that stopped answering, is answered with the
// saved body, reported as a stale-while-revalidate serve reports it
// (rebuilding, with the saved body's age and index), instead of holding the
// reader until its client times out. Later fresh reads queue one build behind
// the stalled one rather than stacking builds, and once the runtime answers
// the builds save their bodies for the next read.
func TestHandleStatusFreshAnswersWithTheSavedBodyWhenItsBuildOverruns(t *testing.T) {
	pinStatusBucket(t)
	shortenStatusFreshBuildWait(t, 10*time.Millisecond)

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	_, rec := getStatusBody(t, h, state, "/status")
	primedIndex := responseIndex(t, rec)
	backdateSavedStatus(t, srv, 140*time.Second)
	if _, err := store.Create(beads.Bead{Title: "new work", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		closeRelease()
		srv.waitForBackground()
	})
	state.sessionProvider = newStalledRuntime(state.sp, release)

	for read := 1; read <= 3; read++ {
		rec, served := serveInBackground(h, freshStatusRequest(state))
		awaitSignal(t, served, fmt.Sprintf("fresh read %d was held by its stalled build past statusFreshBuildWait", read))
		if rec.Code != http.StatusOK {
			t.Fatalf("fresh read %d = %d, want 200: %s", read, rec.Code, rec.Body.String())
		}
		var body StatusBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode fresh read %d: %v", read, err)
		}
		if body.Work.Open != 0 {
			t.Errorf("fresh read %d Work.Open = %d, want 0: the saved body, which predates the task", read, body.Work.Open)
		}
		if got := rec.Header().Get("X-GC-Cache-Refresh"); got != "rebuilding" {
			t.Errorf("fresh read %d X-GC-Cache-Refresh = %q, want rebuilding", read, got)
		}
		if age := cacheAgeIn(t, "/status?fresh=true", rec.Header()); math.Floor(age) != 140 {
			t.Errorf("fresh read %d X-GC-Cache-Age-S = %v, want 140, the saved body's age", read, age)
		}
		if idx := responseIndex(t, rec); idx != primedIndex {
			t.Errorf("fresh read %d X-GC-Index = %d, want the saved body's %d", read, idx, primedIndex)
		}
	}

	closeRelease()
	srv.waitForBackground()
	if store.listCalls != 3 {
		t.Errorf("List calls = %d, want 3: the primed build, the stalled one, and ONE build reads 2 and 3 queued behind it", store.listCalls)
	}
	saved, _ := getStatusBody(t, h, state, "/status")
	if saved.Work.Open != 1 {
		t.Errorf("plain read Work.Open = %d, want 1: the overrun builds save their bodies once the runtime answers", saved.Work.Open)
	}
	fresh, rec := getStatusBody(t, h, state, "/status?fresh=true")
	if fresh.Work.Open != 1 || rec.Header().Get("X-GC-Cache-Refresh") == "rebuilding" {
		t.Errorf("fresh read after the builds Work.Open = %d, X-GC-Cache-Refresh = %q; want 1, not rebuilding: a body built for the read", fresh.Work.Open, rec.Header().Get("X-GC-Cache-Refresh"))
	}
	if store.listCalls != 4 {
		t.Errorf("List calls = %d, want 4: a fresh read with no build running starts one", store.listCalls)
	}
}

// A ?fresh=true read that arrives while another build of its view is running,
// here a poller's stale-while-revalidate refresh, waits for the build queued
// behind it, not for the running one: that build may have read the city
// before a change the caller just made. Every read that queues meanwhile
// shares the one queued build.
func TestFreshStatusBuildQueuesBehindARunningBuild(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	release := make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		closeRelease()
		srv.waitForBackground()
	})
	stalled := newStalledRuntime(state.sp, release)
	state.sessionProvider = stalled

	srv.refreshStatusResponseInBackground("status", false)
	awaitSignal(t, stalled.entered, "the poller's refresh to reach the runtime")
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	head, err := ep.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}

	first := srv.freshStatusBuild("status", false)
	if second := srv.freshStatusBuild("status", false); second != first {
		t.Error("two fresh reads that arrived during one running build got different builds, want the one queued build")
	}
	closeRelease()
	awaitSignal(t, first.done, "the queued build to run once the refresh ends")
	if first.built == nil {
		t.Fatal("the queued build saved no body")
	}
	if first.built.Index < head {
		t.Fatalf("queued build X-GC-Index = %d, want at least %d, the head when the reads queued: the build a fresh read waits on must start after it", first.built.Index, head)
	}
}

// A ?fresh=true read with nothing saved has nothing better to answer with
// once its build overruns statusFreshBuildWait, so it waits for the build, as
// a cold read does, and the build saves its body for later reads.
func TestHandleStatusFreshWithNothingSavedWaitsForItsBuild(t *testing.T) {
	pinStatusBucket(t)
	shortenStatusFreshBuildWait(t, 0)

	state := newFakeState(t)
	store := &countingStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store
	if _, err := store.Create(beads.Bead{Title: "new work", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)
	release := make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		closeRelease()
		srv.waitForBackground()
	})
	stalled := newStalledRuntime(state.sp, release)
	state.sessionProvider = stalled

	rec, served := serveInBackground(h, freshStatusRequest(state))
	awaitSignal(t, stalled.entered, "the fresh read's build to reach the runtime")
	select {
	case <-served:
		t.Fatalf("fresh read answered %d while its build was stalled and nothing was saved, want it to wait: %s", rec.Code, rec.Body.String())
	default:
	}
	closeRelease()
	awaitSignal(t, served, "the fresh read to be answered once its build finished")
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh read = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body StatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode fresh read: %v", err)
	}
	if body.Work.Open != 1 {
		t.Errorf("fresh read Work.Open = %d, want 1: the body its build made", body.Work.Open)
	}
	if got := rec.Header().Get("X-GC-Cache-Refresh"); got == "rebuilding" {
		t.Error("fresh read X-GC-Cache-Refresh = rebuilding, want never on a body built for the read")
	}
	srv.waitForBackground()
	saved, _, ok := staleResponseAs[indexedResponse[StatusBody]](srv, "status")
	if !ok || saved.Body.Work.Open != 1 {
		t.Errorf("saved /status entry = %+v (ok=%v), want the fresh read's build, Work.Open 1", saved.Body.Work, ok)
	}
}

// A ?fresh=true read still waiting on its build when its request ends is
// answered 503 at once instead of holding the handler, and the build runs on
// to save its body for the next read.
func TestHandleStatusFreshReadEndsWithItsRequest(t *testing.T) {
	pinStatusBucket(t)
	shortenStatusFreshBuildWait(t, 0)

	state := newFakeState(t)
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)
	release := make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		closeRelease()
		srv.waitForBackground()
	})
	stalled := newStalledRuntime(state.sp, release)
	state.sessionProvider = stalled

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec, served := serveInBackground(h, freshStatusRequest(state).WithContext(ctx))
	awaitSignal(t, stalled.entered, "the fresh read's build to reach the runtime")
	cancel()
	awaitSignal(t, served, "the fresh read to end with its request")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fresh read whose request ended = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	closeRelease()
	srv.waitForBackground()
	if _, _, ok := staleResponseAs[indexedResponse[StatusBody]](srv, "status"); !ok {
		t.Fatal("no /status body saved: the build must run on after its read ended")
	}
}

// A ?fresh=true read whose build panics is answered 500: the panic does not
// take the server down, and the build does not stay registered to wedge the
// next read.
func TestHandleStatusFreshBuildPanicIsAServerError(t *testing.T) {
	state := newFakeState(t)
	state.sessionProvider = &panicOnIsRunningProvider{Provider: state.sp}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/status?fresh=true"), nil))
	srv.waitForBackground()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("fresh read with a panicking build = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if len(srv.statusBuilds) != 0 {
		t.Fatalf("statusBuilds = %v after the panicking build, want empty (a leaked build wedges every later fresh read)", srv.statusBuilds)
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

// ?lite=true&fresh=true builds the lite body for the read and saves it as the
// lite entry, leaving the full entry alone.
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
	srv.waitForBackground()
	if got := savedStatusAt(t, srv, "status"); !got.Equal(fullAt) {
		t.Error("the fresh lite read replaced the saved full body")
	}
	if got := savedStatusAt(t, srv, "status?lite"); !got.After(liteAt) {
		t.Error("the fresh lite read's build did not save its body as the lite entry")
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
