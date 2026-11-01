package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
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
// swaps the city store is visible to the next accessor read, and an unchanged
// state hands back the same minted Stores. The expected stores go through the
// split-strict views, which are identity outside the gate.
func TestControllerStoresFollowTheSwappedStores(t *testing.T) {
	first := beads.NewMemStore()
	cs := &controllerState{cityName: "c", cityBeadStore: first}
	if got := cs.sessionsBeadStore().Store; got != splitStrictClassView(first) {
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
	if got := cs.GraphBeadStore().Store; got != splitStrictClassView(second) {
		t.Errorf("graph after the swap = %p, want the new city store %p", got, second)
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
	if got := cs.SessionsBeadStore().Store; got != beads.Store(engine) {
		t.Fatalf("pre-cache sessions = %T, want the engine", got)
	}
	routes.withControllerCache(t.Context(), nil)
	cached, _ := routes.storeFor(coordclass.ClassSessions)
	if _, ok := cached.(*beads.CachingStore); !ok {
		t.Fatalf("withControllerCache left sessions on %T", cached)
	}
	t.Cleanup(func() { _ = routes.close() })
	if got := cs.SessionsBeadStore().Store; got != cached {
		t.Errorf("sessions after withControllerCache = %T, want the controller's cache", got)
	}
}

// TestStoresReadersSeeTheFinalEpoch (review probe P1): readers race reload
// swaps under -race; once the swaps stop, every accessor answers the final
// stores.
func TestStoresReadersSeeTheFinalEpoch(t *testing.T) {
	binding := beads.NewMemStore()
	graphOnly := &storageRoutes{stores: map[coordclass.Class]beads.Store{coordclass.ClassGraph: binding}, binding: "infra"}
	cs := &controllerState{cityName: "c", cityBeadStore: beads.NewMemStore(), storageRoutes: graphOnly}
	// Mint the first epoch before any swap, so a key that missed the work
	// store would keep it deterministically rather than by scheduling.
	_ = cs.SessionsBeadStore()
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
				_ = cs.SessionsBeadStore()
				_ = cs.GraphBeadStore()
				_ = cs.cityWorkStore()
			}
		}()
	}
	var last beads.Store
	for range 3000 {
		city := beads.NewMemStore()
		cs.mu.Lock()
		cs.cityBeadStore = city
		cs.mu.Unlock()
		last = city
	}
	close(stop)
	wg.Wait()
	if got := cs.SessionsBeadStore().Store; got != splitStrictClassView(last) {
		t.Errorf("sessions after the swaps = %p, want the final city store %p", got, last)
	}
	if got := cs.GraphBeadStore().Store; got != beads.Store(binding) {
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
			_ = cs.SessionsBeadStore()
		}
	})
}
