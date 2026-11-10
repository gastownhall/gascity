package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The session handlers take the sessions class from State.Stores() as a
// beads.SessionStore (typed-stores T6b). These tests run each handler group
// on the shapes a city's sessions store has: the work store itself or a
// binding beside it, either one a CachingStore as the controller serves it.
// A handler that read another class's store (a relabel of the city work
// store) misses the rows on a split city; one that lost the cache's
// capabilities through the handle stops gating and stops reporting the
// cache's age.

// sessionsShape is one shape of a city's sessions store.
type sessionsShape struct {
	name   string
	split  bool // the sessions and messaging classes live in a binding beside the work store
	cached bool // the sessions store is a primed CachingStore
}

var sessionsShapes = []sessionsShape{
	{name: "single store"},
	{name: "single cached store", cached: true},
	{name: "split", split: true},
	{name: "split, cached binding", split: true, cached: true},
}

// newSessionsShapeState is a session fake state of shape, and the raw store
// its sessions class lives in. The stores are splittest's strict leaves: on a
// split shape the work store refuses a session row and holds none, and the
// binding is the one every non-work class shares.
func newSessionsShapeState(t *testing.T, shape sessionsShape) (*fakeState, beads.Store) {
	t.Helper()
	fs := newSessionFakeState(t)
	work := splittest.NewSingleStore(t, "gc")
	sessions := work
	if shape.split {
		work, sessions = splittest.NewWorkStore(t, "gc"), splittest.NewBindingStore(t)
	}
	if shape.cached {
		cache := beads.NewCachingStoreForTest(sessions, nil)
		if err := cache.Prime(t.Context()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		sessions = cache
	}
	if shape.split {
		fs.cityBeadStore = work
		fs.sessionsBeadStore = sessions
	} else {
		fs.cityBeadStore = sessions
	}
	return fs, sessions
}

// sessionRowsIn lists the session rows in store, open or closed.
func sessionRowsIn(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Label: session.LabelSession, IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing session rows: %v", err)
	}
	return rows
}

// TestSessionHandlersServeTheSessionsStore drives one request of each session
// handler group (query, waits, control, named-session resolution) over every
// shape: each finds its rows in the sessions store, and on a split city
// nothing reads or writes a session row in the work store.
func TestSessionHandlersServeTheSessionsStore(t *testing.T) {
	for _, shape := range sessionsShapes {
		t.Run(shape.name, func(t *testing.T) {
			fs, sessions := newSessionsShapeState(t, shape)
			info := createTestSession(t, sessions, fs.sp, "probe")
			wait, err := sessions.Create(beads.Bead{
				Title:    "probe wait",
				Type:     session.WaitBeadType,
				Labels:   []string{session.WaitBeadLabel, "session:" + info.ID},
				Metadata: map[string]string{"session_id": info.ID, "state": "ready", "kind": "deps"},
			})
			if err != nil {
				t.Fatalf("Create(wait): %v", err)
			}
			h := newTestCityHandler(t, fs)

			// Each step resolves the session in the sessions store; one that
			// resolved it elsewhere answers 404. respond answers 409: it found
			// the session, which has nothing pending.
			id := info.ID
			for _, step := range []struct {
				method, path, body string
				code               int
				want               string
			}{
				{http.MethodGet, "/session/" + id, "", http.StatusOK, id},
				{http.MethodGet, "/sessions", "", http.StatusOK, id},
				{http.MethodGet, "/session/" + id + "/pending", "", http.StatusOK, ""},
				{http.MethodGet, "/session/" + id + "/turn", "", http.StatusOK, id},
				{http.MethodGet, "/wait/" + wait.ID, "", http.StatusOK, wait.ID},
				{http.MethodGet, "/waits?session=" + id, "", http.StatusOK, wait.ID},
				{http.MethodPatch, "/session/" + id, `{"title":"patched"}`, http.StatusOK, "patched"},
				{http.MethodPost, "/session/" + id + "/rename", `{"title":"renamed"}`, http.StatusOK, "renamed"},
				{http.MethodPost, "/session/" + id + "/pin", "", http.StatusOK, id},
				{http.MethodPost, "/session/" + id + "/messages", `{"message":"hi"}`, http.StatusAccepted, "accepted"},
				{http.MethodPost, "/session/" + id + "/respond", `{"action":"allow"}`, http.StatusConflict, "no_pending"},
				{http.MethodPost, "/session/" + id + "/stop", `{}`, http.StatusOK, id},
				{http.MethodPost, "/session/" + id + "/suspend", "", http.StatusOK, ""},
				{http.MethodPost, "/session/" + id + "/wake", "", http.StatusOK, id},
				{http.MethodPost, "/session/" + id + "/reset", "", http.StatusOK, id},
				{http.MethodPost, "/session/" + id + "/kill", "", http.StatusOK, id},
				{http.MethodPost, "/session/" + id + "/close", "", http.StatusOK, "ok"},
				{http.MethodPost, "/session/worker/wake", "", http.StatusOK, ""},
			} {
				req := httptest.NewRequest(step.method, cityURL(fs, step.path), strings.NewReader(step.body))
				req.Header.Set("X-GC-Request", "true")
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != step.code {
					t.Fatalf("%s %s = %d, want %d: %s", step.method, step.path, rec.Code, step.code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), step.want) {
					t.Errorf("%s %s answered %s, want it to name %s", step.method, step.path, rec.Body.String(), step.want)
				}
			}

			// The wake of "worker" materialized its configured named session,
			// a write that must land in the sessions store.
			var named bool
			for _, row := range sessionRowsIn(t, sessions) {
				named = named || row.Metadata[apiNamedSessionIdentityKey] != ""
			}
			if !named {
				t.Errorf("the named session the wake materialized is not in the sessions store")
			}
			if shape.split {
				if rows := sessionRowsIn(t, fs.cityBeadStore); len(rows) != 0 {
					t.Errorf("the work store holds %d session rows, want none: a handler wrote the sessions class there", len(rows))
				}
			}
		})
	}
}

// TestLegacySessionHandlersServeTheSessionsStore is the table above through
// the legacy net/http session handlers, on every shape: each resolves the
// session in the sessions store, and the create mints its row there.
func TestLegacySessionHandlersServeTheSessionsStore(t *testing.T) {
	for _, shape := range sessionsShapes {
		t.Run(shape.name, func(t *testing.T) {
			fs, sessions := newSessionsShapeState(t, shape)
			info := createTestSession(t, sessions, fs.sp, "probe")
			h := New(fs).legacySessionHandler()
			id := info.ID
			for _, step := range []struct {
				method, path, body string
				code               int
				want               string
			}{
				{http.MethodGet, "/v0/session/" + id, "", http.StatusOK, id},
				{http.MethodGet, "/v0/sessions", "", http.StatusOK, id},
				{http.MethodGet, "/v0/session/" + id + "/pending", "", http.StatusOK, "supported"},
				{http.MethodGet, "/v0/session/" + id + "/transcript", "", http.StatusOK, id},
				{http.MethodGet, "/v0/session/" + id + "/agents", "", http.StatusOK, "agents"},
				{http.MethodPatch, "/v0/session/" + id, `{"title":"patched"}`, http.StatusOK, "patched"},
				{http.MethodPost, "/v0/session/" + id + "/rename", `{"title":"renamed"}`, http.StatusOK, "renamed"},
				{http.MethodPost, "/v0/session/" + id + "/messages", `{"message":"hi"}`, http.StatusAccepted, id},
				{http.MethodPost, "/v0/session/" + id + "/respond", `{"action":"allow"}`, http.StatusConflict, "no_pending"},
				{http.MethodPost, "/v0/session/" + id + "/stop", "", http.StatusOK, id},
				{http.MethodPost, "/v0/session/" + id + "/suspend", "", http.StatusOK, "ok"},
				{http.MethodPost, "/v0/session/" + id + "/wake", "", http.StatusOK, id},
				{http.MethodPost, "/v0/session/" + id + "/kill", "", http.StatusOK, id},
				{http.MethodPost, "/v0/session/" + id + "/close", "", http.StatusOK, "ok"},
				{http.MethodPost, "/v0/sessions", `{"kind":"agent","name":"myrig/worker"}`, http.StatusAccepted, "myrig/worker"},
			} {
				req := httptest.NewRequest(step.method, step.path, strings.NewReader(step.body))
				req.Header.Set("X-GC-Request", "true")
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != step.code {
					t.Fatalf("%s %s = %d, want %d: %s", step.method, step.path, rec.Code, step.code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), step.want) {
					t.Errorf("%s %s answered %s, want it to name %s", step.method, step.path, rec.Body.String(), step.want)
				}
			}
			if got := len(sessionRowsIn(t, sessions)); got != 2 {
				t.Errorf("the sessions store holds %d session rows, want the probe and the created one", got)
			}
			if shape.split {
				if rows := sessionRowsIn(t, fs.cityBeadStore); len(rows) != 0 {
					t.Errorf("the work store holds %d session rows, want none", len(rows))
				}
			}
		})
	}
}

// TestSessionReadsGateOnTheSessionsStoreCache pins the cache gate of the
// session-class reads (cacheLiveOr503) to the sessions store: a sessions
// cache still priming answers 503 cache_not_live, whatever the work store's
// cache says, and the gate sees through the class handle to the cache.
func TestSessionReadsGateOnTheSessionsStoreCache(t *testing.T) {
	for _, split := range []bool{false, true} {
		fs := newSessionFakeState(t)
		priming := beads.NewCachingStoreForTest(beads.NewMemStore(), nil) // unprimed: not live
		if split {
			live := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
			if err := live.Prime(t.Context()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			fs.cityBeadStore = live
			fs.sessionsBeadStore = priming
		} else {
			fs.cityBeadStore = priming
		}
		h := newTestCityHandler(t, fs)
		for _, path := range []string{"/waits", "/wait/w-any"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, path), nil))
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "cache_not_live") {
				t.Errorf("split=%v: GET %s = %d %s, want 503 cache_not_live from the priming sessions cache", split, path, rec.Code, rec.Body.String())
			}
		}
	}
}

// TestSessionReadsReportTheSessionsStoreCacheAge pins X-GC-Cache-Refresh on
// the session-class reads (cacheAgeSeconds) to the sessions store: on a split
// city whose work store reports no cache, they report the binding cache's
// freshness, and on a single cached store its own, through the class handle.
func TestSessionReadsReportTheSessionsStoreCacheAge(t *testing.T) {
	for _, shape := range []sessionsShape{
		{name: "single cached store", cached: true},
		{name: "split, cached binding", split: true, cached: true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			fs, sessions := newSessionsShapeState(t, shape)
			info := createTestSession(t, sessions, fs.sp, "probe")
			wait, err := sessions.Create(beads.Bead{
				Title:    "probe wait",
				Type:     session.WaitBeadType,
				Labels:   []string{session.WaitBeadLabel, "session:" + info.ID},
				Metadata: map[string]string{"session_id": info.ID, "state": "ready", "kind": "deps"},
			})
			if err != nil {
				t.Fatalf("Create(wait): %v", err)
			}
			h := newTestCityHandler(t, fs)
			for _, path := range []string{"/sessions", "/session/" + info.ID, "/pending", "/waits", "/wait/" + wait.ID} {
				if got := cacheHeadersOf(t, h, fs, path).Get("X-GC-Cache-Refresh"); got != string(CacheRefreshScheduled) {
					t.Errorf("GET %s X-GC-Cache-Refresh = %q, want %q from the sessions store's cache", path, got, CacheRefreshScheduled)
				}
			}
		})
	}
}

// TestServerOpsKeepsItsOpStoreAcrossStoresReads pins serverOps.store's
// identity test to SameStore over the handles: every request reads
// Stores().Sessions() afresh, and a handle over the store the cached op store
// was opened over keeps it, while a handle over another store (a reload that
// reopened the sessions store) reopens it.
func TestServerOpsKeepsItsOpStoreAcrossStoresReads(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	opens := 0
	srv.ops.openStore = func(st beads.SessionStore, clk clock.Clock) (*opjournal.Store, error) {
		opens++
		return opjournal.NewStore(st, clk, opjournal.WithIDPrefix("gc"))
	}
	for range 3 {
		if _, err := srv.ops.store(fs.Stores().Sessions()); err != nil {
			t.Fatalf("store: %v", err)
		}
	}
	if opens != 1 {
		t.Fatalf("three reads of one sessions store opened the op store %d times, want once", opens)
	}
	fs.cityBeadStore = beads.NewMemStore()
	if _, err := srv.ops.store(fs.Stores().Sessions()); err != nil {
		t.Fatalf("store after the swap: %v", err)
	}
	if opens != 2 {
		t.Fatalf("a swapped sessions store opened the op store %d times in all, want a reopen", opens)
	}
}

// TestNamedSessionHandOffOnACachedCityReadsWorkOwnershipLive: on a single
// store city the hand-off sweeps the sessions handle itself, which over the
// controller's CachingStore must still decide on live ownership. Work the
// cache shows on the retired row, but which was reassigned behind it, keeps
// its new owner.
func TestNamedSessionHandOffOnACachedCityReadsWorkOwnershipLive(t *testing.T) {
	fs, sessions := newSessionsShapeState(t, sessionsShape{cached: true})
	cache := sessions.(*beads.CachingStore)
	fs.cfg.Agents[0].Dir = ""
	fs.cfg.NamedSessions[0].Dir = ""
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	spec, ok, err := srv.findNamedSessionSpecForTarget("worker")
	if err != nil || !ok {
		t.Fatalf("findNamedSessionSpecForTarget(worker) = %v, %v", ok, err)
	}
	retiredID, err := srv.materializeNamedSession(fs.Stores().Sessions(), spec)
	if err != nil {
		t.Fatalf("materializeNamedSession(worker): %v", err)
	}
	if err := cache.SetMetadataBatch(retiredID, map[string]string{"state": "archived", "continuity_eligible": "false"}); err != nil {
		t.Fatal(err)
	}
	task := mustCreateAssigned(t, cache, "task", retiredID)
	owner := "picked-up-elsewhere"
	if err := cache.Backing().Update(task.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
		t.Fatal(err)
	}
	if cached, err := cache.Get(task.ID); err != nil || cached.Assignee != retiredID {
		t.Fatalf("the cache shows %q, %v; the fixture needs it to still show the retired row %s", cached.Assignee, err, retiredID)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/worker/wake"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("wake = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := cache.Backing().Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != owner {
		t.Errorf("task assignee = %q, want %q: the hand-off moved work on the cache's stale ownership", got.Assignee, owner)
	}
}

// TestWorkerFactoryOverAnAbsentSessionsStoreHasNoStore pins the zero handle at
// the worker boundary (R11): with no sessions store the factory is built over
// no store, so the agent output paths that reach it degrade instead of
// panicking on a zero handle boxed into worker.FactoryConfig.Store.
func TestWorkerFactoryOverAnAbsentSessionsStoreHasNoStore(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = nil
	srv := New(fs)
	factory, err := srv.workerFactory(fs.Stores().Sessions())
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	if _, err := factory.SessionByID("gc-absent"); err == nil {
		t.Fatalf("SessionByID over no sessions store succeeded, want an error")
	}
}

// splitSessionsBinding relocates fs's sessions and messaging classes to a
// binding beside a fresh work store, after fs's fixture rows were made in
// its city store: that store, which holds them, becomes the binding.
func splitSessionsBinding(t *testing.T, fs *fakeState) {
	t.Helper()
	fs.sessionsBeadStore = fs.cityBeadStore
	fs.cityBeadStore = splittest.NewWorkStore(t, "gc")
}

// TestSessionCreateAndPermissionModeUseTheSessionsBinding: on a split city an
// API-created session is minted in the sessions binding, and a permission-mode
// change resolves it there.
func TestSessionCreateAndPermissionModeUseTheSessionsBinding(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	work, binding := splittest.NewWorkStore(t, "gc"), splittest.NewBindingStore(t)
	fs.cityBeadStore = work
	fs.sessionsBeadStore = binding
	h := newTestCityHandler(t, fs)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	success, failure := waitForSessionCreateResult(t, fs.eventProv, decodeAsyncAccepted(t, rec.Body).RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	id := success.Session.ID
	if _, err := binding.Get(id); err != nil {
		t.Fatalf("the created session %s is not in the sessions binding: %v", id, err)
	}
	if rows := sessionRowsIn(t, work); len(rows) != 0 {
		t.Fatalf("the work store holds %d session rows, want none", len(rows))
	}

	mgr := session.NewManagerWithOptions(fs.Stores().Sessions(), fs.sp, session.WithCityPath(fs.cityPath))
	if err := mgr.Suspend(t.Context(), testOperatorIn(t, fs.cityPath), id, false); err != nil {
		t.Fatalf("suspend session: %v", err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/"+id+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("permission-mode = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestConversationOpsResolveTheSessionInTheSessionsBinding: on a split city
// the producer API proposes and reads a session's ops by resolving the
// session in the sessions binding.
func TestConversationOpsResolveTheSessionInTheSessionsBinding(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Lane")
	splitSessionsBinding(t, e.fs)

	rec := e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"msg-1","payload":"hello"}`)
	wantStatus(t, "propose", rec, http.StatusCreated)
	rec = e.get(t, "/session/"+info.ID+"/ops")
	wantStatus(t, "list", rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "msg-1") {
		t.Fatalf("the session's ops %s do not list the proposed op", rec.Body.String())
	}
}

// TestSessionsBindingServesTheSessionHelpers covers the session-class helpers
// no single request isolates: a mail query names the session's own inbox, a
// run's holders are interrupted, and a session's metadata is read, each
// through the sessions binding of a split city.
func TestSessionsBindingServesTheSessionHelpers(t *testing.T) {
	fs, sessions := newSessionsShapeState(t, sessionsShape{split: true})
	info := createTestSession(t, sessions, fs.sp, "probe")
	srv := New(fs)

	if q := srv.resolveMailQueryWithContext(t.Context(), info.ID); q.session == nil || q.session.ID != info.ID {
		t.Errorf("the mail query for %s reads no session inbox (%+v): the session was looked up outside the sessions binding", info.ID, q)
	}
	if got := srv.sessionMetadata(info.ID); got["session_name"] == "" {
		t.Errorf("sessionMetadata(%s) = %v, want the row's metadata", info.ID, got)
	}
	interrupted, failures := srv.interruptRunHolders(t.Context(), "run-1", []string{info.ID})
	if len(interrupted) != 1 || interrupted[0] != info.ID || len(failures) != 0 {
		t.Errorf("interruptRunHolders = %v, %v; want [%s] interrupted", interrupted, failures, info.ID)
	}
}

// TestSessionPeeksStreamsAndPendingReadTheSessionsStore drives the session
// reads that reach the runtime through the session's worker handle (the
// transcript, the agents, a peek, the city's pending interactions and the
// streams) on a split city: each finds the session, and its runtime, through
// the sessions binding.
func TestSessionPeeksStreamsAndPendingReadTheSessionsStore(t *testing.T) {
	for _, shape := range []sessionsShape{{name: "split", split: true}, {name: "split, cached binding", split: true, cached: true}} {
		t.Run(shape.name, func(t *testing.T) {
			fs, sessions := newSessionsShapeState(t, shape)
			info := createTestSession(t, sessions, fs.sp, "probe")
			const pane = "pane-output-of-the-probe"
			fs.sp.SetPeekOutput(info.SessionName, pane)
			fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-probe", Kind: "approval", Prompt: "ok?"})
			city, legacy := newTestCityHandler(t, fs), New(fs).legacySessionHandler()
			id := info.ID
			for _, step := range []struct {
				legacy bool
				path   string
				code   int
				want   string
			}{
				{false, "/session/" + id + "/transcript", http.StatusOK, pane},
				{false, "/session/" + id + "/agents", http.StatusOK, "agents"},
				// The session resolved; it has no transcript to hold an agent.
				{false, "/session/" + id + "/agents/agent-x", http.StatusNotFound, "no transcript found"},
				{true, "/v0/session/" + id + "/agents/agent-x", http.StatusNotFound, "no transcript found"},
				{false, "/session/" + id + "?peek=true", http.StatusOK, pane},
				{false, "/pending", http.StatusOK, "req-probe"},
				{false, "/session/" + id + "/stream", http.StatusOK, pane},
				{false, "/session/" + id + "/stream?format=raw", http.StatusOK, pane},
				{true, "/v0/session/" + id + "/stream", http.StatusOK, pane},
			} {
				// A stream runs until its request ends; the others answer first.
				ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
				rec := newSyncResponseRecorder()
				if step.legacy {
					legacy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, step.path, nil).WithContext(ctx))
				} else {
					city.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, step.path), nil).WithContext(ctx))
				}
				cancel()
				if body := rec.Body.String(); rec.Code != step.code || !strings.Contains(body, step.want) {
					t.Errorf("GET %s = %d %s, want %d naming %q", step.path, rec.Code, body, step.code, step.want)
				}
			}
		})
	}
}

// TestSessionAddressingReadsTheSessionsStore covers the session lookups that
// address a session other than by a name the answer falls back to, on a
// split city: an extmsg selector naming the session's runtime name, answered
// with its alias, and a mail sender naming a session of a configured agent.
func TestSessionAddressingReadsTheSessionsStore(t *testing.T) {
	fs, sessions := newSessionsShapeState(t, sessionsShape{split: true})
	mgr := session.NewManagerWithOptions(beadstest.Sessions(sessions), fs.sp, session.WithCityPath(t.TempDir()))
	info, err := mgr.CreateSession(t.Context(), session.CreateOptions{
		Template: "myrig/worker", Title: "Addressed", Command: "test-agent", WorkDir: t.TempDir(), Provider: "test-agent",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	const alias = "addressed-alias"
	if err := sessions.SetMetadata(info.ID, "alias", alias); err != nil {
		t.Fatal(err)
	}
	srv := New(fs)

	if got := srv.extmsgSessionHandleForSelector(info.SessionName); got != alias {
		t.Errorf("extmsgSessionHandleForSelector(%q) = %q, want the session's alias %q: the session name resolved outside the sessions binding", info.SessionName, got, alias)
	}
	if err := srv.refuseUnconfiguredMailSender(t.Context(), info.ID); err != nil {
		t.Errorf("a mail sender naming a configured agent's session %s was refused: %v", info.ID, err)
	}
}

// TestDefaultOpStoreOpensOverTheLiveServersSessionsStore pins
// openDefaultOpStore to the live Server's sessions store: a Server the
// supervisor replaced (a city restart that reopened its sessions store) asks
// whether, and opens, the op store over the store the city is served with
// now, not the one its own State held. Here the old store is not SQLite, so
// the old State's answer would be "not recorded by default".
func TestDefaultOpStoreOpensOverTheLiveServersSessionsStore(t *testing.T) {
	e := newOpsEnv(t, "auto")
	old := e.server(t)
	reopened, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.(interface{ CloseStore() error }).CloseStore() })
	e.rebuildOver(t, reopened, nil)
	live := e.server(t)
	var openedOver beads.SessionStore
	live.ops.openStore = func(st beads.SessionStore, clk clock.Clock) (*opjournal.Store, error) {
		openedOver = st
		return opjournal.NewStore(st, clk)
	}

	if _, err := old.openDefaultOpStore(); err != nil {
		t.Fatalf("the replaced Server's default op store: %v, want it opened over the live sessions store", err)
	}
	if !beads.SameStore(openedOver, reopened) {
		t.Fatalf("the op store was opened over %v, want the reopened sessions store", openedOver)
	}
}

// TestAgentOutputPeeksTheSessionTheSessionsStoreNames pins agentWorkerHandle
// (the legacy agent output's peek fallback) to the sessions store: the
// agent's session name is the alias of a session
// whose runtime runs under another name, which only the session row in the
// sessions binding of a split city can tell. Resolved anywhere else, the
// handle falls back to a runtime named for the agent, which is not running.
func TestAgentOutputPeeksTheSessionTheSessionsStoreNames(t *testing.T) {
	fs, sessions := newSessionsShapeState(t, sessionsShape{split: true})
	// No configured named session reserves the agent's session name.
	fs.cfg.NamedSessions = nil
	info := createTestSession(t, sessions, fs.sp, "agent")
	agentName := agentSessionName(fs.CityName(), "myrig/worker", fs.cfg.Workspace.SessionTemplate)
	if err := sessions.SetMetadata(info.ID, "alias", agentName); err != nil {
		t.Fatal(err)
	}
	const pane = "agent-pane-output"
	fs.sp.SetPeekOutput(info.SessionName, pane)

	rec := httptest.NewRecorder()
	New(fs).legacyAgentHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/agent/myrig/worker/output", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), pane) {
		t.Fatalf("GET agent output = %d %s, want 200 with the pane of session %s (runtime %s)", rec.Code, rec.Body.String(), info.ID, info.SessionName)
	}
}
