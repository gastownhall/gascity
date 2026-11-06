package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
