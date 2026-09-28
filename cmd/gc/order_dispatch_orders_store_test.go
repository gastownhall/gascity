package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
)

// The tests in this file cover the ORDERS coordination class the way
// order_dispatch_graph_store_test.go covers the graph class. coordclass defines
// ClassOrders as exactly "order-dispatch tracking beads (the order-tracking /
// order-run records that gate repeat order firing)", so on a city whose
// infrastructure classes are served by their own binding those beads belong in
// the binding — not in the work ledger the order's target scope resolves to,
// where the city's own convergence check reads them as stranded.
//
// The move is only safe as one change. The tracking bead is a lifecycle, not a
// record: it is created OPEN as the single-flight marker, stamped with an
// outcome, closed when the dispatch returns, swept when a controller dies
// holding it, and read by the gate, the cooldown clock, the CLI and the API.
// Moving the birth alone leaves every one of those addressing a database the
// bead is not in — the marker never clears and the order stops firing forever,
// which is strictly worse than the strand. So each half is pinned here.

// newExecOrderFixture writes a city-scoped exec order the dispatcher can fire
// without a formula layer, and returns the city path, config and order.
func newExecOrderFixture(t *testing.T) (string, *config.City, orders.Order) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := orders.Order{
		Name:     "dolt-health",
		Exec:     "true",
		Trigger:  "cooldown",
		Interval: "15m",
	}
	return cityPath, cfg, a
}

// newSplitOrderDispatcher builds a dispatcher for a city whose infrastructure
// classes are served by one binding, with the order's target scope on a separate
// work store — the converged whole-split shape openStorageRoutes produces.
func newSplitOrderDispatcher(t *testing.T, cityPath string, cfg *config.City, aa []orders.Order, workStore, binding beads.Store, rec events.Recorder) *memoryOrderDispatcher {
	t.Helper()
	dispatchCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &memoryOrderDispatcher{
		aa:                   aa,
		storeFn:              func(execStoreTarget) (beads.Store, error) { return workStore, nil },
		storageRoutes:        messagingSplitRoutes(binding),
		execRun:              func(context.Context, string, string, []string) ([]byte, error) { return nil, nil },
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
}

// dispatchOrderTick fires one tick and waits for the dispatch goroutine to
// persist its outcome.
func dispatchOrderTick(t *testing.T, m *memoryOrderDispatcher, cityPath string) {
	t.Helper()
	m.dispatch(context.Background(), cityPath, time.Now())
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if !m.drain(drainCtx) {
		t.Fatal("order dispatch did not drain")
	}
}

// onlyTrackingBead returns the single order-tracking bead a store holds, failing
// when the count is anything but one.
func onlyTrackingBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	found := trackingBeads(t, store, labelOrderTracking)
	if len(found) != 1 {
		t.Fatalf("order-tracking beads = %+v, want exactly one", found)
	}
	return found[0]
}

// TestOrderDispatchTrackingBeadLandsInTheOrdersBinding is the producer half.
//
// This is the production incident: on maintainer-city the tracking beads a
// running controller wrote kept accruing in the work ledger at ~42/h, every one
// of them an infrastructure-class bead outside its binding, and the city's own
// convergence check reads that as stranded — which is fatal to boot.
func TestOrderDispatchTrackingBeadLandsInTheOrdersBinding(t *testing.T) {
	cityPath, cfg, a := newExecOrderFixture(t)
	workStore := beads.NewMemStore()
	workStore.IDPrefix = "mc"
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"

	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, []orders.Order{a}, workStore, binding, &rec)
	dispatchOrderTick(t, m, cityPath)

	tracking := onlyTrackingBead(t, binding)
	if !hasLabel(tracking.Labels, "order-run:dolt-health") {
		t.Fatalf("tracking bead labels = %v, want order-run:dolt-health", tracking.Labels)
	}
	for _, b := range allBeads(t, workStore) {
		if hasLabel(b.Labels, labelOrderTracking) {
			t.Fatalf("work store holds order-tracking bead %s (%s); orders-class beads outside their binding are what the convergence check reports as stranded, and stranded is fatal to boot", b.ID, b.Title)
		}
	}
}

// TestOrderDispatchClearsTheTrackingBeadItCreated is the lifecycle half, and it
// is the reason the move cannot be split.
//
// The tracking bead is created OPEN as the single-flight marker. dispatchOne's
// deferred close and the outcome stamp are what release it. If the birth moves
// to the binding and those writes keep addressing the target scope, the close
// finds nothing, the marker stays open forever, and the open-work gate
// suppresses the order on every subsequent tick — permanently.
func TestOrderDispatchClearsTheTrackingBeadItCreated(t *testing.T) {
	cityPath, cfg, a := newExecOrderFixture(t)
	workStore := beads.NewMemStore()
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"

	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, []orders.Order{a}, workStore, binding, &rec)
	dispatchOrderTick(t, m, cityPath)

	tracking := onlyTrackingBead(t, binding)
	if tracking.Status != "closed" {
		t.Fatalf("tracking bead %s status = %q, want closed; an open marker in a store the close never addresses suppresses this order on every later tick, forever", tracking.ID, tracking.Status)
	}
	run, ok := orders.RunFromTrackingBead(tracking)
	if !ok || run.Outcome != orders.RunOutcomeExec {
		t.Fatalf("tracking bead labels = %v, want the exec outcome stamped on the bead the dispatch created", tracking.Labels)
	}
}

// TestOrderDispatchWispOutcomeLandsOnTheOrdersResidentBead is the same lifecycle
// property for the formula arm, which stamps its outcome from dispatchWisp
// rather than dispatchExec — a separate call site, and separately revertible.
func TestOrderDispatchWispOutcomeLandsOnTheOrdersResidentBead(t *testing.T) {
	cityPath, cfg, a := newGraphOrderFixture(t)
	workStore := beads.NewMemStore()
	workStore.IDPrefix = "mc"
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"

	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, []orders.Order{a}, workStore, binding, &rec)
	dispatchOrderTick(t, m, cityPath)

	if rec.hasType(events.OrderFailed) || !rec.hasType(events.OrderCompleted) {
		t.Fatalf("events = %+v, want completed without failure", rec.events)
	}
	tracking := onlyTrackingBead(t, binding)
	run, ok := orders.RunFromTrackingBead(tracking)
	if !ok || run.Outcome != orders.RunOutcomeWisp {
		t.Fatalf("tracking bead labels = %v, want the wisp outcome stamped in the binding", tracking.Labels)
	}
	if tracking.Status != "closed" {
		t.Fatalf("tracking bead %s status = %q, want closed", tracking.ID, tracking.Status)
	}
}

// TestOrderDispatchTriggerEnvFailureTrackingBeadLandsInTheOrdersBinding pins the
// third creation site: the pre-dispatch trigger-env failure deliberately leaves
// an OPEN tracking bead so the gate suppresses repeat ticks until the stale
// sweep retries. That bead is the most damaging one to misplace — it is designed
// to stay open, so a sweep that cannot see it never gives the order another try.
func TestOrderDispatchTriggerEnvFailureTrackingBeadLandsInTheOrdersBinding(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	// A scope whose beads metadata names a Postgres backend with no reachable
	// password: building the trigger env fails before dispatch, which is the arm
	// that mints an open tracking bead of its own.
	cityPath := t.TempDir()
	writeUnregisteredBackendMetadata(t, cityPath)
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := orders.Order{Name: "pg-condition", Exec: "true", Trigger: "condition", Check: "true"}

	workStore := beads.NewMemStore()
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"

	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, []orders.Order{a}, workStore, binding, &rec)
	m.dispatch(context.Background(), cityPath, time.Now())

	tracking := onlyTrackingBead(t, binding)
	run, ok := orders.RunFromTrackingBead(tracking)
	if !ok || run.Outcome != orders.RunOutcomeTriggerEnvFailed {
		t.Fatalf("tracking bead labels = %v, want the trigger-env-failed marker in the binding", tracking.Labels)
	}
	if !run.Open {
		t.Fatalf("tracking bead %s is closed; the trigger-env-failure marker is deliberately left open", tracking.ID)
	}
	for _, b := range allBeads(t, workStore) {
		if hasLabel(b.Labels, labelOrderTracking) {
			t.Fatalf("work store holds order-tracking bead %s; the trigger-env-failure marker is orders class too", b.ID)
		}
	}
}

// TestWebhookOrderDispatchTrackingBeadLandsInTheOrdersBinding covers the seam a
// webhook delivery fires through. It builds its own store handle per delivery
// and calls launchResolvedDispatch directly, so it is a second entry into the
// same creation path and would keep writing to the work ledger if the routing
// lived at the tick loop's call site instead of inside the shared core.
func TestWebhookOrderDispatchTrackingBeadLandsInTheOrdersBinding(t *testing.T) {
	cityPath, cfg, a := newExecOrderFixture(t)
	workStore := beads.NewMemStore()
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"

	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, nil, workStore, binding, &rec)

	result, err := m.Dispatch(context.Background(), orderdispatch.DispatchRequest{Order: a})
	if err != nil {
		t.Fatalf("webhook dispatch: %v", err)
	}
	if !result.Fired {
		t.Fatalf("webhook dispatch result = %+v, want fired", result)
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if !m.drain(drainCtx) {
		t.Fatal("webhook dispatch did not drain")
	}

	tracking := onlyTrackingBead(t, binding)
	if tracking.ID != result.TrackingID {
		t.Fatalf("binding holds tracking bead %s, want the dispatch's own %s", tracking.ID, result.TrackingID)
	}
	for _, b := range allBeads(t, workStore) {
		if hasLabel(b.Labels, labelOrderTracking) {
			t.Fatalf("work store holds order-tracking bead %s from a webhook dispatch", b.ID)
		}
	}
}

// TestOrderDispatchGateReadsTheBindingAloneOnASplitCity pins the read half of
// the single-flight gate.
//
// The gate, the cooldown clock and the event cursor all read order-run evidence,
// and a dispatch writes that evidence through ordersStoreFor/graphStoreFor — so
// on a split city it is in the binding. The federation must therefore include
// the binding, ONCE (the whole-split shape serves both classes from one
// database, and appending it twice reads it twice per order per tick) — and must
// include NOTHING ELSE: the order's target scope is a work ledger that cannot
// hold the answer, and reading it per order per tick was most of the 86s
// dispatch_orders leg on maintainer-city (ga-l7jdg; bd memory
// gascity-runtime-infra-store-invariant).
func TestOrderDispatchGateReadsTheBindingAloneOnASplitCity(t *testing.T) {
	cityPath, cfg, a := newExecOrderFixture(t)
	workStore := beads.NewMemStore()
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"

	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, []orders.Order{a}, workStore, binding, &rec)

	if got := m.ordersStoreFor(workStore); got != beads.Store(binding) {
		t.Fatalf("ordersStoreFor = %T(%p), want the binding %p; the tracking bead's own store is not in the gate's reach", got, got, binding)
	}

	storesForGate, storeKeysForGate := m.gateStoresFor(cityPath, workStore, "city\x00"+cityPath, nil)
	if !storeListContains(storesForGate, beads.Store(binding)) {
		t.Fatalf("gate store list = %+v, missing the binding the tracking bead lives in; the marker the dispatch just wrote is invisible and the order re-fires every tick", storesForGate)
	}
	if len(storesForGate) != 1 {
		t.Fatalf("gate store list = %+v, want exactly the one binding serving both classes; a work-ledger leg on the runtime plane is a misrouting bug and the whole-split binding must not be read twice", storesForGate)
	}
	if storeListContains(storesForGate, beads.Store(workStore)) {
		t.Fatal("the work ledger is still a gate leg; it cannot hold order-run evidence on a split city and reading it costs one remote round trip per order per tick")
	}
	if len(storeKeysForGate) != len(storesForGate) {
		t.Fatalf("gate store keys = %d for %d stores; the cooldown cache key and the store it memoizes have drifted apart", len(storeKeysForGate), len(storesForGate))
	}
}

// TestOrderDispatchGateStaysOnTheOneStoreOnSingleStoreCity is the byte-identity
// half: a city that relocates nothing gates on exactly the list it always did.
func TestOrderDispatchGateStaysOnTheOneStoreOnSingleStoreCity(t *testing.T) {
	cityPath, cfg, a := newExecOrderFixture(t)
	store := beads.NewMemStore()

	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa:             []orders.Order{a},
		storeFn:        func(execStoreTarget) (beads.Store, error) { return store, nil },
		storageRoutes:  nil,
		cfg:            cfg,
		cityPath:       cityPath,
		stderr:         io.Discard,
		dispatchCtx:    dispatchCtx,
		dispatchCancel: cancel,
	}
	storesForGate, storeKeysForGate := m.gateStoresFor(cityPath, store, "city\x00"+cityPath, nil)
	if len(storesForGate) != 1 || storesForGate[0] != beads.Store(store) {
		t.Fatalf("gate store list = %+v, want exactly the one store the city has", storesForGate)
	}
	if len(storeKeysForGate) != 1 || storeKeysForGate[0] != "city\x00"+cityPath {
		t.Fatalf("gate store keys = %+v, want exactly the target scope key; a changed key set silently invalidates the cooldown cache", storeKeysForGate)
	}
}

// TestOrderDispatchTrackingStaysOnTheOneStoreOnSingleStoreCity is the
// compatibility guarantee, and it is green before and after this change by
// design: a city that relocates nothing routes nothing. resolveOrderStore
// returns the exact store value it was handed, so the dispatch writes the
// tracking bead through the same instance it always did — not a re-wrapped one,
// which would drop the optional-capability assertions the store path makes.
func TestOrderDispatchTrackingStaysOnTheOneStoreOnSingleStoreCity(t *testing.T) {
	cityPath, cfg, a := newExecOrderFixture(t)
	store := beads.NewMemStore()

	var rec memRecorder
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa:                   []orders.Order{a},
		storeFn:              func(execStoreTarget) (beads.Store, error) { return store, nil },
		storageRoutes:        nil, // no [storage] section: every class is the work store
		execRun:              func(context.Context, string, string, []string) ([]byte, error) { return nil, nil },
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             cityPath,
		rec:                  &rec,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}
	if got := m.ordersStoreFor(store); got != beads.Store(store) {
		t.Fatalf("ordersStoreFor returned %T(%p), want the identical store value %p", got, got, store)
	}
	dispatchOrderTick(t, m, cityPath)

	tracking := onlyTrackingBead(t, store)
	if tracking.Status != "closed" {
		t.Fatalf("tracking bead %s status = %q, want closed", tracking.ID, tracking.Status)
	}
}

// TestBootOrphanSweepReachesTheOrdersBinding pins the recovery the production
// incident actually blocks: maintainer-city could not be restarted without a
// manual prune. A tracking bead left open by a killed controller is a permanent
// single-flight marker, and the boot sweep is what clears it. Once the beads are
// born in the binding, a sweep that only opens the city work store closes
// nothing and every order that was mid-dispatch at the kill stays suppressed.
func TestBootOrphanSweepReachesTheOrdersBinding(t *testing.T) {
	cityPath := t.TempDir()
	workStore := beads.NewMemStore()
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"

	prev := newCityRuntimeOpenSweepStore
	newCityRuntimeOpenSweepStore = func(string, string) (beads.Store, error) { return workStore, nil }
	t.Cleanup(func() { newCityRuntimeOpenSweepStore = prev })

	orphans := map[string]beads.Store{"binding": binding, "work store": workStore}
	seeded := make(map[string]string, len(orphans))
	for name, store := range orphans {
		bead, err := store.Create(beads.Bead{
			Title:  "order:dolt-health",
			Labels: []string{"order-run:dolt-health", labelOrderTracking},
		})
		if err != nil {
			t.Fatalf("seeding the %s orphan: %v", name, err)
		}
		seeded[name] = bead.ID
	}

	sweepOrphanedOrderTrackingAtBoot(messagingSplitRoutes(binding), cityPath, &config.City{Workspace: config.Workspace{Name: "test-city"}}, events.Discard, io.Discard)

	for name, store := range orphans {
		got, err := store.Get(seeded[name])
		if err != nil {
			t.Fatalf("reading the %s orphan back: %v", name, err)
		}
		if got.Status != "closed" {
			t.Fatalf("the %s orphan %s is still open after the boot sweep; its order never fires again and the city cannot be restarted without a manual prune", name, got.ID)
		}
	}
}

// TestOrderTrackingWatchdogsReachTheOrdersBinding pins the two controller
// watchdogs that keep tracking beads from accumulating: the stale-close sweep
// that recovers the #2168 jam, and the retention prune that bounds closed
// history. Both resolve their stores from the runtime, so both go blind on a
// split city if the resolution stays on the scope stores.
func TestOrderTrackingWatchdogsReachTheOrdersBinding(t *testing.T) {
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"
	cityStore := beads.NewMemStore()

	stale, err := binding.Create(beads.Bead{
		Title:  "order:dolt-health",
		Labels: []string{"order-run:dolt-health", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("seeding the stale tracking bead: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "test-city",
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		storageRoutes:       messagingSplitRoutes(binding),
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{},
		stdout:              io.Discard,
		stderr:              io.Discard,
		logPrefix:           "gc test",
	}

	if got := cr.relocatedOrdersStore(); got != beads.Store(binding) {
		t.Fatalf("relocatedOrdersStore = %T(%p), want the binding %p", got, got, binding)
	}

	now := stale.CreatedAt.Add(orderTrackingSweepWatchdogStaleAfter + time.Millisecond)
	cr.runOrderTrackingSweepWatchdog(now)

	closed, err := binding.Get(stale.ID)
	if err != nil {
		t.Fatalf("reading the stale tracking bead back: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("stale tracking bead %s status = %q, want closed; the watchdog cannot see the binding, so the tracking jam (#2168) has no recovery on a split city", closed.ID, closed.Status)
	}

	// The retention watchdog prunes closed tracking history through the same
	// resolver, so the binding has to be in the list it is handed. Asserted on
	// the resolver rather than by running the prune, because the prune's own
	// gates (backup freshness, the retain-last floor) are policy this change
	// does not touch.
	stores, _, closeOpened, err := cr.orderTrackingSweepStores()
	defer closeOpened()
	if err != nil {
		t.Fatalf("resolving the watchdog sweep stores: %v", err)
	}
	var reachesBinding bool
	for _, store := range stores {
		if unwrapOrderTrackingSweepStore(store) == beads.Store(binding) {
			reachesBinding = true
		}
	}
	if !reachesBinding {
		t.Fatalf("watchdog sweep stores = %d, none of them the binding; the retention watchdog never prunes a split city's closed tracking history", len(stores))
	}
}

// TestOneShotOrderReadsReachTheOrdersBinding pins the CLI read federations.
// `gc order check` decides whether an order is due from its last run, and
// `gc order history` lists those runs; both resolve per-order stores from the
// order's target scope. On a split city every run they are being asked about is
// in the binding, so a resolver that stops at the scope stores reports a city
// whose orders fire every few minutes as never having run.
func TestOneShotOrderReadsReachTheOrdersBinding(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"
	resetCLIStorageRoutes(t)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() { entry.routes = messagingSplitRoutes(binding) })

	a := orders.Order{Name: "dolt-health", Exec: "true", Trigger: "cooldown", Interval: "15m"}
	run, err := binding.Create(beads.Bead{
		Title:  "order:dolt-health",
		Labels: []string{"order-run:dolt-health", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("seeding the run: %v", err)
	}

	for name, resolve := range map[string]orderStoresResolver{
		"gc order check":   cachedOrderStoresResolver(cityPath, cfg),
		"gc order history": cachedOrderHistoryStoresResolver(cityPath, cfg, io.Discard),
	} {
		stores, err := resolve(a)
		if err != nil {
			t.Fatalf("%s: resolving stores: %v", name, err)
		}
		last, err := orders.LastRunAcross(orderFrontDoorsForTypedStores(stores))(a.ScopedName())
		if err != nil {
			t.Fatalf("%s: reading last run: %v", name, err)
		}
		if last.IsZero() {
			t.Fatalf("%s: last run is zero though %s is in the binding; the order reads as never run and every cooldown/cron trigger fires again immediately", name, run.ID)
		}
	}
}

// TestOrderSweepTrackingReachesTheOrdersBinding pins the operator-facing
// recovery. `gc order sweep-tracking` is the documented way to clear a stuck
// tracking bead by hand; handed only the scope stores on a split city it closes
// nothing, reports zero and exits zero — a silent no-op at exactly the moment an
// operator is trying to unwedge a city.
func TestOrderSweepTrackingReachesTheOrdersBinding(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"
	resetCLIStorageRoutes(t)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() { entry.routes = messagingSplitRoutes(binding) })

	stale, err := binding.Create(beads.Bead{
		Title:  "order:dolt-health",
		Labels: []string{"order-run:dolt-health", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("seeding the stale tracking bead: %v", err)
	}

	stores, _, err := orderTrackingSweepStoresForConfigTargets(cityPath, cfg, nil)
	if err != nil {
		t.Fatalf("resolving sweep stores: %v", err)
	}
	result, err := sweepStaleOrderTrackingAcrossStores(stores, nil, stale.CreatedAt.Add(2*time.Hour), time.Hour, nil, false)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.trackingClosed == 0 {
		t.Fatal("gc order sweep-tracking closed nothing in the binding; the documented recovery for a stuck order is a silent no-op on a split city")
	}
	got, err := binding.Get(stale.ID)
	if err != nil {
		t.Fatalf("reading the stale bead back: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("stale tracking bead %s status = %q, want closed", got.ID, got.Status)
	}
}

// TestOneShotOrderFederationsStayOnTheOneStoreOnSingleStoreCity is the
// compatibility half of the one-shot changes: a city with no [storage] resolves
// no orders binding at all, so every federation keeps exactly the stores it had.
func TestOneShotOrderFederationsStayOnTheOneStoreOnSingleStoreCity(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	resetCLIStorageRoutes(t)

	if got := relocatedOrdersClassStore(cityPath, cfg); got != nil {
		t.Fatalf("relocatedOrdersClassStore = %T(%p) for a city with no [storage]; want nil so every federation stays exactly as it was", got, got)
	}

	scope := beads.NewMemStore()
	if got := appendOrdersClassStore([]beads.OrdersStore{{Store: scope}}, nil); len(got) != 1 || got[0].Store != beads.Store(scope) {
		t.Fatalf("typed federation = %+v, want the single scope store unchanged", got)
	}
	if got := appendOrdersSweepStore([]beads.Store{scope}, nil); len(got) != 1 || got[0] != beads.Store(scope) {
		t.Fatalf("sweep federation = %+v, want the single scope store unchanged", got)
	}
	if got := appendOrdersSweepStore([]beads.Store{scope}, scope); len(got) != 1 {
		t.Fatalf("sweep federation = %+v, want the store it already reads added no second time", got)
	}
}

// TestDoctorOrderTrackingRetentionCountsTheOrdersBinding pins the advisory an
// operator reads to decide whether retention is keeping up. It counts closed
// tracking beads, and on a split city every one of them is in the binding, so a
// work-store-only count reports a reassuring zero on exactly the city whose
// backlog is growing.
func TestDoctorOrderTrackingRetentionCountsTheOrdersBinding(t *testing.T) {
	cityPath := t.TempDir()
	binding := beads.NewMemStoreFrom(700, makeClosedOrderTrackingBeads(600), nil)
	resetCLIStorageRoutes(t)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() { entry.routes = messagingSplitRoutes(binding) })

	check := newOrderTrackingRetentionCheck(cityPath, func(string) (beads.Store, error) {
		return beads.NewMemStore(), nil
	})
	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s), want a warning: the binding holds 600 closed tracking beads and the advisory reports the work store's zero", res.Status, res.Message)
	}
}

// TestOrderRunRecordsItsTrackingBeadInTheOrdersBinding pins the manual arm.
// `gc order run` writes the tracking bead whose CreatedAt is the cooldown clock
// the next controller tick reads. Written to a store the tick does not consult,
// a manual run advances a clock nothing sees and the order re-fires immediately
// — the #3294 failure with the stores swapped.
func TestOrderRunRecordsItsTrackingBeadInTheOrdersBinding(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	scope := beads.NewMemStore()
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"
	resetCLIStorageRoutes(t)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() { entry.routes = messagingSplitRoutes(binding) })

	front := orderTrackingFrontDoor(cityPath, cfg, beads.OrdersStore{Store: scope})
	if _, err := front.CreateRun("dolt-health", orders.RunOpts{}); err != nil {
		t.Fatalf("recording the manual run: %v", err)
	}

	if got := trackingBeads(t, binding, labelOrderTracking); len(got) != 1 {
		t.Fatalf("binding holds %d order-tracking beads, want the manual run's own; a run recorded off the binding advances a cooldown clock nothing reads", len(got))
	}
	if got := trackingBeads(t, scope, labelOrderTracking); len(got) != 0 {
		t.Fatalf("scope store holds order-tracking beads %+v, want none", got)
	}
}

// TestOrderWispSweepCountsTheWrappedOrdersBindingOnce is the count-once
// invariant of sweepStaleOrderTrackingAcrossStoresLimitMode reached through the
// sweep's own store list. The orders binding enters that list wrapped in the
// scope wrapper carrying its label, so an identity check against the wrapper
// cannot see the database inside it and sweeps the binding a second time. On the
// shape this build serves — one binding for graph and orders — that is the same
// store as the hoisted wisp pass, and `gc order sweep-tracking --dry-run
// --include-wisps` reports twice as much stale work as there is.
func TestOrderWispSweepCountsTheWrappedOrdersBindingOnce(t *testing.T) {
	binding := beads.NewMemStore()
	root, _ := seedOrderWispSubtreeForTest(t, binding, "dolt-health")

	stores := appendOrdersSweepStore(nil, binding)
	if len(stores) != 1 {
		t.Fatalf("sweep stores = %d, want the orders binding alone", len(stores))
	}

	now := root.CreatedAt.Add(2 * time.Hour)
	result, err := sweepStaleOrderTrackingAcrossStoresDryRun(stores, binding, now, time.Hour, orderFilterForTest("dolt-health"), true)
	if err != nil {
		t.Fatalf("dry-run sweep across the wrapped binding: %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("dry-run wispClosed = %d, want 2; the wrapped binding was swept twice and the operator is told twice as much work is stale as there is", result.wispClosed)
	}
}
