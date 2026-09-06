package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The tick's read budget, as a golden table.
//
// A controller tick's latency is not a wall-clock property of this machine; it
// is the number of SEQUENTIAL store round trips the tick makes, multiplied by
// whatever the slowest leg's RTT happens to be. On maintainer-city the work
// ledger is remote postgres at ~5.4s per query, and that multiplication is the
// whole incident: the detached-orphan sweep's per-store open scan was 180.8s of
// a 373s tick, the completions walk 69.7s on top, and the route-recovery scan was
// blamed for another 185s before the trace showed it had been the orphan sweep
// all along (ga-l7jdg, ga-4qdfn).
//
// So the regression gate counts round trips. A leg that quietly goes back to
// scanning per tick fails here as a deterministic integer diff — not as a flaky
// timing assertion on a loaded CI box. The latency injector below exists only so
// the wall-clock half can be stated as narrative: at a stand-in RTT, this many
// round trips is this many seconds.
//
// The remaining two hot legs carry their budgets in their own files, because
// theirs are not zero and their fixtures are whole subsystems: order dispatch in
// order_gate_budget_test.go (gate reads scale with STORES, not orders; zero
// ledger round trips) and the demand snapshot in demand_snapshot_budget_test.go
// (zero ledger round trips in the cache check and the routed-demand read). Same
// gate, same unit, three files.

// tickRTT is the stand-in for the remote ledger's per-query latency —
// maintainer-city's work store answers in ~5.4s, scaled down to keep the
// arithmetic readable.
//
// It is MODELED, not slept. The gate is the deterministic round-trip count; a
// real sleep would buy nothing but wall-clock the test does not assert on, would
// make the suite slower for every future run, and would put a fixed sleep in the
// test-resource census for no coverage. The elapsed figure below is the count
// times the RTT, which is exactly what a real sleep would have produced.
const tickRTT = 2 * time.Millisecond

// latencyStore charges tickRTT for every store round trip and counts them.
type latencyStore struct {
	beads.Store
	roundTrips int
	elapsed    time.Duration
}

func (s *latencyStore) charge() {
	s.roundTrips++
	s.elapsed += tickRTT
}

func (s *latencyStore) Get(id string) (beads.Bead, error) {
	s.charge()
	return s.Store.Get(id)
}

func (s *latencyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.charge()
	return s.Store.List(q)
}

func (s *latencyStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.charge()
	return s.Store.ListByMetadata(filters, limit, opts...)
}

func (s *latencyStore) SetMetadata(id, key, value string) error {
	s.charge()
	return s.Store.SetMetadata(id, key, value)
}

func (s *latencyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.charge()
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestSteadyTickStoreRoundTripBudget is the regression gate for every leg moved
// off the tick. All three budgets are ZERO: a steady tick names no candidate and
// no root, so it must not reach a store at all.
//
// Every row carries its own control in the third column — the same leg over the
// same corpus in its convergence form — because a budget of zero is also what a
// leg that stopped working reports.
func TestSteadyTickStoreRoundTripBudget(t *testing.T) {
	for _, row := range []struct {
		leg          string
		steadyBudget int
		// convergence runs the leg's backstop form over the identical corpus and
		// returns its round-trip count, which must EXCEED the steady budget.
		steady      func(t *testing.T) (*latencyStore, int)
		convergence func(t *testing.T) int
	}{
		{
			leg:          "recover_unrouted_work_routes",
			steadyBudget: 0,
			steady: func(t *testing.T) (*latencyStore, int) {
				store, cr := budgetRouteRuntime(t)
				cr.recoverUnroutedWorkRoutesDelta()
				return store, store.roundTrips
			},
			convergence: func(t *testing.T) int {
				store, cr := budgetRouteRuntime(t)
				cr.runRouteRecoveryBackstop(backstopReasonCadence)
				return store.roundTrips
			},
		},
		{
			leg:          "reconcile_execution_completions",
			steadyBudget: 0,
			steady: func(t *testing.T) (*latencyStore, int) {
				store, recorder, _ := budgetCompletionsCorpus(t)
				executionevent.ReconcileCompletedRoots(recorder, []beads.GraphStore{{Store: store}}, nil, "execution-reconcile")
				return store, store.roundTrips
			},
			convergence: func(t *testing.T) int {
				store, recorder, _ := budgetCompletionsCorpus(t)
				executionevent.ReconcileCompletedStores(recorder, []beads.GraphStore{{Store: store}}, "execution-reconcile")
				return store.roundTrips
			},
		},
		{
			// The leg nobody scoped until the post-S1/S2 profile named it:
			// 180.8s of a 373s tick, 48.5%, restored_count=0 on every tick
			// (ga-l7jdg). A live open-corpus scan of the city ledger and every
			// rig, serially, to discover nothing.
			leg:          "sweep_detached_handoff_orphans",
			steadyBudget: 0,
			steady: func(t *testing.T) (*latencyStore, int) {
				store, cr := budgetOrphanRuntime(t)
				cr.sweepDetachedHandoffOrphansDelta()
				return store, store.roundTrips
			},
			convergence: func(t *testing.T) int {
				store, cr := budgetOrphanRuntime(t)
				cr.runDetachedOrphanBackstop(backstopReasonCadence)
				return store.roundTrips
			},
		},
	} {
		t.Run(row.leg, func(t *testing.T) {
			start := time.Now()
			store, got := row.steady(t)
			wall := time.Since(start)
			if got != row.steadyBudget {
				t.Fatalf("%s cost %d store round trip(s) on a steady tick, budget %d — at maintainer-city's ~5.4s ledger RTT that is %v of tick",
					row.leg, got, row.steadyBudget, time.Duration(got)*5400*time.Millisecond)
			}
			if store.elapsed != time.Duration(got)*tickRTT {
				t.Fatalf("the latency injector charged %v for %d round trip(s); the budget is not being measured through it", store.elapsed, got)
			}
			// Soft, narrative only: the deterministic count above is the gate.
			// Modeled cost is got*tickRTT; real wall clock is logged when it
			// diverges wildly, which would mean the pass is spending time
			// somewhere the round-trip model does not see.
			if wall > time.Second {
				t.Logf("%s steady pass took %v of wall clock, which is far more than %d round trips explain", row.leg, wall, got)
			}
			// Control: the convergence form of the SAME leg over the SAME corpus
			// costs strictly more. Without it, a leg that had simply stopped
			// reading would pass the budget above.
			if conv := row.convergence(t); conv <= got {
				t.Fatalf("%s costs %d round trip(s) in its convergence form and %d on a steady tick; the budget is not measuring anything",
					row.leg, conv, got)
			}
		})
	}
}

// journalReadCounter counts the journal reads a completions pass issues, split
// by the only distinction that matters for cost.
//
// A read with no AfterSeq is a FULL read: ReadFiltered gunzips and scans every
// sibling archive before the active file, so it is O(retained history) — 69.7s
// of a 373s tick on maintainer-city. A read with AfterSeq set skips every
// archive whose seq window is already behind the cursor
// (archiveOverlapsFilter), so it costs the active file alone.
//
// It deliberately does NOT implement events.InFlightProvider: embedding the
// plain Provider interface means completedFacts takes its List branch, so the
// counts below are the ones the test names.
type journalReadCounter struct {
	events.Provider
	fullReads  int
	tailReads  int
	latestSeqs int
}

func (p *journalReadCounter) List(filter events.Filter) ([]events.Event, error) {
	if filter.AfterSeq == 0 {
		p.fullReads++
	} else {
		p.tailReads++
	}
	return p.Provider.List(filter)
}

func (p *journalReadCounter) LatestSeq() (uint64, error) {
	p.latestSeqs++
	return p.Provider.LatestSeq()
}

// TestCompletionsDeltaJournalReadBudgetWithNamedRoots is the golden the runtime
// never reached.
//
// The shipped budget rows below assert the named_roots == 0 case. On
// maintainer-city the journal names 1-2 roots on EVERY tick, so
// ReconcileCompletedRoots passed its early return and paid a full
// O(retained-history) journal read on every one — 69.7s of a 373s tick, flat and
// independent of how many roots were named (ga-l7jdg). The zero-read property
// was real and the tick never once got it.
//
// So the budget for a WORKING tick is stated here: exactly one full journal read
// to warm the index, and none thereafter no matter how many passes run.
func TestCompletionsDeltaJournalReadBudgetWithNamedRoots(t *testing.T) {
	store, base, rootIDs := budgetCompletionsCorpusOfSize(t, 8)
	journal := &journalReadCounter{Provider: base}
	graphStores := []beads.GraphStore{{Store: store}}
	named := rootIDs[:2]

	index := &executionevent.CompletedFactIndex{}
	if emitted := index.ReconcileRoots(journal, graphStores, named, "execution-reconcile"); emitted != len(named) {
		t.Fatalf("the warm-up pass emitted %d fact(s) for %d named root(s); the fixture has no completion work and the budget below is vacuous", emitted, len(named))
	}
	if journal.fullReads != 1 {
		t.Fatalf("warming the fact index cost %d full journal read(s), want exactly 1", journal.fullReads)
	}

	// Steady ticks: roots named every time, as on maintainer-city.
	for range 8 {
		index.ReconcileRoots(journal, graphStores, named, "execution-reconcile")
	}
	if journal.fullReads != 1 {
		t.Fatalf("8 ticks with named roots cost %d full journal read(s), budget 1 — at maintainer-city's measured 69.7s per full read that is %v of tick time",
			journal.fullReads, time.Duration(journal.fullReads-1)*69700*time.Millisecond)
	}
	// And not a cheaper journal read either: a warm index reads NOTHING. The
	// facts it does not emit itself arrive through the journal feed the lane
	// already tails, which costs the tick nothing at all.
	if journal.tailReads != 0 || journal.latestSeqs != 0 {
		t.Fatalf("a warm index issued %d incremental read(s) and %d head read(s), want 0 and 0", journal.tailReads, journal.latestSeqs)
	}

	// Control: the index is warm, not broken. A root whose step closes AFTER the
	// index warmed must still get its recovery fact, and the pass must still
	// refuse to emit a duplicate for a fact the journal already carries.
	fresh, freshBase, freshRoots := budgetCompletionsCorpusOfSize(t, 2)
	freshJournal := &journalReadCounter{Provider: freshBase}
	freshIndex := &executionevent.CompletedFactIndex{}
	first := freshIndex.ReconcileRoots(freshJournal, []beads.GraphStore{{Store: fresh}}, freshRoots[:1], "execution-reconcile")
	second := freshIndex.ReconcileRoots(freshJournal, []beads.GraphStore{{Store: fresh}}, freshRoots, "execution-reconcile")
	if first != 1 || second != 1 {
		t.Fatalf("first pass emitted %d and the second (one more root) emitted %d, want 1 and 1: the warm index must still repair new work and must not repeat old facts", first, second)
	}
}

// TestDeltaPassReadBudgetScalesWithWorkNotWithCorpus is the second half of the
// gate: when a delta pass DOES have work, its cost must track the number of
// things the journal named, never the size of the corpus behind them.
func TestDeltaPassReadBudgetScalesWithWorkNotWithCorpus(t *testing.T) {
	// Route repair: one batched IN-list re-verify plus one write per repaired
	// bead, whatever the corpus size.
	for _, corpus := range []int{4, 40} {
		store := &latencyStore{Store: beads.NewMemStoreFrom(0, budgetUnroutedBeads(corpus), nil)}
		cr := &CityRuntime{cityName: "city", standaloneCityStore: store, stderr: io.Discard}
		lane := cr.routeRecoveryLaneOf()
		named := []string{"T-0", "T-1"}
		for _, id := range named {
			lane.observe(beadCreatedEvent(t, unroutedWorkBead(id)))
		}
		cr.recoverUnroutedWorkRoutesDelta()
		if want := 1 + len(named); store.roundTrips != want {
			t.Fatalf("route repair over a %d-bead corpus cost %d round trip(s) for %d named bead(s), budget %d",
				corpus, store.roundTrips, len(named), want)
		}
	}

	// Completions: one batched read for the named roots plus one steps read per
	// named root, whatever the corpus size.
	for _, corpus := range []int{4, 40} {
		store, recorder, rootIDs := budgetCompletionsCorpusOfSize(t, corpus)
		named := rootIDs[:2]
		executionevent.ReconcileCompletedRoots(recorder, []beads.GraphStore{{Store: store}}, named, "execution-reconcile")
		if want := 1 + len(named); store.roundTrips != want {
			t.Fatalf("completions over a %d-root corpus cost %d round trip(s) for %d named root(s), budget %d",
				corpus, store.roundTrips, len(named), want)
		}
	}

	// Control: the convergence forms DO scale with the corpus, so the invariance
	// above is a property of the delta lanes and not of the fixtures.
	small, _, _ := budgetCompletionsCorpusOfSize(t, 4)
	large, _, _ := budgetCompletionsCorpusOfSize(t, 40)
	executionevent.ReconcileCompletedStores(events.NewFake(), []beads.GraphStore{{Store: small}}, "execution-reconcile")
	executionevent.ReconcileCompletedStores(events.NewFake(), []beads.GraphStore{{Store: large}}, "execution-reconcile")
	if large.roundTrips <= small.roundTrips {
		t.Fatalf("the full pass cost %d round trip(s) over 40 roots and %d over 4; it does not scale with the corpus and the invariance above is vacuous",
			large.roundTrips, small.roundTrips)
	}
}

func budgetUnroutedBeads(n int) []beads.Bead {
	out := make([]beads.Bead, 0, n)
	for i := range n {
		out = append(out, unroutedWorkBead("T-"+itoaSmall(i)))
	}
	return out
}

func itoaSmall(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func budgetRouteRuntime(t *testing.T) (*latencyStore, *CityRuntime) {
	t.Helper()
	store := &latencyStore{Store: beads.NewMemStoreFrom(0, budgetUnroutedBeads(8), nil)}
	return store, &CityRuntime{cityName: "city", standaloneCityStore: store, stderr: io.Discard}
}

// budgetOrphanRuntime seeds a repairable detached handoff orphan — the shape the
// convergence scan finds and the delta lane must not go looking for — plus the
// session bead its route is recovered from.
func budgetOrphanRuntime(t *testing.T) (*latencyStore, *CityRuntime) {
	t.Helper()
	seed := append(budgetUnroutedBeads(8), detachedOrphanSessionBead(), detachedOrphanWorkBead("D-1"))
	store := &latencyStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
	return store, &CityRuntime{cityName: "city", standaloneCityStore: store, logPrefix: "gc", stderr: io.Discard}
}

func budgetCompletionsCorpus(t *testing.T) (*latencyStore, events.Provider, []string) {
	t.Helper()
	return budgetCompletionsCorpusOfSize(t, 8)
}

// budgetCompletionsCorpusOfSize seeds n graph.v2 roots, each with one closed
// step — the closed-molecule corpus the full pass walks and the delta pass must
// not.
func budgetCompletionsCorpusOfSize(t *testing.T, n int) (*latencyStore, events.Provider, []string) {
	t.Helper()
	backing := beads.NewMemStore()
	closed := "closed"
	rootIDs := make([]string, 0, n)
	for i := range n {
		root, err := backing.Create(beads.Bead{Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}})
		if err != nil {
			t.Fatalf("create root: %v", err)
		}
		rootIDs = append(rootIDs, root.ID)
		step, err := backing.Create(beads.Bead{
			ID: "gcg-budget-step-" + itoaSmall(i),
			Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: root.ID,
				beadmeta.StepIDMetadataKey:     "build",
			},
		})
		if err != nil {
			t.Fatalf("create step: %v", err)
		}
		if err := backing.Update(step.ID, beads.UpdateOpts{
			Status:   &closed,
			Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-session"},
		}); err != nil {
			t.Fatalf("close step: %v", err)
		}
	}
	return &latencyStore{Store: backing}, events.NewFake(), rootIDs
}

// TestSteadyTickLedgerRoundTripBudgetIsZero is the operator invariant as a
// budget row (ga-l7jdg, bd memory gascity-runtime-infra-store-invariant).
//
// The other rows in this file count round trips per leg. This one counts them
// per STORE CLASS, because the invariant is not "the tick is fast" but "the tick
// does not touch the work ledger at all". A ledger read on the runtime path is a
// misrouting bug by definition; a budget that only totals round trips would let
// one back in as long as it were cheap enough, and on maintainer-city it never
// is — that leg is 5.4s each.
//
// Both halves of the invariant are asserted: the ledger is zero on a steady tick
// AND on a tick that has work to do, and the binding is what answers instead.
func TestSteadyTickLedgerRoundTripBudgetIsZero(t *testing.T) {
	newCity := func(t *testing.T) (storeref.ResolvedPlan, *latencyStore, *latencyStore) {
		t.Helper()
		ledger := &latencyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("CW-1")}, nil)}
		binding := &latencyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("GB-1")}, nil)}
		topo := assembleResidencyTopology(&config.City{}, ledger, nil,
			[]storeref.ClassBinding{{
				Classes: []coordclass.Class{coordclass.ClassGraph},
				Leg:     storeref.Leg{Ref: storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
			}}, nil)
		plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
		if err != nil {
			t.Fatalf("Plan(RoutedWork): %v", err)
		}
		return plan, ledger, binding
	}

	// A steady tick names nothing and touches nothing.
	plan, ledger, binding := newCity(t)
	if report := newRouteRecoveryLane().deltaPass(plan, nil); report.legReads != 0 {
		t.Fatalf("a steady tick reported %d leg read(s), want 0", report.legReads)
	}
	if ledger.roundTrips != 0 || binding.roundTrips != 0 {
		t.Fatalf("a steady tick issued ledger=%d binding=%d round trip(s), want 0 and 0", ledger.roundTrips, binding.roundTrips)
	}

	// A tick WITH work still owes the ledger nothing: the binding answers.
	plan, ledger, binding = newCity(t)
	report := newRouteRecoveryLane().deltaPass(plan, []string{"CW-1", "GB-1"})
	if ledger.roundTrips != 0 {
		t.Fatalf("a working tick issued %d ledger round trip(s), want 0 — that is %v of tick at maintainer-city's RTT",
			ledger.roundTrips, time.Duration(ledger.roundTrips)*5400*time.Millisecond)
	}
	// Control: the binding did the work, so the ledger zero is a routing fact
	// and not a pass that declined to run.
	if binding.roundTrips == 0 || report.restored != 1 {
		t.Fatalf("binding round trips=%d restored=%d, want non-zero and 1", binding.roundTrips, report.restored)
	}
}
