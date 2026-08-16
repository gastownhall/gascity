package main

import (
	"bytes"
	"context"
	"io"
	"strings"
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

const detachedOrphanTestPool = "gascity/gastown.polecat"

// detachedOrphanWorkBead is the recoverable shape: open, unassigned, kind-less,
// no route of its own, a pushed branch, and a session back-reference.
func detachedOrphanWorkBead(id, sessionName string) beads.Bead {
	return beads.Bead{ID: id, Title: "orphaned work", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.WorkBranchMetadataKey:  "polecat/" + id,
		beadmeta.SessionNameMetadataKey: sessionName,
	}}
}

// detachedOrphanSessionBead is the session bead the route is recovered from.
func detachedOrphanSessionBead(id, sessionName string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  "session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     detachedOrphanTestPool,
		},
	}
}

func detachedOrphanRuntime(t *testing.T, seed ...beads.Bead) (*CityRuntime, *countingRouteStore) {
	t.Helper()
	store := &countingRouteStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
	return &CityRuntime{cityName: "city", standaloneCityStore: store, logPrefix: "gc", stderr: io.Discard}, store
}

// TestDetachedOrphanSweepSteadyTickIssuesZeroWorkLedgerReads is the slice's
// headline property, in the unit the incident was measured in.
//
// sweep_detached_handoff_orphans was 180.8s of a 373s tick — 48.5%, dead
// constant, restored_count=0 on every tick (ga-l7jdg). A tick that names no
// candidate must now touch the stores zero times.
//
// The control is the same store on the SAME data one line later: the backstop
// scans it and repairs the bead. Without that, "zero reads" would be satisfied
// by a lane that had simply stopped working.
func TestDetachedOrphanSweepSteadyTickIssuesZeroWorkLedgerReads(t *testing.T) {
	cr, store := detachedOrphanRuntime(t,
		detachedOrphanSessionBead("S-1", "sess-1"),
		detachedOrphanWorkBead("D-1", "sess-1"))

	for range 4 {
		report := cr.sweepDetachedHandoffOrphansDelta()
		if report.legReads != 0 {
			t.Fatalf("steady tick reported %d leg read(s), want 0", report.legReads)
		}
	}
	if store.reads() != 0 {
		t.Fatalf("steady ticks issued %d store read(s) (%d List, %d Get), want 0", store.reads(), store.lists, store.gets)
	}
	if store.writes != 0 {
		t.Fatalf("steady ticks issued %d write(s), want 0", store.writes)
	}
	if got := mustRoutedTo(t, store, "D-1"); got != "" {
		t.Fatalf("D-1 gc.routed_to = %q after delta-only ticks, want empty", got)
	}

	// Control: the backstop over the identical store DOES scan and DOES repair.
	report := cr.runDetachedOrphanBackstop(backstopReasonCadence)
	if report.restored != 1 {
		t.Fatalf("backstop restored %d (err=%v), want 1 — the zero-read assertion above measured a lane that cannot repair", report.restored, report.err)
	}
	if store.scanned == 0 {
		t.Fatal("backstop issued no open-corpus scan; the scan was deleted rather than demoted")
	}
	if got := mustRoutedTo(t, store, "D-1"); got != detachedOrphanTestPool {
		t.Fatalf("D-1 gc.routed_to = %q after backstop, want %q", got, detachedOrphanTestPool)
	}
}

// TestDetachedOrphanDeltaRepairsOnlyEventNamedBeads pins both halves of the
// delta pass: it touches only what the journal named, and it re-verifies the
// whole batch in ONE round trip rather than one per bead.
func TestDetachedOrphanDeltaRepairsOnlyEventNamedBeads(t *testing.T) {
	seed := []beads.Bead{
		detachedOrphanSessionBead("S-1", "sess-1"),
		detachedOrphanWorkBead("D-1", "sess-1"),
		detachedOrphanWorkBead("D-2", "sess-1"),
		detachedOrphanWorkBead("D-3", "sess-1"),
	}
	cr, store := detachedOrphanRuntime(t, seed...)
	lane := cr.detachedOrphanLaneOf()
	lane.observe(beadCreatedEvent(t, detachedOrphanWorkBead("D-1", "sess-1")))
	lane.observe(beadCreatedEvent(t, detachedOrphanWorkBead("D-2", "sess-1")))

	report := cr.sweepDetachedHandoffOrphansDelta()
	if report.restored != 2 {
		t.Fatalf("delta restored %d (err=%v), want 2", report.restored, report.err)
	}
	if store.scanned != 0 {
		t.Fatalf("the delta pass issued %d open-corpus scan(s), want 0 — it is supposed to read only what was named", store.scanned)
	}
	// One batched IN-list re-verify of the candidates, plus the two-leg session
	// index. The per-candidate Get fan-out is what makes a remote ledger slow.
	if store.gets != 0 {
		t.Fatalf("the delta pass issued %d per-candidate Get(s), want 0 — the re-verify must be one batched read", store.gets)
	}
	if got := mustRoutedTo(t, store, "D-3"); got != "" {
		t.Fatalf("D-3 was repaired (gc.routed_to=%q) but the journal never named it", got)
	}
}

// TestDetachedOrphanDeltaReadCountDoesNotScaleWithCandidates: the batched
// re-verify plus one shared session index, whatever the candidate count.
func TestDetachedOrphanDeltaReadCountDoesNotScaleWithCandidates(t *testing.T) {
	counts := map[int]int{}
	for _, n := range []int{2, 8} {
		seed := []beads.Bead{detachedOrphanSessionBead("S-1", "sess-1")}
		var named []string
		for i := range n {
			id := "D-" + string(rune('a'+i))
			seed = append(seed, detachedOrphanWorkBead(id, "sess-1"))
			named = append(named, id)
		}
		cr, store := detachedOrphanRuntime(t, seed...)
		lane := cr.detachedOrphanLaneOf()
		for _, id := range named {
			lane.observe(beadCreatedEvent(t, detachedOrphanWorkBead(id, "sess-1")))
		}
		report := cr.sweepDetachedHandoffOrphansDelta()
		if report.restored != n {
			t.Fatalf("%d candidates: restored %d (err=%v), want %d", n, report.restored, report.err, n)
		}
		counts[n] = store.lists + store.gets
	}
	if counts[2] != counts[8] {
		t.Fatalf("the delta pass cost %d read(s) for 2 candidates and %d for 8; it must batch, not fan out", counts[2], counts[8])
	}
}

// TestDetachedOrphanBackstopHealsWhatTheEventFeedLost is the control the whole
// two-lane doctrine rests on. Events CAN be lost — an agent's bd write reaches
// the journal through a hook chain that can be killed, and graph stores emit no
// bead events at all — so an orphan nothing named must still be repaired.
func TestDetachedOrphanBackstopHealsWhatTheEventFeedLost(t *testing.T) {
	cr, store := detachedOrphanRuntime(t,
		detachedOrphanSessionBead("S-1", "sess-1"),
		detachedOrphanWorkBead("D-1", "sess-1"))

	// No observe() call: the event was lost.
	if report := cr.sweepDetachedHandoffOrphansDelta(); report.restored != 0 {
		t.Fatalf("the delta pass repaired %d bead(s) with no event naming any; it is scanning", report.restored)
	}
	if report := cr.runDetachedOrphanBackstop(backstopReasonCadence); report.restored != 1 {
		t.Fatalf("the backstop restored %d (err=%v), want 1 — a lost event is a permanent loss", report.restored, report.err)
	}
	if got := mustRoutedTo(t, store, "D-1"); got != detachedOrphanTestPool {
		t.Fatalf("D-1 gc.routed_to = %q, want %q", got, detachedOrphanTestPool)
	}
}

// TestDetachedOrphanRuntimePlaneReadsTheBindingAndNeverTheLedger is the operator
// invariant on this lane (ga-l7jdg, bd memory
// gascity-runtime-infra-store-invariant): a work-ledger leg on the tick is a
// misrouting bug by definition, and the binding is what answers instead.
//
// Its mirror is the second half: the reconcile plane does NOT narrow, so the
// convergence scan reads the ledger the tick refused. Both are asserted, because
// a plane rule that narrowed both ways would be a convergence hole.
func TestDetachedOrphanRuntimePlaneReadsTheBindingAndNeverTheLedger(t *testing.T) {
	newCity := func(t *testing.T) (storeref.ResolvedPlan, *countingRouteStore, *countingRouteStore, beads.Store) {
		t.Helper()
		sessions := beads.NewMemStoreFrom(0, []beads.Bead{detachedOrphanSessionBead("S-1", "sess-1")}, nil)
		ledger := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{detachedOrphanWorkBead("CW-1", "sess-1")}, nil)}
		binding := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{detachedOrphanWorkBead("GB-1", "sess-1")}, nil)}
		topo := assembleResidencyTopology(&config.City{}, ledger, nil,
			[]storeref.ClassBinding{{
				Classes: []coordclass.Class{coordclass.ClassGraph},
				Leg:     storeref.Leg{Ref: storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
			}}, nil)
		plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
		if err != nil {
			t.Fatalf("Plan(RoutedWork): %v", err)
		}
		return plan, ledger, binding, sessions
	}

	// A tick WITH work owes the ledger nothing: the binding answers.
	plan, ledger, binding, sessions := newCity(t)
	report := newDetachedOrphanLane().deltaPass(plan, sessions, []string{"CW-1", "GB-1"})
	if ledger.reads() != 0 || ledger.writes != 0 {
		t.Fatalf("a working tick issued %d ledger read(s) and %d write(s), want 0 — that is %v of tick at maintainer-city's RTT",
			ledger.reads(), ledger.writes, time.Duration(ledger.reads())*5400*time.Millisecond)
	}
	// Control: the binding did the work, so the ledger zero is a routing fact
	// and not a pass that declined to run.
	if binding.reads() == 0 || report.restored != 1 {
		t.Fatalf("binding reads=%d restored=%d, want non-zero and 1", binding.reads(), report.restored)
	}
	// The ledger-resident orphan is NOT repaired on the tick, and the report
	// says so rather than reporting silence.
	if report.unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1: the ledger-resident candidate waits for the convergence lane and that must be visible", report.unresolved)
	}

	// The reconcile plane does not narrow: it repairs both.
	plan, ledger, binding, sessions = newCity(t)
	conv := newDetachedOrphanLane().backstopPassOnPlane(plan, sessions, backstopReasonCadence, reconcilePlane)
	if conv.restored != 2 {
		t.Fatalf("the convergence scan restored %d (err=%v), want 2 — a leg it skips is a leg nothing converges", conv.restored, conv.err)
	}
	if ledger.reads() == 0 || binding.reads() == 0 {
		t.Fatalf("the convergence scan read ledger=%d binding=%d, want both non-zero", ledger.reads(), binding.reads())
	}
}

// TestDetachedOrphanCursorGapAndOverflowForceTheBackstop pins the two shapes of
// "the feed can no longer promise to name everything".
func TestDetachedOrphanCursorGapAndOverflowForceTheBackstop(t *testing.T) {
	now := time.Now()

	lane := newDetachedOrphanLane()
	lane.noteBackstopRan(now, backstopReasonStartup, false)
	if _, due := lane.backstopDue(now.Add(time.Minute)); due {
		t.Fatal("the scan is due a minute after a clean one; the cadence gate is not gating")
	}
	lane.force(backstopReasonCursorGap)
	if reason, due := lane.backstopDue(now.Add(time.Second)); !due || reason != backstopReasonCursorGap {
		t.Fatalf("a feed gap left the scan due=%t reason=%q, want due with %q", due, reason, backstopReasonCursorGap)
	}

	// Overflow is the same gap: more candidates than the lane will hold means
	// candidates would have to be dropped, and a dropped orphan is work nothing
	// else is looking for.
	overflow := newDetachedOrphanLane()
	overflow.noteBackstopRan(now, backstopReasonStartup, false)
	for i := range detachedOrphanCandidateCap + 1 {
		overflow.observe(beadCreatedEvent(t, detachedOrphanWorkBead(overflowBeadID(i), "sess-1")))
	}
	if reason, due := overflow.backstopDue(now); !due || reason != backstopReasonCursorGap {
		t.Fatalf("candidate overflow left the scan due=%t reason=%q, want due with %q", due, reason, backstopReasonCursorGap)
	}
	if got := overflow.takePending(); len(got) != 0 {
		t.Fatalf("overflow kept %d candidate(s); it must hand the question to the scan rather than keep a partial set", len(got))
	}

	// Control: below the cap the lane keeps its candidates and stays un-forced.
	small := newDetachedOrphanLane()
	small.noteBackstopRan(now, backstopReasonStartup, false)
	small.observe(beadCreatedEvent(t, detachedOrphanWorkBead("D-1", "sess-1")))
	if _, due := small.backstopDue(now); due {
		t.Fatal("a single named candidate forced the scan; overflow is not what the assertion above measured")
	}
	if got := small.takePending(); len(got) != 1 || got[0] != "D-1" {
		t.Fatalf("pending = %v, want [D-1]", got)
	}
}

// TestDetachedOrphanObserveKeepsOnlyTheOrphanShape: a busy city's ordinary bead
// traffic must cost the tick nothing, so the feed filter is the same predicate
// the scan uses.
func TestDetachedOrphanObserveKeepsOnlyTheOrphanShape(t *testing.T) {
	lane := newDetachedOrphanLane()
	routed := detachedOrphanWorkBead("R-1", "sess-1")
	routed.Metadata[beadmeta.RoutedToMetadataKey] = detachedOrphanTestPool
	assigned := detachedOrphanWorkBead("A-1", "sess-1")
	assigned.Assignee = "someone"
	noBranch := detachedOrphanWorkBead("N-1", "sess-1")
	delete(noBranch.Metadata, beadmeta.WorkBranchMetadataKey)

	for _, b := range []beads.Bead{routed, assigned, noBranch} {
		lane.observe(beadCreatedEvent(t, b))
	}
	if got := lane.takePending(); len(got) != 0 {
		t.Fatalf("ordinary bead traffic named %v as candidates; the tick would read for every bead a city touches", got)
	}
	// Control: the orphan shape IS named, so the filter is not simply rejecting
	// everything.
	lane.observe(beadCreatedEvent(t, detachedOrphanWorkBead("D-1", "sess-1")))
	if got := lane.takePending(); len(got) != 1 || got[0] != "D-1" {
		t.Fatalf("pending = %v, want [D-1]", got)
	}
	// And an event type that carries no bead snapshot is ignored rather than
	// decoded into a phantom candidate.
	lane.observe(events.Event{Type: events.BeadClosed, Subject: "D-2"})
	if got := lane.takePending(); len(got) != 0 {
		t.Fatalf("a payload-less event named %v", got)
	}
}

// TestDetachedOrphanBackstopAlwaysReportsItselfAndItsAge: a convergence lane runs
// on a background goroutine where a silent clean pass is indistinguishable from a
// lane that stopped.
func TestDetachedOrphanBackstopAlwaysReportsItselfAndItsAge(t *testing.T) {
	var stderr bytes.Buffer
	store := &countingRouteStore{Store: beads.NewMemStore()} // nothing to repair
	cr := &CityRuntime{cityName: "city", standaloneCityStore: store, logPrefix: "gc", stderr: &stderr}

	report := cr.runDetachedOrphanBackstop(backstopReasonCadence)
	if report.restored != 0 {
		t.Fatalf("the fixture had something to repair (restored=%d); it is not testing the QUIET pass", report.restored)
	}
	line := stderr.String()
	if !strings.Contains(line, "detached handoff orphan sweep (backstop): pass reason=cadence") {
		t.Fatalf("a quiet backstop logged %q, want a pass line naming why it was due", line)
	}
	for _, want := range []string{"legs=", "reads=", "restored=0", "took="} {
		if !strings.Contains(line, want) {
			t.Fatalf("the backstop line %q is missing %q", line, want)
		}
	}
	if report.legs == 0 {
		t.Fatal("the backstop reported 0 legs, want at least the city work leg — a pass over no leg converges nothing")
	}

	at, reason, ran := cr.detachedOrphanLaneOf().lastBackstop()
	if !ran || reason != backstopReasonCadence || time.Since(at) > time.Minute {
		t.Fatalf("lastBackstop = (%s, %q, %t), want a recent cadence pass", at, reason, ran)
	}
	// Control: a lane that has not run reports so, rather than reporting an age
	// of zero that reads as "just converged".
	if _, _, freshRan := newDetachedOrphanLane().lastBackstop(); freshRan {
		t.Fatal("a lane that never scanned reports that it did")
	}
}

// TestDetachedOrphanTickTraceCarriesTheBackstopAge: the backstop runs off-tick,
// so the tick record is the only place its liveness is observable from `gc
// trace`. The pre-lane field name survives so an operator's query still resolves.
func TestDetachedOrphanTickTraceCarriesTheBackstopAge(t *testing.T) {
	report := detachedOrphanReport{lane: "delta", restored: 0}
	fields := report.fields()
	if _, ok := fields["restored_count"]; !ok {
		t.Fatalf("the trace fields %v dropped restored_count, which is what the pre-lane leg was queried by", fields)
	}
	addBackstopAgeFields(fields, time.Time{}, "", false)
	if fields["backstop_ran"] != false {
		t.Fatalf("a lane that never converged reported backstop_ran=%v", fields["backstop_ran"])
	}
	addBackstopAgeFields(fields, time.Now().Add(-90*time.Second), backstopReasonCadence, true)
	if fields["backstop_ran"] != true || fields["backstop_age_seconds"].(int) < 89 {
		t.Fatalf("backstop age fields = %v, want a ~90s cadence age", fields)
	}
}

// TestBackstopLoopsReArmPastTheirStartupPass is the ALSO of this slice.
//
// The live profile showed both convergence lanes reporting reason=startup with
// backstop_age climbing monotonically past 23 minutes, which is what a lane that
// only runs at boot looks like — and a backstop that only runs at boot is the
// convergence hole reopened. The existing tests assert on backstopDue/sweepDue in
// isolation and prove nothing about whether the LOOP ever calls them again.
//
// This drives the real loops. Both must issue a second pass whose reason is NOT
// startup.
func TestBackstopLoopsReArmPastTheirStartupPass(t *testing.T) {
	t.Run("route recovery", func(t *testing.T) {
		cr, _ := routeRecoveryRuntime(t, unroutedWorkBead("T-1"))
		lane := cr.routeRecoveryLaneOf()
		lane.interval = time.Millisecond
		lane.poll = time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go cr.runRouteRecoveryBackstopLoop(ctx, lane)
		waitForBackstopReason(t, func() (string, bool) {
			_, reason, ran := lane.lastBackstop()
			return reason, ran
		}, backstopReasonCadence)
	})

	t.Run("detached orphans", func(t *testing.T) {
		cr, _ := detachedOrphanRuntime(t,
			detachedOrphanSessionBead("S-1", "sess-1"),
			detachedOrphanWorkBead("D-1", "sess-1"))
		lane := cr.detachedOrphanLaneOf()
		lane.interval = time.Millisecond
		lane.poll = time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go cr.runDetachedOrphanBackstopLoop(ctx, lane)
		waitForBackstopReason(t, func() (string, bool) {
			_, reason, ran := lane.lastBackstop()
			return reason, ran
		}, backstopReasonCadence)
	})
}

// waitForBackstopReason polls until a convergence lane reports the wanted
// reason, or fails with what it actually reported.
func waitForBackstopReason(t *testing.T, read func() (string, bool), want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		reason, ran := read()
		if ran {
			last = reason
			if reason == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the backstop loop never ran a %q pass; its last reason was %q — a lane that only converges at boot is the hole the lane exists to close", want, last)
}

// TestTickDeltaLanesShareOneJournalFeed pins the fan-out. Three lanes and the
// completion-fact index all consume the same tail, because a second watcher on
// the same journal would be a second cursor to keep honest and the gap semantics
// have to be identical anyway.
func TestTickDeltaLanesShareOneJournalFeed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backing := events.NewFake()
	backing.Record(beadCreatedEvent(t, unroutedWorkBead("T-1")))
	backing.Record(beadCreatedEvent(t, detachedOrphanWorkBead("D-1", "sess-1")))
	backing.Record(events.Event{Type: events.ExecutionStepCompleted, RunID: "gcg-root-1"})
	prov := &observedEventProvider{Provider: backing, observed: make(chan struct{}, 8), after: 4}
	prov.watchFrom = 0

	cr, _ := detachedOrphanRuntime(t)
	cr.startTickDeltaLanes(ctx, prov)

	select {
	case <-prov.observed:
	case <-time.After(10 * time.Second):
		t.Fatal("the feed never consumed a fourth event; it is not reaching the observers")
	}
	if got := cr.routeRecoveryLaneOf().takePending(); len(got) != 1 || got[0] != "T-1" {
		t.Fatalf("route lane pending = %v, want [T-1]", got)
	}
	if got := cr.detachedOrphanLaneOf().takePending(); len(got) != 1 || got[0] != "D-1" {
		t.Fatalf("orphan lane pending = %v, want [D-1]", got)
	}
	if got := cr.completionsLaneOf().takePending(); len(got) != 1 || got[0] != "gcg-root-1" {
		t.Fatalf("completions lane pending = %v, want [gcg-root-1]", got)
	}
}

// TestCompletionFactIndexWiringAbsorbsAndInvalidates pins the two hooks the
// warm index depends on: the feed keeps it current, and the gap hook drops it.
//
// Without the absorb hook the index would only know the facts it emitted itself
// and would duplicate every close the journal recorded elsewhere. Without the
// invalidate hook a feed that stopped promising completeness would leave the
// record permanently stale.
func TestCompletionFactIndexWiringAbsorbsAndInvalidates(t *testing.T) {
	journal := events.NewFake()
	graph := beads.NewMemStore()
	cs := &controllerState{}
	cr := &CityRuntime{cityName: "city", cs: cs, logPrefix: "gc", stderr: io.Discard}
	stores := []beads.GraphStore{{Store: graph}}

	// Warm the index against an empty corpus so Absorb has somewhere to land.
	cs.completionsDeltaIndex.ReconcileRoots(journal, stores, []string{"gcg-absent"}, "execution-reconcile")

	root, err := graph.Create(beads.Bead{Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step, err := graph.Create(beads.Bead{ID: "gcg-wired-step", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.StepIDMetadataKey:     "build",
	}})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	closed := "closed"
	if err := graph.Update(step.ID, beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-session"},
	}); err != nil {
		t.Fatalf("close step: %v", err)
	}
	live, err := graph.Get(step.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}

	// The close path records the fact; the feed hands it to the index.
	fact, ok := executionevent.LifecycleEvent(events.ExecutionStepCompleted, root, live, "close-path")
	if !ok {
		t.Fatal("the fixture step produces no lifecycle fact; the assertions below would be vacuous")
	}
	journal.Record(fact)
	cr.absorbCompletionFact(fact)
	if emitted := cs.completionsDeltaIndex.ReconcileRoots(journal, stores, []string{root.ID}, "execution-reconcile"); emitted != 0 {
		t.Fatalf("the pass emitted %d duplicate fact(s) for a close the feed already named", emitted)
	}

	// Control: an index whose feed declared a gap rebuilds — and still refuses
	// the duplicate, because the rebuild reads the journal the fact is in.
	cr.invalidateCompletionFacts()
	if emitted := cs.completionsDeltaIndex.ReconcileRoots(journal, stores, []string{root.ID}, "execution-reconcile"); emitted != 0 {
		t.Fatalf("the rebuilt index emitted %d duplicate fact(s)", emitted)
	}
	// Second control: the hooks are nil-safe on a runtime with no controller
	// state, which every one-shot and standalone runtime is.
	standalone := &CityRuntime{cityName: "city", stderr: io.Discard}
	standalone.absorbCompletionFact(fact)
	standalone.invalidateCompletionFacts()
}
