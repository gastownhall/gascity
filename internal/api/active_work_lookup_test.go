package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// isActiveWorkLookupQuery reports whether query is an active-bead lookup's
// read: in_progress work assigned to one or more identities.
func isActiveWorkLookupQuery(query beads.ListQuery) bool {
	return query.Status == "in_progress" && (query.Assignee != "" || len(query.Assignees) > 0)
}

// faultyActiveWorkStore stands in for a bd-backed work store whose
// active-work reads stall or fail. Each such read is counted; with release
// set it waits for release to close, and it returns err alongside the rows
// the wrapped store matched. Once healthy is set, and for every other read,
// it is the wrapped store.
type faultyActiveWorkStore struct {
	beads.Store
	release <-chan struct{}
	err     error
	healthy atomic.Bool
	reads   atomic.Int32
}

func (s *faultyActiveWorkStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if !isActiveWorkLookupQuery(query) || s.healthy.Load() {
		return s.Store.List(query)
	}
	s.reads.Add(1)
	if s.release != nil {
		<-s.release
	}
	rows, err := s.Store.List(query)
	if err != nil {
		return nil, err
	}
	return rows, s.err
}

// activeWorkListBody is the part of a /sessions or /agents list body the
// active-work tests read.
type activeWorkListBody struct {
	Items []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		ActiveBead string `json:"active_bead"`
	} `json:"items"`
	Partial          bool     `json:"partial"`
	PartialErrors    []string `json:"partial_errors"`
	ActiveWorkErrors []string `json:"active_work_errors"`
}

// namesActiveWorkError reports whether errs names an active-work read of rig.
func namesActiveWorkError(errs []string, rig string) bool {
	return slices.ContainsFunc(errs, func(e string) bool {
		return strings.Contains(e, "active work") && strings.Contains(e, `"`+rig+`"`)
	})
}

// hasActiveWorkError reports whether partial_errors names an active-work
// read of rig.
func (b activeWorkListBody) hasActiveWorkError(rig string) bool {
	return namesActiveWorkError(b.PartialErrors, rig)
}

// getActiveWorkList serves req on h and fails the test when h has not
// answered within a hang guard, so a handler that waits out a stalled store
// fails the test instead of wedging the suite. It returns the decoded 200
// body and how long the request took.
func getActiveWorkList(t *testing.T, h http.Handler, req *http.Request) (activeWorkListBody, time.Duration) {
	t.Helper()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second): // hang guard, not a timing bound
		t.Fatalf("GET %s did not answer", req.URL.Path)
	}
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d: %s", req.URL.Path, rec.Code, http.StatusOK, rec.Body.String())
	}
	var body activeWorkListBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode GET %s: %v", req.URL.Path, err)
	}
	return body, elapsed
}

// pinStatusStoreReadTimeout sets the active-work budget for the test.
func pinStatusStoreReadTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := statusStoreReadTimeout
	statusStoreReadTimeout = d
	t.Cleanup(func() { statusStoreReadTimeout = old })
}

// activeWorkListRoute is one list endpoint whose rows resolve active work.
// partial says how it reports a failed lookup: the agent list as partial,
// the session lists in active_work_errors.
type activeWorkListRoute struct {
	name    string
	handler func(t *testing.T, fs *fakeState, srv *Server) http.Handler
	path    func(fs *fakeState) string
	partial bool
}

// activeWorkListRoutes are the list endpoints the bounded lookup covers: the
// Huma sessions list, the legacy sessions list, and the agents list.
var activeWorkListRoutes = []activeWorkListRoute{
	{
		name:    "sessions",
		handler: newActiveWorkCityHandler,
		path:    func(fs *fakeState) string { return cityURL(fs, "/sessions") },
	},
	{
		name:    "legacy sessions",
		handler: func(_ *testing.T, _ *fakeState, srv *Server) http.Handler { return srv.legacySessionHandler() },
		path:    func(*fakeState) string { return "/v0/sessions" },
	},
	{
		name:    "agents",
		handler: newActiveWorkCityHandler,
		path:    func(fs *fakeState) string { return cityURL(fs, "/agents") },
		partial: true,
	},
}

// checkActiveWorkFailure fails the test unless body reports the failed
// active-work read of rig as route does: the agent list as partial, a
// session list in active_work_errors, with partial left for gaps in the
// list itself.
func (route activeWorkListRoute) checkActiveWorkFailure(t *testing.T, body activeWorkListBody, rig string) {
	t.Helper()
	if route.partial {
		if !body.Partial || !body.hasActiveWorkError(rig) {
			t.Errorf("partial = %v, partial_errors = %q; want partial naming the active-work read of %s", body.Partial, body.PartialErrors, rig)
		}
		return
	}
	if body.Partial || len(body.PartialErrors) > 0 {
		t.Errorf("partial = %v, partial_errors = %q; want neither: every session is listed", body.Partial, body.PartialErrors)
	}
	if !namesActiveWorkError(body.ActiveWorkErrors, rig) {
		t.Errorf("active_work_errors = %q, want the active-work read of %s", body.ActiveWorkErrors, rig)
	}
}

// newActiveWorkCityHandler is the city-scoped handler serving srv.
func newActiveWorkCityHandler(t *testing.T, fs *fakeState, srv *Server) http.Handler {
	return newTestCityHandlerWith(t, fs, srv)
}

// newActiveWorkListState is a city with two active sessions and two agents in
// rig myrig, whose store is work.
func newActiveWorkListState(t *testing.T, work beads.Store) *fakeState {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.cfg.Agents = append(fs.cfg.Agents, config.Agent{Name: "helper", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)})
	fs.stores["myrig"] = work
	createTestSession(t, fs.cityBeadStore, fs.sp, "First")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Second")
	return fs
}

// TestListActiveWorkLookupHonorsRequestDeadline: every row of a list resolves
// its active bead from the work stores. When an uncached read stalls, the
// response waits for one shared statusStoreReadTimeout budget, not the
// store's own timeout once per row, answers 200 reporting the failure as the
// route does, and reads no store again after the failure.
func TestListActiveWorkLookupHonorsRequestDeadline(t *testing.T) {
	pinStatusStoreReadTimeout(t, 200*time.Millisecond)
	for _, route := range activeWorkListRoutes {
		t.Run(route.name, func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			work := &faultyActiveWorkStore{Store: beads.NewMemStore(), release: release}
			fs := newActiveWorkListState(t, work)
			srv := New(fs)

			body, elapsed := getActiveWorkList(t, route.handler(t, fs, srv), httptest.NewRequest("GET", route.path(fs), nil))

			if elapsed > statusStoreReadTimeout+2*time.Second {
				t.Errorf("answered after %s, want within one %s budget plus slack", elapsed, statusStoreReadTimeout)
			}
			if len(body.Items) < 2 {
				t.Fatalf("items = %+v, want both rows", body.Items)
			}
			route.checkActiveWorkFailure(t, body, "myrig")
			if n := work.reads.Load(); n != 1 {
				t.Errorf("active-work reads = %d, want 1: no read after the first failure", n)
			}
		})
	}
}

// ctxBoundActiveWorkClone is the context-bound clone ScopedStoreLike hands out
// for a bd-CLI-backed store: an active-work read waits for its context to end,
// as a killed bd child would, and reports when it started and when its
// context ended.
type ctxBoundActiveWorkClone struct {
	beads.Store
	ctx      context.Context
	entered  chan struct{}
	canceled chan struct{}
	once     *atomic.Bool
}

func (c ctxBoundActiveWorkClone) List(query beads.ListQuery) ([]beads.Bead, error) {
	if !isActiveWorkLookupQuery(query) {
		return c.Store.List(query)
	}
	first := c.once.CompareAndSwap(false, true)
	if first {
		close(c.entered)
	}
	<-c.ctx.Done()
	if first {
		close(c.canceled)
	}
	return nil, c.ctx.Err()
}

// scopeActiveWorkReads makes fs hand out a ctxBoundActiveWorkClone for every
// store and returns the channels closed when the first clone read starts and
// when its context ends.
func scopeActiveWorkReads(fs *fakeState) (entered, canceled <-chan struct{}) {
	enteredCh, canceledCh := make(chan struct{}), make(chan struct{})
	once := &atomic.Bool{}
	fs.scopedStoreFn = func(ctx context.Context, existing beads.Store) (beads.Store, error) {
		return ctxBoundActiveWorkClone{Store: existing, ctx: ctx, entered: enteredCh, canceled: canceledCh, once: once}, nil
	}
	return enteredCh, canceledCh
}

// TestActiveWorkLookupCancelsScopedStore: an uncached active-work read goes
// through the context-bound clone ScopedStoreLike supplies, never the shared
// store, and the clone's context ends when the lookup's budget runs out or
// the client goes away, which kills a bd child instead of abandoning it.
func TestActiveWorkLookupCancelsScopedStore(t *testing.T) {
	for _, route := range activeWorkListRoutes {
		t.Run("deadline/"+route.name, func(t *testing.T) {
			pinStatusStoreReadTimeout(t, 200*time.Millisecond)
			shared := &faultyActiveWorkStore{Store: beads.NewMemStore()}
			fs := newActiveWorkListState(t, shared)
			_, canceled := scopeActiveWorkReads(fs)
			srv := New(fs)

			body, _ := getActiveWorkList(t, route.handler(t, fs, srv), httptest.NewRequest("GET", route.path(fs), nil))

			awaitSignal(t, canceled, "canceling the scoped clone's read at the deadline")
			if n := shared.reads.Load(); n != 0 {
				t.Errorf("shared store active-work reads = %d, want 0", n)
			}
			route.checkActiveWorkFailure(t, body, "myrig")
		})
	}
	t.Run("client cancel", func(t *testing.T) {
		pinStatusStoreReadTimeout(t, time.Minute)
		shared := &faultyActiveWorkStore{Store: beads.NewMemStore()}
		fs := newActiveWorkListState(t, shared)
		entered, canceled := scopeActiveWorkReads(fs)
		srv := New(fs)
		h := newTestCityHandlerWith(t, fs, srv)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil).WithContext(ctx))
		}()

		awaitSignal(t, entered, "an active-work read of the scoped clone")
		cancel()
		awaitSignal(t, canceled, "canceling the scoped clone's read with the request")
		awaitSignal(t, done, "the handler returning after the client went away")
		if n := shared.reads.Load(); n != 0 {
			t.Errorf("shared store active-work reads = %d, want 0", n)
		}
	})
}

// activeBeadOf returns the active bead of the row whose id or name is key.
func (b activeWorkListBody) activeBeadOf(t *testing.T, key string) string {
	t.Helper()
	for _, item := range b.Items {
		if item.ID == key || item.Name == key {
			return item.ActiveBead
		}
	}
	t.Fatalf("no row %q in %+v", key, b.Items)
	return ""
}

// TestActiveWorkLookupPreservesPriorityAfterFailure: identity order, then
// store order, decides a row's active bead, so when store a, read first,
// fails, the bead store z holds for the same identity or a later one may not
// be the winner. The row reports no work-store bead, and the response names
// the failure: a session list in active_work_errors, the agent list in
// partial_errors beside the errors it already had. Rows a partial result
// decoded cannot prove the newest match either.
func TestActiveWorkLookupPreservesPriorityAfterFailure(t *testing.T) {
	sessionRoutes := activeWorkListRoutes[:2]
	for _, fault := range []struct {
		name     string
		err      error
		aHoldsID bool
	}{
		{name: "error", err: errors.New("bd list: connection refused")},
		{name: "partial result with rows", err: &beads.PartialResultError{Op: "bd list", Err: errors.New("bad row")}, aHoldsID: true},
		{name: "partial result without rows", err: &beads.PartialResultError{Op: "bd list", Err: errors.New("bad row")}},
	} {
		for _, route := range sessionRoutes {
			t.Run(fault.name+"/"+route.name, func(t *testing.T) {
				fs := newSessionFakeState(t)
				a := &faultyActiveWorkStore{Store: beads.NewMemStore(), err: fault.err}
				z := beads.NewMemStore()
				fs.stores = map[string]beads.Store{"a": a, "z": z}
				info := createTestSession(t, fs.cityBeadStore, fs.sp, "Session")
				if fault.aHoldsID {
					addActiveWork(t, a.Store, info.ID)
				}
				addActiveWork(t, z, info.ID)
				addActiveWork(t, z, info.SessionName)
				srv := New(fs)

				body, _ := getActiveWorkList(t, route.handler(t, fs, srv), httptest.NewRequest("GET", route.path(fs), nil))

				if got := body.activeBeadOf(t, info.ID); got != "" {
					t.Errorf("active_bead = %q, want none while store a, which outranks z, is unread", got)
				}
				route.checkActiveWorkFailure(t, body, "a")
			})
		}
	}
	t.Run("agents keep earlier errors", func(t *testing.T) {
		state := newFakeState(t)
		state.sp = runtime.NewFailFake()
		initCfg := config.DefaultCity(state.cityName)
		state.cfg.Agents = initCfg.Agents
		state.cfg.NamedSessions = initCfg.NamedSessions
		state.stores = map[string]beads.Store{"myrig": &faultyActiveWorkStore{Store: beads.NewMemStore(), err: errors.New("bd list: connection refused")}}
		srv := New(state)

		body, _ := getActiveWorkList(t, newTestCityHandlerWith(t, state, srv), httptest.NewRequest("GET", cityURL(state, "/agents"), nil))

		if !body.Partial || !body.hasActiveWorkError("myrig") {
			t.Errorf("partial = %v, partial_errors = %q; want partial naming the active-work read of myrig", body.Partial, body.PartialErrors)
		}
		if !slices.ContainsFunc(body.PartialErrors, func(e string) bool { return strings.HasPrefix(e, "agent ") }) {
			t.Errorf("partial_errors = %q, want the pool listing error kept beside the active-work one", body.PartialErrors)
		}
	})
}

// twoRigActiveWorkState is a city with rigs a and z, whose stores are a and z;
// a sorts first, so an unscoped lookup reads it before z.
func twoRigActiveWorkState(t *testing.T, a, z beads.Store) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cfg.Rigs = []config.Rig{
		{Name: "a", Path: "/tmp/a"},
		{Name: "z", Path: "/tmp/z"},
	}
	fs.stores = map[string]beads.Store{"a": a, "z": z}
	return fs
}

// TestActiveWorkLookupFailureIsNamedAndLoggedOnce: the recorded active-work
// failure names the lookup and the rig it failed on, and is logged once per
// request however many rows follow.
func TestActiveWorkLookupFailureIsNamedAndLoggedOnce(t *testing.T) {
	fs := twoRigActiveWorkState(t,
		activeBeadErrStore{Store: beads.NewMemStore(), err: errors.New("store a is down")},
		&listCountingStore{Store: beads.NewMemStore()})
	srv := New(fs)
	lookup := newTestActiveWorkLookup(t)

	var logs bytes.Buffer
	t.Cleanup(captureLog(t, &logs))
	for range 3 {
		srv.findActiveBeadForAssignees(lookup, "", "session-id")
	}
	errs := lookup.failures()
	if len(errs) != 1 {
		t.Fatalf("failures = %q, want exactly the first failure", errs)
	}
	if !strings.Contains(errs[0], `looking up active work in rig "a"`) || !strings.Contains(errs[0], "store a is down") {
		t.Fatalf("error = %q, want it to name the active-work lookup and rig a with the cause", errs[0])
	}
	if n := strings.Count(logs.String(), "looking up active work in rig"); n != 1 {
		t.Fatalf("failure logged %d times over 3 rows, want 1; log:\n%s", n, logs.String())
	}
}

// TestActiveWorkLookupUsesCleanCacheAfterDeadline: a clean cache answers a
// row, hit or authoritative miss, after the lookup's budget is spent, with
// no uncached read and no reported failure. Once a read has failed, though,
// a row whose search reaches the store that failed resolves nothing, even
// when a lower-priority cached store holds its work. The session lists show
// the spent budget: the agents list's own session snapshot shares
// statusStoreReadTimeout, so a zero budget would fail it first.
func TestActiveWorkLookupUsesCleanCacheAfterDeadline(t *testing.T) {
	for _, route := range activeWorkListRoutes[:2] {
		t.Run("spent budget/"+route.name, func(t *testing.T) {
			pinStatusStoreReadTimeout(t, 0)
			work := &cachedOnlyListStoreForSessionTest{MemStore: beads.NewMemStore()}
			city := &cachedOnlyListStoreForSessionTest{MemStore: beads.NewMemStore()}
			fs := newSessionFakeState(t)
			fs.stores["myrig"] = work
			fs.cityBeadStore = city
			withWork := createTestSession(t, city, fs.sp, "With work")
			idle := createTestSession(t, city, fs.sp, "Idle")
			want := addActiveWork(t, work, withWork.ID)
			work.blockList, city.blockList = true, true
			srv := New(fs)

			body, _ := getActiveWorkList(t, route.handler(t, fs, srv), httptest.NewRequest("GET", route.path(fs), nil))

			if got := body.activeBeadOf(t, withWork.ID); got != want {
				t.Errorf("active_bead = %q, want cached %q", got, want)
			}
			if got := body.activeBeadOf(t, idle.ID); got != "" {
				t.Errorf("idle active_bead = %q, want none (authoritative cache miss)", got)
			}
			if body.Partial || len(body.ActiveWorkErrors) > 0 {
				t.Errorf("partial_errors = %q, active_work_errors = %q; want none: cache answers need no budget", body.PartialErrors, body.ActiveWorkErrors)
			}
			if work.listCalls != 0 || city.listCalls != 0 {
				t.Errorf("uncached reads = %d work, %d city; want 0", work.listCalls, city.listCalls)
			}
		})
	}
	for _, route := range activeWorkListRoutes[:2] {
		t.Run("failed store outranks cached rows/"+route.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			a := &faultyActiveWorkStore{Store: beads.NewMemStore(), err: errors.New("bd list: connection refused")}
			cached := &cachedOnlyListStoreForSessionTest{MemStore: beads.NewMemStore()}
			fs.stores = map[string]beads.Store{"a": a, "myrig": cached}
			first := createTestSession(t, fs.cityBeadStore, fs.sp, "First")
			second := createTestSession(t, fs.cityBeadStore, fs.sp, "Second")
			addActiveWork(t, cached, first.ID)
			addActiveWork(t, cached, second.ID)
			cached.blockList = true
			srv := New(fs)

			body, _ := getActiveWorkList(t, route.handler(t, fs, srv), httptest.NewRequest("GET", route.path(fs), nil))

			for _, info := range []string{first.ID, second.ID} {
				if got := body.activeBeadOf(t, info); got != "" {
					t.Errorf("%s active_bead = %q, want none while store a, which outranks myrig, is unread", info, got)
				}
			}
			if n := a.reads.Load(); n != 1 {
				t.Errorf("store a active-work reads = %d, want 1", n)
			}
			route.checkActiveWorkFailure(t, body, "a")
		})
	}
}

// TestActiveWorkLookupUsesRealCacheAfterDeadline: a primed CachingStore
// answers a row, hit or authoritative miss, after the lookup's parent context
// ended and its budget is spent, with no backing read and no reported
// failure; once rig a's read has failed, a separately scoped row in rig z
// still resolves from z's clean cache, and rig a is not read again.
func TestActiveWorkLookupUsesRealCacheAfterDeadline(t *testing.T) {
	t.Run("clean cache answers after the deadline expired", func(t *testing.T) {
		backing := &listCountingStore{Store: beads.NewMemStore()}
		hit := addActiveWork(t, backing.Store, "session-id")
		cache := beads.NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		readsAfterPrime := backing.listCalls
		fs := newFakeState(t)
		fs.stores["myrig"] = cache
		srv := New(fs)
		parent, cancelParent := context.WithCancel(context.Background())
		lookup := newActiveWorkLookup(parent)
		cancelParent()  // the request is gone, and
		lookup.left = 0 // the budget is spent; nothing has failed

		if got := srv.findActiveBeadForAssignees(lookup, "myrig", "session-id"); got != hit {
			t.Fatalf("cache hit after expiry = %q, want %q", got, hit)
		}
		if got := srv.findActiveBeadForAssignees(lookup, "myrig", "nobody"); got != "" {
			t.Fatalf("authoritative cache miss after expiry = %q, want empty", got)
		}
		if errs := lookup.failures(); len(errs) != 0 {
			t.Fatalf("failures = %q, want none: a spent budget alone is not a failure", errs)
		}
		if backing.listCalls != readsAfterPrime {
			t.Fatalf("uncached reads after expiry = %d, want 0", backing.listCalls-readsAfterPrime)
		}
	})

	t.Run("clean cache answers a later row after a failure", func(t *testing.T) {
		backingZ := &listCountingStore{Store: beads.NewMemStore()}
		zWork := addActiveWork(t, backingZ.Store, "session-2")
		cacheZ := beads.NewCachingStoreForTest(backingZ, nil)
		if err := cacheZ.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		readsAfterPrime := backingZ.listCalls
		a := &faultyActiveWorkStore{Store: beads.NewMemStore(), err: errors.New("store a is down")}
		fs := twoRigActiveWorkState(t, a, cacheZ)
		srv := New(fs)
		lookup := newTestActiveWorkLookup(t)

		if got := srv.findActiveBeadForAssignees(lookup, "a", "session-1"); got != "" {
			t.Fatalf("first row = %q, want empty after store a failed", got)
		}
		if got := srv.findActiveBeadForAssignees(lookup, "z", "session-2"); got != zWork {
			t.Fatalf("later row in rig z = %q, want %q from z's clean cache: rig a's failure does not reach it", got, zWork)
		}
		if got := srv.findActiveBeadForAssignees(lookup, "a", "session-3"); got != "" {
			t.Fatalf("later row in rig a = %q, want empty: store a failed", got)
		}
		if n := a.reads.Load(); n != 1 {
			t.Fatalf("store a reads = %d, want 1: no store is read after the failure", n)
		}
		if backingZ.listCalls != readsAfterPrime {
			t.Fatalf("store z backing reads after prime = %d, want 0", backingZ.listCalls-readsAfterPrime)
		}
		if errs := lookup.failures(); len(errs) != 1 {
			t.Fatalf("failures = %q, want only the first failure", errs)
		}
	})
}

// TestActiveWorkLookupFailureSparesCacheAnsweredRows: a failed read blanks
// only the rows whose search reaches a store its cache cannot answer. A
// later row whose winner a clean cache answers still resolves, whether an
// earlier store's cache answers it in an unscoped search or it is an agent
// row scoped to another rig, so a busy agent does not read as idle.
func TestActiveWorkLookupFailureSparesCacheAnsweredRows(t *testing.T) {
	t.Run("unscoped row answered by an earlier store's cache", func(t *testing.T) {
		backingA := beads.NewMemStore()
		aWork := addActiveWork(t, backingA, "session-2")
		cacheA := beads.NewCachingStoreForTest(backingA, nil)
		if err := cacheA.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		z := &faultyActiveWorkStore{Store: beads.NewMemStore(), err: errors.New("store z is down")}
		srv := New(twoRigActiveWorkState(t, cacheA, z))
		lookup := newTestActiveWorkLookup(t)

		if got := srv.findActiveBeadForAssignees(lookup, "", "session-1"); got != "" {
			t.Fatalf("first row = %q, want empty: a's cache misses and store z failed", got)
		}
		if got := srv.findActiveBeadForAssignees(lookup, "", "session-2"); got != aWork {
			t.Fatalf("later row = %q, want %q: a's clean cache answers it before z is reached", got, aWork)
		}
		if got := srv.findActiveBeadForAssignees(lookup, "", "session-3"); got != "" {
			t.Fatalf("third row = %q, want empty: its search reaches store z, unread", got)
		}
		if n := z.reads.Load(); n != 1 {
			t.Fatalf("store z reads = %d, want 1: no store is read after the failure", n)
		}
	})
	t.Run("agent row in another rig", func(t *testing.T) {
		state := newFakeState(t)
		backing := beads.NewMemStore()
		want := addActiveWork(t, backing, "myrig/worker")
		cache := beads.NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		state.stores["myrig"] = cache
		state.stores["a"] = activeBeadErrStore{Store: beads.NewMemStore(), err: errors.New("store a is down")}
		state.cfg.Rigs = append(state.cfg.Rigs, config.Rig{Name: "a", Path: "/tmp/a"})
		state.cfg.Agents = append([]config.Agent{{Name: "early", Dir: "a", Provider: "test-agent", MaxActiveSessions: intPtr(1)}}, state.cfg.Agents...)

		body, _ := getActiveWorkList(t, newTestCityHandlerWith(t, state, New(state)), httptest.NewRequest("GET", cityURL(state, "/agents"), nil))

		if got := body.activeBeadOf(t, "myrig/worker"); got != want {
			t.Fatalf("myrig/worker active_bead = %q, want %q from its own rig's clean cache", got, want)
		}
		if !body.Partial || !body.hasActiveWorkError("a") {
			t.Fatalf("partial = %v, partial_errors = %q; want partial naming the active-work read of a", body.Partial, body.PartialErrors)
		}
	})
}

// slowActiveWorkStore answers every active-work read after delay, on the
// clock of the synctest bubble it is read in, and counts those reads.
type slowActiveWorkStore struct {
	beads.Store
	delay time.Duration
	reads atomic.Int32
}

func (s *slowActiveWorkStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if isActiveWorkLookupQuery(query) {
		s.reads.Add(1)
		<-time.After(s.delay)
	}
	return s.Store.List(query)
}

// TestActiveWorkLookupRowsShareOneBudget: every row's read draws on the one
// budget the lookup started with, so reads that each fit the budget but
// together outlast it fail the lookup, and the total wait cannot grow with
// the number of rows. The failure says how much of the budget the reads
// before it spent, so it does not read as a stall of the rig it timed out on.
func TestActiveWorkLookupRowsShareOneBudget(t *testing.T) {
	pinStatusStoreReadTimeout(t, 400*time.Millisecond)
	work := &slowActiveWorkStore{Store: beads.NewMemStore(), delay: 300 * time.Millisecond}
	fs := newFakeState(t)
	fs.stores = map[string]beads.Store{"myrig": work}
	srv := New(fs)
	synctest.Test(t, func(t *testing.T) {
		defer func() { <-time.After(work.delay) }() // a read the budget abandoned returns
		lookup := newTestActiveWorkLookup(t)
		start := time.Now()

		for range 3 {
			srv.findActiveBeadForAssignees(lookup, "myrig", "nobody")
		}

		if elapsed := time.Since(start); elapsed > statusStoreReadTimeout {
			t.Errorf("rows took %s, want at most the %s budget", elapsed, statusStoreReadTimeout)
		}
		errs := lookup.failures()
		if len(errs) != 1 {
			t.Fatalf("%d reads of %s each under one %s budget left failures %q, want one: each read had a budget of its own", work.reads.Load(), work.delay, statusStoreReadTimeout, errs)
		}
		if n := work.reads.Load(); n != 2 {
			t.Fatalf("reads = %d, want 2 under one shared budget", n)
		}
		if want := "(earlier reads had spent 300ms of the 400ms active-work budget)"; !strings.Contains(errs[0], want) {
			t.Fatalf("failure = %q, want it to say %q", errs[0], want)
		}
	})
}

// TestActiveWorkLookupChargesOnlyStoreReads: time a list response spends
// between store reads, such as each row's runtime probes, spends none of the
// budget, so a read late in the row loop still gets what the reads before it
// left.
func TestActiveWorkLookupChargesOnlyStoreReads(t *testing.T) {
	pinStatusStoreReadTimeout(t, 400*time.Millisecond)
	work := &slowActiveWorkStore{Store: beads.NewMemStore(), delay: 300 * time.Millisecond}
	want := addActiveWork(t, work.Store, "late")
	fs := newFakeState(t)
	fs.stores = map[string]beads.Store{"myrig": work}
	srv := New(fs)
	synctest.Test(t, func(t *testing.T) {
		lookup := newTestActiveWorkLookup(t)
		<-time.After(time.Second) // the rows before it probe their runtimes

		if got := srv.findActiveBeadForAssignees(lookup, "myrig", "late"); got != want {
			t.Fatalf("late row = %q, want %q: the probes before it spent none of the budget", got, want)
		}
		if errs := lookup.failures(); len(errs) != 0 {
			t.Fatalf("failures = %q, want none", errs)
		}
	})
}

// TestActiveWorkLookupNamesASpentBudget: a read the spent budget refuses
// fails the lookup with an error that says so, not as a timeout of the rig it
// never read, and that rig's store is not read.
func TestActiveWorkLookupNamesASpentBudget(t *testing.T) {
	work := &listCountingStore{Store: beads.NewMemStore()}
	fs := newFakeState(t)
	fs.stores = map[string]beads.Store{"myrig": work}
	lookup := newTestActiveWorkLookup(t)
	lookup.left = 0

	if got := New(fs).findActiveBeadForAssignees(lookup, "myrig", "nobody"); got != "" {
		t.Fatalf("active bead = %q, want none", got)
	}

	errs := lookup.failures()
	want := `looking up active work in rig "myrig": not read: earlier reads spent the ` + statusStoreReadTimeout.String() + " active-work budget"
	if len(errs) != 1 || errs[0] != want {
		t.Fatalf("failures = %q, want %q", errs, want)
	}
	if work.listCalls != 0 {
		t.Fatalf("store reads = %d, want 0", work.listCalls)
	}
}

// refreshingActiveWorkBacking is a bd-backed cache's backing. While fail is
// set its Get fails, so the cache's refresh after a write it forwards fails
// and leaves the written row dirty; otherwise each point read, Get and
// DepList, first takes delay on the clock of the synctest bubble it runs in,
// as a bd show or a bd dep list does. It counts its point reads and lists.
type refreshingActiveWorkBacking struct {
	beads.Store
	fail              atomic.Bool
	delay             time.Duration
	gets, deps, lists atomic.Int32
}

func (s *refreshingActiveWorkBacking) Get(id string) (beads.Bead, error) {
	s.gets.Add(1)
	if s.fail.Load() {
		return beads.Bead{}, errors.New("bd show: connection refused")
	}
	if s.delay > 0 {
		<-time.After(s.delay)
	}
	return s.Store.Get(id)
}

func (s *refreshingActiveWorkBacking) DepList(id, direction string) ([]beads.Dep, error) {
	s.deps.Add(1)
	if s.delay > 0 {
		<-time.After(s.delay)
	}
	return s.Store.DepList(id, direction)
}

func (s *refreshingActiveWorkBacking) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	return s.Store.List(query)
}

// activeWorkQuery is the lookup's read of assignee's work in one store.
func activeWorkQuery(assignee string) beads.ListQuery {
	return beads.ListQuery{Assignee: assignee, Status: "in_progress", Limit: 1, Sort: beads.SortCreatedDesc}
}

// newDirtyActiveWorkCache is a bd-backed store's primed cache holding work
// claimed for assignee through it, a claim whose post-write refresh failed,
// so the cache holds the claimed row dirty. It returns the cache, its
// backing, answering again with its read counts zeroed, and the work's id.
func newDirtyActiveWorkCache(t *testing.T, assignee string) (*beads.CachingStore, *refreshingActiveWorkBacking, string) {
	t.Helper()
	mem := beads.NewMemStore()
	backing := &refreshingActiveWorkBacking{Store: mem}
	work, err := mem.Create(beads.Bead{Title: "work", Status: "open"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.fail.Store(true)
	status := "in_progress"
	if err := cache.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("claim through the cache: %v", err)
	}
	if _, ok := cache.CachedList(activeWorkQuery(assignee)); ok {
		t.Fatal("fixture: the claim whose refresh failed left the cache clean")
	}
	backing.fail.Store(false)
	backing.gets.Store(0)
	backing.deps.Store(0)
	backing.lists.Store(0)
	return cache, backing, work.ID
}

// countCacheClones is a ScopedStoreLike that counts the context-bound clones
// asked of cache, through a handle or the split-strict gate's work view of
// it, and hands out none.
func countCacheClones(cache beads.Store, clones *atomic.Int32) func(context.Context, beads.Store) (beads.Store, error) {
	return func(_ context.Context, existing beads.Store) (beads.Store, error) {
		if beads.SameStore(existing, splittest.GateWorkView(cache)) {
			clones.Add(1)
		}
		return nil, nil
	}
}

// TestActiveWorkLookupRefreshesADirtyCache: a bd-backed store whose cache
// holds a dirty row, a claim whose post-write refresh failed, is answered from
// its cache once the cache's refresh has re-read that one row from the
// backing, a Get and a DepList and no scan: no context-bound clone is asked
// for, the dirty mark clears, and the other rows are answered by the clean
// cache.
func TestActiveWorkLookupRefreshesADirtyCache(t *testing.T) {
	for _, route := range activeWorkListRoutes[:2] {
		t.Run(route.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			first := createTestSession(t, fs.cityBeadStore, fs.sp, "First")
			second := createTestSession(t, fs.cityBeadStore, fs.sp, "Second")
			cache, backing, work := newDirtyActiveWorkCache(t, first.ID)
			fs.stores["myrig"] = cache
			var clones atomic.Int32
			fs.scopedStoreFn = countCacheClones(cache, &clones)

			body, _ := getActiveWorkList(t, route.handler(t, fs, New(fs)), httptest.NewRequest("GET", route.path(fs), nil))

			if got := body.activeBeadOf(t, first.ID); got != work {
				t.Errorf("active_bead = %q, want the claimed %q", got, work)
			}
			if got := body.activeBeadOf(t, second.ID); got != "" {
				t.Errorf("idle active_bead = %q, want none", got)
			}
			if body.Partial || len(body.ActiveWorkErrors) > 0 {
				t.Errorf("partial_errors = %q, active_work_errors = %q; want none", body.PartialErrors, body.ActiveWorkErrors)
			}
			if g, d, l := backing.gets.Load(), backing.deps.Load(), backing.lists.Load(); g != 1 || d != 1 || l != 0 {
				t.Errorf("backing reads: %d Gets, %d DepLists, %d Lists; want the dirty row's one Get and DepList", g, d, l)
			}
			if n := clones.Load(); n != 0 {
				t.Errorf("clones asked of the cached store = %d, want none", n)
			}
			if _, ok := cache.CachedList(activeWorkQuery(first.ID)); !ok {
				t.Error("the dirty mark is still set after the lookup refreshed the row")
			}
		})
	}
}

// TestActiveWorkLookupRefreshOutlivesTheBudget is the regression for a dirty
// row whose refresh ended with the response that started it: at bd's
// latency, 750ms each for the row's Get and DepList, the refresh outlasts the
// 1s budget. That response reports the store's cache as still refreshing, not
// the store as stalled, but the refresh runs on, so the next response
// resolves the row from the cache with no wait, its mark cleared, after the
// one refresh's backing reads and no clone.
func TestActiveWorkLookupRefreshOutlivesTheBudget(t *testing.T) {
	pinStatusStoreReadTimeout(t, time.Second)
	cache, backing, work := newDirtyActiveWorkCache(t, "worker")
	backing.delay = 750 * time.Millisecond
	fs := newFakeState(t)
	fs.stores = map[string]beads.Store{"myrig": cache}
	var clones atomic.Int32
	fs.scopedStoreFn = countCacheClones(cache, &clones)
	srv := New(fs)
	synctest.Test(t, func(t *testing.T) {
		lookup := newTestActiveWorkLookup(t)
		start := time.Now()

		if got := srv.findActiveBeadForAssignees(lookup, "myrig", "worker"); got != "" {
			t.Fatalf("first response's row = %q, want none: the refresh outlasts the budget", got)
		}
		if elapsed := time.Since(start); elapsed > statusStoreReadTimeout {
			t.Errorf("first response's row took %s, want at most the %s budget", elapsed, statusStoreReadTimeout)
		}
		errs := lookup.failures()
		if want := `looking up active work in rig "myrig": waiting for its cache to refresh its dirty rows`; len(errs) != 1 || !strings.HasPrefix(errs[0], want) {
			t.Fatalf("failures = %q, want one starting %q", errs, want)
		}

		<-time.After(time.Second) // the next poll
		synctest.Wait()
		lookup = newTestActiveWorkLookup(t)
		start = time.Now()

		if got := srv.findActiveBeadForAssignees(lookup, "myrig", "worker"); got != work {
			t.Fatalf("next response's row = %q, want %q from the refreshed cache", got, work)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("next response's row took %s, want no wait on a clean cache", elapsed)
		}
		if errs := lookup.failures(); len(errs) != 0 {
			t.Errorf("failures = %q, want none", errs)
		}
	})
	if g, d, l := backing.gets.Load(), backing.deps.Load(), backing.lists.Load(); g != 1 || d != 1 || l != 0 {
		t.Errorf("backing reads: %d Gets, %d DepLists, %d Lists; want the one refresh's Get and DepList", g, d, l)
	}
	if n := clones.Load(); n != 0 {
		t.Errorf("clones asked of the cached store = %d, want none", n)
	}
	if _, ok := cache.CachedList(activeWorkQuery("worker")); !ok {
		t.Error("the dirty mark is still set after the refresh ended")
	}
}

// TestActiveWorkLookupRefreshesNothingOnceFailed: once the lookup has failed,
// or its budget is spent, a row whose search reaches a dirty cache starts no
// refresh and waits for none. It resolves nothing, and nothing is read, not
// even by a refresh left running once the row returns: the counters are read
// only when every goroutine of the bubble has finished or blocked.
func TestActiveWorkLookupRefreshesNothingOnceFailed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*activeWorkLookup)
	}{
		{name: "failed", setup: func(l *activeWorkLookup) { l.fail("a", errors.New("store a is down")) }},
		{name: "budget spent", setup: func(l *activeWorkLookup) { l.left = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, backing, _ := newDirtyActiveWorkCache(t, "worker")
			fs := newFakeState(t)
			fs.stores = map[string]beads.Store{"myrig": cache}
			srv := New(fs)
			synctest.Test(t, func(t *testing.T) {
				lookup := newTestActiveWorkLookup(t)
				tc.setup(lookup)

				if got := srv.findActiveBeadForAssignees(lookup, "myrig", "worker"); got != "" {
					t.Fatalf("row = %q, want none", got)
				}
				synctest.Wait()

				if g, d, l := backing.gets.Load(), backing.deps.Load(), backing.lists.Load(); g != 0 || d != 0 || l != 0 {
					t.Fatalf("backing reads: %d Gets, %d DepLists, %d Lists; want none", g, d, l)
				}
				if _, ok := cache.CachedList(activeWorkQuery("worker")); ok {
					t.Fatal("the dirty row was refreshed")
				}
			})
		})
	}
}

// TestListActiveWorkLookupPreservesPrecedence: with healthy stores the bounded
// list lookup answers as before: identity order beats store order, the newest
// match wins within a store, and a rig-scoped agent row reads its rig alone.
// A single-session read stays live and outside the list's budget.
func TestListActiveWorkLookupPreservesPrecedence(t *testing.T) {
	for _, route := range activeWorkListRoutes[:2] {
		t.Run(route.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			a, z := beads.NewMemStore(), beads.NewMemStore()
			fs.stores = map[string]beads.Store{"a": a, "z": z}
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Session")
			addActiveWork(t, a, info.Template)
			addActiveWork(t, z, info.ID)
			newest := addActiveWork(t, z, info.ID)
			srv := New(fs)

			body, _ := getActiveWorkList(t, route.handler(t, fs, srv), httptest.NewRequest("GET", route.path(fs), nil))

			if got := body.activeBeadOf(t, info.ID); got != newest {
				t.Errorf("active_bead = %q, want %q: the session id outranks the template, newest first", got, newest)
			}
			if body.Partial {
				t.Errorf("partial_errors = %q, want none", body.PartialErrors)
			}
		})
	}
	t.Run("agents", func(t *testing.T) {
		state := newFakeState(t)
		other := beads.NewMemStore()
		state.stores["a"] = other
		addActiveWork(t, other, "myrig--worker")
		want := addActiveWork(t, state.stores["myrig"], "myrig/worker")
		srv := New(state)

		body, _ := getActiveWorkList(t, newTestCityHandlerWith(t, state, srv), httptest.NewRequest("GET", cityURL(state, "/agents"), nil))

		if got := body.activeBeadOf(t, "myrig/worker"); got != want {
			t.Errorf("active_bead = %q, want %q from rig myrig only", got, want)
		}
	})
	t.Run("session detail stays live", func(t *testing.T) {
		pinStatusStoreReadTimeout(t, 0)
		fs := newSessionFakeState(t)
		info := createTestSession(t, fs.cityBeadStore, fs.sp, "Session")
		want := addActiveWork(t, fs.stores["myrig"], info.ID)
		srv := New(fs)
		h := newTestCityHandlerWith(t, fs, srv)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/session/"+info.ID), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var resp sessionResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ActiveBead != want {
			t.Errorf("active_bead = %q, want live %q", resp.ActiveBead, want)
		}
	})
}

// TestListActiveWorkLookupReadsStoresInPriorityOrder: with healthy stores the
// bounded list lookup reads identities outer and stores inner, stopping at
// the first match: the first store wins for the same identity, and no later
// store is read once the top identity has matched.
func TestListActiveWorkLookupReadsStoresInPriorityOrder(t *testing.T) {
	t.Run("first store wins for the same identity", func(t *testing.T) {
		a := &listCountingStore{Store: &beads.MemStore{IDPrefix: "a"}}
		z := &listCountingStore{Store: &beads.MemStore{IDPrefix: "z"}}
		fs := twoRigActiveWorkState(t, a, z)
		first := addActiveWork(t, a, "alias")
		addActiveWork(t, z, "alias")

		if got := New(fs).findActiveBeadForAssignees(newTestActiveWorkLookup(t), "", "session-id", "alias"); got != first {
			t.Fatalf("active bead = %q, want %q from the first store", got, first)
		}
		// The unmatched first identity probes both stores once, then a's
		// "alias" match ends the lookup before z is read for it.
		if a.listCalls != 2 || z.listCalls != 1 {
			t.Fatalf("reads a=%d z=%d, want 2 and 1", a.listCalls, z.listCalls)
		}
	})

	t.Run("no later store is read once the top identity matches", func(t *testing.T) {
		a := &listCountingStore{Store: &beads.MemStore{IDPrefix: "a"}}
		z := &listCountingStore{Store: &beads.MemStore{IDPrefix: "z"}}
		fs := twoRigActiveWorkState(t, a, z)
		own := addActiveWork(t, a, "session-id")
		addActiveWork(t, z, "template")

		if got := New(fs).findActiveBeadForAssignees(newTestActiveWorkLookup(t), "", "session-id", "template"); got != own {
			t.Fatalf("active bead = %q, want the session's own %q", got, own)
		}
		if a.listCalls != 1 || z.listCalls != 0 {
			t.Fatalf("reads a=%d z=%d, want 1 and 0: no later store can outrank the top identity", a.listCalls, z.listCalls)
		}
	})
}

// TestAgentListActiveWorkPartialNotCached: an agent list whose active-work
// lookup failed is partial, so the response cache does not keep it; once the
// store recovers, the next request rebuilds the rows. The graph-store
// fallback still names a row's active bead while the work store is down.
func TestAgentListActiveWorkPartialNotCached(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })

	t.Run("work store", func(t *testing.T) {
		state := newFakeState(t)
		work := &faultyActiveWorkStore{Store: beads.NewMemStore(), err: errors.New("bd list: timed out")}
		state.stores["myrig"] = work
		want := addActiveWork(t, work.Store, "myrig/worker")
		h := newTestCityHandlerWith(t, state, New(state))

		degraded, _ := getActiveWorkList(t, h, httptest.NewRequest("GET", cityURL(state, "/agents"), nil))
		if !degraded.Partial || degraded.activeBeadOf(t, "myrig/worker") != "" {
			t.Fatalf("first list = %+v, want partial with no active bead", degraded)
		}
		work.healthy.Store(true)
		recovered, _ := getActiveWorkList(t, h, httptest.NewRequest("GET", cityURL(state, "/agents"), nil))
		if recovered.Partial || recovered.activeBeadOf(t, "myrig/worker") != want {
			t.Fatalf("list after recovery = %+v, want rebuilt with active bead %q", recovered, want)
		}
	})
	t.Run("graph fallback", func(t *testing.T) {
		st := bdDogState(t)
		city := &faultyActiveWorkStore{Store: beads.NewMemStore(), err: errors.New("bd list: timed out")}
		st.cityBeadStore = city
		graph := beads.NewMemStore()
		st.graphBeadStore = graph
		wisp := seedGraphWisp(t, graph, "bd__dog", "917c4dd26bd772e2463919029a889e0e")
		h := newTestCityHandlerWith(t, st, New(st))

		degraded, _ := getActiveWorkList(t, h, httptest.NewRequest("GET", cityURL(st, "/agents"), nil))
		if got := degraded.activeBeadOf(t, "bd.dog"); got != wisp {
			t.Errorf("active_bead = %q, want graph bead %q while the work store is down", got, wisp)
		}
		if !degraded.Partial || !degraded.hasActiveWorkError(st.cityName) {
			t.Errorf("partial = %v, partial_errors = %q; want partial naming the active-work read of %s", degraded.Partial, degraded.PartialErrors, st.cityName)
		}
		city.healthy.Store(true)
		recovered, _ := getActiveWorkList(t, h, httptest.NewRequest("GET", cityURL(st, "/agents"), nil))
		if recovered.Partial {
			t.Errorf("list after recovery partial_errors = %q, want a rebuilt complete list", recovered.PartialErrors)
		}
	})
	t.Run("aggregation error stays without blocking the rebuild", func(t *testing.T) {
		state := onDemandPoolState(t)
		state.sp = runtime.NewFailFake()
		work := &faultyActiveWorkStore{Store: beads.NewMemStore(), err: errors.New("bd list: timed out")}
		state.stores["myrig"] = work
		h := newTestCityHandlerWith(t, state, New(state))
		isPoolError := func(e string) bool { return strings.Contains(e, "myrig/claude") }

		degraded, _ := getActiveWorkList(t, h, httptest.NewRequest("GET", cityURL(state, "/agents"), nil))
		if !degraded.Partial || !degraded.hasActiveWorkError("myrig") || !slices.ContainsFunc(degraded.PartialErrors, isPoolError) {
			t.Fatalf("first partial_errors = %q, want the pool aggregation error and the active-work read of myrig", degraded.PartialErrors)
		}
		work.healthy.Store(true)
		want := addActiveWork(t, work.Store, "myrig--worker")

		recovered, _ := getActiveWorkList(t, h, httptest.NewRequest("GET", cityURL(state, "/agents"), nil))
		if !recovered.Partial || len(recovered.PartialErrors) != 1 || !isPoolError(recovered.PartialErrors[0]) {
			t.Fatalf("partial_errors after recovery = %q, want only the retained pool aggregation error", recovered.PartialErrors)
		}
		for _, item := range recovered.Items {
			if item.ActiveBead == want {
				return
			}
		}
		t.Fatalf("items after recovery = %+v, want a row rebuilt with active bead %q", recovered.Items, want)
	})
}

// TestSessionListClientReadsAWholeListPastAnActiveWorkFailure: the client gc
// session list reads GET /sessions through, whose partial answer the command
// refuses, reads a list whose only failure is an active-work read as whole,
// though the server reports that failure in active_work_errors.
func TestSessionListClientReadsAWholeListPastAnActiveWorkFailure(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.stores = map[string]beads.Store{"a": activeBeadErrStore{Store: beads.NewMemStore(), err: errors.New("bd list: connection refused")}}
	for _, title := range []string{"One", "Two"} {
		createTestSession(t, fs.cityBeadStore, fs.sp, title)
	}
	h := newTestCityHandlerWith(t, fs, New(fs))
	served, _ := getActiveWorkList(t, h, httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil))
	activeWorkListRoutes[0].checkActiveWorkFailure(t, served, "a")
	client := NewCityScopedClientWithHTTPClient("http://example.com", fs.CityName(), &http.Client{Transport: sessionPeekTransport{h: h}})

	cr, err := client.ListSessions(ListSessionsOpts{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if cr.Body.Partial || len(cr.Body.PartialErrors) > 0 {
		t.Errorf("partial = %v, partial_errors = %q; want a whole list", cr.Body.Partial, cr.Body.PartialErrors)
	}
	if len(cr.Body.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(cr.Body.Sessions))
	}
}
