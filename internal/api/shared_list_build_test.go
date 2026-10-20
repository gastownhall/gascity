package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// gatedSessionReadStore counts the session-bead reads a list build makes and
// holds each one until release is closed.
type gatedSessionReadStore struct {
	beads.Store
	release chan struct{}
	reads   atomic.Int32
}

func (s *gatedSessionReadStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == session.LabelSession || query.Type == session.BeadType {
		s.reads.Add(1)
		<-s.release
	}
	return s.Store.List(query)
}

// A dashboard asks for /sessions again on every session or bead event. On a
// busy city the copies piled up and each took 15s to 2 minutes, so no copy
// ever answered. Identical requests that overlap must share one build.
func TestHandleSessionListSharesOneBuildAcrossConcurrentRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const callers = 8

		fs := newSessionFakeState(t)
		info := createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
		gate := &gatedSessionReadStore{Store: fs.cityBeadStore, release: make(chan struct{})}
		fs.cityBeadStore = gate
		srv := New(fs)
		input := &SessionListInput{CityScope: CityScope{CityName: fs.cityName}}

		// One request alone, to learn how many session reads one build makes.
		close(gate.release)
		if _, err := srv.humaHandleSessionList(context.Background(), input); err != nil {
			t.Fatalf("solo list: %v", err)
		}
		readsPerBuild := gate.reads.Load()
		if readsPerBuild == 0 {
			t.Fatal("a list build made no session reads; the gate cannot hold it")
		}
		gate.reads.Store(0)
		gate.release = make(chan struct{})

		outs := make([]*ListOutput[sessionResponse], callers)
		for i := range callers {
			go func() {
				out, err := srv.humaHandleSessionList(context.Background(), input)
				if err != nil {
					t.Errorf("caller %d: %v", i, err)
				}
				outs[i] = out
			}()
		}
		// Every caller is now building or waiting on the one build in flight.
		synctest.Wait()
		close(gate.release)
		synctest.Wait()

		if got := gate.reads.Load(); got != readsPerBuild {
			t.Errorf("session reads for %d overlapping requests = %d, want %d (one build)", callers, got, readsPerBuild)
		}
		for i, out := range outs {
			if out == nil || len(out.Body.Items) != 1 || out.Body.Items[0].ID != info.ID {
				t.Fatalf("caller %d got %+v, want the one session %s", i, out, info.ID)
			}
		}
		// Each sharer holds its own copy, so no caller can change another's answer.
		if &outs[0].Body.Items[0] == &outs[1].Body.Items[0] {
			t.Error("two callers share one items array, want a copy each")
		}
	})
}

// gatedInProgressStore counts the in_progress reads an agent list build makes
// (one per store, for the active-bead index) and holds each until release.
type gatedInProgressStore struct {
	beads.Store
	release chan struct{}
	reads   atomic.Int32
}

func (s *gatedInProgressStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Status == "in_progress" {
		s.reads.Add(1)
		<-s.release
	}
	return s.Store.List(query)
}

// The agents list has a response cache, but it only helps a request that
// arrives after a build has finished. On a busy city a crowd of /agents
// requests arrived together, all missed, and each built its own list for up
// to 4 minutes. Identical requests that overlap must share one build.
func TestAgentListSharesOneBuildAcrossConcurrentRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const callers = 8

		state := newFakeState(t)
		if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		gate := &gatedInProgressStore{Store: state.stores["myrig"], release: make(chan struct{})}
		state.stores["myrig"] = gate
		srv := New(state)
		input := &AgentListInput{CityScope: CityScope{CityName: state.cityName}}

		// One request alone, to learn how many reads one build makes.
		close(gate.release)
		if _, err := srv.humaHandleAgentList(context.Background(), input); err != nil {
			t.Fatalf("solo list: %v", err)
		}
		readsPerBuild := gate.reads.Load()
		if readsPerBuild == 0 {
			t.Fatal("an agent list build made no in_progress reads; the gate cannot hold it")
		}
		gate.reads.Store(0)
		gate.release = make(chan struct{})
		// Forget the solo answer, so the crowd below has to build.
		srv.responseCacheMu.Lock()
		srv.responseCacheEntries = nil
		srv.responseCacheMu.Unlock()

		outs := make([]*ListOutput[agentResponse], callers)
		for i := range callers {
			go func() {
				out, err := srv.humaHandleAgentList(context.Background(), input)
				if err != nil {
					t.Errorf("caller %d: %v", i, err)
				}
				outs[i] = out
			}()
		}
		synctest.Wait()
		close(gate.release)
		synctest.Wait()

		if got := gate.reads.Load(); got != readsPerBuild {
			t.Errorf("in_progress reads for %d overlapping requests = %d, want %d (one build)", callers, got, readsPerBuild)
		}
		for i, out := range outs {
			if out == nil || len(out.Body.Items) != 1 || out.Body.Items[0].Name != "myrig/worker" {
				t.Fatalf("caller %d got %+v, want the one agent myrig/worker", i, out)
			}
		}
		if &outs[0].Body.Items[0] == &outs[1].Body.Items[0] {
			t.Error("two callers share one items array, want a copy each")
		}
	})
}

// These tests pin the store read budget: GET /agents and GET /sessions must
// answer fast with the live names even while the bead store is slow, and the
// details must fill in once the store read lands. If a handler goes back to waiting on the
// store, the "within 2s" checks below go red.

// storeReadGate holds every bead store read until the test opens it, or for 30
// seconds, which stands in for Dolt under a full pool.
type storeReadGate struct {
	once sync.Once
	ch   chan struct{}
}

func newStoreReadGate(t *testing.T) *storeReadGate {
	g := &storeReadGate{ch: make(chan struct{})}
	// Let any build still waiting on the gate finish once the test is done.
	t.Cleanup(g.open)
	return g
}

func (g *storeReadGate) wait() {
	select {
	case <-g.ch:
	case <-time.After(30 * time.Second):
	}
}

func (g *storeReadGate) open() { g.once.Do(func() { close(g.ch) }) }

// slowReadStore makes every read of the store it wraps wait on a gate. It
// embeds the Store interface, not a concrete store, so no read can slip past
// the gate through a method the concrete store adds.
type slowReadStore struct {
	beads.Store
	gate *storeReadGate
}

func (s slowReadStore) Get(id string) (beads.Bead, error) {
	s.gate.wait()
	return s.Store.Get(id)
}

func (s slowReadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.gate.wait()
	return s.Store.List(q)
}

func (s slowReadStore) ListOpen(status ...string) ([]beads.Bead, error) {
	s.gate.wait()
	return s.Store.ListOpen(status...)
}

func (s slowReadStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.gate.wait()
	return s.Store.Ready(q...)
}

func (s slowReadStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.gate.wait()
	return s.Store.Children(parentID, opts...)
}

func (s slowReadStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.gate.wait()
	return s.Store.ListByLabel(label, limit, opts...)
}

func (s slowReadStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	s.gate.wait()
	return s.Store.ListByAssignee(assignee, status, limit)
}

func (s slowReadStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.gate.wait()
	return s.Store.ListByMetadata(filters, limit, opts...)
}

func (s slowReadStore) GetLocalString(id, key string) (string, error) {
	s.gate.wait()
	return s.Store.GetLocalString(id, key)
}

func (s slowReadStore) Ping() error {
	s.gate.wait()
	return s.Store.Ping()
}

// slowEveryStore puts every bead store the fake state serves behind one gate.
func slowEveryStore(t *testing.T, fs *fakeState) *storeReadGate {
	t.Helper()
	gate := newStoreReadGate(t)
	fs.cityBeadStore = slowReadStore{Store: fs.cityBeadStore, gate: gate}
	for rig, store := range fs.stores {
		fs.stores[rig] = slowReadStore{Store: store, gate: gate}
	}
	return gate
}

func shortenListStoreReadBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	old := listStoreReadBudget
	listStoreReadBudget = budget
	t.Cleanup(func() { listStoreReadBudget = old })
}

// getListWithin GETs url and fails if the answer takes longer than limit.
func getListWithin[T any](t *testing.T, h http.Handler, url string, limit time.Duration) ListBody[T] {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		done <- w
	}()
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s", url, w.Code, w.Body.String())
		}
		var body ListBody[T]
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("GET %s: decode: %v", url, err)
		}
		return body
	case <-time.After(limit):
		t.Fatalf("GET %s took longer than %s: the handler waited on the slow bead store", url, limit)
		return ListBody[T]{}
	}
}

// getFullList GETs url until the answer is no longer partial, which is how
// the dashboard sees the details fill in after the store read lands.
func getFullList[T any](t *testing.T, h http.Handler, url string) ListBody[T] {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		body := getListWithin[T](t, h, url, 5*time.Second)
		if !body.Partial {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s still partial after the store opened: %v", url, body.PartialErrors)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSessionListAnswersWithLiveNamesWhileStoreIsSlow(t *testing.T) {
	shortenListStoreReadBudget(t, 200*time.Millisecond)
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Session behind a slow store")
	workerSession := agentSessionName(fs.cityName, "myrig/worker", fs.cfg.Workspace.SessionTemplate)
	if err := fs.sp.Start(context.Background(), workerSession, runtime.Config{}); err != nil {
		t.Fatalf("start %s: %v", workerSession, err)
	}
	gate := slowEveryStore(t, fs)
	h := newTestCityHandlerWith(t, fs, New(fs))
	url := cityURL(fs, "/sessions")

	fast := getListWithin[sessionResponse](t, h, url, 2*time.Second)
	if !fast.Partial || len(fast.PartialErrors) == 0 || !strings.Contains(fast.PartialErrors[0], "still loading") {
		t.Fatalf("fast answer partial=%v errors=%v, want partial and 'still loading' first", fast.Partial, fast.PartialErrors)
	}
	fastRows := sessionRowsByName(fast.Items)
	bead, ok := fastRows[info.SessionName]
	if !ok || bead.ID != "" || !bead.Running {
		t.Fatalf("fast row for %s = %+v (found %v), want a running row with no id yet", info.SessionName, bead, ok)
	}
	worker, ok := fastRows[workerSession]
	if !ok || worker.Template != "myrig/worker" || worker.Rig != "myrig" {
		t.Fatalf("fast row for %s = %+v (found %v), want the configured template and rig", workerSession, worker, ok)
	}

	gate.open()
	full := getFullList[sessionResponse](t, h, url)
	if got := sessionRowsByName(full.Items)[info.SessionName].ID; got != info.ID {
		t.Fatalf("full row id for %s = %q, want %q: the details never filled in", info.SessionName, got, info.ID)
	}
}

func TestAgentListAnswersWithLiveAgentsWhileStoreIsSlow(t *testing.T) {
	shortenListStoreReadBudget(t, 200*time.Millisecond)
	fs := newSessionFakeState(t)
	workerSession := agentSessionName(fs.cityName, "myrig/worker", fs.cfg.Workspace.SessionTemplate)
	if err := fs.sp.Start(context.Background(), workerSession, runtime.Config{}); err != nil {
		t.Fatalf("start %s: %v", workerSession, err)
	}
	work, err := fs.stores["myrig"].Create(beads.Bead{Title: "live work"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress, assignee := "in_progress", "myrig/worker"
	if err := fs.stores["myrig"].Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	gate := slowEveryStore(t, fs)
	h := newTestCityHandlerWith(t, fs, New(fs))
	url := cityURL(fs, "/agents")

	fast := getListWithin[agentResponse](t, h, url, 2*time.Second)
	if !fast.Partial || len(fast.PartialErrors) == 0 || !strings.Contains(fast.PartialErrors[0], "still loading") {
		t.Fatalf("fast answer partial=%v errors=%v, want partial and 'still loading' first", fast.Partial, fast.PartialErrors)
	}
	row, ok := agentRowsByName(fast.Items)["myrig/worker"]
	// "running", not "idle": without the store the active bead is unknown,
	// and idle would claim the agent has no work.
	if !ok || !row.Running || row.State != "running" || row.ActiveBead != "" {
		t.Fatalf("fast row = %+v (found %v), want running, state running, no bead yet", row, ok)
	}

	gate.open()
	full := getFullList[agentResponse](t, h, url)
	if got := agentRowsByName(full.Items)["myrig/worker"].ActiveBead; got != work.ID {
		t.Fatalf("full row active_bead = %q, want %q: the details never filled in", got, work.ID)
	}
}

func sessionRowsByName(items []sessionResponse) map[string]sessionResponse {
	out := make(map[string]sessionResponse, len(items))
	for _, item := range items {
		out[item.SessionName] = item
	}
	return out
}

func agentRowsByName(items []agentResponse) map[string]agentResponse {
	out := make(map[string]agentResponse, len(items))
	for _, item := range items {
		out[item.Name] = item
	}
	return out
}

func listOf(items ...string) *ListOutput[string] {
	return &ListOutput[string]{Body: ListBody[string]{Items: items, Total: len(items)}}
}

func TestBoundedListBuildServesTheLastListWhileANewBuildIsSlow(t *testing.T) {
	shortenListStoreReadBudget(t, 50*time.Millisecond)
	var b listBuilds
	noStoreFree := func() *ListOutput[string] {
		t.Fatal("storeFree ran, but a saved list was there to serve")
		return nil
	}

	first, err := boundedListBuild(&b, "k", func() (*ListOutput[string], error) { return listOf("a"), nil }, noStoreFree)
	if err != nil || first.Body.Partial || len(first.Body.Items) != 1 {
		t.Fatalf("first = %+v, %v; want the full list", first, err)
	}

	gate := newStoreReadGate(t)
	slowBuild := func() (*ListOutput[string], error) {
		gate.wait()
		return listOf("a", "b"), nil
	}
	second, err := boundedListBuild(&b, "k", slowBuild, noStoreFree)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Body.Partial || !strings.Contains(second.Body.PartialErrors[0], "ago") || len(second.Body.Items) != 1 {
		t.Fatalf("second = %+v, want the saved list, partial, with its age", second.Body)
	}
	if first.Body.Partial {
		t.Fatal("serving the saved list changed the caller's copy")
	}

	// The slow build keeps going after its caller stopped waiting, and its
	// answer is what the next caller gets.
	gate.open()
	deadline := time.Now().Add(5 * time.Second)
	for {
		next, err := boundedListBuild(&b, "k", func() (*ListOutput[string], error) { return listOf("a", "b", "c"), nil }, noStoreFree)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !next.Body.Partial {
			if len(next.Body.Items) < 2 {
				t.Fatalf("next = %+v, want the list the slow build or a later one made", next.Body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the slow build never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBoundedListBuildServesStoreFreeWhenTheSavedListIsTooOld(t *testing.T) {
	shortenListStoreReadBudget(t, 50*time.Millisecond)
	var b listBuilds
	b.save("k", listOf("old"))
	b.lastGood["k"] = savedList{out: b.lastGood["k"].out, at: time.Now().Add(-listLastGoodMaxAge - time.Minute)}

	gate := newStoreReadGate(t)
	out, err := boundedListBuild(&b, "k",
		func() (*ListOutput[string], error) { gate.wait(); return listOf("new"), nil },
		func() *ListOutput[string] { return listOf("live") })
	if err != nil {
		t.Fatalf("boundedListBuild: %v", err)
	}
	if len(out.Body.Items) != 1 || out.Body.Items[0] != "live" || !out.Body.Partial {
		t.Fatalf("out = %+v, want the store-free list, partial", out.Body)
	}
}

func TestBoundedListBuildReturnsAFastError(t *testing.T) {
	var b listBuilds
	want := errors.New("bad cursor")
	_, err := boundedListBuild(&b, "k",
		func() (*ListOutput[string], error) { return nil, want },
		func() *ListOutput[string] { return listOf("live") })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// A panic in a build must come back as an error. singleflight re-panics a
// DoChan panic on its own goroutine, where nothing recovers it, so without
// the recover this test takes the whole test binary down.
func TestBoundedListBuildTurnsABuildPanicIntoAnError(t *testing.T) {
	var b listBuilds
	_, err := boundedListBuild(&b, "k",
		func() (*ListOutput[string], error) { panic("boom") },
		func() *ListOutput[string] { return listOf("live") })
	if err == nil {
		t.Fatal("err = nil, want the panic as an error")
	}
}

func TestListBuildsKeepsAtMostMaxKeys(t *testing.T) {
	var b listBuilds
	for i := range listLastGoodMaxKeys + 1 {
		b.save(fmt.Sprintf("k%d", i), listOf("x"))
		time.Sleep(time.Millisecond)
	}
	if len(b.lastGood) != listLastGoodMaxKeys {
		t.Fatalf("saved %d lists, want %d", len(b.lastGood), listLastGoodMaxKeys)
	}
	if _, ok := b.lastGood["k0"]; ok {
		t.Fatal("the oldest list was kept; want it dropped")
	}
}
