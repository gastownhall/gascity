package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Native input admission on the controller's demand side (#8832): a withheld
// step — one whose workflow input is held, blocked, deferred or waiting on a
// prerequisite — is not work for its assignee. The hook's claim path will not
// serve it, so counting it as keep-awake work, wake demand, drain-ack
// claimability or ladder work wakes or holds a session for work it cannot
// claim: the loop a dispatch hold caused (ga-5736js), read through the same
// live verdict the ready reader applies.
//
// Every probe runs on both topologies (forEachTopology). On the split one the
// workflow spans two stores — its root and steps in the class binding, its
// input convoy and input in the work store — so a probe that judged the store
// it read the step from alone saw a root with no members and admitted the
// step the hook refuses.

// inputAdmissionSeatFixture is one native workflow over a held input: the
// input bead and its tracking convoy in the work store, and in the graph
// store (the binding on a split city) the root recording gc.input_convoy_id,
// an OPEN step assigned to seat-1, and an OPEN unassigned step (a trigger).
type inputAdmissionSeatFixture struct {
	e       splitEnv
	input   beads.Bead
	conv    beads.Bead
	root    beads.Bead
	step    beads.Bead
	trigger beads.Bead
	graph   beads.Store
}

// materializeInputAdmissionSeat stamps the fixture into e's stores and holds
// the input on hold:external.
func materializeInputAdmissionSeat(t *testing.T, e splitEnv) inputAdmissionSeatFixture {
	t.Helper()
	fx := inputAdmissionSeatFixture{e: e, graph: e.graphStore()}
	mustCreateBead(t, e.work, &fx.input, beads.Bead{Title: "input", Type: "task", Labels: []string{"hold:external"}})
	mustCreateBead(t, e.work, &fx.conv, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := e.work.DepAdd(fx.conv.ID, fx.input.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd tracks: %v", err)
	}
	mustCreateBead(t, fx.graph, &fx.root, beads.Bead{
		Title:    "workflow root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.input_convoy_id": fx.conv.ID},
	})
	mustCreateBead(t, fx.graph, &fx.step, beads.Bead{
		Title:    "graph-step",
		Type:     "task",
		Assignee: "seat-1",
		Metadata: map[string]string{"gc.root_bead_id": fx.root.ID},
	})
	mustCreateBead(t, fx.graph, &fx.trigger, beads.Bead{
		Title:    "unclaimed graph-step",
		Type:     "task",
		Metadata: map[string]string{"gc.root_bead_id": fx.root.ID},
	})
	return fx
}

// legs is the leg set the controller mints for the fixture's city.
func (fx inputAdmissionSeatFixture) legs() WorkLegs {
	return testWorkLegs(fx.e.cityPath, fx.e.cfg, fx.e.work, nil)
}

// probe is a fresh read's input-admission probe, as one tick's SeatWork
// builds it.
func (fx inputAdmissionSeatFixture) probe() *inputAdmissionProbe {
	return newSeatWork(fx.legs()).inputAdmission()
}

// release lifts the input's hold.
func (fx inputAdmissionSeatFixture) release(t *testing.T) {
	t.Helper()
	if err := fx.e.work.Update(fx.input.ID, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
		t.Fatalf("releasing hold: %v", err)
	}
}

// mustCreateBead creates b in store and stores the created row back through
// out.
func mustCreateBead(t *testing.T, store beads.Store, out *beads.Bead, b beads.Bead) {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("creating %q: %v", b.Title, err)
	}
	*out = created
}

// TestNativeInputAdmissionKeepAwakeIgnoresWithheldAssignedStep covers AC-5's
// keep-awake arm: a seat whose only assigned ready work is a step input
// admission withholds does not keep the session awake, and does again once
// the input's hold is released.
func TestNativeInputAdmissionKeepAwakeIgnoresWithheldAssignedStep(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		fx := materializeInputAdmissionSeat(t, e)
		has, err := sessionHasReadyAssignedWorkForTier(fx.graph, "seat-1", beads.TierIssues, fx.probe())
		if err != nil {
			t.Fatalf("sessionHasReadyAssignedWorkForTier: %v", err)
		}
		if has {
			t.Fatalf("keep-awake counts assigned step %s whose input %s carries hold:external; want it ignored", fx.step.ID, fx.input.ID)
		}
		fx.release(t)
		has, err = sessionHasReadyAssignedWorkForTier(fx.graph, "seat-1", beads.TierIssues, fx.probe())
		if err != nil {
			t.Fatalf("sessionHasReadyAssignedWorkForTier after release: %v", err)
		}
		if !has {
			t.Fatalf("keep-awake omits assigned step %s whose input's hold is released; want it counted", fx.step.ID)
		}
	})
}

// TestNativeInputAdmissionKeepAwakeKeepsInProgressStep covers AC-7's ownership
// arm on the same probe: an in_progress step is existing ownership and is
// never judged, whatever its input carries.
func TestNativeInputAdmissionKeepAwakeKeepsInProgressStep(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		fx := materializeInputAdmissionSeat(t, e)
		status := "in_progress"
		if err := fx.graph.Update(fx.step.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("starting step: %v", err)
		}
		has, err := sessionHasAwakeAssignedWorkInStoreByIdentifiers(fx.graph, []string{"seat-1"}, fx.probe())
		if err != nil {
			t.Fatalf("sessionHasAwakeAssignedWorkInStoreByIdentifiers: %v", err)
		}
		if !has {
			t.Fatalf("keep-awake omits in_progress step %s; input admission must never judge existing ownership", fx.step.ID)
		}
	})
}

// TestNativeInputAdmissionDrainAckClassifiesWithheldAsNonClaimable covers
// AC-5's drain-ack arm: an open assigned row input admission withholds is
// provably non-claimable — the seat that drained past it pulled correctly —
// and is not once the input's hold is released.
func TestNativeInputAdmissionDrainAckClassifiesWithheldAsNonClaimable(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		fx := materializeInputAdmissionSeat(t, e)
		nonClaimable, err := openAssignedRowProvablyNonClaimable(fx.graph, fx.step, time.Now(), fx.probe())
		if err != nil {
			t.Fatalf("openAssignedRowProvablyNonClaimable: %v", err)
		}
		if !nonClaimable {
			t.Fatalf("open assigned step %s whose input %s carries hold:external classifies claimable; want input_withheld proof", fx.step.ID, fx.input.ID)
		}
		fx.release(t)
		nonClaimable, err = openAssignedRowProvablyNonClaimable(fx.graph, fx.step, time.Now(), fx.probe())
		if err != nil {
			t.Fatalf("openAssignedRowProvablyNonClaimable after release: %v", err)
		}
		if nonClaimable {
			t.Fatalf("step %s whose input's hold is released still classifies non-claimable; want it settled on its own facts", fx.step.ID)
		}
	})
}

// TestNativeInputAdmissionParkIgnoresWithheldReadyStep covers AC-5's park
// arm: a seat whose only work is a ready step input admission withholds is
// parked, and is not once the input's hold is released.
func TestNativeInputAdmissionParkIgnoresWithheldReadyStep(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		fx := materializeInputAdmissionSeat(t, e)
		work := []strandedAssignedWork{{bead: fx.step, store: fx.graph}}
		parked, err := liveParkedWork(work, nil, releaseSeat{}, time.Now(), fx.probe())
		if err != nil {
			t.Fatalf("liveParkedWork: %v", err)
		}
		if !parked {
			t.Fatalf("seat holding only step %s, whose input %s carries hold:external, is not parked; want it parked", fx.step.ID, fx.input.ID)
		}
		fx.release(t)
		parked, err = liveParkedWork(work, nil, releaseSeat{}, time.Now(), fx.probe())
		if err != nil {
			t.Fatalf("liveParkedWork after release: %v", err)
		}
		if parked {
			t.Fatalf("seat holding step %s, whose input's hold is released, is parked; want its ready work to keep it", fx.step.ID)
		}
	})
}

// TestNativeInputAdmissionLadderHoldsWithheldWork covers AC-5's ladder arm:
// the allocation's assigned step, its pre-nudge re-check and an unclaimed
// trigger of the same workflow are not the ladder's while the input is held,
// and are once it is released.
func TestNativeInputAdmissionLadderHoldsWithheldWork(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		fx := materializeInputAdmissionSeat(t, e)
		pass := func() *ladderPass {
			legs := fx.legs()
			return &ladderPass{env: externalReadsEnv{WorkLegs: legs}, cfg: e.cfg, admit: newInputAdmissionProbe(legs)}
		}
		summary := &allocSummary{AssignedWork: []beads.Bead{fx.step}, AssignedStores: []beads.Store{fx.graph}}
		p := pass()
		if held := p.assignedInputWithheld(summary); !held[fx.step.ID] {
			t.Fatalf("ladder holds %v, want assigned step %s whose input %s carries hold:external", held, fx.step.ID, fx.input.ID)
		}
		if got := p.unlessHeld(backstopResolutionOutstanding, fx.step, fx.graph); got != backstopResolutionClear {
			t.Fatalf("pre-nudge re-check of step %s = %v, want clear while its input is held", fx.step.ID, got)
		}
		if got := p.triggerRecheck(fx.trigger.ID)(); got != backstopResolutionClear {
			t.Fatalf("trigger re-check of %s = %v, want clear while its input is held", fx.trigger.ID, got)
		}

		fx.release(t)
		p = pass()
		if held := p.assignedInputWithheld(summary); held[fx.step.ID] {
			t.Fatalf("ladder holds %v after the hold was released; want step %s free", held, fx.step.ID)
		}
		if got := p.unlessHeld(backstopResolutionOutstanding, fx.step, fx.graph); got != backstopResolutionOutstanding {
			t.Fatalf("pre-nudge re-check of step %s = %v after release, want outstanding", fx.step.ID, got)
		}
		if got := p.triggerRecheck(fx.trigger.ID)(); got != backstopResolutionOutstanding {
			t.Fatalf("trigger re-check of %s = %v after release, want outstanding", fx.trigger.ID, got)
		}
	})
}

// TestInputAdmissionProbeJudgesNothingWithoutLegs pins the probe's failure
// rule: a leg set that cannot be planned judges nothing (the probe keeps its
// ungated answer) rather than failing the seat decision.
func TestInputAdmissionProbeJudgesNothingWithoutLegs(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		fx := materializeInputAdmissionSeat(t, e)
		probe := newInputAdmissionProbe(WorkLegs{})
		if got := probe.withheld(fx.graph, []beads.Bead{fx.step}); got != nil {
			t.Fatalf("probe over no leg set withheld %v, want nothing", got)
		}
		if !probe.failed() {
			t.Fatal("probe over no leg set did not record why it judges nothing")
		}
	})
}
