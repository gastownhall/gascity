package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
)

// newGraphOrderFixture writes a city-scoped graph.v2 order whose single worker
// step routes to a city agent, and returns the city path, config and order the
// dispatcher tests below fire.
func newGraphOrderFixture(t *testing.T) (string, *config.City, orders.Order) {
	t.Helper()
	cityPath := t.TempDir()
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "reaper.toml"), []byte(`
formula = "reaper"
version = 2
contract = "graph.v2"

[[steps]]
id = "sweep"
title = "Reaper sweep"
metadata = { "gc.run_target" = "worker" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	maxOne, maxTwo := 1, 2
	cfg := &config.City{
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: &maxTwo},
			{Name: config.ControlDispatcherAgentName, MaxActiveSessions: &maxOne},
		},
	}
	// The order's formula declares the graph.v2 contract, so the compiler
	// capability has to be on for it to compile at all. Derive it from the city
	// config the way a booting city does, so the dispatch under test runs the
	// same gate pairing production runs; the cleanup restores the default.
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	a := orders.Order{
		Name:         "reaper",
		Formula:      "reaper",
		Trigger:      "cooldown",
		Interval:     "15m",
		FormulaLayer: formulaDir,
	}
	return cityPath, cfg, a
}

// dispatchGraphOrder fires one tick of the order and waits for the dispatch
// goroutine to persist its outcome.
func dispatchGraphOrder(t *testing.T, m *memoryOrderDispatcher, cityPath string) {
	t.Helper()
	m.dispatch(context.Background(), cityPath, time.Now())
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if !m.drain(drainCtx) {
		t.Fatal("order dispatch did not drain")
	}
}

// allBeads returns every bead a store holds, open or closed.
func allBeads(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	list, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	return list
}

// workflowRoot returns the graph.v2 workflow root a dispatch materialized.
func workflowRoot(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	for _, b := range allBeads(t, store) {
		if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow {
			return b
		}
	}
	t.Fatalf("no graph.v2 workflow root in store; beads = %+v", allBeads(t, store))
	return beads.Bead{}
}

// TestOrderDispatchWispRootLandsInGraphStoreOnSplitCity pins the producer half
// of the graph-store split for order dispatch. coordclass classifies a wisp root
// as ClassGraph, so on a city whose graph class is served by its own binding the
// molecule an order materializes must be created in — and minted by — that
// binding. Creating it through the order's own target store puts graph beads in
// the work ledger under the work prefix, and the city's convergence check then
// reads them as graph-class beads stranded off their binding.
func TestOrderDispatchWispRootLandsInGraphStoreOnSplitCity(t *testing.T) {
	cityPath, cfg, a := newGraphOrderFixture(t)
	workStore := beads.NewMemStore()
	workStore.IDPrefix = "mc"
	graphStore := beads.NewMemStore()
	graphStore.IDPrefix = "gcg"

	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa:                   []orders.Order{a},
		storeFn:              func(execStoreTarget) (beads.Store, error) { return workStore, nil },
		storageRoutes:        messagingSplitRoutes(graphStore),
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	dispatchGraphOrder(t, m, cityPath)

	if rec.hasType(events.OrderFailed) || !rec.hasType(events.OrderCompleted) {
		t.Fatalf("events = %+v, want completed without failure", rec.events)
	}

	root := workflowRoot(t, graphStore)
	if !strings.HasPrefix(root.ID, "gcg-") {
		t.Fatalf("wisp root id = %q, want the graph binding's %q prefix", root.ID, "gcg-")
	}
	if !hasLabel(root.Labels, "order-run:reaper") {
		t.Fatalf("wisp root labels = %v, want order-run:reaper stamped in the graph store", root.Labels)
	}

	for _, b := range allBeads(t, workStore) {
		if kind := b.Metadata[beadmeta.KindMetadataKey]; kind != "" {
			t.Fatalf("work store holds graph bead %s (%s, gc.kind=%q); graph beads belong in the graph binding", b.ID, b.Title, kind)
		}
	}
}

// TestOrderDispatchSingleFlightGateSeesGraphResidentWisp pins the read half of
// the same move. The wisp root carries this order's order-run label, and it is
// the evidence the wisp-aware open-work gate uses to suppress a re-fire while
// the previous run is still in flight. Once the root lives in the graph binding,
// a gate that only scans the order's target store finds nothing and the order
// re-dispatches on every tick.
func TestOrderDispatchSingleFlightGateSeesGraphResidentWisp(t *testing.T) {
	cityPath, cfg, a := newGraphOrderFixture(t)
	workStore := beads.NewMemStore()
	workStore.IDPrefix = "mc"
	graphStore := beads.NewMemStore()
	graphStore.IDPrefix = "gcg"

	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa:                   []orders.Order{a},
		storeFn:              func(execStoreTarget) (beads.Store, error) { return workStore, nil },
		storageRoutes:        messagingSplitRoutes(graphStore),
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	dispatchGraphOrder(t, m, cityPath)
	firstRoot := workflowRoot(t, graphStore)

	// The order's cooldown is 15m, so drop the tracking-bead evidence and the
	// cooldown cache to leave the still-open wisp root as the only thing that
	// can hold the gate shut.
	for _, b := range allBeads(t, workStore) {
		if err := workStore.Delete(b.ID); err != nil {
			t.Fatalf("deleting %s: %v", b.ID, err)
		}
	}
	m.cacheMu.Lock()
	m.lastRunCache = nil
	m.cacheMu.Unlock()

	hasOpen := false
	for _, store := range []beads.Store{workStore, graphStore} {
		open, err := m.hasOpenWorkStrict(store, "reaper")
		if err != nil {
			t.Fatalf("open-work gate: %v", err)
		}
		hasOpen = hasOpen || open
	}
	if !hasOpen {
		t.Fatalf("open-work gate did not see open wisp root %s in the graph store", firstRoot.ID)
	}

	m.dispatch(context.Background(), cityPath, time.Now())
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if !m.drain(drainCtx) {
		t.Fatal("second order dispatch did not drain")
	}
	var roots int
	for _, b := range allBeads(t, graphStore) {
		if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow {
			roots++
		}
	}
	if roots != 1 {
		t.Fatalf("workflow roots in the graph store = %d, want 1; the gate re-fired the order while %s was still open", roots, firstRoot.ID)
	}
}

// TestOrderDispatchWispStaysOnTheOneStoreOnSingleStoreCity is the compatibility
// guarantee: a city that relocates nothing routes nothing. The dispatcher must
// hand the molecule create the exact store value it was already using — not a
// re-wrapped one, which would drop the optional-capability assertions the
// create path makes — and the wisp must keep minting under that store's prefix.
func TestOrderDispatchWispStaysOnTheOneStoreOnSingleStoreCity(t *testing.T) {
	cityPath, cfg, a := newGraphOrderFixture(t)
	store := beads.NewMemStore()

	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa:                   []orders.Order{a},
		storeFn:              func(execStoreTarget) (beads.Store, error) { return store, nil },
		storageRoutes:        nil, // no [storage] section: every class is the work store
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	if got := m.graphStoreFor(store); got != beads.Store(store) {
		t.Fatalf("graphStoreFor returned %T(%p), want the identical store value %p", got, got, store)
	}
	dispatchGraphOrder(t, m, cityPath)

	if rec.hasType(events.OrderFailed) || !rec.hasType(events.OrderCompleted) {
		t.Fatalf("events = %+v, want completed without failure", rec.events)
	}
	root := workflowRoot(t, store)
	if !strings.HasPrefix(root.ID, "gc-") {
		t.Fatalf("wisp root id = %q, want the single store's %q prefix", root.ID, "gc-")
	}
	if !hasLabel(root.Labels, "order-run:reaper") {
		t.Fatalf("wisp root labels = %v, want order-run:reaper", root.Labels)
	}
	tracking := trackingBeads(t, store, "order-run:reaper")
	if len(tracking) == 0 {
		t.Fatal("no order-run beads in the single store")
	}
	var foundTracking bool
	for _, b := range tracking {
		if hasLabel(b.Labels, labelOrderTracking) {
			foundTracking = true
		}
	}
	if !foundTracking {
		t.Fatalf("order-run beads = %+v, want the tracking bead colocated with the wisp", tracking)
	}
}

// TestOrderDispatchExecutionFactsProjectFromTheGraphStore pins the two
// event-emission legs. The graph store exclusively owns the workflow root and
// its physical steps; the work store owns the tracks edges of any input convoy
// the root names. Wrapping one store as both legs projects the snapshot out of
// whichever ledger happens to hold the root, so on a split city the emitted
// step-definition subjects are work-store ids for beads the graph binding is
// supposed to own.
func TestOrderDispatchExecutionFactsProjectFromTheGraphStore(t *testing.T) {
	cityPath, cfg, a := newGraphOrderFixture(t)
	workStore := beads.NewMemStore()
	workStore.IDPrefix = "mc"
	graphStore := beads.NewMemStore()
	graphStore.IDPrefix = "gcg"

	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa:                   []orders.Order{a},
		storeFn:              func(execStoreTarget) (beads.Store, error) { return workStore, nil },
		storageRoutes:        messagingSplitRoutes(graphStore),
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	dispatchGraphOrder(t, m, cityPath)

	var subjects []string
	rec.mu.Lock()
	for _, e := range rec.events {
		if e.Type == events.ExecutionStepDefined {
			subjects = append(subjects, e.Subject)
		}
	}
	rec.mu.Unlock()
	if len(subjects) == 0 {
		t.Fatalf("events = %+v, want execution step-definition facts projected from the graph store", rec.events)
	}
	for _, subject := range subjects {
		if _, err := graphStore.Get(subject); err != nil {
			t.Fatalf("step-definition subject %q not in the graph store: %v", subject, err)
		}
		if _, err := workStore.Get(subject); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("step-definition subject %q resolves in the work store (err = %v); the graph leg projected from the wrong class", subject, err)
		}
	}

	// The work leg is still the order's own store — it is where an input
	// convoy's tracks edges live — but nothing the dispatch WRITES lands there
	// on a split city: the graph class owns the root and its steps, the orders
	// class owns the tracking bead, and both are served by the binding.
	if got := allBeads(t, workStore); len(got) != 0 {
		t.Fatalf("work store holds %+v after a split-city dispatch, want nothing; every bead a dispatch writes is infrastructure class and belongs in the binding", got)
	}
}

// TestOrderDispatchConstructorDeliversRoutesToTheWispBirth closes the gap
// between "the routing decision is right" and "the routing decision is reached".
//
// Every other test in this file hands memoryOrderDispatcher its storageRoutes as
// a struct-literal field, which proves dispatchWisp USES the field and nothing
// about the four-hop argument chain that fills it in production
// (newCityRuntime/rescan/reload/webhook -> buildOrderDispatcher* ->
// newMemoryOrderDispatcher). Reverting those call sites to nil is a one-token
// edit — the shape a rebase or a bad conflict resolution produces — and it
// leaves build, vet, lint and every routing test green while putting every
// dispatched wisp back in the work ledger, which is the stranded-graph-bead
// incident this change exists to fix.
//
// So this one builds the dispatcher through the constructor and asserts on
// behavior. Only storeFn is replaced afterwards, because the production opener
// would reach for a real bd store; the routes come from the constructor, which
// is the thing under test.
func TestOrderDispatchConstructorDeliversRoutesToTheWispBirth(t *testing.T) {
	cityPath, cfg, a := newGraphOrderFixture(t)
	workStore := beads.NewMemStore()
	workStore.IDPrefix = "mc"
	graphStore := beads.NewMemStore()
	graphStore.IDPrefix = "gcg"

	var rec memRecorder
	od := buildOrderDispatcherFromOrderSet(messagingSplitRoutes(graphStore), cityPath, cfg, []orders.Order{a}, &rec, io.Discard)
	m, ok := od.(*memoryOrderDispatcher)
	if !ok {
		t.Fatalf("buildOrderDispatcherFromOrderSet returned %T, want *memoryOrderDispatcher", od)
	}
	t.Cleanup(m.dispatchCancel)
	m.storeFn = func(execStoreTarget) (beads.Store, error) { return workStore, nil }
	m.maxDispatchesPerTick = 1

	dispatchGraphOrder(t, m, cityPath)

	root := workflowRoot(t, graphStore)
	if !strings.HasPrefix(root.ID, "gcg-") {
		t.Fatalf("wisp root id = %q, want the graph binding's %q prefix", root.ID, "gcg-")
	}
	for _, b := range allBeads(t, workStore) {
		if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow {
			t.Fatalf("wisp root %s was born in the work ledger; the constructor did not carry the storage routes to dispatchWisp", b.ID)
		}
	}
}

// TestOrderDispatcherServesTheBootResolvedBinding pins the three production
// wiring sites that hand a live CityRuntime's opened binding to its order
// dispatcher: boot, the order rescan, and a config reload.
//
// Pointer identity is the assertion, and it is the point: the dispatcher must
// serve the binding this process OPENED AT BOOT, not a second resolution of the
// same config. Storage handles are immutable for the life of a process — a
// reload that changes [storage] is refused rather than applied — so a dispatcher
// holding anything other than cr.storageRoutes is holding a handle to a database
// nothing else in the process is reading.
func TestOrderDispatcherServesTheBootResolvedBinding(t *testing.T) {
	cr, tomlPath, _ := bootSplitCityForReloadWithOrder(t)

	assertDispatcherRoutes := func(stage string) {
		t.Helper()
		if cr.od == nil {
			t.Fatalf("%s: the split city has no order dispatcher, so this test asserts nothing", stage)
		}
		m, ok := cr.od.(*memoryOrderDispatcher)
		if !ok {
			t.Fatalf("%s: cr.od is %T, want *memoryOrderDispatcher", stage, cr.od)
		}
		if m.storageRoutes != cr.storageRoutes {
			t.Fatalf("%s: dispatcher routes = %p, want the runtime's boot-resolved routes %p; every wisp it dispatches is born in the work ledger and lands stranded off the binding",
				stage, m.storageRoutes, cr.storageRoutes)
		}
	}

	assertDispatcherRoutes("boot")

	// The rescan rebuilds the dispatcher only when the order SET changed, so
	// add an order rather than rescanning an unchanged one.
	writeCityOrder(t, cr.cityPath, "sweeper")
	changed, _, err := cr.rescanOrderDispatcher(context.Background(), cr.cityPath, cr.cfg, "test: order scan", time.Now())
	if err != nil {
		t.Fatalf("rescanOrderDispatcher: %v", err)
	}
	if !changed {
		t.Fatal("the rescan reported no change, so it never rebuilt the dispatcher and this stage asserts nothing")
	}
	assertDispatcherRoutes("after rescanOrderDispatcher")

	// A reload that leaves [storage] alone but changes something else: the
	// dispatcher is rebuilt, and it must be rebuilt over the same binding.
	writeSplitCityConfig(t, tomlPath, cr.cfg.Storage.Bindings["infra"].Path, "\n[daemon]\nshutdown_timeout = \"7s\"\n")
	lastProviderName := "fake"
	if reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cr.cityPath, nil, reloadSourceManual); reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reload failed, so the rebuilt dispatcher proves nothing: %s", reply.Error)
	}
	assertDispatcherRoutes("after reloadConfigTraced")
}

// TestWebhookOrderDispatcherServesTheBootResolvedBinding is the fourth wiring
// site. The webhook receiver builds its own detached dispatcher per delivery
// from controllerState rather than reusing cr.od, so it is a separate call to
// newMemoryOrderDispatcher with its own lock-scoped read of the routes — and a
// webhook-fired wisp has to land in the same graph store a tick-fired one does.
func TestWebhookOrderDispatcherServesTheBootResolvedBinding(t *testing.T) {
	graphStore := beads.NewMemStore()
	routes := messagingSplitRoutes(graphStore)
	cs := &controllerState{
		cityPath:      t.TempDir(),
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city"}},
		storageRoutes: routes,
	}

	md := controllerWebhookDispatcher{cs: cs}.dispatcher()
	if md.storageRoutes != routes {
		t.Fatalf("webhook dispatcher routes = %p, want the controller's %p; a webhook-fired wisp would be born in the work ledger", md.storageRoutes, routes)
	}
	if got := md.graphStoreFor(beads.NewMemStore()); got != beads.Store(graphStore) {
		t.Fatalf("webhook dispatcher graph store = %T(%p), want the binding %p", got, got, graphStore)
	}
}

// TestOrderWispForceCloseReachesTheGraphBinding pins the recovery half of the
// wisp move.
//
// This change puts order wisp roots in the graph binding and federates the
// single-flight gate over it, so a wisp whose agent died mid-run keeps
// hasOpenWorkStrict returning true and suppresses its order on every tick. The
// only gc-level force-close for that state is `gc order sweep-tracking
// --include-wisps`, and its root selection (staleOrderWispRoots, by the
// "order-run:<name>" label) runs against whatever store it is handed. Handed the
// work stores, it finds no root, reports wispClosed: 0, exits 0 — and the order
// stays wedged with no documented recovery but raw bd against the binding.
func TestOrderWispForceCloseReachesTheGraphBinding(t *testing.T) {
	workStore := beads.NewMemStore()
	graphStore := beads.NewMemStore()
	graphStore.IDPrefix = "gcg"

	root, err := graphStore.Create(beads.Bead{
		Title:    "reaper wisp",
		Type:     "molecule",
		Labels:   []string{"order-run:reaper"},
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create wisp root: %v", err)
	}
	child, err := graphStore.Create(beads.Bead{
		Title:    "reaper step",
		Type:     "task",
		ParentID: root.ID,
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("create wisp step: %v", err)
	}

	onlyOrders := orderFilterForTest("reaper")
	// Same shape the existing wisp-sweep tests use: push "now" past the fixture
	// so the whole open subtree sits before the cutoff.
	now := root.CreatedAt.Add(2 * time.Hour)

	// The regression: sweeping only the work stores is a silent no-op.
	blind, err := sweepStaleOrderTrackingAcrossStores([]beads.Store{workStore}, nil, now, time.Hour, onlyOrders, true)
	if err != nil {
		t.Fatalf("work-store-only sweep: %v", err)
	}
	if blind.wispClosed != 0 {
		t.Fatalf("work-store-only sweep closed %d wisp bead(s); the fixture no longer models a graph-resident wisp", blind.wispClosed)
	}
	if got := beadByIDForTest(t, graphStore, root.ID); got.Status == "closed" {
		t.Fatal("the work-store-only sweep closed the graph-resident root; the fixture is wrong")
	}

	result, err := sweepStaleOrderTrackingAcrossStores([]beads.Store{workStore}, graphStore, now, time.Hour, onlyOrders, true)
	if err != nil {
		t.Fatalf("graph-routed sweep: %v", err)
	}
	if result.wispClosed == 0 {
		t.Fatal("gc order sweep-tracking --include-wisps closed no wisp beads in the graph binding; a stalled graph-resident wisp has no gc-level recovery and its order stays suppressed forever")
	}
	for _, id := range []string{root.ID, child.ID} {
		if got := beadByIDForTest(t, graphStore, id); got.Status != "closed" {
			t.Fatalf("bead %s status = %q, want closed by the force-close", id, got.Status)
		}
	}
}

// TestOrderWispForceCloseStaysInTheOneStoreOnSingleStoreCity is the
// compatibility half: a city that relocates nothing resolves no separate wisp
// store, so the sweep keeps closing wisp subtrees in the same store as the
// tracking beads, once per scope, exactly as before.
func TestOrderWispForceCloseStaysInTheOneStoreOnSingleStoreCity(t *testing.T) {
	cityPath := t.TempDir()
	resetCLIStorageRoutes(t)
	if got := orderWispSweepStore(cityPath, &config.City{Workspace: config.Workspace{Name: "test-city"}}); got != nil {
		t.Fatalf("orderWispSweepStore = %T(%p) for a city with no [storage]; want nil so the sweep stays on the store it always used", got, got)
	}

	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:    "reaper wisp",
		Type:     "molecule",
		Labels:   []string{"order-run:reaper"},
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create wisp root: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "reaper step",
		Type:     "task",
		ParentID: root.ID,
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	}); err != nil {
		t.Fatalf("create wisp step: %v", err)
	}
	result, err := sweepStaleOrderTrackingAcrossStores([]beads.Store{store}, nil, root.CreatedAt.Add(2*time.Hour), time.Hour, orderFilterForTest("reaper"), true)
	if err != nil {
		t.Fatalf("single-store sweep: %v", err)
	}
	if result.wispClosed == 0 {
		t.Fatal("single-store sweep closed no wisp beads; the per-store wisp half was lost")
	}
	if got := beadByIDForTest(t, store, root.ID); got.Status != "closed" {
		t.Fatalf("wisp root status = %q, want closed", got.Status)
	}
}

// TestOrderWispForceCloseSweepsBothClassesOnASplitCity is the other half of the
// same recovery, and the two halves are not exclusive.
//
// Routing the force-close at the graph binding is only correct if it does not
// stop looking in the work stores. A converged split city holds wisp roots in
// both: every subtree born before cutover stayed in the work ledger, and
// `gc order run` still mints its root through the unrouted scope store. Those
// roots keep hasOpenWorkStrict returning true — the gate federates over both
// stores — so a sweep that trades the work half for the graph half leaves them
// suppressing their order with the same no-gc-level-recovery wedge the graph
// half was written to close, only with the stores swapped.
//
// Both roots are also asserted as a count, not just as a status, because the
// hoisted binding sweep must stay outside the per-scope loop: a work-half fix
// that re-swept the binding once per scope would still turn both of these
// closed while multiplying wispClosed.
func TestOrderWispForceCloseSweepsBothClassesOnASplitCity(t *testing.T) {
	workStore := beads.NewMemStore()
	graphStore := beads.NewMemStore()
	graphStore.IDPrefix = "gcg"

	workRoot, workChild := seedOrderWispSubtreeForTest(t, workStore, "reaper")
	graphRoot, graphChild := seedOrderWispSubtreeForTest(t, graphStore, "reaper")

	now := workRoot.CreatedAt.Add(2 * time.Hour)
	result, err := sweepStaleOrderTrackingAcrossStores([]beads.Store{workStore}, graphStore, now, time.Hour, orderFilterForTest("reaper"), true)
	if err != nil {
		t.Fatalf("split-city sweep: %v", err)
	}

	for _, id := range []string{workRoot.ID, workChild.ID} {
		if got := beadByIDForTest(t, workStore, id); got.Status != "closed" {
			t.Fatalf("work-resident wisp bead %s status = %q, want closed; --include-wisps stopped sweeping the work stores, so a work-resident wisp root has no gc-level force-close and keeps its order suppressed on every tick", id, got.Status)
		}
	}
	for _, id := range []string{graphRoot.ID, graphChild.ID} {
		if got := beadByIDForTest(t, graphStore, id); got.Status != "closed" {
			t.Fatalf("graph-resident wisp bead %s status = %q, want closed", id, got.Status)
		}
	}
	if result.wispClosed != 4 {
		t.Fatalf("wispClosed = %d, want 4 (both subtrees, each closed once)", result.wispClosed)
	}
}

// TestOrderWispForceCloseCountsAStoreServingBothClassesOnce pins the reason the
// binding sweep is hoisted out of the per-scope loop: a city whose graph binding
// IS one of the swept stores must report its subtree once, not twice. Dry-run is
// the mode that can actually double-count — the live path skips an
// already-closed root on the second pass and would hide the double sweep.
func TestOrderWispForceCloseCountsAStoreServingBothClassesOnce(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := seedOrderWispSubtreeForTest(t, store, "reaper")

	now := root.CreatedAt.Add(2 * time.Hour)
	result, err := sweepStaleOrderTrackingAcrossStoresDryRun([]beads.Store{store}, store, now, time.Hour, orderFilterForTest("reaper"), true)
	if err != nil {
		t.Fatalf("collapsed-class dry-run sweep: %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("dry-run wispClosed = %d, want 2; one store serving both classes was swept twice and the operator is told twice as much work is stale as there is", result.wispClosed)
	}
}

// seedOrderWispSubtreeForTest creates the open wisp root and step the stale-wisp
// force-close selects on: the "order-run:<name>" label plus gc.kind=workflow is
// what staleOrderWispRoots matches.
func seedOrderWispSubtreeForTest(t *testing.T, store beads.Store, order string) (beads.Bead, beads.Bead) {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title:    order + " wisp",
		Type:     "molecule",
		Labels:   []string{"order-run:" + order},
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create wisp root: %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:    order + " step",
		Type:     "task",
		ParentID: root.ID,
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("create wisp step: %v", err)
	}
	return root, child
}

func beadByIDForTest(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}

// bootSplitCityForReloadWithOrder boots a split city that actually has an order,
// so its runtime has a real order dispatcher to assert on. Orders are discovered
// from <city>/orders/<name>.toml, not from city.toml, so the order file is
// written before the runtime boots and survives the reloads below.
func bootSplitCityForReloadWithOrder(t *testing.T) (*CityRuntime, string, *bytes.Buffer) {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeSplitCityConfig(t, tomlPath, filepath.Join(t.TempDir(), "store"), "")

	writeCityOrder(t, cityPath, "reaper")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	stubInfraMigrationSource(t)

	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if cr.storageRoutes == nil {
		t.Fatal("the split city opened no storage routes, so nothing below is about a binding")
	}
	return cr, tomlPath, &stderr
}

// writeCityOrder drops a city-scoped cron order on disk. Orders are discovered
// from <city>/orders/<name>.toml, so adding one is what makes an order rescan
// report a changed set and rebuild the dispatcher.
func writeCityOrder(t *testing.T, cityPath, name string) {
	t.Helper()
	ordersDir := filepath.Join(cityPath, "orders")
	if err := os.MkdirAll(ordersDir, 0o755); err != nil {
		t.Fatalf("mkdir orders: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ordersDir, name+".toml"), []byte(`
[order]
exec = "true"
trigger = "cron"
schedule = "*/1 * * * *"
`), 0o644); err != nil {
		t.Fatalf("write order %s: %v", name, err)
	}
}

// TestOrderTrackingSweepResolverPairsTheWispStoreWithTheTrackingStores pins
// what `gc order sweep-tracking` is handed, which is the other half of the
// force-close fix: the sweep can only reach the graph binding if its resolver
// hands the binding over in the first place.
func TestOrderTrackingSweepResolverPairsTheWispStoreWithTheTrackingStores(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	t.Run("split city resolves the graph binding", func(t *testing.T) {
		cityPath := t.TempDir()
		graphStore := beads.NewMemStore()
		resetCLIStorageRoutes(t)
		entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
		entry.once.Do(func() { entry.routes = messagingSplitRoutes(graphStore) })

		_, wispStore, _ := orderTrackingSweepStoresForConfigTargets(cityPath, cfg, nil)
		if wispStore != beads.Store(graphStore) {
			t.Fatalf("wisp sweep store = %T(%p), want the graph binding %p; --include-wisps would report wispClosed: 0 and exit 0", wispStore, wispStore, graphStore)
		}
	})

	t.Run("single-store city resolves none", func(t *testing.T) {
		cityPath := t.TempDir()
		resetCLIStorageRoutes(t)

		_, wispStore, _ := orderTrackingSweepStoresForConfigTargets(cityPath, cfg, nil)
		if wispStore != nil {
			t.Fatalf("wisp sweep store = %T(%p) for a city with no [storage]; want nil so the sweep stays on the store it always used", wispStore, wispStore)
		}
	})
}

// newPouredOrderFixture is newGraphOrderFixture's v1 twin: an order whose
// formula is a legacy POURED molecule. Its root is a molecule container and its
// steps are plain assigned tasks — no gc.kind, no gc.root_bead_id — so
// coordclass.Classify calls every bead it materializes ClassWork.
func newPouredOrderFixture(t *testing.T) (string, *config.City, orders.Order) {
	t.Helper()
	cityPath := t.TempDir()
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "poured.toml"), []byte(`
formula = "poured"
version = 1
pour = true

[[steps]]
id = "sweep"
title = "Poured sweep"
assignee = "worker"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	maxOne, maxTwo := 1, 2
	cfg := &config.City{
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: &maxTwo},
			{Name: config.ControlDispatcherAgentName, MaxActiveSessions: &maxOne},
		},
	}
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	a := orders.Order{
		Name:         "poured",
		Formula:      "poured",
		Trigger:      "cooldown",
		Interval:     "15m",
		FormulaLayer: formulaDir,
	}
	return cityPath, cfg, a
}

// TestOrderDispatchPouredV1MoleculeRelocatesWithTheGraphClass pins a KNOWN GAP
// rather than a desirable behavior — ga-fk1a5.
//
// dispatchWisp sends every order molecule to the graph binding. That is right
// for a graph.v2 pour and a root-only wisp, but a v1 POURED order formula
// compiles to a molecule container plus plain assigned task steps, and
// coordclass.Classify calls all of them ClassWork. Relocated, the step assigned
// to `worker` sits in a binding `gc hook` never scans, so the dispatch produces
// work nobody can pick up. The CLI twin (doOrderRunWithJSON) is gated on
// moleculeClassStore and is correct; the dispatcher is not, because this root is
// a TWO-CLASS object: it also carries the order:/seq: event-cursor labels, and
// orders.Store unions order-run evidence across the ORDERS and GRAPH legs only.
// Routing by class alone would fix the steps and lose the cursor. Closing it
// means moving the cursor onto the orders-class tracking bead first.
//
// When ga-fk1a5 lands, this test flips: the molecule moves to workStore and the
// assertions become the ones its CLI twin already makes
// (TestOrderRunPouredV1MoleculeStaysOnTheWorkStoreOnSplitCity).
func TestOrderDispatchPouredV1MoleculeRelocatesWithTheGraphClass(t *testing.T) {
	cityPath, cfg, a := newPouredOrderFixture(t)
	workStore := beads.NewMemStore()
	workStore.IDPrefix = "mc"
	graphStore := beads.NewMemStore()
	graphStore.IDPrefix = "gcg"

	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa:                   []orders.Order{a},
		storeFn:              func(execStoreTarget) (beads.Store, error) { return workStore, nil },
		storageRoutes:        messagingSplitRoutes(graphStore),
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	dispatchGraphOrder(t, m, cityPath)

	if rec.hasType(events.OrderFailed) || !rec.hasType(events.OrderCompleted) {
		t.Fatalf("events = %+v, want completed without failure", rec.events)
	}
	assigned := false
	for _, b := range allBeads(t, graphStore) {
		if b.Assignee == "worker" {
			assigned = true
		}
	}
	if !assigned {
		t.Fatalf("no step assigned to `worker` in the graph binding; if it moved to the work store, ga-fk1a5 has landed — flip this test to the assertions its CLI twin makes")
	}
	for _, b := range allBeads(t, workStore) {
		if b.Assignee == "worker" {
			t.Fatalf("a work-class order step reached the work store; ga-fk1a5 has landed — flip this test")
		}
	}
	t.Logf("KNOWN GAP (ga-fk1a5): a v1 poured order molecule is materialized in the graph binding, where `gc hook`'s work-scope query cannot see its assigned steps")
}
