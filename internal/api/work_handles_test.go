package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/orders"
)

// The work handlers take the work class from State.Stores() as a
// beads.WorkStore (typed-stores T6c): convoy CRUD, the active-bead lookup,
// formula detail, rig idempotency, and the cache gate and cache-age headers
// every work-store read reports. These tests run each group on the shapes a
// city's work store has: the only store of the city, or a work store beside a
// binding that serves every other class, either one plain or a primed
// CachingStore as the controller serves it. On a split shape the binding is a
// primed CachingStore whose re-scans are paused, so a read that took its gate
// or its cache age from the binding instead of the work store reports
// "paused", and the stores are splittest's strict leaves, so a work row
// written to the binding is refused.

// workShape is one shape of a city's work store.
type workShape struct {
	name   string
	split  bool // every non-work class lives in a binding beside the work store
	cached bool // the work store is a primed CachingStore
}

var workShapes = []workShape{
	{name: "single store"},
	{name: "single cached store", cached: true},
	{name: "split", split: true},
	{name: "split, cached work store", split: true, cached: true},
}

// primedCache is a primed CachingStore over store whose re-scans run, or
// pause when paused is set.
func primedCache(t *testing.T, store beads.Store, paused bool) *beads.CachingStore {
	t.Helper()
	cache := beads.NewCachingStoreForTest(store, nil)
	if paused {
		beads.WithReconcileGate(func() bool { return false })(cache)
	}
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache
}

// shapeWorkState sets fs's stores to shape and returns its work store and its
// binding (nil on a single-store shape).
func shapeWorkState(t *testing.T, fs *fakeState, shape workShape) (work, binding beads.Store) {
	t.Helper()
	work = splittest.NewSingleStore(t, "gc")
	if shape.split {
		work = splittest.NewWorkStore(t, "gc")
		binding = primedCache(t, splittest.NewBindingStore(t), true)
		fs.graphBeadStore, fs.sessionsBeadStore, fs.ordersBeadStore, fs.nudgesBeadStore, fs.mailBeadStore = binding, binding, binding, binding, binding
	}
	if shape.cached {
		work = primedCache(t, work, false)
	}
	fs.cityBeadStore = work
	return work, binding
}

// newWorkShapeState is a fake state of shape, its work store and its binding.
func newWorkShapeState(t *testing.T, shape workShape) (*fakeState, beads.Store, beads.Store) {
	t.Helper()
	fs := newFakeState(t)
	work, binding := shapeWorkState(t, fs, shape)
	return fs, work, binding
}

// workRequest serves one request through h and fails unless it answers code.
func workRequest(t *testing.T, h http.Handler, fs State, method, path, body string, code int) string {
	t.Helper()
	req := httptest.NewRequest(method, cityURL(fs, path), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != code {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, code, rec.Body.String())
	}
	return rec.Body.String()
}

// convoysIn lists the convoys in store, open or closed.
func convoysIn(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Type: "convoy", IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing convoys: %v", err)
	}
	return rows
}

// TestConvoyHandlersServeTheWorkStore drives the convoy CRUD group over every
// shape: the create lands in the city's work store, and list, get, check,
// add, remove and close find it there. On a split city the binding holds no
// convoy.
func TestConvoyHandlersServeTheWorkStore(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape.name, func(t *testing.T) {
			fs, work, binding := newWorkShapeState(t, shape)
			first, err := work.Create(beads.Bead{Title: "first"})
			if err != nil {
				t.Fatal(err)
			}
			second, err := work.Create(beads.Bead{Title: "second"})
			if err != nil {
				t.Fatal(err)
			}
			h := newTestCityHandler(t, fs)

			var created beads.Bead
			if err := json.Unmarshal([]byte(workRequest(t, h, fs, http.MethodPost, "/convoys", `{"title":"probe","items":["`+first.ID+`"]}`, http.StatusCreated)), &created); err != nil {
				t.Fatal(err)
			}
			if got := convoysIn(t, work); len(got) != 1 || got[0].ID != created.ID {
				t.Fatalf("work store convoys = %v, want the created %s", got, created.ID)
			}
			id := created.ID
			for _, step := range []struct {
				method, path, body string
				want               string
			}{
				{http.MethodGet, "/convoys", "", id},
				{http.MethodGet, "/convoy/" + id, "", first.ID},
				{http.MethodGet, "/convoy/" + id + "/check", "", first.ID},
				{http.MethodPost, "/convoy/" + id + "/add", `{"items":["` + second.ID + `"]}`, "updated"},
				{http.MethodGet, "/convoy/" + id + "/check", "", second.ID},
				{http.MethodPost, "/convoy/" + id + "/remove", `{"items":["` + second.ID + `"]}`, "updated"},
				{http.MethodPost, "/convoy/" + id + "/close", "", "closed"},
			} {
				if got := workRequest(t, h, fs, step.method, step.path, step.body, http.StatusOK); !strings.Contains(got, step.want) {
					t.Errorf("%s %s answered %s, want it to name %s", step.method, step.path, got, step.want)
				}
			}
			if b, err := work.Get(id); err != nil || b.Status != "closed" {
				t.Errorf("the convoy in the work store = %+v (%v), want it closed", b, err)
			}
			if binding != nil {
				if got := convoysIn(t, binding); len(got) != 0 {
					t.Errorf("the binding holds %d convoys, want none: a convoy handler served the work class from it", len(got))
				}
			}
		})
	}
}

// TestActiveBeadLookupServesTheWorkStore pins the active-bead lookup to the
// work stores over every shape: the cached lookup and the live one both find
// an agent's in-progress work bead in the city's work store.
func TestActiveBeadLookupServesTheWorkStore(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape.name, func(t *testing.T) {
			fs, work, _ := newWorkShapeState(t, shape)
			task := mustCreateInProgress(t, work, "task", "worker")
			srv := New(fs)
			if got := srv.findActiveBeadForAssignees("", "worker"); got != task.ID {
				t.Errorf("cached lookup = %q, want %s from the work store", got, task.ID)
			}
			if got := srv.findLiveActiveBeadForAssignees("", "worker"); got != task.ID {
				t.Errorf("live lookup = %q, want %s from the work store", got, task.ID)
			}
		})
	}
}

// TestLiveActiveBeadLookupReadsPastTheCache: over a cached work store the live
// lookup decides on the backing store's assignment and the cached lookup on
// the cache's. Work reassigned behind the cache is the new owner's to a live
// read, so a handle that turned the live read into a cache read would still
// name it for the old owner.
func TestLiveActiveBeadLookupReadsPastTheCache(t *testing.T) {
	for _, shape := range workShapes {
		if !shape.cached {
			continue
		}
		t.Run(shape.name, func(t *testing.T) {
			fs, work, _ := newWorkShapeState(t, shape)
			cache := work.(*beads.CachingStore)
			task := mustCreateInProgress(t, cache, "task", "worker")
			owner := "picked-up-elsewhere"
			if err := cache.Backing().Update(task.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
				t.Fatal(err)
			}
			srv := New(fs)
			if got := srv.findLiveActiveBeadForAssignees("", "worker"); got != "" {
				t.Errorf("live lookup = %q, want none: the backing store reassigned it", got)
			}
			if got := srv.findActiveBeadForAssignees("", "worker"); got != task.ID {
				t.Errorf("cached lookup = %q, want %s as the cache still shows it", got, task.ID)
			}
		})
	}
}

// TestActiveBeadLookupKeepsBeadStoresPrecedence pins workStoresByName to
// BeadStores' shape (R4): the city sits at its name's sorted position among
// the rigs, not first, so an assignee holding work in the city and in a rig
// that sorts before it is answered from the rig, on the cached and the live
// path alike, as before; a rig named like the city replaces it.
func TestActiveBeadLookupKeepsBeadStoresPrecedence(t *testing.T) {
	fs := newFakeState(t)
	city, early := beads.NewMemStore(), beads.NewMemStore()
	fs.cityBeadStore = city
	fs.stores = map[string]beads.Store{"aaa-rig": early}
	mustCreateInProgress(t, city, "city task", "worker")
	rigTask := mustCreateInProgress(t, early, "rig task", "worker")
	srv := New(fs)
	if got := srv.findActiveBeadForAssignees("", "worker"); got != rigTask.ID {
		t.Errorf("cached lookup = %q, want the rig's %s: aaa-rig sorts before test-city", got, rigTask.ID)
	}
	if got := srv.findLiveActiveBeadForAssignees("", "worker"); got != rigTask.ID {
		t.Errorf("live lookup = %q, want the rig's %s", got, rigTask.ID)
	}

	shadow := beads.NewMemStore()
	shadowTask := mustCreateInProgress(t, shadow, "shadow task", "other")
	mustCreateInProgress(t, city, "hidden city task", "other")
	fs.stores = map[string]beads.Store{fs.cityName: shadow}
	if got := srv.findActiveBeadForAssignees("", "other"); got != shadowTask.ID {
		t.Errorf("cached lookup = %q, want %s: a rig named like the city replaces the city store", got, shadowTask.ID)
	}
}

// TestActiveBeadLookupReadsASharedStoreOnce pins sortedWorkStoreNames' dedupe:
// rigs sharing one store (legacy shared-file mode) are read through one name,
// and a lookup scoped to a suspended rig reads the store it shares with a rig
// that is not suspended.
func TestActiveBeadLookupReadsASharedStoreOnce(t *testing.T) {
	shared := beads.NewMemStore()
	stores := map[string]beads.WorkStore{"alpha": beadstest.Work(shared), "beta": beadstest.Work(shared), "gamma": beadstest.Work(beads.NewMemStore())}
	if got := sortedWorkStoreNames(stores); strings.Join(got, ",") != "alpha,gamma" {
		t.Errorf("sortedWorkStoreNames = %v, want [alpha gamma]: beta shares alpha's store", got)
	}
	scoped, names := activeBeadSearchScope(stores, map[string]bool{"beta": true}, "beta")
	if strings.Join(names, ",") != "alpha" || !beads.SameStore(scoped["alpha"], shared) {
		t.Errorf("a lookup scoped to suspended beta reads %v, want alpha, which shares its store", names)
	}
	_, names = activeBeadSearchScope(stores, map[string]bool{"gamma": true}, "gamma")
	if len(names) != 0 {
		t.Errorf("a lookup scoped to suspended gamma reads %v, want nothing: its store is shared with no warm rig", names)
	}
}

// TestFindWorkStore pins convoy create's store choice to findStore's: a named
// rig's store (none for an unknown rig), else the city's work store, else the
// sole store left after deduplication, else none.
func TestFindWorkStore(t *testing.T) {
	fs := newFakeState(t)
	fs.cityBeadStore = nil
	rig, other := beads.NewMemStore(), beads.NewMemStore()
	fs.stores = map[string]beads.Store{"myrig": rig}
	if got, ok := findWorkStore(fs.Stores(), fs.cityName, ""); !ok || !beads.SameStore(got, fs.BeadStore("myrig")) {
		t.Errorf("no city store and one rig: %v, %v; want the sole rig store", got, ok)
	}
	fs.stores["alias"] = rig
	if got, ok := findWorkStore(fs.Stores(), fs.cityName, ""); !ok || !beads.SameStore(got, fs.BeadStore("myrig")) {
		t.Errorf("no city store and two rigs over one store: %v, %v; want that store", got, ok)
	}
	fs.stores["other"] = other
	if _, ok := findWorkStore(fs.Stores(), fs.cityName, ""); ok {
		t.Error("no city store and two distinct rig stores: want no store")
	}
	if got, ok := findWorkStore(fs.Stores(), fs.cityName, "other"); !ok || !beads.SameStore(got, fs.BeadStore("other")) {
		t.Errorf("rig other: %v, %v; want its store", got, ok)
	}
	if _, ok := findWorkStore(fs.Stores(), fs.cityName, "unknown"); ok {
		t.Error("an unknown rig: want no store")
	}
	fs.cityBeadStore = beads.NewMemStore()
	if got, ok := findWorkStore(fs.Stores(), fs.cityName, ""); !ok || !beads.SameStore(got, fs.CityBeadStore()) {
		t.Errorf("with a city store: %v, %v; want the city store", got, ok)
	}
	if _, ok := findWorkStore(fs.Stores(), fs.cityName, fs.cityName); ok {
		t.Error("a rig named like the city that is not a rig: want no store, as BeadStore answers")
	}
}

// TestFormulaDetailReadsTheScopeWorkStore pins formula detail's target lookup
// to the scope's work store over every shape: a city-scope preview resolves a
// convoy in the city's work store, a rig-scope one a convoy in the rig's store
// and not the city's, and an unknown rig scope is not found.
func TestFormulaDetailReadsTheScopeWorkStore(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	for _, shape := range workShapes {
		t.Run(shape.name, func(t *testing.T) {
			fs, work, _ := newWorkShapeState(t, shape)
			rig := beads.NewMemStore()
			fs.stores = map[string]beads.Store{"myrig": rig}
			formulaDir := t.TempDir()
			fs.cfg.FormulaLayers.City = []string{formulaDir}
			fs.cfg.FormulaLayers.Rigs = map[string][]string{"myrig": {formulaDir}}
			writeTestFormula(t, formulaDir, "graph-preview", `
description = "Preview {{convoy_id}}"
formula = "graph-preview"
version = 2

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
			// A first bead in the city store, so the two convoys' ids differ.
			if _, err := work.Create(beads.Bead{Title: "padding"}); err != nil {
				t.Fatal(err)
			}
			cityConvoy, err := work.Create(beads.Bead{Title: "city input", Type: "convoy"})
			if err != nil {
				t.Fatal(err)
			}
			rigConvoy, err := rig.Create(beads.Bead{Title: "rig input", Type: "convoy"})
			if err != nil {
				t.Fatal(err)
			}
			h := newTestCityHandler(t, fs)
			if got := workRequest(t, h, fs, http.MethodGet, "/formulas/graph-preview?scope_kind=city&scope_ref=test-city&target="+cityConvoy.ID, "", http.StatusOK); !strings.Contains(got, "Preview "+cityConvoy.ID) {
				t.Errorf("city scope answered %s, want the city convoy %s", got, cityConvoy.ID)
			}
			if got := workRequest(t, h, fs, http.MethodGet, "/formulas/graph-preview?scope_kind=rig&scope_ref=myrig&target="+rigConvoy.ID, "", http.StatusOK); !strings.Contains(got, "Preview "+rigConvoy.ID) {
				t.Errorf("rig scope answered %s, want the rig convoy %s", got, rigConvoy.ID)
			}
			workRequest(t, h, fs, http.MethodGet, "/formulas/graph-preview?scope_kind=rig&scope_ref=myrig&target="+cityConvoy.ID, "", http.StatusBadRequest)
			workRequest(t, h, fs, http.MethodGet, "/formulas/graph-preview?scope_kind=rig&scope_ref=nope&target="+rigConvoy.ID, "", http.StatusNotFound)
		})
	}
}

// TestRigCreateRecordsIdempotencyInTheWorkStore drives an async rig create
// over every shape: the durable idempotency record is reserved in the city's
// work store and marked succeeded there when the provision lands.
func TestRigCreateRecordsIdempotencyInTheWorkStore(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape.name, func(t *testing.T) {
			state := newFakeMutatorState(t)
			work, binding := shapeWorkState(t, state.fakeState, shape)
			h := newTestCityHandler(t, state)
			workRequest(t, h, state, http.MethodPost, "/rigs", `{"name":"gitrig","git_url":"https://example.com/repo.git","request_id":"req-shape-0001"}`, http.StatusAccepted)
			waitForEventType(t, state.eventProv, events.RequestResultRigCreate, 3*time.Second)
			rec, err := lookupIdemRecord(beadstest.Work(work), New(state).rigIdemCity(), "req-shape-0001")
			if err != nil || rec == nil {
				t.Fatalf("the work store holds no idempotency record (%v)", err)
			}
			if got := rec.Metadata[metaIdemState]; got != idemStateSucceeded {
				t.Errorf("record state = %q, want %q", got, idemStateSucceeded)
			}
			if binding != nil {
				if rows, _ := binding.List(beads.ListQuery{Label: idemLabel, IncludeClosed: true}); len(rows) != 0 {
					t.Errorf("the binding holds %d idempotency records, want none", len(rows))
				}
			}
		})
	}
}

// workReadIDs are the rows the cached reads name: a convoy, a bead and a
// message, and on a split city a graph workflow in the binding.
type workReadIDs struct{ convoy, bead, message, workflow string }

// workCachedReads are the work-store reads that report the bead cache's age
// (a repeated all=true bead list is served from its response cache), each
// served with the X-GC-Cache-Refresh of the store its gate reads: the
// city's work store, even where E1 serves the rows from a binding (mail,
// order history, bead get, a graph convoy), as the typed-stores T6 plan's
// OD-4 preserves.
func workCachedReads(ids workReadIDs) []string {
	reads := []string{
		"/convoys", "/convoy/" + ids.convoy, "/convoy/" + ids.convoy + "/check",
		"/status", "/status?fresh=true", "/beads", "/beads?all=true", "/beads?all=true", "/bead/" + ids.bead,
		"/mail", "/mail/" + ids.message, "/mail/count", "/orders/history?scoped_name=nightly",
	}
	if ids.workflow != "" {
		reads = append(reads, "/convoy/"+ids.workflow)
	}
	return reads
}

// newWorkReadsState is a fake state of shape with a convoy and a bead in its
// work store, a message in its mail, an order configured and, on a split
// shape, a graph workflow in the binding, for the cached reads. A single
// store's graph workflow is read through the work store's relic leg (T7), so
// that shape leaves it out.
func newWorkReadsState(t *testing.T, shape workShape) (*fakeState, workReadIDs) {
	t.Helper()
	fs, work, binding := newWorkShapeState(t, shape)
	// The mail provider writes into the default rig store, whose ids would
	// shadow the work store's: this city has no rigs.
	fs.stores = map[string]beads.Store{}
	fs.allOrders = []orders.Order{{Name: "nightly", Exec: "scripts/nightly.sh", Trigger: "cooldown", Interval: "1h"}}
	convoy, err := work.Create(beads.Bead{Title: "probe", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	bead, err := work.Create(beads.Bead{Title: "task"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := fs.cityMailProv.Send("human", "myrig/worker", "Hi", "hello")
	if err != nil {
		t.Fatal(err)
	}
	ids := workReadIDs{convoy: convoy.ID, bead: bead.ID, message: msg.ID}
	if binding != nil {
		root, _, _ := seedGraphWorkflowWithAFinishedStep(t, binding, "wf-probe")
		ids.workflow = root.ID
	}
	return fs, ids
}

// TestWorkReadsReportTheWorkStoreCacheAge pins cacheAgeSeconds on every work
// read to the work store over every shape: a cached work store's re-scans are
// "scheduled", a plain one reports no cache, and the binding's paused cache is
// never the one reported.
func TestWorkReadsReportTheWorkStoreCacheAge(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape.name, func(t *testing.T) {
			fs, ids := newWorkReadsState(t, shape)
			h := newTestCityHandler(t, fs)
			want := ""
			if shape.cached {
				want = string(CacheRefreshScheduled)
			}
			for _, path := range workCachedReads(ids) {
				if got := cacheHeadersOf(t, h, fs, path).Get("X-GC-Cache-Refresh"); got != want {
					t.Errorf("GET %s X-GC-Cache-Refresh = %q, want %q from the work store", path, got, want)
				}
			}
		})
	}
}

// TestWorkReadsGateOnTheWorkStoreCache pins cacheLiveOr503 on every work read
// to the work store: a work cache still priming answers 503 cache_not_live,
// alone or beside a live binding, and a priming binding beside a live work
// cache does not.
func TestWorkReadsGateOnTheWorkStoreCache(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, primingWork := range []bool{true, false} {
			if !split && !primingWork {
				continue
			}
			fs := newFakeState(t)
			fs.allOrders = []orders.Order{{Name: "nightly", Exec: "scripts/nightly.sh", Trigger: "cooldown", Interval: "1h"}}
			work := beads.Store(beads.NewCachingStoreForTest(splittest.NewSingleStore(t, "gc"), nil)) // unprimed: not live
			if split {
				priming := beads.NewCachingStoreForTest(splittest.NewBindingStore(t), nil)
				live := primedCache(t, splittest.NewBindingStore(t), false)
				binding := beads.Store(live)
				work = beads.NewCachingStoreForTest(splittest.NewWorkStore(t, "gc"), nil)
				if !primingWork {
					work, binding = primedCache(t, splittest.NewWorkStore(t, "gc"), false), priming
				}
				fs.graphBeadStore, fs.sessionsBeadStore, fs.ordersBeadStore, fs.nudgesBeadStore, fs.mailBeadStore = binding, binding, binding, binding, binding
			}
			fs.cityBeadStore = work
			h := newTestCityHandler(t, fs)
			absent := workReadIDs{convoy: "gc-none", bead: "gc-none", message: "gc-none"}
			for _, path := range append(workCachedReads(absent), "/rigs") {
				if strings.HasPrefix(path, "/convoy/") || strings.HasPrefix(path, "/bead/") || strings.HasPrefix(path, "/mail/gc-") {
					if !primingWork {
						continue // a live work store answers these 404 for the absent id
					}
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, path), nil))
				gated := rec.Code == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), "cache_not_live")
				if gated != primingWork {
					t.Errorf("split=%v priming work=%v: GET %s = %d %s, want the gate %v: it reads the work store's cache", split, primingWork, path, rec.Code, rec.Body.String(), primingWork)
				}
			}
		}
	}
}

// mustCreateInProgress creates a task in store that assignee is working on.
func mustCreateInProgress(t *testing.T, store beads.Store, title, assignee string) beads.Bead {
	t.Helper()
	b := mustCreateAssigned(t, store, title, assignee)
	status := "in_progress"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(%s): %v", title, err)
	}
	return b
}

// TestStatusCachedBodiesReportTheWorkStoreCacheAge pins cacheAgeSeconds on
// every /status path that answers with a body built earlier to the work
// store: the time bucket, the TTL floor, the stale-while-revalidate serve and
// a fresh read whose build overran. The city is split with a cached work store
// beside the paused binding, and the work cache is older than the saved body,
// so each serve reports the cache's own refresh state: "scheduled" from the
// work store, never the binding's "paused".
func TestStatusCachedBodiesReportTheWorkStoreCacheAge(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bucket, floor time.Duration
		fresh         bool
	}{
		{name: "time bucket", bucket: time.Hour},
		{name: "ttl floor", bucket: time.Nanosecond, floor: time.Hour},
		{name: "stale while revalidating", bucket: time.Nanosecond},
		{name: "fresh read past its build", bucket: time.Nanosecond, fresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinStatusBucket(t)
			timeBucketResponseCacheTTL, statusResponseTTLFloor = tc.bucket, tc.floor
			fs, work, _ := newWorkShapeState(t, workShape{split: true, cached: true})
			t.Cleanup(SetLivenessClockForTest(&clock.Fake{Time: work.(*beads.CachingStore).Stats().LastFreshAt.Add(45 * time.Second)}))
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)
			cacheHeadersOf(t, h, fs, "/status") // builds and saves the body
			srv.waitForBackground()

			hdr := http.Header(nil)
			if tc.fresh {
				shortenStatusFreshBuildWait(t, 10*time.Millisecond)
				release := make(chan struct{})
				closeRelease := sync.OnceFunc(func() { close(release) })
				t.Cleanup(func() {
					closeRelease()
					srv.waitForBackground()
				})
				fs.sessionProvider = newStalledRuntime(fs.sp, release)
				rec, served := serveInBackground(h, freshStatusRequest(fs))
				awaitSignal(t, served, "the fresh read was held by its stalled build")
				if rec.Code != http.StatusOK {
					t.Fatalf("fresh read = %d: %s", rec.Code, rec.Body.String())
				}
				hdr = rec.Header()
			} else {
				hdr = cacheHeadersOf(t, h, fs, "/status")
				srv.waitForBackground()
			}
			if got := hdr.Get("X-GC-Cache-Refresh"); got != string(CacheRefreshScheduled) {
				t.Errorf("X-GC-Cache-Refresh = %q, want %q from the work store's cache", got, CacheRefreshScheduled)
			}
			if age := cacheAgeIn(t, "/status", hdr); math.Floor(age) != 45 {
				t.Errorf("X-GC-Cache-Age-S = %v, want 45, the work cache's age", age)
			}
		})
	}
}

// TestRigIdempotencyTreatsAZeroWorkHandleAsNoStore pins the zero-handle
// guards that replaced the nil-store ones: a city with no work store answers
// a rig create 503 before any admission, and the manifest persist and the
// boot sweep do nothing over a zero handle (a method call on one panics).
func TestRigIdempotencyTreatsAZeroWorkHandleAsNoStore(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = nil
	h := newTestCityHandler(t, state)
	if got := workRequest(t, h, state, http.MethodPost, "/rigs", `{"name":"gitrig","git_url":"https://example.com/repo.git","request_id":"req-nostore-01"}`, http.StatusServiceUnavailable); !strings.Contains(got, "no bead store configured") {
		t.Errorf("POST /rigs answered %s, want no bead store configured", got)
	}
	if err := persistManifest(beads.WorkStore{}, "gc-1", RigProvisionManifest{CreatedDir: "/tmp/rig"}); err != nil {
		t.Errorf("persistManifest over a zero handle = %v, want nil", err)
	}
	deps := &fakeSweepDeps{}
	if err := SweepOrphanRigProvisions(t.Context(), beads.WorkStore{}, "c1", deps); err != nil || len(deps.teardowns) != 0 {
		t.Errorf("SweepOrphanRigProvisions over a zero handle = %v after %d teardowns, want nil and none", err, len(deps.teardowns))
	}
}

// TestConvoyCreateHonorsItsRig pins convoy create's rig argument: on a city
// with a city store and a rig store, a create naming the rig lands in the
// rig's store, and the city store gains no convoy.
func TestConvoyCreateHonorsItsRig(t *testing.T) {
	fs := newFakeState(t)
	city, rig := beads.NewMemStore(), beads.NewMemStore()
	fs.cityBeadStore = city
	fs.stores = map[string]beads.Store{"myrig": rig}
	item, err := rig.Create(beads.Bead{Title: "rig task"})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestCityHandler(t, fs)
	var created beads.Bead
	if err := json.Unmarshal([]byte(workRequest(t, h, fs, http.MethodPost, "/convoys", `{"title":"probe","rig":"myrig","items":["`+item.ID+`"]}`, http.StatusCreated)), &created); err != nil {
		t.Fatal(err)
	}
	if got := convoysIn(t, rig); len(got) != 1 || got[0].ID != created.ID {
		t.Errorf("rig store convoys = %v, want the created %s", got, created.ID)
	}
	if got := convoysIn(t, city); len(got) != 0 {
		t.Errorf("city store convoys = %v, want none: the create named the rig", got)
	}
}
