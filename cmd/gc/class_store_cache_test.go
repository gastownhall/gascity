package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
)

// openBindingEngineForTest opens a SQLite engine the way the built-in binding
// provider does: the graph mint prefix, fenced to every namespace the
// infrastructure classes reserve.
func openBindingEngineForTest(t *testing.T) *beads.SQLiteStore {
	t.Helper()
	var namespaces []string
	for _, class := range coordclass.Classes() {
		if class.IsInfrastructure() {
			namespaces = append(namespaces, config.ReservedClassPrefixesFor(class.String())...)
		}
	}
	prefix, _ := config.ReservedClassPrefix(config.BeadClassGraph)
	opened, err := beads.OpenSQLiteStore(t.TempDir(),
		beads.WithSQLiteStoreIDPrefix(prefix),
		beads.WithSQLiteStoreReservedIDPrefixes(namespaces...),
	)
	if err != nil {
		t.Fatalf("opening the binding engine: %v", err)
	}
	engine := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = engine.CloseStore() })
	return engine
}

// newCachedSplitControllerState builds the controller state a split city runs,
// with a recording event provider. Under a context with no Done channel the
// binding cache pre-primes synchronously and runs no background loop, which
// keeps its state deterministic; a cancellable one gets the production async
// prime and reconcile loop.
func newCachedSplitControllerState(ctx context.Context, t *testing.T, routes *storageRoutes, work beads.Store) (*controllerState, *events.Fake) {
	t.Helper()
	stubControllerCityStore(t, work)
	// Fed-back bead.closed events run autoclose; keep it on the test's
	// goroutine so it cannot outlive the engine the test closes.
	prevDispatch := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prevDispatch })
	ep := events.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	return newControllerStateWithRoutes(ctx, routes, cfg, nil, ep, "test-city", t.TempDir()), ep
}

// bindingCacheOf returns the CachingStore the controller serves the sessions
// class from, failing the test when the binding is not cached.
func bindingCacheOf(t *testing.T, cs *controllerState) *beads.CachingStore {
	t.Helper()
	cache, ok := cs.SessionsBeadStore().Store.(*beads.CachingStore)
	if !ok {
		t.Fatalf("the sessions class resolves to %T, want the controller's CachingStore over the binding", cs.SessionsBeadStore().Store)
	}
	return cache
}

// beadEventsFor returns "type/actor" for every bead.* row recorded about id.
func beadEventsFor(ep *events.Fake, id string) []string {
	var got []string
	for _, evt := range ep.Events {
		if evt.Subject == id && strings.HasPrefix(evt.Type, "bead.") {
			got = append(got, evt.Type+"/"+evt.Actor)
		}
	}
	return got
}

// cacheOmittedCapabilities are the engine methods the controller's binding
// cache does not carry, each with why. A method listed with a discovery must
// still be found through its handle over a SQLite engine. The rest are
// engine-only: nothing reaches them through a class store (migration, recovery
// and the one-shot claim route open the engine themselves), and carrying them
// structurally on every CachingStore would advertise them over backings that
// lack them — a cached bd store would type-assert as a claimer.
var cacheOmittedCapabilities = map[string]func(beads.Store) bool{
	"ApplyGraphPlan": func(store beads.Store) bool {
		_, ok := beads.GraphApplyFor(store)
		return ok
	},
	"ApplyGraphPlanWithStorage": func(store beads.Store) bool {
		applier, ok := beads.GraphApplyFor(store)
		_, storage := applier.(beads.StorageGraphApplyStore)
		return ok && storage
	},
	"CloseWithMetadataIfMatch": func(store beads.Store) bool {
		_, ok := beads.AtomicConditionalCloserFor(store)
		return ok
	},
	"SupportsEphemeralGraphApply": func(store beads.Store) bool {
		applier, ok := beads.GraphApplyFor(store)
		_, ephemeral := applier.(beads.EphemeralGraphApplyStore)
		return ok && ephemeral
	},
	// Engine-only (see above). CloseStore is also lifecycle: the storage
	// routes stop the cache and close the engine once (storageRoutes.close).
	"CloseStore":           nil,
	"CreateWithForeignID":  nil,
	"DepAddWithMetadata":   nil,
	"HasResidentOutside":   nil,
	"SequenceFloor":        nil,
	"SetSequenceFloor":     nil,
	"AdvanceSequenceFloor": nil,
	"StoreHealthPath":      nil,
	"ReadOnly":             nil,
	// Enterprise (native Dolt batch dep-edges, gc-enterprise #32): the batch
	// exists for `gc storage recover-stranded`, which opens the engine
	// itself. A live cache answers per-anchor down-dep reads from memory, and
	// the scope-skip dispatch path falls back to them when the assertion
	// misses, so forwarding DepListBatch on every CachingStore would
	// advertise a batch capability over backings (e.g. a cached bd store)
	// that do not actually have it.
	"DepListBatch": nil,
	// A declaration about what a cache over the engine holds: the v2
	// demand reads ask the cache's backing (demandLegCache), and a cache
	// carrying it would advertise exactness over a bd backing.
	"CachedReadExact": nil,
}

// TestControllerBindingCacheForwardsEveryEngineCapability holds the cache to
// the same engine method sets the one-shot emitter is held to, and checks the
// answers the cache gives, not just that it has the method. Kills: a dropped
// forwarder that silently downgrades a caller once the binding is cached, a
// handle-only capability whose discovery stops reaching it, and a forwarder
// that reports a capability the engine does not have.
func TestControllerBindingCacheForwardsEveryEngineCapability(t *testing.T) {
	cache := reflect.TypeOf(&beads.CachingStore{})
	for _, engine := range bindingEngineTypes {
		var missing []string
		for i := 0; i < engine.NumMethod(); i++ {
			method := engine.Method(i)
			got, ok := cache.MethodByName(method.Name)
			if !ok {
				if _, omitted := cacheOmittedCapabilities[method.Name]; !omitted {
					missing = append(missing, method.Name)
				}
				continue
			}
			if got.Type.String() != strings.Replace(method.Type.String(), engine.String(), cache.String(), 1) {
				missing = append(missing, method.Name+" (signature differs)")
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("the controller's binding cache drops %v from %s; every one is a capability assertion that stops matching", missing, engine)
		}
	}

	engine := openBindingEngineForTest(t)
	routes := splitClassRoutes(engine)
	routes.withControllerCache(context.Background(), nil)
	wrapped := routes.stores[coordclass.ClassGraph]
	for name, discover := range cacheOmittedCapabilities {
		if discover != nil && !discover(wrapped) {
			t.Errorf("%s is not discoverable through the cache over a SQLite engine", name)
		}
		if _, carried := reflect.TypeOf(wrapped).MethodByName(name); carried {
			t.Errorf("%s is listed as omitted but the cache carries it; drop it from the list", name)
		}
	}
	// Answers, not only presence: each carried capability answers as the engine does.
	applier, _ := beads.GraphApplyFor(wrapped)
	if applier.(beads.EphemeralGraphApplyStore).SupportsEphemeralGraphApply() {
		t.Error("the cache over SQLite reports ephemeral graph apply, which the engine does not support")
	}
	if got, want := wrapped.(interface{ IDPrefix() string }).IDPrefix(), engine.IDPrefix(); got != want {
		t.Errorf("cache IDPrefix = %q, engine %q", got, want)
	}
	if got, want := beads.StoreSupportsAtomicTx(wrapped), beads.StoreSupportsAtomicTx(engine); got != want {
		t.Errorf("cache AtomicTx = %v, engine %v", got, want)
	}
	if _, ok := beads.NamespaceCensusFor(wrapped); ok {
		t.Error("the cache advertises a relic census; the census reads closed history the cache does not hold")
	}
}

// TestControllerBindingWriteEmitsExactlyOnce pins the event contract of a
// controller write to the binding. Kills: zero events (the dark write main had
// before the binding was cached) and double emission (a cache stacked on the
// one-shot emitter).
func TestControllerBindingWriteEmitsExactlyOnce(t *testing.T) {
	cs, ep := newCachedSplitControllerState(context.Background(), t, splitClassRoutes(openBindingEngineForTest(t)), beads.NewMemStore())
	store := cs.SessionsBeadStore().Store

	created, err := store.Create(beads.Bead{Title: "worker", Type: "session"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(created.ID, "state", "active"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := store.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []string{
		events.BeadCreated + "/" + cacheLocalActor,
		events.BeadUpdated + "/" + cacheLocalActor,
		events.BeadClosed + "/" + cacheLocalActor,
	}
	if got := beadEventsFor(ep, created.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("events for %s = %v, want %v", created.ID, got, want)
	}

	// The one-shot funnel's routes already emit. Caching them would put a
	// second emitter on every write and hide the engine's ready projection.
	cliRoutes := splitClassRoutes(openBindingEngineForTest(t)).withCLIEmission(t.TempDir())
	cliRoutes.withControllerCache(context.Background(), events.NewFake())
	if got := cliRoutes.stores[coordclass.ClassSessions]; reflect.TypeOf(got) != reflect.TypeOf(&emittingClassStore{}) {
		t.Fatalf("emitting routes were re-wrapped as %T; a store must have exactly one emitter", got)
	}
}

// TestBindingCacheAppliesCLIEmittedEventWithoutReemit pins the out-of-process
// path: a one-shot command writes the engine and emits, and the controller's
// watcher applies that event to the binding cache. Kills: the watcher not
// routing a class namespace to the cache (including the nudge queue's
// auxiliary gcnq- prefix and the older gcs- one), a cache filtering events on
// the engine's mint prefix alone, and a re-emit of an applied event (the
// ga-yoix1 loop shape).
func TestBindingCacheAppliesCLIEmittedEventWithoutReemit(t *testing.T) {
	engine := openBindingEngineForTest(t)
	var ids []string
	for _, id := range []string{"gcg-cli", "gcs-legacy", "gcnq-queued"} {
		if _, err := engine.CreateWithForeignID(beads.Bead{ID: id, Title: "before", Type: "task"}); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	cs, ep := newCachedSplitControllerState(context.Background(), t, splitClassRoutes(engine), beads.NewMemStore())
	cache := bindingCacheOf(t, cs)
	before := len(ep.Events)

	for _, id := range ids {
		title := "after"
		if err := engine.Update(id, beads.UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("writing %s behind the cache: %v", id, err)
		}
		row, err := engine.Get(id)
		if err != nil {
			t.Fatalf("re-reading %s: %v", id, err)
		}
		payload, err := beads.EncodeBeadEventPayload(row)
		if err != nil {
			t.Fatalf("encoding %s: %v", id, err)
		}
		cs.applyBeadEventToStores(events.Event{Type: events.BeadUpdated, Actor: "worker-1", Subject: id, Payload: payload})
	}

	rows, ok := cache.CachedList(beads.ListQuery{Status: "open"})
	if !ok {
		t.Fatal("the binding cache declined a cached list after applying events")
	}
	for _, row := range rows {
		if row.Title != "after" {
			t.Errorf("cached %s still reads %q; the CLI's event never reached the binding cache", row.ID, row.Title)
		}
	}
	if len(rows) != len(ids) {
		t.Fatalf("cached rows = %d, want %d", len(rows), len(ids))
	}
	for _, evt := range ep.Events[before:] {
		if strings.HasPrefix(evt.Type, "bead.") {
			t.Errorf("applying a CLI event re-emitted %s for %s", evt.Type, evt.Subject)
		}
	}
}

// TestBindingCacheSurvivesConfigReload pins the binding cache's lifecycle
// across a config reload. The routes are immutable for the process (a [storage]
// change requires a restart), so a reload must neither reopen the binding nor
// rebuild its cache, while the work ledger's cache is rebuilt under a new epoch.
// Kills: a reload that reopens or re-wraps the binding (a second writer on the
// engine, a re-prime per reload) and one that forgets to rebuild the work cache.
func TestBindingCacheSurvivesConfigReload(t *testing.T) {
	cs, _ := newCachedSplitControllerState(context.Background(), t, splitClassRoutes(openBindingEngineForTest(t)), beads.NewMemStore())
	binding := bindingCacheOf(t, cs)
	work, ok := cs.CityBeadStore().(*beads.CachingStore)
	if !ok {
		t.Fatalf("city store is %T, want a CachingStore", cs.CityBeadStore())
	}
	bindingEpoch := binding.WriteRev("").Epoch
	workEpoch := work.WriteRev("").Epoch

	cs.update(&config.City{Workspace: config.Workspace{Name: "test-city"}}, nil)

	if got := cs.SessionsBeadStore().Store; got != beads.Store(binding) {
		t.Fatalf("after reload the sessions class resolves to %p, want the same binding cache %p", got, binding)
	}
	if got := binding.WriteRev("").Epoch; got != bindingEpoch {
		t.Fatalf("binding epoch moved %d -> %d across a reload", bindingEpoch, got)
	}
	if _, err := cs.SessionsBeadStore().Create(beads.Bead{Title: "after reload", Type: "session"}); err != nil {
		t.Fatalf("writing the binding after a reload: %v", err)
	}
	reloaded, ok := cs.CityBeadStore().(*beads.CachingStore)
	if !ok || reloaded == work {
		t.Fatalf("the work cache was not rebuilt on reload (%T)", cs.CityBeadStore())
	}
	if got := reloaded.WriteRev("").Epoch; got == workEpoch {
		t.Fatalf("the rebuilt work cache kept epoch %d", got)
	}
}

// TestClassBindingHasLegacyResidentsUnwrapsCache pins the boot census lookup
// once the binding is cached: the census keyed its verdict by the engine it
// read, and the class accessors now hand out the cache. Kills: a lookup keyed on
// the wrapper, which misses and answers the pessimistic "relics" for a clean
// binding, keeping every by-id probe alive forever.
func TestClassBindingHasLegacyResidentsUnwrapsCache(t *testing.T) {
	engine := openBindingEngineForTest(t)
	if _, err := engine.Create(beads.Bead{Title: "minted inside the namespace", Type: "task"}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	routes := splitClassRoutes(engine)
	censusBindingRelics(routes)
	if verdict, censused := routes.relics[engine]; !censused || verdict {
		t.Fatalf("boot census over a clean binding = (%v, censused %v), want a clean verdict", verdict, censused)
	}

	cs, _ := newCachedSplitControllerState(context.Background(), t, routes, beads.NewMemStore())
	graph := cs.GraphBeadStore().Store
	if _, cached := graph.(*beads.CachingStore); !cached {
		t.Fatalf("graph class resolves to %T, want the binding cache", graph)
	}
	if cs.ClassBindingHasLegacyResidents(graph) {
		t.Fatal("a clean binding reads as holding relics once cached; the census lookup is keyed on the wrapper")
	}
}

// TestSplitCityControllerBindingCacheComposition runs the production
// composition end to end: a converged split city's routes from the storage
// boot gate, the controller state over them with a cancellable context (async
// prime plus reconcile loop, exactly as the work ledger's cache), and the
// bead-event watcher's apply path.
func TestSplitCityControllerBindingCacheComposition(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "carried across", Type: "session"})
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the migration reported %s: %s", got.Outcome, log.String())
	}
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("a converged city was refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs, ep := newCachedSplitControllerState(ctx, t, routes, beads.NewMemStore())
	cache := bindingCacheOf(t, cs)
	engine, isSQLite := cache.Backing().(*beads.SQLiteStore)
	if !isSQLite {
		t.Fatalf("the binding cache's backing is %T, want the raw *beads.SQLiteStore: any layer between them hides the engine's ready projection", cache.Backing())
	}
	awaitCond(t, func() bool { return cache.Stats().State == "live" }, "the binding cache's async prime")
	if interval := cache.Stats().CurrentReconcileInterval; interval <= 0 || interval > 120*time.Second {
		t.Fatalf("binding cache reconcile cadence = %s, want the adaptive 30/60/120s bound", interval)
	}

	// A controller write is in the census, and the census covers its watermark.
	written, err := cs.SessionsBeadStore().Create(beads.Bead{Title: "controller session", Type: "session"})
	if err != nil {
		t.Fatalf("controller write: %v", err)
	}
	rows, obs, ok := cache.ObservedList(beads.ListQuery{Type: "session", Status: "open"})
	if !ok {
		t.Fatal("the binding cache refused a census after a clean write")
	}
	if !containsBeadID(rows, written.ID) {
		t.Fatalf("census %v is missing the controller's write %s", beadIDsOf(rows), written.ID)
	}
	if w, r := cache.WriteRev(written.ID), obs.CacheRev(); w.Epoch != r.Epoch || w.Seq > r.Seq {
		t.Fatalf("WriteRev %+v is not covered by the census CacheRev %+v", w, r)
	}
	if got, want := beadEventsFor(ep, written.ID), []string{events.BeadCreated + "/" + cacheLocalActor}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events for the controller write = %v, want %v", got, want)
	}

	// An out-of-process write that emitted is visible at event delivery.
	status := "closed"
	if err := engine.Update(written.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("writing behind the cache: %v", err)
	}
	if got, _ := cache.Get(written.ID); got.Status != "open" {
		t.Fatalf("the cache saw a write it was never told about (status %q); this test no longer separates delivery from staleness", got.Status)
	}
	row, err := engine.Get(written.ID)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	payload, err := beads.EncodeBeadEventPayload(row)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: "worker-1", Subject: written.ID, Payload: payload})
	rows, _, ok = cache.ObservedList(beads.ListQuery{Type: "session", Status: "open"})
	if !ok || containsBeadID(rows, written.ID) {
		t.Fatalf("census after the CLI close event = %v (admitted %v), want %s gone", beadIDsOf(rows), ok, written.ID)
	}
	// A dark write (no event) is the re-scan's; a live read sees it now.
	dark := "dark"
	carried, err := cs.SessionsBeadStore().Create(beads.Bead{Title: "carried", Type: "session"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := engine.Update(carried.ID, beads.UpdateOpts{Title: &dark}); err != nil {
		t.Fatalf("dark write: %v", err)
	}
	if got, err := beads.HandlesFor(cache).Live.Get(carried.ID); err != nil || got.Title != dark {
		t.Fatalf("live read after a dark write = (%q, %v), want %q", got.Title, err, dark)
	}

	// A reload rebuilds the work leg under a new epoch; the binding leg keeps
	// its cache and its epoch, because the routes cannot change without a restart.
	bindingEpoch := obs.CacheRev().Epoch
	cs.update(cfg, nil)
	if got := bindingCacheOf(t, cs); got != cache || got.WriteRev("").Epoch != bindingEpoch {
		t.Fatal("a config reload replaced the binding cache or moved its epoch")
	}

	// Closing the routes stops the cache before the engine goes away.
	if err := routes.close(); err != nil {
		t.Fatalf("closing the routes: %v", err)
	}
	if err := cache.Prime(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prime after the routes closed = %v, want the stopped cache's context.Canceled; its background work outlived the engine", err)
	}
}

// TestControllerBindingCacheSeesSQLiteReadyProjection is the PR-B composition
// row: a cache over the relocated SQLite binding must answer readiness with the
// engine's own predicate (SQLiteStore.enrichReadyProjectionForCache), where a
// missing blocker blocks. Kills: any layer between the controller's cache and
// the engine, which turns the projection off and offers work the engine holds
// back.
func TestControllerBindingCacheSeesSQLiteReadyProjection(t *testing.T) {
	engine := openBindingEngineForTest(t)
	blocked, err := engine.Create(beads.Bead{Title: "blocked on a missing bead", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := engine.DepAdd(blocked.ID, "gcg-missing", "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	cs, _ := newCachedSplitControllerState(context.Background(), t, splitClassRoutes(engine), beads.NewMemStore())
	cache := bindingCacheOf(t, cs)
	if cache.Backing() != beads.Store(engine) {
		t.Fatalf("the binding cache's backing is %T, want the raw engine", cache.Backing())
	}
	live, err := engine.Ready()
	if err != nil {
		t.Fatalf("engine Ready: %v", err)
	}
	if containsBeadID(live, blocked.ID) {
		t.Fatalf("the engine serves %s as ready; this fixture no longer blocks", blocked.ID)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cached, err := beads.HandlesFor(cache).Cached.Ready()
	if err != nil {
		t.Fatalf("cached Ready: %v", err)
	}
	if containsBeadID(cached, blocked.ID) {
		t.Fatalf("the binding cache offers %s, which the engine holds back", blocked.ID)
	}
}

func containsBeadID(rows []beads.Bead, id string) bool {
	for _, row := range rows {
		if row.ID == id {
			return true
		}
	}
	return false
}

// The control dispatcher's passive control-ready snapshot reads a split city's
// binding through the one-shot funnel, whose leg is the emitter. Kills: a
// snapshot cache stacked on the emitter, which hides the engine's ready
// projection from it and offers control beads the engine holds back.
func TestControlReadySnapshotCachesTheBindingEngine(t *testing.T) {
	dir := t.TempDir()
	engine := openBindingEngineForTest(t)
	leg := splitClassRoutes(engine).withCLIEmission(t.TempDir()).stores[coordclass.ClassGraph]
	installControlReadyCacheSourcesFn(t, dir, func(string, string, *config.City) ([]beads.Store, []beads.Store, error) {
		return []beads.Store{leg}, nil, nil
	})
	caches := controlReadyCachesFor(dir, t.TempDir(), &config.City{})
	if len(caches) != 1 {
		t.Fatalf("control-ready caches = %d, want 1", len(caches))
	}
	if got := caches[0].Backing(); got != beads.Store(engine) {
		t.Fatalf("control-ready snapshot backs onto %T, want the raw engine", got)
	}
}

// TestBindingCacheKeepsEngineEdgesThroughEveryWriteVerb drives every write verb
// through the controller's binding cache, then compares the cache's
// dependency edges and readiness with the engine, before and after the
// controller's own bead events are fed back through the watcher. Kills: a
// write-through absorb that drops a row's edges (a blocked step turns ready in
// the cache), and a fed-back cache-reconcile snapshot without a dependencies
// key wiping them.
func TestBindingCacheKeepsEngineEdgesThroughEveryWriteVerb(t *testing.T) {
	engine := openBindingEngineForTest(t)
	blocker, err := engine.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	step, err := engine.Create(beads.Bead{Title: "step", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := engine.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	cs, ep := newCachedSplitControllerState(context.Background(), t, splitClassRoutes(engine), beads.NewMemStore())
	cache := bindingCacheOf(t, cs)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	fed := len(ep.Events)
	feedBack := func() {
		for _, evt := range ep.Events[fed:] {
			if strings.HasPrefix(evt.Type, "bead.") {
				cs.applyBeadEventToStores(evt)
			}
		}
		fed = len(ep.Events)
	}
	check := func(verb, when string) {
		t.Helper()
		want, err := engine.DepList(step.ID, "down")
		if err != nil {
			t.Fatalf("engine DepList: %v", err)
		}
		got, err := beads.HandlesFor(cache).Cached.DepList(step.ID, "down")
		if err != nil {
			t.Fatalf("%s %s: cached DepList: %v", verb, when, err)
		}
		if len(got) != len(want) || (len(got) == 1 && got[0].DependsOnID != want[0].DependsOnID) {
			t.Fatalf("%s %s: cached edges %+v, engine edges %+v", verb, when, got, want)
		}
		ready, err := beads.HandlesFor(cache).Cached.Ready()
		if err != nil {
			t.Fatalf("%s %s: cached Ready: %v", verb, when, err)
		}
		if containsBeadID(ready, step.ID) {
			t.Fatalf("%s %s: cached Ready offers %s, which the engine blocks", verb, when, step.ID)
		}
	}
	store := cs.GraphBeadStore().Store
	revision := func() int64 {
		row, err := engine.Get(step.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return row.Revision
	}
	title := "renamed"
	verbs := []struct {
		name  string
		write func() error
	}{
		{"Update", func() error { return store.Update(step.ID, beads.UpdateOpts{Title: &title}) }},
		{"SetMetadata", func() error { return store.SetMetadata(step.ID, "k", "v") }},
		{"SetMetadataBatch", func() error { return store.SetMetadataBatch(step.ID, map[string]string{"a": "1"}) }},
		{"Close+Reopen", func() error {
			if err := store.Close(step.ID); err != nil {
				return err
			}
			return store.Reopen(step.ID)
		}},
		{"CloseAll+Reopen", func() error {
			if _, err := store.CloseAll([]string{step.ID}, map[string]string{"why": "test"}); err != nil {
				return err
			}
			return store.Reopen(step.ID)
		}},
		{"Tx", func() error {
			return store.Tx("tx", func(tx beads.Tx) error { return tx.SetMetadataBatch(step.ID, map[string]string{"tx": "1"}) })
		}},
		{"UpdateIfMatch", func() error {
			writer, _ := beads.ConditionalWriterFor(store)
			return writer.UpdateIfMatch(step.ID, revision(), beads.UpdateOpts{Title: &title})
		}},
		{"CompareAndSetMetadataKey", func() error {
			_, err := store.(*beads.CachingStore).CompareAndSetMetadataKey(step.ID, "k", "v", "w")
			return err
		}},
		{"Update(assign)", func() error {
			status, worker := "in_progress", "worker-1"
			return store.Update(step.ID, beads.UpdateOpts{Status: &status, Assignee: &worker})
		}},
		{"ReleaseIfCurrent", func() error {
			_, err := store.(*beads.CachingStore).ReleaseIfCurrent(step.ID, "worker-1")
			return err
		}},
		// A new blocked step created through the cache; the checks follow it.
		{"Create with Needs", func() error {
			created, err := store.Create(beads.Bead{Title: "new step", Type: "task", Needs: []string{blocker.ID}})
			if err == nil {
				step = created
			}
			return err
		}},
	}
	for _, verb := range verbs {
		if err := verb.write(); err != nil {
			t.Fatalf("%s: %v", verb.name, err)
		}
		check(verb.name, "through the cache")
		feedBack()
		check(verb.name, "after its events came back")
	}
}

// feedBackBeadEvents applies every bead.* event recorded since *cursor through
// the controller's watcher path, advances the cursor, and returns how many
// there were.
func feedBackBeadEvents(cs *controllerState, ep *events.Fake, cursor *int) int {
	n := 0
	for _, evt := range ep.Events[*cursor:] {
		if strings.HasPrefix(evt.Type, "bead.") {
			cs.applyBeadEventToStores(evt)
			n++
		}
	}
	*cursor = len(ep.Events)
	return n
}

// cliEventFor builds the bead.updated a one-shot CLI emits for id after a
// write. withEdges false reproduces a binary that predates the explicit empty
// dependencies array: an edge-less row's payload then carries no key at all.
func cliEventFor(t *testing.T, engine beads.Store, id string, withEdges bool) events.Event {
	t.Helper()
	row, err := engine.Get(id)
	if err != nil {
		t.Fatalf("re-reading %s: %v", id, err)
	}
	payload, err := beads.EncodeBeadEventPayload(row)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if withEdges && len(row.Dependencies) == 0 {
		row.Dependencies = []beads.Dep{}
		payload = withExplicitEdges(payload, row)
	}
	return events.Event{Type: events.BeadUpdated, Actor: "worker-1", Subject: id, Payload: payload}
}

// A relic is a bead `gc storage migrate` carried into the binding under its
// work-ledger id; the work ledger keeps a frozen same-id copy. Its events route
// by prefix to the work cache. Kills: applying relic events to the binding
// cache too, which lets the two caches trade the frozen copy and the real row
// through their re-scans forever and leaves the binding serving the copy.
func TestRelicEditQuiescesWithTheBindingCacheEqualToTheEngine(t *testing.T) {
	engine := openBindingEngineForTest(t)
	if _, err := engine.CreateWithForeignID(beads.Bead{ID: "ga-relic", Title: "carried", Type: "task"}); err != nil {
		t.Fatalf("seeding the relic: %v", err)
	}
	work := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "ga-relic", Title: "carried", Type: "task", Status: "open"}}, nil)
	cs, ep := newCachedSplitControllerState(context.Background(), t, splitClassRoutes(engine), work)
	cs.cfg.Workspace.Prefix = "ga"
	binding := bindingCacheOf(t, cs)
	workCache, ok := cs.CityBeadStore().(*beads.CachingStore)
	if !ok {
		t.Fatalf("city store is %T", cs.CityBeadStore())
	}
	for _, cache := range []*beads.CachingStore{binding, workCache} {
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
	}

	title := "edited through the CLI"
	if err := engine.Update("ga-relic", beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("editing the relic: %v", err)
	}
	cursor := len(ep.Events)
	cs.applyBeadEventToStores(cliEventFor(t, engine, "ga-relic", true))

	var perRound []int
	for round := 0; round < 4; round++ {
		binding.ReconcileNowForTest()
		workCache.ReconcileNowForTest()
		perRound = append(perRound, feedBackBeadEvents(cs, ep, &cursor))
	}
	got, err := beads.HandlesFor(binding).Cached.Get("ga-relic")
	if err != nil {
		t.Fatalf("cached relic: %v", err)
	}
	if got.Title != title {
		t.Fatalf("binding cache serves %q, engine holds %q (events per round %v)", got.Title, title, perRound)
	}
	for round, n := range perRound[2:] {
		if n != 0 {
			t.Fatalf("round %d still emitted %d bead events (per round %v); the caches never quiesce", round+3, n, perRound)
		}
	}
}

// One CLI event on an edge-less binding row must not cost the whole cache its
// ready verdicts. Kills: applying the event as a bd hook patch, which drops the
// row's edges and marks the cache's edge set incomplete, so the next status
// change clears every ready verdict and the next re-scan re-emits every row.
func TestCLIEventOnAnEdgelessRowDoesNotStormTheBindingCache(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withEdges bool
	}{
		{"explicit empty dependencies", true},
		{"older CLI without the key", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := openBindingEngineForTest(t)
			blocker, err := engine.Create(beads.Bead{Title: "blocker", Type: "task"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			step, err := engine.Create(beads.Bead{Title: "step", Type: "task"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := engine.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
				t.Fatalf("DepAdd: %v", err)
			}
			var loose []string
			for i := 0; i < 5; i++ {
				row, err := engine.Create(beads.Bead{Title: "edge-less", Type: "task"})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				loose = append(loose, row.ID)
			}
			other, err := engine.Create(beads.Bead{Title: "closes later", Type: "task"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			cs, ep := newCachedSplitControllerState(context.Background(), t, splitClassRoutes(engine), beads.NewMemStore())
			cache := bindingCacheOf(t, cs)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			cursor := len(ep.Events)

			if err := engine.SetMetadata(loose[0], "touched", "yes"); err != nil {
				t.Fatalf("CLI write: %v", err)
			}
			cs.applyBeadEventToStores(cliEventFor(t, engine, loose[0], tc.withEdges))
			if err := cs.GraphBeadStore().Close(other.ID); err != nil {
				t.Fatalf("status change: %v", err)
			}
			feedBackBeadEvents(cs, ep, &cursor)

			cache.ReconcileNowForTest()
			if n := feedBackBeadEvents(cs, ep, &cursor); n != 0 {
				t.Fatalf("the re-scan after one CLI event and one status change emitted %d bead events, want 0", n)
			}
			ready, err := beads.HandlesFor(cache).Cached.Ready()
			if err != nil {
				t.Fatalf("cached Ready: %v", err)
			}
			if containsBeadID(ready, step.ID) {
				t.Fatalf("cached Ready offers %s, which its blocker holds back", step.ID)
			}
		})
	}
}

// The one-shot emitter writes an explicit empty dependencies array for a row it
// confirmed has no edges. Kills: omitempty dropping the key, which leaves the
// controller unable to tell "no edges" from "unknown".
func TestEmitterWritesAnExplicitEmptyDependenciesArray(t *testing.T) {
	cityPath := t.TempDir()
	engine := openBindingEngineForTest(t)
	store := splitClassRoutes(engine).withCLIEmission(cityPath).stores[coordclass.ClassGraph]
	row, err := store.Create(beads.Bead{Title: "edge-less", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(row.ID, "k", "v"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	found := false
	for _, evt := range readCityJournal(t, cityPath) {
		if evt.Subject != row.ID || evt.Type != events.BeadUpdated {
			continue
		}
		found = true
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(evt.Payload, &fields); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if deps, ok := fields["dependencies"]; !ok || string(deps) != "[]" {
			t.Fatalf("payload dependencies = %q (present %v), want an explicit []", deps, ok)
		}
	}
	if !found {
		t.Fatal("the emitter wrote no bead.updated for the write")
	}
}

// Every class the binding serves shares one cache. Kills (M29): a cache per
// class, which turns one binding into five legs (callers dedup by store
// identity), primes and re-scans the engine five times, and lets a write
// through one class leave another class's cache stale.
func TestControllerBindingCacheIsSharedByEveryClass(t *testing.T) {
	routes := splitClassRoutes(openBindingEngineForTest(t))
	routes.withControllerCache(context.Background(), nil)
	var shared beads.Store
	for class, store := range routes.stores {
		if _, cached := store.(*beads.CachingStore); !cached {
			t.Fatalf("%s resolves to %T, want the binding cache", class, store)
		}
		if shared == nil {
			shared = store
			continue
		}
		if store != shared {
			t.Fatalf("%s has its own cache; every class of one engine must share one", class)
		}
	}
	if len(routes.caches) != 1 {
		t.Fatalf("routes track %d caches, want 1", len(routes.caches))
	}
}
