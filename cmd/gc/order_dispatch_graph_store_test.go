package main

import (
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
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/orders"
)

// splitCityStorageRoutes builds the routes a converged split city runs on: work
// keeps its own ledger and every infrastructure class — graph included — is
// served by one shared binding. That is the arrangement storageSplitWhole names
// and openStorageRoutes produces, so a dispatcher handed these routes resolves
// classes exactly as it does after a real cutover.
func splitCityStorageRoutes(infra beads.Store) *storageRoutes {
	routes := &storageRoutes{stores: make(map[coordclass.Class]beads.Store), binding: "infra"}
	for _, class := range coordclass.Classes() {
		if class.IsInfrastructure() {
			routes.stores[class] = infra
		}
	}
	return routes
}

// newGraphOrderFixture writes a city-scoped graph.v2 order whose single worker
// step routes to a city agent, and returns the city path, config and order the
// dispatcher tests below fire.
func newGraphOrderFixture(t *testing.T) (string, *config.City, orders.Order) {
	t.Helper()
	formulatest.EnableV2ForTest(t)
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
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: &maxTwo},
			{Name: config.ControlDispatcherAgentName, MaxActiveSessions: &maxOne},
		},
	}
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
		storageRoutes:        splitCityStorageRoutes(graphStore),
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
	for _, b := range allBeads(t, graphStore) {
		if hasLabel(b.Labels, labelOrderTracking) {
			t.Fatalf("graph store holds order-tracking bead %s; order tracking stays on the order store", b.ID)
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
		storageRoutes:        splitCityStorageRoutes(graphStore),
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

	hasOpen, err := m.hasOpenWorkInStoresStrict([]beads.Store{workStore, graphStore}, "reaper")
	if err != nil {
		t.Fatalf("open-work gate: %v", err)
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
		storageRoutes:        splitCityStorageRoutes(graphStore),
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

	// The work leg stays the order's own store, so the two classes end the
	// dispatch in their own bindings rather than piled into one.
	tracking := trackingBeads(t, workStore, "order-run:reaper")
	if len(tracking) == 0 {
		t.Fatal("no order-run tracking bead in the work store")
	}
	if got := trackingBeads(t, graphStore, labelOrderTracking); len(got) != 0 {
		t.Fatalf("graph store holds order-tracking beads %+v, want none", got)
	}
}
