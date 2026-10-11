package storeref

import (
	"errors"
	"strings"
	"testing"
)

// Walk is the executor for the union questions whose answer is not a merged row
// set — an existence probe that stops at the first yes, a release sweep that
// mutates as it goes, a pass bounded by a time budget. Those were the sites that
// used to hand-roll a []beads.Store and read it themselves, so the executor has
// to carry the same per-leg error policy Union does or the migration would trade
// one restatement for another.

func TestWalkVisitsEveryLegInPlanOrder(t *testing.T) {
	f := newT2()
	plan := mustPlan(t, AssignedWork{}, f.topo)

	var visited []string
	res, err := Walk(plan, func(l Leg) (bool, error) {
		visited = append(visited, string(l.Ref))
		return false, nil
	})
	if err != nil {
		t.Fatalf("Walk over healthy legs: %v", err)
	}
	if got, want := strings.Join(visited, ">"), `>rig:alpha>rig:bravo>class:gmnos`; got != want {
		t.Fatalf("Walk visited %q, want the sweep plan's own order %q", got, want)
	}
	if res.Stopped || res.Partial || res.Visited != 4 {
		t.Fatalf("Walk result = %+v, want a complete four-leg pass", res)
	}
}

func TestWalkStopsAtTheFirstLegThatAnswers(t *testing.T) {
	f := newT2()
	plan := mustPlan(t, AssignedWork{}, f.topo)

	visits := 0
	res, err := Walk(plan, func(l Leg) (bool, error) {
		visits++
		return l.Ref == RigRef("alpha"), nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !res.Stopped || res.StoppedAt != RigRef("alpha") {
		t.Fatalf("Walk result = %+v, want a stop at rig:alpha", res)
	}
	if visits != 2 {
		t.Fatalf("Walk read %d legs after the answer, want it to stop at the second: an existence probe that keeps reading pays for every leg of every tick", visits)
	}
}

// A Fatal leg failing fails the whole pass with the store NAMED. On the release
// and gate paths that is the difference between "this session holds nothing" and
// "one ledger could not be read", and reading the second as the first is how a
// live claim-holder gets drained.
func TestWalkFatalLegFailsTheWholePassWithTheStoreNamed(t *testing.T) {
	f := newT2()
	boom := errors.New("binding unreachable")
	f.bindings[ClassRef(infraClasses)].listErr = boom
	plan := mustPlan(t, AssignedWork{}, f.topo)

	_, err := Walk(plan, func(l Leg) (bool, error) {
		if _, lerr := l.Store.ListOpen(); lerr != nil {
			return false, lerr
		}
		return false, nil
	})
	if err == nil {
		t.Fatal("an unreadable BINDING leg completed the walk; a work-only answer is indistinguishable from \"the DAG finished\"")
	}
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "class:gmnos") {
		t.Fatalf("Walk error = %v, want the failure wrapped and the leg named", err)
	}
}

// Control that fails DIFFERENTLY: a rig leg failing is one scope reporting a
// hole. The pass completes, and the result says so rather than presenting a
// smaller answer as authoritative.
func TestWalkRigLegFailureIsPartialNotFatal(t *testing.T) {
	f := newT2()
	boom := errors.New("rig dark")
	f.rigs["alpha"].listErr = boom
	plan := mustPlan(t, AssignedWork{}, f.topo)

	var visited []string
	res, err := Walk(plan, func(l Leg) (bool, error) {
		if _, lerr := l.Store.ListOpen(); lerr != nil {
			return false, lerr
		}
		visited = append(visited, string(l.Ref))
		return false, nil
	})
	if err != nil {
		t.Fatalf("a PartialDegrade leg aborted the walk: %v", err)
	}
	if !res.Partial || len(res.LegErrors) != 1 || res.LegErrors[0].Ref != RigRef("alpha") {
		t.Fatalf("Walk result = %+v, want Partial with rig:alpha named", res)
	}
	if got, want := strings.Join(visited, ">"), `>rig:bravo>class:gmnos`; got != want {
		t.Fatalf("Walk visited %q after the degraded leg, want %q — the remaining legs still answer", got, want)
	}
}

// The one tolerated non-fault: a standing storage refusal on a leg whose policy
// tolerates it. Nothing in this build plans a RefusalTolerated union leg, so the
// case is asserted on a hand-built plan rather than left unexercised.
func TestWalkToleratesAStandingRefusalOnATolerantLeg(t *testing.T) {
	f := newT1()
	refusal := newRefusal()
	binding := f.bindings[ClassRef(infraClasses)]
	binding.listErr = refusal

	plan := ResolvedPlan{Mode: ModeUnion, Legs: []PlanLeg{
		{Leg: f.topo.Work, Role: RoleAuthority, OnError: PolicyFatal},
		{Leg: f.topo.Bindings[0].Leg, Role: RoleResidenceProbe, OnError: PolicyRefusalTolerated},
	}}
	res, err := Walk(plan, func(l Leg) (bool, error) {
		if _, lerr := l.Store.ListOpen(); lerr != nil {
			return false, lerr
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("a tolerated standing refusal failed the walk: %v", err)
	}
	if res.Partial {
		t.Fatalf("a tolerated refusal marked the result Partial: %+v — the city is refused, not degraded", res)
	}
}

func TestWalkRefusesANonUnionPlan(t *testing.T) {
	f := newT1()
	plan := mustPlan(t, ByID{ID: graphNamespacedID}, f.topo)
	if _, err := Walk(plan, func(Leg) (bool, error) { return false, nil }); err == nil {
		t.Fatal("Walk accepted a FirstOwner plan; a by-id probe list has ResolveOwner's short-circuit, not this one's")
	}
}

// Union is implemented ON Walk so the two executors cannot disagree about leg
// order or error policy — which would be this contract's own bug class, one
// level up. Asserted rather than assumed: the two must report the same partial
// legs for the same failure.
func TestUnionAndWalkAgreeAboutADegradedLeg(t *testing.T) {
	f := newT2()
	boom := errors.New("rig dark")
	f.rigs["bravo"].listErr = boom
	plan := mustPlan(t, AssignedWork{}, f.topo)

	union, err := Union(plan, beadID, listOpenLeg)
	if err != nil {
		t.Fatalf("Union: %v", err)
	}
	walk, err := Walk(plan, func(l Leg) (bool, error) {
		_, lerr := l.Store.ListOpen()
		return false, lerr
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if union.Partial != walk.Partial || len(union.LegErrors) != len(walk.LegErrors) {
		t.Fatalf("Union reported (partial=%v, %d leg errors) and Walk (partial=%v, %d) for the same failure", union.Partial, len(union.LegErrors), walk.Partial, len(walk.LegErrors))
	}
	if union.LegErrors[0].Ref != walk.LegErrors[0].Ref {
		t.Fatalf("Union named %q and Walk named %q", union.LegErrors[0].Ref, walk.LegErrors[0].Ref)
	}
}
