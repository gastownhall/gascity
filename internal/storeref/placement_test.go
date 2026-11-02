package storeref

// The single-owner executor.
//
// ModeSingleOwner is the placement contract's mode — "this class is created
// HERE, there is nothing to probe" — and until now it had no executor. Plan
// produced it for every Class intent and no caller could run it: ResolveOwner
// refuses a non-FirstOwner plan and Union refuses a non-Union one, so the only
// way to act on a placement plan was to reach into its legs, which is exactly
// what plan.go's mode doc warns against and what the residency-boundary guard's
// plan-leg-store-access row forbids at every consumer.
//
// These rows are the executor's contract: it names the one store, and it
// refuses the two plan shapes it is not for. The refusals are the load-bearing
// half — running a FirstOwner plan through a placement executor would silently
// place a bead in the leg that merely LEADS the probe order.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestResolvePlacementNamesTheBinding(t *testing.T) {
	f := newT1()
	plan := mustPlan(t, Class{C: coordclass.ClassGraph}, f.topo)

	leg, err := ResolvePlacement(plan)
	if err != nil {
		t.Fatalf("ResolvePlacement(%s): %v", plan, err)
	}
	want := f.legStore(ClassRef(infraClasses))
	if want == nil {
		t.Fatalf("fixture %s has no whole-split binding to name", f.name)
	}
	if leg.Store != beads.Store(want) {
		t.Fatalf("placement store = %s, want the class binding %s", storeNameOf(leg.Store), want.name)
	}
	if !IsClassRef(string(leg.Ref)) {
		t.Fatalf("placement leg ref = %q, want a class binding ref", leg.Ref)
	}
}

func TestResolvePlacementNamesTheWorkStoreWhenNothingIsRelocated(t *testing.T) {
	f := newT0()
	plan := mustPlan(t, Class{C: coordclass.ClassGraph}, f.topo)

	leg, err := ResolvePlacement(plan)
	if err != nil {
		t.Fatalf("ResolvePlacement(%s): %v", plan, err)
	}
	if leg.Store != beads.Store(f.work) {
		t.Fatalf("placement store = %s, want the work store %s", storeNameOf(leg.Store), f.work.name)
	}
	if leg.Ref != WorkRef {
		t.Fatalf("placement leg ref = %q, want the work ref", leg.Ref)
	}
}

// A refused city must not place an infrastructure-class bead at all. The
// refusal is the whole point: the binding is the owner, this build must not
// serve it, and writing to the work ledger instead is the stranded write the
// refusal exists to prevent.
func TestResolvePlacementCarriesTheStandingRefusal(t *testing.T) {
	if _, err := Plan(Class{C: coordclass.ClassGraph}, newT3().topo); err == nil {
		t.Fatal("planning a placement on a REFUSED city succeeded; a refused binding has no placement to hand out")
	}
}

func TestResolvePlacementRejectsAFirstOwnerPlan(t *testing.T) {
	f := newT1()
	plan := mustPlan(t, ByID{ID: workShapedID}, f.topo)
	if _, err := ResolvePlacement(plan); err == nil {
		t.Fatal("ResolvePlacement accepted a FirstOwner plan; its leading leg merely leads a PROBE order and is not a placement")
	}
}

func TestResolvePlacementRejectsAUnionPlan(t *testing.T) {
	f := newT2()
	plan := mustPlan(t, RoutedWork{}, f.topo)
	if _, err := ResolvePlacement(plan); err == nil {
		t.Fatal("ResolvePlacement accepted a Union plan; the mode decides the executor")
	}
}

// The other two executors must refuse a placement plan for the same reason.
// Without this row a Class plan could be run through ResolveOwner, which
// PROBES its leg and reports absence as a miss — turning "create it here" into
// "there is nothing here".
func TestTheOtherExecutorsRejectAPlacementPlan(t *testing.T) {
	f := newT1()
	plan := mustPlan(t, Class{C: coordclass.ClassGraph}, f.topo)
	if _, _, err := ResolveOwner(plan, workShapedID); err == nil {
		t.Error("ResolveOwner accepted a SingleOwner plan")
	}
	if _, err := Union(plan, beadID, listOpenLeg); err == nil {
		t.Error("Union accepted a SingleOwner plan")
	}
}
