package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// legacyResolveClassStore is resolveClassStore as it stood before the router
// (origin/main adf99aa6c8), kept verbatim as the oracle the router must agree
// with: the worker-site refusal, else the routes' store, else the scope store
// through the split-strict class view.
func legacyResolveClassStore(routes *storageRoutes, workStore beads.Store, cityPath, class string) beads.Store {
	store, relocated := routes.storeFor(coordclassFor(class))
	binding := config.StorageWorkBinding
	if relocated {
		binding = routes.binding
	}
	if refusal := workerSiteClassRefusal(cityPath, class, binding); refusal != nil {
		return refusedClassStore{err: standingStorageRefusal{err: refusal}}
	}
	if relocated {
		return store
	}
	return splitStrictClassView(workStore)
}

// resolvedKey renders a resolved store for comparison: a refusal by its
// sentence (each resolution builds its own value), a tagged view by side and
// the store under it (the tagged seam builds a fresh view per call), and any
// other store by identity.
func resolvedKey(s beads.Store) any {
	switch v := s.(type) {
	case refusedClassStore:
		return "refused: " + v.err.Error()
	case *taggedView:
		return fmt.Sprintf("view(class=%v) over %p", v.class, v.Store)
	case nil:
		return "<nil>"
	}
	return s
}

// cityStoresClassHandle reads the handle of the class a config class name
// names back as the store it wraps, for comparison.
func cityStoresClassHandle(stores interface {
	Graph() beads.GraphStore
	Orders() beads.OrdersStore
	Sessions() beads.SessionStore
	Mail() beads.MailStore
	Nudges() beads.NudgesStore
}, class string,
) beads.Store {
	switch coordclassFor(class) {
	case coordclass.ClassGraph:
		return stores.Graph().Store
	case coordclass.ClassOrders:
		return stores.Orders().Store
	case coordclass.ClassSessions:
		return stores.Sessions().Store
	case coordclass.ClassMessaging:
		return stores.Mail().Store
	case coordclass.ClassNudges:
		return stores.Nudges().Store
	}
	return nil
}

var routedClassNames = []string{
	config.BeadClassGraph, config.BeadClassMessaging, config.BeadClassSessions,
	config.BeadClassOrders, config.BeadClassNudges,
}

// TestCityStoresAgreeWithTheLegacyResolver is T2's equivalence table: for
// every routes shape (none, whole split, graph-only split, refused), plane
// (local, worker site attached to the work ledger only, unusable descriptor),
// projection (off, on), class, and work store (one, none), the router hands
// out exactly the store the pre-router resolver did, both as a handle and
// through resolveClassStore.
func TestCityStoresAgreeWithTheLegacyResolver(t *testing.T) {
	binding := beads.NewMemStore()
	routeShapes := map[string]func() *storageRoutes{
		"none":        func() *storageRoutes { return nil },
		"whole-split": func() *storageRoutes { return messagingSplitRoutes(binding) },
		"graph-only": func() *storageRoutes {
			return &storageRoutes{stores: map[coordclass.Class]beads.Store{coordclass.ClassGraph: binding}, binding: "infra"}
		},
		"refused": func() *storageRoutes {
			return refusingStorageRoutes("infra", errors.New("refused: run gc storage migrate"))
		},
	}
	planes := map[string]func(t *testing.T, cityDir string){
		"local":           func(*testing.T, string) {},
		"worker":          func(t *testing.T, cityDir string) { writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil)) },
		"worker-unusable": func(t *testing.T, cityDir string) { writeMalformedDescriptor(t, cityDir) },
	}
	for planeName, plane := range planes {
		for _, projected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/projected=%v", planeName, projected), func(t *testing.T) {
				resetControlPlaneForTest(t)
				cityDir := newWorkerSiteCity(t)
				plane(t, cityDir)
				if projected {
					saved := splitStrictSeam
					splitStrictSeam = taggedSeam{}
					t.Cleanup(func() { splitStrictSeam = saved })
				}
				city := beads.NewMemStore()
				for shapeName, shape := range routeShapes {
					routes := shape()
					for _, work := range []beads.Store{city, nil} {
						stores := scopeStores(cityDir, routes, work)
						for _, class := range routedClassNames {
							want := resolvedKey(legacyResolveClassStore(routes, work, cityDir, class))
							if got := resolvedKey(cityStoresClassHandle(stores, class)); got != want {
								t.Errorf("%s %s over %v: handle wraps %v, legacy resolver %v", shapeName, class, work, got, want)
							}
							if got := resolvedKey(resolveClassStore(routes, work, nil, cityDir, class, nil)); got != want {
								t.Errorf("%s %s over %v: resolveClassStore %v, legacy resolver %v", shapeName, class, work, got, want)
							}
						}
						if got, want := resolvedKey(stores.Work().Store), resolvedKey(splitStrictWorkView(work)); got != want {
							t.Errorf("%s: work handle %v, want the work store's work view %v", shapeName, got, want)
						}
					}
				}
			})
		}
	}
}

// TestControllerStoresFollowTheSwappedStores pins the epoch: a reload that
// swaps the city store, or only the rig stores, is visible to the next
// accessor read, and an unchanged state hands back the same minted Stores. The expected stores go through the
// split-strict views, which are identity outside the gate.
func TestControllerStoresFollowTheSwappedStores(t *testing.T) {
	first := beads.NewMemStore()
	cs := &controllerState{cityName: "c", cityBeadStore: first}
	if got := cs.Stores().Sessions().Store; got != splitStrictClassView(first) {
		t.Fatalf("sessions = %p, want the city store %p", got, first)
	}
	epoch := cs.stores.current.Load()
	_ = cs.Stores()
	if cs.stores.current.Load() != epoch {
		t.Error("an unchanged state re-minted its Stores")
	}

	second := beads.NewMemStore()
	cs.mu.Lock()
	cs.cityBeadStore = second
	cs.mu.Unlock()
	if got := cs.Stores().Graph().Store; got != splitStrictClassView(second) {
		t.Errorf("graph after the swap = %p, want the new city store %p", got, second)
	}

	// A reload that swaps only the rig stores is visible too: the epoch keys
	// on the rig set's generation, not just the city store.
	rig := beads.NewMemStore()
	cs.mu.Lock()
	cs.beadStores = map[string]beads.Store{"r": rig}
	cs.rigStoresGen++
	cs.mu.Unlock()
	if got, ok := cs.Stores().Rig("r"); !ok || got.Store != splitStrictWorkView(rig) {
		t.Errorf("rig r after the rig swap = %v, %v; want the new rig store %p", got.Store, ok, rig)
	}
	epoch = cs.stores.current.Load()
	_ = cs.Stores()
	if cs.stores.current.Load() != epoch {
		t.Error("an unchanged rig set re-minted its Stores")
	}
	cs.mu.Lock()
	cs.beadStores = map[string]beads.Store{}
	cs.rigStoresGen++
	cs.mu.Unlock()
	if _, ok := cs.Stores().Rig("r"); ok {
		t.Error("a rig the reload dropped is still served")
	}
}

// TestCityRuntimeStoresUseTheStateWhenItHasOne pins that a runtime with a
// controllerState mints over the state's stores, not its standalone ones.
func TestCityRuntimeStoresUseTheStateWhenItHasOne(t *testing.T) {
	stateCity, standalone := beads.NewMemStore(), beads.NewMemStore()
	cr := &CityRuntime{
		cityName:            "c",
		cs:                  &controllerState{cityName: "c", cityBeadStore: stateCity},
		standaloneCityStore: standalone,
	}
	if got := cr.nudgesBeadStore().Store; got != splitStrictClassView(stateCity) {
		t.Errorf("nudges = %p, want the state's city store %p", got, stateCity)
	}
	cr.cs = nil
	if got := cr.nudgesBeadStore().Store; got != splitStrictClassView(standalone) {
		t.Errorf("nudges without a state = %p, want the standalone store %p", got, standalone)
	}
}

// TestStoresEpochSeesAnInPlaceRoutesRewrite (review probe P2): the controller
// rewrites its routes IN PLACE when it puts its CachingStore over the binding
// engine (withControllerCache keeps the pointer). An epoch minted before that
// must not survive it, or the handles keep the uncached engine — reads bypass
// the cache and writes skip its event emission — while the raw resolvers
// already answer the cache. The work store is unchanged (the city store failed
// to open), so only the class stores can tell the two epochs apart.
func TestStoresEpochSeesAnInPlaceRoutesRewrite(t *testing.T) {
	engine := beads.NewMemStore()
	routes := messagingSplitRoutes(engine)
	cr := &CityRuntime{cityName: "c", cityPath: t.TempDir(), storageRoutes: routes}
	if got := cr.graphBeadStore().Store; got != beads.Store(engine) {
		t.Fatalf("pre-cache graph = %p, want the engine", got)
	}
	cache := beads.NewMemStore() // stands in for the CachingStore withControllerCache installs
	for c := range routes.stores {
		routes.stores[c] = cache
	}
	if got := cr.graphBeadStore().Store; got != beads.Store(cache) {
		t.Errorf("standalone epoch after the rewrite = %p, want the cache %p", got, cache)
	}
	cr.cs = &controllerState{cityName: "c", storageRoutes: routes} // city store failed to open
	raw := cr.v2GraphStore()
	handle := cr.graphBeadStore().Store
	if raw != beads.Store(cache) {
		t.Fatalf("raw resolver = %p, want the cache %p", raw, cache)
	}
	if handle != raw {
		t.Errorf("epoch handle = %p (the uncached engine), raw resolver = %p (the cache): a stale epoch survived the in-place routes rewrite", handle, raw)
	}
}

// TestStoresEpochRemintsAfterTheRealControllerCache drives the same rewrite
// through withControllerCache itself: the epoch minted before it does not
// survive, and the controller's handles are the cache.
func TestStoresEpochRemintsAfterTheRealControllerCache(t *testing.T) {
	engine := beads.NewMemStore()
	routes := messagingSplitRoutes(engine)
	cs := &controllerState{cityName: "c", storageRoutes: routes, cityBeadStore: beads.NewMemStore()}
	if got := cs.Stores().Sessions().Store; got != beads.Store(engine) {
		t.Fatalf("pre-cache sessions = %T, want the engine", got)
	}
	routes.withControllerCache(t.Context(), nil)
	cached, _ := routes.storeFor(coordclass.ClassSessions)
	if _, ok := cached.(*beads.CachingStore); !ok {
		t.Fatalf("withControllerCache left sessions on %T", cached)
	}
	t.Cleanup(func() { _ = routes.close() })
	if got := cs.Stores().Sessions().Store; got != cached {
		t.Errorf("sessions after withControllerCache = %T, want the controller's cache", got)
	}
}

// TestStoresReadersSeeTheFinalEpoch (review probe P1): readers race reload
// swaps of the city and rig stores under -race; once the swaps stop, every
// accessor answers the final stores.
func TestStoresReadersSeeTheFinalEpoch(t *testing.T) {
	binding := beads.NewMemStore()
	graphOnly := &storageRoutes{stores: map[coordclass.Class]beads.Store{coordclass.ClassGraph: binding}, binding: "infra"}
	cs := &controllerState{cityName: "c", cityBeadStore: beads.NewMemStore(), storageRoutes: graphOnly}
	// Mint the first epoch before any swap, so a key that missed the work
	// store would keep it deterministically rather than by scheduling.
	_ = cs.Stores().Sessions()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = cs.Stores().Sessions()
				_ = cs.Stores().Graph()
				_ = cs.cityWorkStore()
				_, _ = cs.Stores().Rig("r")
			}
		}()
	}
	var last, lastRig beads.Store
	for i := range 3000 {
		city := beads.NewMemStore()
		cs.mu.Lock()
		cs.cityBeadStore = city
		if i%3 == 0 {
			lastRig = beads.NewMemStore()
			cs.beadStores = map[string]beads.Store{"r": lastRig}
			cs.rigStoresGen++
		}
		cs.mu.Unlock()
		last = city
	}
	close(stop)
	wg.Wait()
	if got, ok := cs.Stores().Rig("r"); !ok || got.Store != splitStrictWorkView(lastRig) {
		t.Errorf("rig after the swaps = %v, %v; want the final rig store %p", got.Store, ok, lastRig)
	}
	if got := cs.Stores().Sessions().Store; got != splitStrictClassView(last) {
		t.Errorf("sessions after the swaps = %p, want the final city store %p", got, last)
	}
	if got := cs.Stores().Graph().Store; got != beads.Store(binding) {
		t.Errorf("graph after the swaps = %p, want the binding", got)
	}
	if got := cs.cityWorkStore().Store; got != splitStrictWorkView(last) {
		t.Errorf("work after the swaps = %p, want the final city store %p", got, last)
	}
}

// TestCLINudgeStoreIsTheBindingOnASplitCity (review probe P6): the CLI nudge
// store root (openNudgeBeadStoreOwned, behind every openNudgeBeadStore caller)
// on a real migrated split city is the nudges binding, not the work ledger the
// command opened.
func TestCLINudgeStoreIsTheBindingOnASplitCity(t *testing.T) {
	cityPath, _ := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	nudges, opened, err := openNudgeBeadStoreOwned(cityPath)
	if err != nil {
		t.Fatalf("openNudgeBeadStoreOwned: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(opened) })
	binding, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassNudges)
	if !relocated || binding == nil {
		t.Fatal("the migrated city does not relocate nudges")
	}
	if nudges.Store != binding {
		t.Errorf("CLI nudges store = %T %p, want the nudges binding %T %p", nudges.Store, nudges.Store, binding, binding)
	}
	if storeEngine(nudges.Store) == storeEngine(opened) {
		t.Error("CLI nudges store is the work ledger on a split city")
	}
}

// splitRunFixture is a split one-shot city whose graph binding holds a
// workflow run (root and one step) that the work store does not.
func splitRunFixture(t *testing.T) (cityPath string, work beads.Store, rootID string) {
	t.Helper()
	cityPath = t.TempDir()
	binding := splittest.NewBindingStore(t)
	work = splittest.NewWorkStore(t, "mc")
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))
	root, err := binding.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	if _, err := binding.Create(beads.Bead{
		Title:    "step",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID, beadmeta.StepIDMetadataKey: "step"},
	}); err != nil {
		t.Fatalf("create step: %v", err)
	}
	return cityPath, work, root.ID
}

// TestFormulaCookAttachedFactsReadTheGraphFromTheBinding: on a split city the
// attached cook's execution facts read the run from the graph binding, not
// from the work store the command opened, which holds none of it.
func TestFormulaCookAttachedFactsReadTheGraphFromTheBinding(t *testing.T) {
	cityPath, work, rootID := splitRunFixture(t)
	var stderr bytes.Buffer
	emitAttachedFormulaCookExecutionFacts(work, nil, cityPath, rootID, &stderr)
	recorded, err := events.ReadAll(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read execution events (stderr=%s): %v", stderr.String(), err)
	}
	steps := 0
	for _, e := range recorded {
		if e.Type == events.ExecutionStepDefined && e.RunID == rootID {
			steps++
		}
	}
	if steps != 1 {
		t.Errorf("execution.step_defined facts for %s = %d, want 1 (stderr=%s): the graph leg is not the binding", rootID, steps, stderr.String())
	}
}

// TestEventsReemitProjectsTheGraphFromTheBinding: `gc events reemit-execution`
// projects the run from the graph binding on a split city.
func TestEventsReemitProjectsTheGraphFromTheBinding(t *testing.T) {
	cityPath, work, rootID := splitRunFixture(t)
	projection, err := projectExecutionReemitRun(cityPath, work, rootID)
	if err != nil {
		t.Fatalf("projectExecutionReemitRun: %v (the graph leg is not the binding)", err)
	}
	if len(projection.Steps) != 1 {
		t.Errorf("projected steps = %d, want 1: the graph leg is not the binding", len(projection.Steps))
	}
}

// BenchmarkResolveClassStore is the per-call cost of the raw router against
// the pre-router body, and of a mint and a cached controller read, on a
// whole-split local city (review benchmark B1).
func BenchmarkResolveClassStore(b *testing.B) {
	cityPath := b.TempDir()
	routes := messagingSplitRoutes(beads.NewMemStore())
	work := beads.NewMemStore()
	b.Run("legacy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = legacyResolveClassStore(routes, work, cityPath, "graph")
		}
	})
	b.Run("t2", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = resolveClassStore(routes, work, nil, cityPath, "graph", nil)
		}
	})
	b.Run("scopeStores-mint", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = scopeStores(cityPath, routes, work).Graph()
		}
	})
	cs := &controllerState{
		cityName: "c", cityPath: cityPath, cityBeadStore: work, storageRoutes: routes,
		beadStores: map[string]beads.Store{"a": beads.NewMemStore(), "b": beads.NewMemStore(), "c": beads.NewMemStore()},
	}
	b.Run("cs-accessor-cached-epoch", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = cs.Stores().Sessions()
		}
	})
}

// TestCLIMailStoreIsTheResolvedMessagingStore pins the session-start mail
// injection's reroute (typed-store T3): cliMailStore hands out the store
// resolveMailMessagesStore did, the binding on a split one-shot city and the
// scope store on an unsplit one.
func TestCLIMailStoreIsTheResolvedMessagingStore(t *testing.T) {
	for name, routes := range map[string]func(binding beads.Store) *storageRoutes{
		"split":   messagingSplitRoutes,
		"unsplit": func(beads.Store) *storageRoutes { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			binding := splittest.NewBindingStore(t)
			work := splittest.NewWorkStore(t, "mc")
			seedCLIStorageRoutes(t, cityPath, routes(binding))
			want := resolveMailMessagesStore(cliStorageRoutes(cityPath), work, nil, cityPath, nil)
			if got := cliMailStore(work, nil, cityPath).Store; resolvedKey(got) != resolvedKey(want) {
				t.Errorf("cliMailStore wraps %v, resolveMailMessagesStore %v", resolvedKey(got), resolvedKey(want))
			}
			// Unsplit, the messaging class falls to the scope store: the
			// store itself in production, its class view under the
			// split-strict gate (identity when the gate is off).
			wantStore := splitStrictClassView(work)
			if name == "split" {
				wantStore = binding
			}
			if !beads.SameStore(want, wantStore) {
				t.Errorf("the messaging store is %v, want %v", resolvedKey(want), resolvedKey(wantStore))
			}
		})
	}
}

// TestControllerMessagingStoreIsTheSessionsStore pins the premise the API's
// named-session hand-off rests on (typed-stores T6b, R13): it reassigns a
// retired session's external-message bindings and participants through the
// city's messaging handle, where they always went through its sessions
// store. On both topologies the controller serves, a single store and the
// whole split, the two handles are one store.
func TestControllerMessagingStoreIsTheSessionsStore(t *testing.T) {
	for name, routes := range map[string]func(binding beads.Store) *storageRoutes{
		"whole split":  messagingSplitRoutes,
		"single store": func(beads.Store) *storageRoutes { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			binding := splittest.NewBindingStore(t)
			cs := &controllerState{cityName: "c", cityPath: t.TempDir(), cityBeadStore: splittest.NewWorkStore(t, "mc"), storageRoutes: routes(binding)}
			st := cs.Stores()
			if !beads.SameStore(st.Mail(), st.Sessions()) {
				t.Errorf("Mail() wraps %v, Sessions() %v: the hand-off would move bindings in a store other than the sessions store", resolvedKey(st.Mail().Store), resolvedKey(st.Sessions().Store))
			}
			if name == "whole split" && !beads.SameStore(st.Mail(), binding) {
				t.Errorf("Mail() wraps %v, want the binding", resolvedKey(st.Mail().Store))
			}
		})
	}
}

// TestSessionStartMailReadsTheMessagingBindingOnASplitCity pins
// sessionStartMailInjection's messaging route end to end: on a split one-shot
// city the auto-handoff mail lives in the binding, the hook is handed the
// work store, and the injection still finds the handoff.
func TestSessionStartMailReadsTheMessagingBindingOnASplitCity(t *testing.T) {
	cityPath := t.TempDir()
	binding := splittest.NewBindingStore(t)
	work := splittest.NewWorkStore(t, "mc")
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))
	sessionBead, err := binding.Create(beads.Bead{
		Title:    "session worker",
		Type:     sessionpkg.BeadType,
		Labels:   []string{sessionpkg.LabelSession},
		Metadata: map[string]string{"session_name": "worker", "alias": "worker"},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	t.Setenv("GC_SESSION_ID", sessionBead.ID)
	t.Setenv("GC_MAIL", "")
	var stderr bytes.Buffer
	if code := doHandoffAuto(binding, binding, events.Discard, "worker", []string{"context cycle"}, "", io.Discard, &stderr); code != 0 {
		t.Fatalf("doHandoffAuto = %d; stderr=%s", code, stderr.String())
	}
	live := sessionpkg.Info{ID: sessionBead.ID, Alias: "worker", SessionName: "worker"}

	startMail := sessionStartMailInjection(work, &live, cityPath, &stderr)

	if len(startMail.autoHandoffIDs) != 1 {
		t.Fatalf("the injection found %d auto-handoffs, want the one in the messaging binding (stderr %q)", len(startMail.autoHandoffIDs), stderr.String())
	}
	for id := range startMail.autoHandoffIDs {
		if _, err := binding.Get(id); err != nil {
			t.Errorf("auto-handoff %s is not the binding's: %v", id, err)
		}
	}
}

// TestControllerNudgeQueueShadowsLiveInTheNudgesStore pins the API nudge
// queue's shadow store to the nudges class (typed-stores T6c: it reads
// Stores() since the exported accessor went), on routes that relocate every
// class to its own store.
func TestControllerNudgeQueueShadowsLiveInTheNudgesStore(t *testing.T) {
	work := beads.NewMemStore()
	graph, sessions, orders, nudges, mail := beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()
	routes := &storageRoutes{stores: map[coordclass.Class]beads.Store{
		coordclass.ClassGraph:     graph,
		coordclass.ClassSessions:  sessions,
		coordclass.ClassOrders:    orders,
		coordclass.ClassNudges:    nudges,
		coordclass.ClassMessaging: mail,
	}, binding: "infra"}
	cs := &controllerState{cityName: "c", cityPath: t.TempDir(), cityBeadStore: work, storageRoutes: routes}
	q, ok := cs.NudgeQueue().(*fileNudgeQueue)
	if !ok {
		t.Fatalf("NudgeQueue = %T, want the file queue", cs.NudgeQueue())
	}
	if q.store.Store != beads.Store(nudges) {
		t.Errorf("the API nudge queue's shadows live in %p, want the nudges store %p", q.store.Store, nudges)
	}
}
