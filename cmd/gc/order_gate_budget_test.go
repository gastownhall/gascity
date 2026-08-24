package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// The order-dispatch leg's read budget.
//
// dispatch_orders was 86.0s of a 373s maintainer-city tick (ga-l7jdg), and the
// shape was per-ORDER reads against a remote work ledger: the strict open-work
// gate called its live fallback for every due order on every gate store — the
// requireStrictFallback flag was hardcoded true — and the event cursor listed
// once per event order per store. At ~5.4s per remote-postgres query that
// multiplies straight into the tick.
//
// Two properties fix it, and both are counted here rather than timed:
//
//  1. gate reads scale with STORES, not with orders — one index read per gate
//     store per tick, shared by every order's gate; and
//  2. the runtime plane issues ZERO reads against the work ledger — the
//     order-run evidence a dispatch writes is orders class and graph class, and
//     on a split city both are the binding (bd memory
//     gascity-runtime-infra-store-invariant).

// orderGateBudgetCity builds a split city whose order-run evidence lives in the
// binding, with n always-due orders each already holding an open wisp — the
// steady shape where a due order is suppressed rather than fired, which is
// exactly when the strict gate runs.
func orderGateBudgetCity(t *testing.T, n int) (*memoryOrderDispatcher, *latencyStore, *latencyStore, []orders.Order, string) {
	t.Helper()
	cityPath, cfg, _ := newExecOrderFixture(t)
	ledgerBacking := beads.NewMemStore()
	bindingBacking := beads.NewMemStore()
	bindingBacking.IDPrefix = "gcg"
	bindingBacking.HonorExplicitIDs = true

	aa := make([]orders.Order, 0, n)
	closed := "closed"
	for i := range n {
		a := orders.Order{
			Name:     fmt.Sprintf("budget-order-%d", i),
			Exec:     "true",
			Trigger:  "cooldown",
			Interval: "1ms",
		}
		aa = append(aa, a)
		// A CLOSED tracking bead, so the order is in the cooldown history index
		// and the run clock is served without a per-order LastRun query. This is
		// the steady shape: an order that has run before and is due again.
		front := orders.NewStore(beads.OrdersStore{Store: bindingBacking})
		run, err := front.CreateRun(a.ScopedName(), orders.RunOpts{})
		if err != nil {
			t.Fatalf("seed tracking bead %d: %v", i, err)
		}
		if err := bindingBacking.Update(run.ID, beads.UpdateOpts{Status: &closed}); err != nil {
			t.Fatalf("close tracking bead %d: %v", i, err)
		}
		// An open root-only wisp carrying the order's order-run label: the gate
		// must see it and suppress the fire. Root-only means the verdict needs no
		// descendant read, so the count below is the INDEX cost and nothing else.
		if _, err := bindingBacking.Create(beads.Bead{
			ID:       fmt.Sprintf("gcg-budget-wisp-%d", i),
			Title:    "wisp",
			Labels:   []string{orders.RunLabel(a.ScopedName())},
			Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
		}); err != nil {
			t.Fatalf("seed wisp %d: %v", i, err)
		}
	}

	ledger := &latencyStore{Store: ledgerBacking}
	binding := &latencyStore{Store: bindingBacking}
	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, aa, ledger, binding, &rec)
	m.maxDispatchesPerTick = 0 // unbounded: every order must reach its gate
	return m, ledger, binding, aa, cityPath
}

// TestOrderGateReadsScaleWithStoresNotOrders is the budget: the gate's cost is a
// property of the city's store topology, never of how many orders it runs.
func TestOrderGateReadsScaleWithStoresNotOrders(t *testing.T) {
	counts := map[int]int{}
	for _, n := range []int{1, 10} {
		m, ledger, binding, _, cityPath := orderGateBudgetCity(t, n)
		m.dispatch(context.Background(), cityPath, time.Now().Add(time.Hour))
		drainOrderDispatch(t, m)
		if ledger.roundTrips != 0 {
			t.Fatalf("%d order(s): the gate issued %d work-ledger round trip(s), want 0", n, ledger.roundTrips)
		}
		counts[n] = binding.roundTrips
	}
	if counts[1] != counts[10] {
		t.Fatalf("the gate cost %d round trip(s) for 1 order and %d for 10; it scales with orders, so at maintainer-city's ~5.4s ledger RTT every added order is another %v of tick",
			counts[1], counts[10], 5400*time.Millisecond)
	}
	if counts[1] == 0 {
		t.Fatal("the gate issued no reads at all; the budget above is measuring a gate that stopped running")
	}
	// Control: the strict live fallback — the shape the index replaces — DOES
	// scale with orders over the identical corpus. Without this the invariance
	// above would also pass for a gate that had simply been deleted.
	m, _, binding, aa, _ := orderGateBudgetCity(t, 10)
	before := binding.roundTrips
	for _, a := range aa {
		if _, err := m.hasOpenWorkStrict(binding, a.ScopedName()); err != nil {
			t.Fatalf("strict fallback for %s: %v", a.ScopedName(), err)
		}
	}
	if got := binding.roundTrips - before; got <= counts[10] {
		t.Fatalf("the per-order strict fallback cost %d round trip(s) for 10 orders and the index cost %d; the budget is not measuring anything", got, counts[10])
	}
}

// TestOrderGateIssuesZeroLedgerReadsOnASplitCity is the operator invariant on
// this leg: a dispatch tick's gate, cooldown clock and event cursor read the
// infra binding and never the work ledger.
func TestOrderGateIssuesZeroLedgerReadsOnASplitCity(t *testing.T) {
	m, ledger, binding, _, cityPath := orderGateBudgetCity(t, 4)
	m.dispatch(context.Background(), cityPath, time.Now().Add(time.Hour))
	drainOrderDispatch(t, m)
	if ledger.roundTrips != 0 {
		t.Fatalf("the dispatch tick issued %d work-ledger round trip(s), want 0 — that is %v of tick at maintainer-city's RTT",
			ledger.roundTrips, time.Duration(ledger.roundTrips)*5400*time.Millisecond)
	}
	// Control: the binding answered instead, so the zero above is a routing fact
	// and not a tick that declined to gate.
	if binding.roundTrips == 0 {
		t.Fatal("the binding was not read either; the gate did not run and the ledger zero proves nothing")
	}
}

// TestOrderGateStillReadsTheLedgerOnASingleStoreCity is the degradation half: a
// city that relocates no class has no binding, and there the work store IS the
// infra store. Narrowing it away would disable the gate outright.
func TestOrderGateStillReadsTheLedgerOnASingleStoreCity(t *testing.T) {
	cityPath, cfg, a := newExecOrderFixture(t)
	backing := beads.NewMemStore()
	backing.HonorExplicitIDs = true
	if _, err := backing.Create(beads.Bead{
		ID:       "ga-budget-wisp",
		Title:    "wisp",
		Labels:   []string{orders.RunLabel(a.ScopedName())},
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
	}); err != nil {
		t.Fatalf("seed wisp: %v", err)
	}
	store := &latencyStore{Store: backing}
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &memoryOrderDispatcher{
		aa:             []orders.Order{a},
		storeFn:        func(execStoreTarget) (beads.Store, error) { return store, nil },
		storageRoutes:  nil,
		cfg:            cfg,
		cityName:       "test-city",
		cityPath:       cityPath,
		stderr:         io.Discard,
		rec:            &memRecorder{},
		execRun:        func(context.Context, string, string, []string) ([]byte, error) { return nil, nil },
		dispatchCtx:    dispatchCtx,
		dispatchCancel: cancel,
	}
	m.dispatch(context.Background(), cityPath, time.Now().Add(time.Hour))
	drainOrderDispatch(t, m)
	if store.roundTrips == 0 {
		t.Fatal("a single-store city's gate read nothing; the runtime plane must degrade to \"the only store there is\", never to \"no store at all\"")
	}
}

func drainOrderDispatch(t *testing.T, m *memoryOrderDispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !m.drain(ctx) {
		t.Fatal("order dispatch did not drain")
	}
}

// barrierCheckScript returns a condition-check shell script for the
// concurrent-overlap fixtures below. Every check registers itself in liveDir
// (a mktemp'd file, removed on exit via trap) and then holds that
// registration for the full ~1s window (50 * 20ms polls), regardless of when
// it personally observes a sibling: a seen flag latches on any poll that
// finds >= 2 registrations, and only that flag — checked once the window is
// over — decides exit 0 vs exit 1. It always exits normally so the trap
// unregisters. Holding to the end of the window (rather than exiting the
// instant a sibling is seen) makes overlap detection independent of which
// pair happens to notice each other first, so a straggler that starts late
// but still within the window is guaranteed to be seen by, and see, whoever
// is still holding. A nonzero startDelay simulates a check whose own
// subprocess spawn was delayed — host contention, a straggler — before it
// ever gets to register.
func barrierCheckScript(liveDir, ranMarker string, startDelay time.Duration) string {
	var delay string
	if startDelay > 0 {
		delay = fmt.Sprintf("sleep %.3f; ", startDelay.Seconds())
	}
	return fmt.Sprintf(
		`%[3]smkdir -p %[1]s; echo . >> %[2]s; f=$(mktemp %[1]s/w.XXXXXX); trap 'rm -f "$f"' EXIT; `+
			`seen=0; i=0; while [ $i -lt 50 ]; do if [ "$(ls %[1]s | wc -l)" -ge 2 ]; then seen=1; fi; sleep 0.02; i=$((i+1)); done; `+
			`if [ "$seen" -eq 1 ]; then exit 0; else exit 1; fi`,
		liveDir, ranMarker, delay)
}

// TestConditionChecksRunConcurrentlyWithinTheirCap is the parallel half of the
// leg, and it proves real overlap rather than measuring wall clock.
//
// Every check registers itself in a shared directory and polls, bounded, for
// the FULL wave to be registered before exiting 0; a check that never sees
// the whole cohort exits 1. Cleanup is conditional on that outcome
// (ga-e7cxrg): only a check that timed out removes its registration, so a
// later non-overlapping cap-1 run never inherits a stale marker. A check that
// DID see the full wave leaves its marker behind, because polling isn't
// synchronized across checks — the moment the cohort completes, whichever
// check notices first would otherwise tear down the evidence (unconditional
// cleanup-on-exit) before a sibling one poll tick behind ever looked, and
// that race — not a slow fork/exec — is what actually produced the flake:
// every check always ran and registered, but the completed-cohort state
// could vanish in under one 20ms poll interval. Leaving success markers in
// place means that state can only grow, never shrink, until every check
// still polling has had a turn to see it.
//
// With a cap that admits the whole wave, every check eventually sees all of
// them and every order fires; with a cap of one, checks run strictly
// sequentially and none ever sees more than itself, so nothing fires. The
// same fixture is its own control, and neither outcome is reachable by a pass
// that simply stopped running checks: that one fires nothing AND leaves the
// registration directory empty, which the first arm asserts against.
func TestConditionChecksRunConcurrentlyWithinTheirCap(t *testing.T) {
	const wave = 4
	for _, tc := range []struct {
		name    string
		cap     int
		wantRun int
	}{
		{name: "cap-admits-the-wave", cap: wave, wantRun: wave},
		{name: "serial-cap-never-overlaps", cap: 1, wantRun: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := orderConditionCheckConcurrency
			orderConditionCheckConcurrency = tc.cap
			t.Cleanup(func() { orderConditionCheckConcurrency = prev })

			cityPath, cfg, _ := newExecOrderFixture(t)
			liveDir := filepath.Join(t.TempDir(), "live")
			ranMarker := filepath.Join(t.TempDir(), "ran")
			// Register, then wait up to ~1s to see the FULL wave registered.
			// Exit 0 only once every sibling has checked in. The trap only
			// unregisters on a timeout ($ok stays 0): a check that DID see the
			// full cohort leaves its marker behind so a sibling polling on a
			// different tick still finds the completed wave rather than racing
			// the first-to-notice check's own cleanup.
			check := fmt.Sprintf(
				`mkdir -p %[1]s; echo . >> %[3]s; f=$(mktemp %[1]s/w.XXXXXX); ok=0; trap '[ "$ok" = 1 ] || rm -f "$f"' EXIT; `+
					`i=0; while [ $i -lt 50 ]; do if [ "$(ls %[1]s | wc -l)" -ge %[2]d ]; then ok=1; exit 0; fi; sleep 0.02; i=$((i+1)); done; exit 1`,
				liveDir, wave, ranMarker)

			aa := make([]orders.Order, 0, wave)
			for i := range wave {
				aa = append(aa, orders.Order{
					Name:         fmt.Sprintf("barrier-order-%d", i),
					Exec:         "true",
					Trigger:      "condition",
					Check:        check,
					CheckTimeout: "10s",
				})
			}
			store := beads.NewMemStore()
			var rec memRecorder
			m := newSplitOrderDispatcher(t, cityPath, cfg, aa, store, beads.NewMemStore(), &rec)
			m.maxDispatchesPerTick = 0
			m.execRun = func(context.Context, string, string, []string) ([]byte, error) { return nil, nil }

			m.dispatch(context.Background(), cityPath, time.Now())
			drainOrderDispatch(t, m)

			if got := len(trackingBeads(t, m.ordersStoreFor(store), labelOrderTracking)); got != tc.wantRun {
				t.Fatalf("%d order(s) fired, want %d: with cap %d the %d checks %s",
					got, tc.wantRun, tc.cap, wave,
					map[bool]string{true: "overlap and see each other", false: "run one at a time and never do"}[tc.cap >= 2])
			}
			// Control: every check DID run, so a zero above is "no overlap" and
			// not "no checks".
			data, err := os.ReadFile(ranMarker)
			if err != nil {
				t.Fatalf("no check ran at all: %v", err)
			}
			if got := strings.Count(string(data), "."); got != wave {
				t.Fatalf("%d of %d checks ran; the fire count above is not measuring overlap", got, wave)
			}
		})
	}
}

// TestConditionCheckOverlapSurvivesAStragglerStart is the deterministic
// regression for ga-v9gt79: under real sharded process load,
// TestConditionChecksRunConcurrentlyWithinTheirCap's cap-admits-the-wave arm
// flaked 3-of-4 instead of 4-of-4. The cause is not a Go-level dispatch bug —
// order_dispatch.go's goroutine fan-out and triggers.go's subprocess
// execution both fire all four checks concurrently, unconditionally — it is
// that the OLD check script unregisters itself the instant it personally
// sees a sibling, so the real overlap tolerance was bounded by however fast
// the fastest pair happened to notice each other, not by the ~1s poll
// budget. Under host contention a straggler's subprocess spawn can be
// delayed past that effective window, so by the time it registers, its
// siblings have already succeeded and vanished.
//
// This reproduces that shape on demand, without depending on real,
// non-deterministic host contention: three checks start immediately and one
// starts after an artificial delay comfortably inside the ~1s window. A
// check that holds its registration for the full window — rather than
// exiting the instant it personally detects a sibling — must still be seen
// by a late-starting sibling, so all four fire regardless of which pair
// notices which first.
func TestConditionCheckOverlapSurvivesAStragglerStart(t *testing.T) {
	const wave = 4
	prev := orderConditionCheckConcurrency
	orderConditionCheckConcurrency = wave
	t.Cleanup(func() { orderConditionCheckConcurrency = prev })

	cityPath, cfg, _ := newExecOrderFixture(t)
	liveDir := filepath.Join(t.TempDir(), "live")
	ranMarker := filepath.Join(t.TempDir(), "ran")

	const stragglerDelay = 300 * time.Millisecond
	aa := make([]orders.Order, 0, wave)
	for i := range wave {
		var delay time.Duration
		if i == wave-1 {
			delay = stragglerDelay
		}
		aa = append(aa, orders.Order{
			Name:         fmt.Sprintf("straggler-order-%d", i),
			Exec:         "true",
			Trigger:      "condition",
			Check:        barrierCheckScript(liveDir, ranMarker, delay),
			CheckTimeout: "10s",
		})
	}
	store := beads.NewMemStore()
	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, aa, store, beads.NewMemStore(), &rec)
	m.maxDispatchesPerTick = 0
	m.execRun = func(context.Context, string, string, []string) ([]byte, error) { return nil, nil }

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := len(trackingBeads(t, m.ordersStoreFor(store), labelOrderTracking)); got != wave {
		t.Fatalf("%d of %d orders fired with a %s straggler start: a check must hold its registration for the "+
			"full overlap window so a late-starting sibling still sees it, not just whichever pair happened to "+
			"notice each other first", got, wave, stragglerDelay)
	}
	// Control: every check DID run, so a shortfall above is "no overlap" and
	// not "the straggler's order never got checked".
	data, err := os.ReadFile(ranMarker)
	if err != nil {
		t.Fatalf("no check ran at all: %v", err)
	}
	if got := strings.Count(string(data), "."); got != wave {
		t.Fatalf("%d of %d checks ran; the fire count above is not measuring overlap", got, wave)
	}
}

// TestConditionCheckRunsExactlyOncePerOrderPerTick guards the seam between the
// parallel pass and the fire loop: the loop must CONSUME the prefetched verdict,
// not recompute it. A second evaluation would fork every check twice per tick —
// a check storm wearing a latency fix's name — and would also make a check with
// an observable side effect run twice.
func TestConditionCheckRunsExactlyOncePerOrderPerTick(t *testing.T) {
	cityPath, cfg, _ := newExecOrderFixture(t)
	countDir := t.TempDir()
	aa := make([]orders.Order, 0, 3)
	for i := range 3 {
		aa = append(aa, orders.Order{
			Name:    fmt.Sprintf("counted-order-%d", i),
			Exec:    "true",
			Trigger: "condition",
			Check:   fmt.Sprintf("echo . >> %s", filepath.Join(countDir, fmt.Sprintf("checks-%d", i))),
		})
	}
	store := beads.NewMemStore()
	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, aa, store, beads.NewMemStore(), &rec)
	m.maxDispatchesPerTick = 0
	m.execRun = func(context.Context, string, string, []string) ([]byte, error) { return nil, nil }

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	for i := range 3 {
		data, err := os.ReadFile(filepath.Join(countDir, fmt.Sprintf("checks-%d", i)))
		if err != nil {
			t.Fatalf("order %d: check never ran: %v", i, err)
		}
		if got := strings.Count(string(data), "."); got != 1 {
			t.Fatalf("order %d's condition check ran %d time(s) in one tick, want 1", i, got)
		}
	}
}

// TestGateIndexSuppressesDispatchExactlyAsTheLiveGateDid ports the #2893/#2921
// single-flight fixtures onto the index path: the gate's ANSWER must be
// unchanged, whichever read produced it.
//
// Both shapes the strict gate recognizes are covered — an open TRACKING bead and
// an open WISP subtree — and the control is the same city with the evidence
// closed, which must fire.
func TestGateIndexSuppressesDispatchExactlyAsTheLiveGateDid(t *testing.T) {
	closed := "closed"
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, store *beads.MemStore, scoped string)
		want int
	}{
		{
			name: "open tracking bead",
			seed: func(t *testing.T, store *beads.MemStore, scoped string) {
				if _, err := orders.NewStore(beads.OrdersStore{Store: store}).CreateRun(scoped, orders.RunOpts{}); err != nil {
					t.Fatalf("seed tracking bead: %v", err)
				}
			},
			want: 0,
		},
		{
			name: "open wisp subtree",
			seed: func(t *testing.T, store *beads.MemStore, scoped string) {
				root, err := store.Create(beads.Bead{
					Title:    "wisp root",
					Type:     "molecule",
					Labels:   []string{orders.RunLabel(scoped)},
					Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
				})
				if err != nil {
					t.Fatalf("seed wisp root: %v", err)
				}
				if _, err := store.Create(beads.Bead{
					Title:    "open step",
					Type:     "task",
					ParentID: root.ID,
					Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
				}); err != nil {
					t.Fatalf("seed wisp step: %v", err)
				}
			},
			want: 0,
		},
		{
			name: "control: evidence closed, the order fires",
			seed: func(t *testing.T, store *beads.MemStore, scoped string) {
				run, err := orders.NewStore(beads.OrdersStore{Store: store}).CreateRun(scoped, orders.RunOpts{})
				if err != nil {
					t.Fatalf("seed tracking bead: %v", err)
				}
				if err := store.Update(run.ID, beads.UpdateOpts{Status: &closed}); err != nil {
					t.Fatalf("close tracking bead: %v", err)
				}
			},
			want: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg, a := newExecOrderFixture(t)
			a.Interval = "1ms"
			binding := beads.NewMemStore()
			binding.IDPrefix = "gcg"
			tc.seed(t, binding, a.ScopedName())
			before := len(trackingBeads(t, binding, labelOrderTracking))

			var rec memRecorder
			m := newSplitOrderDispatcher(t, cityPath, cfg, []orders.Order{a}, beads.NewMemStore(), binding, &rec)
			m.execRun = func(context.Context, string, string, []string) ([]byte, error) { return nil, nil }
			m.dispatch(context.Background(), cityPath, time.Now().Add(time.Hour))
			drainOrderDispatch(t, m)

			if got := len(trackingBeads(t, binding, labelOrderTracking)) - before; got != tc.want {
				t.Fatalf("the index-served gate let %d new dispatch(es) through, want %d", got, tc.want)
			}
		})
	}
}

// TestOrderTrackingSweepStillReadsEveryLegTheGateNoLongerDoes is the
// convergence half of the plane doctrine on this leg.
//
// The gate narrowed to the infra binding because that is where a dispatch writes
// its order-run evidence. That leaves one thing behind: a tracking bead a
// PRE-SPLIT dispatch left in the work ledger, which the gate can no longer see.
// It must not therefore be immortal — the wide reader is the order-tracking
// sweep, and it is deliberately NOT narrowed. Runtime narrows for latency;
// reconcile never narrows, because a store it skips is a store nothing
// converges.
func TestOrderTrackingSweepStillReadsEveryLegTheGateNoLongerDoes(t *testing.T) {
	cityPath, cfg, _ := newExecOrderFixture(t)
	work := beads.NewMemStore()
	binding := beads.NewMemStore()
	binding.IDPrefix = "gcg"
	var rec memRecorder
	m := newSplitOrderDispatcher(t, cityPath, cfg, nil, work, binding, &rec)

	gateStores, _ := m.gateStoresFor(cityPath, work, "city\x00"+cityPath, nil)
	if storeListContains(gateStores, beads.Store(work)) {
		t.Fatal("the gate still reads the work ledger; this control is testing the wrong build")
	}

	sweepStores, err := orderTrackingSweepStoresFromTargets(
		orderTrackingSweepTargetsForConfig(cityPath, cfg),
		func(orderTrackingSweepTarget) (beads.Store, error) { return work, nil },
	)
	if err != nil {
		t.Fatalf("orderTrackingSweepStoresFromTargets: %v", err)
	}
	if !sweepStoreListContains(sweepStores, beads.Store(work)) {
		t.Fatalf("the order-tracking sweep no longer reads the work ledger either (%d leg(s)); a pre-split tracking bead there would gate nothing and be closed by nothing", len(sweepStores))
	}
}
