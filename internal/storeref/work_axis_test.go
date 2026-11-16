package storeref

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// countingRouter is the corpus's and these tests' stand-in for a plane whose
// by-id work axis is ROUTED: it answers with a fixed ref list and counts how
// often it was asked, so "the router is not consulted for an id the binding
// mints" is an assertion rather than a claim.
type countingRouter struct {
	refs  []StoreRef
	calls *int
	ids   *[]string
}

func newCountingRouter(refs ...StoreRef) *countingRouter {
	n := 0
	var ids []string
	return &countingRouter{refs: refs, calls: &n, ids: &ids}
}

func (r *countingRouter) WorkLegsForID(id string) []StoreRef {
	*r.calls++
	*r.ids = append(*r.ids, id)
	return r.refs
}

// routedRefs is the two-leg routed axis the corpus rows use: the work store
// the plane's router pinned, and a rig behind it. Only the topologies with
// that rig hold a store for it; on the others the route is a plan error.
func routedRefs() []StoreRef { return []StoreRef{WorkRef, RigRef("bravo")} }

// TestByIDRoutedWorkAxisReplacesTheProbedTail pins the whole point of the
// field: when a plane's own by-id router has already resolved the work axis for
// an id no binding namespace claims, those legs — in that order — are the plan's
// tail, and this package's [work, covering-shadows] rule does not run.
func TestByIDRoutedWorkAxisReplacesTheProbedTail(t *testing.T) {
	f := newT2()
	router := newCountingRouter(routedRefs()...)

	plan := mustPlan(t, ByID{ID: rigShapedID, WorkAxis: router}, f.topo)

	want := `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:bravo[Shadow,Fatal]`
	if got := plan.String(); got != want {
		t.Fatalf("plan = %s\nwant %s", got, want)
	}
	if *router.calls != 1 {
		t.Fatalf("router consulted %d times, want exactly 1", *router.calls)
	}
	if len(*router.ids) != 1 || (*router.ids)[0] != rigShapedID {
		t.Fatalf("router asked about %v, want [%s]", *router.ids, rigShapedID)
	}
	// The shadow rule did NOT also run: rig:alpha covers ra-7 and would be in
	// the plan if both tails were applied.
	for _, leg := range plan.Legs {
		if leg.Leg.Ref == RigRef("alpha") {
			t.Fatalf("plan kept the shadow leg %s alongside the routed axis: %s", leg.Leg.Ref, plan)
		}
	}
}

// TestByIDRoutedWorkAxisIsNotConsultedInsideANamespace is the laziness pin and
// the correctness one at once.
//
// Inside a binding's reserved namespace the binding is the AUTHORITY and the
// work axis is a probe list behind it — the plane's router answers a different
// question there (a shadowing rig prefix would capture the id, and the routes
// read is a disk hit on the hot path). So the router must not be called at all.
func TestByIDRoutedWorkAxisIsNotConsultedInsideANamespace(t *testing.T) {
	f := newT2()
	router := newCountingRouter(routedRefs()...)

	plan := mustPlan(t, ByID{ID: graphNamespacedID, WorkAxis: router}, f.topo)

	if *router.calls != 0 {
		t.Fatalf("router consulted %d times for an id the binding mints, want 0", *router.calls)
	}
	if got, want := plan.String(), residencyCorpus["ByID(gcg-abc) x T2"]; got != want {
		t.Fatalf("plan = %s\nwant the unrouted row %s", got, want)
	}
}

// TestByIDRoutedWorkAxisFallsBackWhenItDeclines pins that a router with nothing
// to say leaves the package's own rule in place, so the field is safe to set
// unconditionally.
func TestByIDRoutedWorkAxisFallsBackWhenItDeclines(t *testing.T) {
	f := newT2()
	router := newCountingRouter()

	plan := mustPlan(t, ByID{ID: rigShapedID, WorkAxis: router}, f.topo)

	if got, want := plan.String(), residencyCorpus["ByID(ra-7) x T2"]; got != want {
		t.Fatalf("plan = %s\nwant the unrouted row %s", got, want)
	}
}

// TestByIDRoutedWorkAxisSingleLegIsReturnedUnprobed is the byte-identity gate:
// a routed axis of exactly one leg makes that leg the plan's residual, so
// ResolveOwner hands it back WITHOUT reading it and the caller's own read
// produces the caller's own error message — which is what keeps a routed
// surface's answers unchanged.
func TestByIDRoutedWorkAxisSingleLegIsReturnedUnprobed(t *testing.T) {
	f := newT2()
	routed := f.rigs["bravo"]
	router := newCountingRouter(RigRef("bravo"))

	plan := mustPlan(t, ByID{ID: workShapedID, WorkAxis: router}, f.topo)
	binding := f.legStore(ClassRef(infraClasses))

	store, ref, err := ResolveOwner(plan, workShapedID)
	if err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	if store != beads.Store(routed) {
		t.Fatalf("ResolveOwner pinned %s, want the routed leg", storeNameOf(store))
	}
	if ref != RigRef("bravo") {
		t.Fatalf("ResolveOwner returned ref %q, want %q", ref, RigRef("bravo"))
	}
	if *routed.gets != 0 {
		t.Fatalf("the residual leg was read %d times, want 0 — it is returned unprobed", *routed.gets)
	}
	if *binding.gets != 1 {
		t.Fatalf("the residence probe ran %d times, want 1", *binding.gets)
	}
}

// TestByIDRoutedWorkAxisScanProvesAbsence is the other miss shape: a routed axis
// of several legs ends in a Shadow, so every leg is read and a clean miss is
// proven absence rather than a residual handed back.
func TestByIDRoutedWorkAxisScanProvesAbsence(t *testing.T) {
	f := newT2()
	router := newCountingRouter(routedRefs()...)

	plan := mustPlan(t, ByID{ID: workShapedID, WorkAxis: router}, f.topo)
	store, _, err := ResolveOwner(plan, workShapedID)
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("ResolveOwner err = %v, want ErrNotFound", err)
	}
	if store != nil {
		t.Fatalf("ResolveOwner returned store %s alongside a not-found", storeNameOf(store))
	}
	for ref, named := range map[StoreRef]*namedStore{WorkRef: f.work, RigRef("bravo"): f.rigs["bravo"]} {
		if *named.gets != 1 {
			t.Fatalf("routed leg %q was read %d times, want 1 — a scan proves absence by reading every leg", ref, *named.gets)
		}
	}
}

// TestByIDRoutedWorkAxisLegErrorIsNeverAbsence keeps the fail-loud rule on the
// legs a plane supplied: they are Fatal, so an unreachable one is named rather
// than skipped.
//
// Control: the same leg answering ErrNotFound continues to the next, which is
// what proves the failure above is about the ERROR and not about the leg being
// present.
func TestByIDRoutedWorkAxisLegErrorIsNeverAbsence(t *testing.T) {
	f := newT2()
	f.work.getErr = errors.New("routed backend unreachable")
	tail := f.rigs["bravo"]
	router := newCountingRouter(routedRefs()...)

	plan := mustPlan(t, ByID{ID: workShapedID, WorkAxis: router}, f.topo)
	if _, _, err := ResolveOwner(plan, workShapedID); err == nil {
		t.Fatal("ResolveOwner succeeded over an unreachable routed leg — a read error reported as absence is the root-loss shape")
	} else if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("ResolveOwner err = %v, want a named read failure rather than ErrNotFound", err)
	}
	if *tail.gets != 0 {
		t.Fatalf("resolution continued past the failing leg (%d reads on the tail)", *tail.gets)
	}
}

// TestByIDRoutedWorkAxisReadsEachRefsLegFromTheTopology pins the ref
// mapping: a routed ref is the topology's own leg (its store, its ref), a
// suspended rig's included, since the route is the plane's and the by-id door
// reads a rig it routes to; and a ref the topology holds no store for is a
// plan error that names it, never a skipped leg.
func TestByIDRoutedWorkAxisReadsEachRefsLegFromTheTopology(t *testing.T) {
	f := newT2()
	for i := range f.topo.Rigs {
		if f.topo.Rigs[i].Ref == RigRef("bravo") {
			f.topo.Rigs[i].Suspended = true
		}
	}
	plan := mustPlan(t, ByID{ID: workShapedID, WorkAxis: newCountingRouter(RigRef("bravo"), WorkRef)}, f.topo)
	if got, want := plan.String(), `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > rig:bravo[WorkFallback,Fatal] > ""[Shadow,Fatal]`; got != want {
		t.Fatalf("plan = %s\nwant %s", got, want)
	}
	if got := plan.Legs[1].Leg.Store; got != beads.Store(f.rigs["bravo"]) {
		t.Fatalf("the routed rig leg reads %s, want the topology's suspended bravo store", storeNameOf(got))
	}
	if got := plan.Legs[2].Leg.Store; got != beads.Store(f.work) {
		t.Fatalf("the routed work leg reads %s, want the topology's work store", storeNameOf(got))
	}

	for name, topo := range map[string]Topology{"an unknown rig": newT2().topo, "a work leg with no store": {Rigs: newT2().topo.Rigs}} {
		ref := RigRef("routed")
		if name == "a work leg with no store" {
			ref = WorkRef
		}
		_, err := Plan(ByID{ID: workShapedID, WorkAxis: newCountingRouter(ref)}, topo)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", legName(ref))) {
			t.Errorf("%s: plan err = %v, want a plan error naming %q", name, err, legName(ref))
		}
	}
}
