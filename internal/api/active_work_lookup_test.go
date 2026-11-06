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
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
	Partial       bool     `json:"partial"`
	PartialErrors []string `json:"partial_errors"`
}

// hasActiveWorkError reports whether partial_errors names an active-work
// read of rig.
func (b activeWorkListBody) hasActiveWorkError(rig string) bool {
	return slices.ContainsFunc(b.PartialErrors, func(e string) bool {
		return strings.Contains(e, "active work") && strings.Contains(e, `"`+rig+`"`)
	})
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

// pinStatusStoreReadTimeout sets the active-work window for the test.
func pinStatusStoreReadTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := statusStoreReadTimeout
	statusStoreReadTimeout = d
	t.Cleanup(func() { statusStoreReadTimeout = old })
}

// activeWorkListRoute is one list endpoint whose rows resolve active work.
type activeWorkListRoute struct {
	name    string
	handler func(t *testing.T, fs *fakeState, srv *Server) http.Handler
	path    func(fs *fakeState) string
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
	},
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
// response waits for one shared statusStoreReadTimeout window, not the
// store's own timeout once per row, answers 200 marked partial, and reads no
// store again after the failure.
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
				t.Errorf("answered after %s, want within one %s window plus slack", elapsed, statusStoreReadTimeout)
			}
			if len(body.Items) < 2 {
				t.Fatalf("items = %+v, want both rows", body.Items)
			}
			if !body.Partial || !body.hasActiveWorkError("myrig") {
				t.Errorf("partial = %v, partial_errors = %q; want partial naming the active-work read of myrig", body.Partial, body.PartialErrors)
			}
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
// store, and the clone's context ends when the lookup's window does or the
// client goes away, which kills a bd child instead of abandoning it.
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
			if !body.Partial || !body.hasActiveWorkError("myrig") {
				t.Errorf("partial = %v, partial_errors = %q; want partial naming the active-work read of myrig", body.Partial, body.PartialErrors)
			}
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
// be the winner. The row reports no work-store bead, and partial_errors names
// the failure beside the errors the list already had. Rows a partial result
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
				if !body.Partial || !body.hasActiveWorkError("a") {
					t.Errorf("partial = %v, partial_errors = %q; want partial naming the active-work read of a", body.Partial, body.PartialErrors)
				}
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
	errs := lookup.partialErrors()
	if len(errs) != 1 {
		t.Fatalf("partial errors = %q, want exactly the first failure", errs)
	}
	if !strings.Contains(errs[0], `looking up active work in rig "a"`) || !strings.Contains(errs[0], "store a is down") {
		t.Fatalf("error = %q, want it to name the active-work lookup and rig a with the cause", errs[0])
	}
	if n := strings.Count(logs.String(), "looking up active work in rig"); n != 1 {
		t.Fatalf("failure logged %d times over 3 rows, want 1; log:\n%s", n, logs.String())
	}
}

// TestActiveWorkLookupUsesCleanCacheAfterDeadline: a clean cache answers a
// row, hit or authoritative miss, after the lookup's window has closed, with
// no uncached read and no partial error. Once a read has failed, though,
// later rows resolve nothing, cached or not. The session lists show the
// expired window: the agents list's own session snapshot shares
// statusStoreReadTimeout, so a zero window would fail it first.
func TestActiveWorkLookupUsesCleanCacheAfterDeadline(t *testing.T) {
	for _, route := range activeWorkListRoutes[:2] {
		t.Run("expired window/"+route.name, func(t *testing.T) {
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
			if body.Partial {
				t.Errorf("partial_errors = %q, want none: cache answers need no window", body.PartialErrors)
			}
			if work.listCalls != 0 || city.listCalls != 0 {
				t.Errorf("uncached reads = %d work, %d city; want 0", work.listCalls, city.listCalls)
			}
		})
	}
	for _, route := range activeWorkListRoutes[:2] {
		t.Run("failure ends cached rows/"+route.name, func(t *testing.T) {
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
					t.Errorf("%s active_bead = %q, want none after store a failed", info, got)
				}
			}
			if n := a.reads.Load(); n != 1 {
				t.Errorf("store a active-work reads = %d, want 1", n)
			}
			if !body.Partial || !body.hasActiveWorkError("a") {
				t.Errorf("partial = %v, partial_errors = %q; want partial naming the active-work read of a", body.Partial, body.PartialErrors)
			}
		})
	}
}

// TestActiveWorkLookupUsesRealCacheAfterDeadline: a primed CachingStore
// answers a row, hit or authoritative miss, after the lookup's parent context
// ended, with no backing read and no partial error; once rig a's read has
// failed, a separately scoped row in rig z resolves nothing even though z's
// clean cache could answer it.
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
		defer lookup.close()
		cancelParent() // the window is spent; nothing has failed

		if got := srv.findActiveBeadForAssignees(lookup, "myrig", "session-id"); got != hit {
			t.Fatalf("cache hit after expiry = %q, want %q", got, hit)
		}
		if got := srv.findActiveBeadForAssignees(lookup, "myrig", "nobody"); got != "" {
			t.Fatalf("authoritative cache miss after expiry = %q, want empty", got)
		}
		if errs := lookup.partialErrors(); len(errs) != 0 {
			t.Fatalf("partial errors = %q, want none: an expired window alone is not a failure", errs)
		}
		if backing.listCalls != readsAfterPrime {
			t.Fatalf("uncached reads after expiry = %d, want 0", backing.listCalls-readsAfterPrime)
		}
	})

	t.Run("recorded failure short-circuits a later cache-backed row", func(t *testing.T) {
		backingZ := &listCountingStore{Store: beads.NewMemStore()}
		zWork := addActiveWork(t, backingZ.Store, "session-2")
		cacheZ := beads.NewCachingStoreForTest(backingZ, nil)
		if err := cacheZ.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		fs := twoRigActiveWorkState(t,
			activeBeadErrStore{Store: beads.NewMemStore(), err: errors.New("store a is down")},
			cacheZ)
		srv := New(fs)
		lookup := newTestActiveWorkLookup(t)

		if got := srv.findActiveBeadForAssignees(lookup, "a", "session-1"); got != "" {
			t.Fatalf("first row = %q, want empty after store a failed", got)
		}
		if got := srv.findActiveBeadForAssignees(lookup, "z", "session-2"); got != "" {
			t.Fatalf("later cache-backed row = %q, want empty: the recorded failure short-circuits it (cached work %s)", got, zWork)
		}
		if errs := lookup.partialErrors(); len(errs) != 1 {
			t.Fatalf("partial errors = %q, want only the first failure", errs)
		}
	})
}
