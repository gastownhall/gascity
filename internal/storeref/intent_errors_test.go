package storeref

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The error-injection corpus: each T7 intent over the whole split with two
// rigs (T2), one leg failing, run the way its consumers run it, with the
// outcome pinned as a row. The plans' policies say what a failure means at
// the executor; the visitors below are the consumers' own rules, so a row
// shows the two together.

// noSeed seeds no leg (WorkRef is the empty ref, so it cannot be the
// sentinel).
const noSeed StoreRef = "<none>"

// errDown is the injected leg failure.
var errDown = errors.New("leg down")

// failLeg makes the store behind ref fail every read.
func failLeg(f topoFixture, ref StoreRef) {
	f.legStore(ref).getErr = errDown
}

// walkOutcome renders a walk's result as a corpus row.
func walkOutcome(res WalkResult, err error, visited []StoreRef) string {
	var refs []string
	for _, r := range visited {
		refs = append(refs, legName(r))
	}
	out := "visited [" + strings.Join(refs, ", ") + "]"
	if res.Partial {
		var degraded []string
		for _, le := range res.LegErrors {
			degraded = append(degraded, legName(le.Ref))
		}
		out += " partial [" + strings.Join(degraded, ", ") + "]"
	}
	if err != nil {
		out += " error: " + err.Error()
	}
	return out
}

// probe is a consumer's per-leg read: a miss is nothing, any other error is
// the visitor's to judge.
func probe(leg TypedLeg) error {
	_, err := leg.Reader.Get("ga-1")
	if errors.Is(err, beads.ErrNotFound) {
		return nil
	}
	return err
}

// slingGuard is the sling source guard's visitor (sling_core.go
// toleratesScanFailure): a failure on the Authority leg (the relocated graph
// binding) or on the source store refuses, and so does any failure without a
// warning sink; any other is a warning and the walk goes on.
func slingGuard(source StoreRef, sink bool, warned *[]StoreRef) func(TypedLeg) error {
	return func(leg TypedLeg) error {
		err := probe(leg)
		if err == nil {
			return nil
		}
		if leg.Role == RoleAuthority || leg.Ref == source || !sink {
			return err
		}
		*warned = append(*warned, leg.Ref)
		return nil
	}
}

// TestWorkflowConsumersErrorInjection: every Workflow leg is Fatal at the
// executor, and each consumer's own visitor keeps (or refuses) the failure as
// it does today: dispatch's live-root scan, the API's run cancel and `gc
// workflow delete` fail on any leg; the sling guard refuses on the graph
// binding, on the source store and without a sink, and warns past the rest.
func TestWorkflowConsumersErrorInjection(t *testing.T) {
	fatal := probe
	cases := []struct {
		name   string
		fail   StoreRef
		unsplt bool
		visit  func(warned *[]StoreRef) func(TypedLeg) error
		want   string
	}{
		{
			"dispatch live-root scan, a rig fails", RigRef("alpha"), false,
			func(*[]StoreRef) func(TypedLeg) error { return fatal },
			`visited [class:gmnos, the city work store, rig:alpha] partial [] error: reading rig:alpha: leg down`,
		},
		{
			"API cancelRun, the city fails", WorkRef, false,
			func(*[]StoreRef) func(TypedLeg) error { return fatal },
			`visited [class:gmnos, the city work store] partial [] error: reading the city work store: leg down`,
		},
		{
			"gc workflow delete, the binding fails", "class:gmnos", false,
			func(*[]StoreRef) func(TypedLeg) error { return fatal },
			`visited [class:gmnos] partial [] error: reading class:gmnos: leg down`,
		},
		{
			"sling guard, the binding fails", "class:gmnos", false,
			func(w *[]StoreRef) func(TypedLeg) error { return slingGuard(WorkRef, true, w) },
			`visited [class:gmnos] partial [] error: reading class:gmnos: leg down`,
		},
		{
			"sling guard, the source store fails", WorkRef, false,
			func(w *[]StoreRef) func(TypedLeg) error { return slingGuard(WorkRef, true, w) },
			`visited [class:gmnos, the city work store] partial [] error: reading the city work store: leg down`,
		},
		{
			"sling guard, another rig fails, with a sink", RigRef("bravo"), false,
			func(w *[]StoreRef) func(TypedLeg) error { return slingGuard(WorkRef, true, w) },
			`visited [class:gmnos, the city work store, rig:alpha, rig:bravo] warned [rig:bravo]`,
		},
		{
			"sling guard, another rig fails, no sink", RigRef("bravo"), false,
			func(w *[]StoreRef) func(TypedLeg) error { return slingGuard(WorkRef, false, w) },
			`visited [class:gmnos, the city work store, rig:alpha, rig:bravo] partial [] error: reading rig:bravo: leg down`,
		},
		{
			"sling guard, single store, the city fails, source a rig", WorkRef, true,
			func(w *[]StoreRef) func(TypedLeg) error { return slingGuard(RigRef("alpha"), true, w) },
			`visited [the city work store, rig:alpha, rig:bravo] warned [the city work store]`,
		},
	}
	for _, tc := range cases {
		f := newT2()
		if tc.unsplt {
			f = buildTopology("T2u", map[string]string{"alpha": alphaPrefix, "bravo": bravoPrefix}, nil, nil)
		}
		failLeg(f, tc.fail)
		plan, err := NewStores(f.topo, StoresOptions{}).Plan(Workflow{SuspendedRigs: WorkflowReadsSuspendedRigs})
		if err != nil {
			t.Fatal(err)
		}
		var visited, warned []StoreRef
		visit := tc.visit(&warned)
		res, err := TypedWalk(plan, func(leg TypedLeg) (bool, error) {
			visited = append(visited, leg.Ref)
			return false, visit(leg)
		})
		got := walkOutcome(res, err, visited)
		if len(warned) > 0 {
			var w []string
			for _, r := range warned {
				w = append(w, legName(r))
			}
			got += " warned [" + strings.Join(w, ", ") + "]"
		}
		if got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

// censusRefs are a whole-split census's work legs, by ref (cmd/gc WorkLegs
// without the graph binding).
var censusRefs = []StoreRef{WorkRef, RigRef("alpha"), RigRef("bravo")}

// TestIntentKindsErrorInjection: each molecule kind and OrderRuns with one
// leg failing, run through the executor its consumers use.
func TestIntentKindsErrorInjection(t *testing.T) {
	type run func(f topoFixture, p ResolvedPlan) string
	firstOwner := func(_ topoFixture, p ResolvedPlan) string {
		owner, err := ResolveOwnerRow(p, "ga-1")
		if err != nil {
			return "error: " + err.Error()
		}
		return "owner " + legName(owner.Ref)
	}
	union := func(_ topoFixture, p ResolvedPlan) string {
		var visited []StoreRef
		res, err := TypedWalk(p, func(leg TypedLeg) (bool, error) {
			visited = append(visited, leg.Ref)
			return false, probe(leg)
		})
		return walkOutcome(res, err, visited)
	}
	cases := []struct {
		name   string
		intent func(f topoFixture) Intent
		fail   StoreRef
		seed   StoreRef
		exec   run
		want   string
	}{
		{
			"RootNear, the graph fails", func(f topoFixture) Intent {
				return Molecule{Kind: MoleculeRootNear, Near: f.work, NearRef: WorkRef}
			}, "class:gmnos", WorkRef, firstOwner,
			`error: reading "ga-1" from class:gmnos: leg down`,
		},
		{
			"ParentNear, near fails", func(f topoFixture) Intent {
				return Molecule{Kind: MoleculeParentNear, Near: f.work, NearRef: WorkRef}
			}, WorkRef, "class:gmnos", firstOwner,
			`error: reading "ga-1" from the city work store: leg down`,
		},
		{
			"Attachment, the graph fails, near holds it", func(f topoFixture) Intent {
				return Molecule{Kind: MoleculeAttachment, Near: f.work, NearRef: WorkRef}
			}, "class:gmnos", WorkRef, firstOwner,
			`owner the city work store`,
		},
		{
			"Attachment, the graph fails, nothing holds it", func(f topoFixture) Intent {
				return Molecule{Kind: MoleculeAttachment, Near: f.work, NearRef: WorkRef}
			}, "class:gmnos", noSeed, firstOwner,
			`error: reading "ga-1" from class:gmnos: leg down`,
		},
		{
			"Roots, a rig fails", func(topoFixture) Intent { return Molecule{Kind: MoleculeRoots, Work: censusRefs} },
			RigRef("alpha"), noSeed, union,
			`visited [class:gmnos, the city work store, rig:alpha, rig:bravo] partial [rig:alpha]`,
		},
		{
			"Roots, the graph fails", func(topoFixture) Intent { return Molecule{Kind: MoleculeRoots, Work: censusRefs} },
			"class:gmnos", noSeed, union,
			`visited [class:gmnos] partial [] error: reading class:gmnos: leg down`,
		},
		{
			"Active, near fails", func(f topoFixture) Intent {
				return Molecule{Kind: MoleculeActive, Near: f.work, NearRef: WorkRef}
			}, WorkRef, noSeed, union,
			`visited [class:gmnos, the city work store] partial [] error: reading the city work store: leg down`,
		},
		{
			"OrderRuns(pours), the graph fails first", func(topoFixture) Intent { return OrderRuns{PoursWork: func() bool { return true }} },
			"class:gmnos", noSeed, union,
			`visited [class:gmnos] partial [] error: reading class:gmnos: leg down`,
		},
		{
			"OrderRuns(pours), the work leg fails", func(topoFixture) Intent { return OrderRuns{PoursWork: func() bool { return true }} },
			WorkRef, noSeed, union,
			`visited [class:gmnos, the city work store] partial [] error: reading the city work store: leg down`,
		},
		{
			"Convoy(Parent), near fails", func(f topoFixture) Intent {
				return Convoy{Kind: ConvoyParent, ID: "ga-1", Near: f.bindings["class:gmnos"], NearRef: "class:gmnos"}
			}, "class:gmnos", WorkRef, firstOwner,
			`error: reading "ga-1" from class:gmnos: leg down`,
		},
		{
			"Convoy(Parent), near misses, the home holds it", func(f topoFixture) Intent {
				return Convoy{Kind: ConvoyParent, ID: "ga-1", Near: f.bindings["class:gmnos"], NearRef: "class:gmnos"}
			}, RigRef("bravo"), WorkRef, firstOwner,
			`owner the city work store`,
		},
		{
			"Convoy(Parent), the home fails", func(f topoFixture) Intent {
				return Convoy{Kind: ConvoyParent, ID: "ga-1", Near: f.bindings["class:gmnos"], NearRef: "class:gmnos"}
			}, WorkRef, noSeed, firstOwner,
			`error: reading "ga-1" from the city work store: leg down`,
		},
		{
			"Convoy(Tracking), the graph fails", func(f topoFixture) Intent {
				return Convoy{Kind: ConvoyTracking, Near: f.work, NearRef: WorkRef}
			}, "class:gmnos", noSeed, union,
			`visited [the city work store, class:gmnos] partial [] error: reading class:gmnos: leg down`,
		},
		{
			"Convoy(Tracking), near fails", func(f topoFixture) Intent {
				return Convoy{Kind: ConvoyTracking, Near: f.work, NearRef: WorkRef}
			}, WorkRef, noSeed, union,
			`visited [the city work store] partial [] error: reading the city work store: leg down`,
		},
		{
			"Convoy(Children), elsewhere fails", func(f topoFixture) Intent {
				return Convoy{Kind: ConvoyChildren, Near: f.work, NearRef: WorkRef, Elsewhere: f.bindings["class:gmnos"]}
			}, "class:gmnos", noSeed, union,
			`visited [the city work store, class:gmnos] partial [] error: reading class:gmnos: leg down`,
		},
		{
			"CloseOwner, the route fails", func(topoFixture) Intent {
				return CloseOwner{ID: "ga-1", Route: []StoreRef{WorkRef}, Relic: "class:gmnos"}
			}, WorkRef, "class:gmnos", firstOwner,
			`error: reading "ga-1" from the city work store: leg down`,
		},
		{
			"CloseOwner, the relic fails", func(topoFixture) Intent {
				return CloseOwner{ID: "ga-1", Route: []StoreRef{WorkRef}, Relic: "class:gmnos"}
			}, "class:gmnos", noSeed, firstOwner,
			`error: reading "ga-1" from class:gmnos: leg down`,
		},
	}
	for _, tc := range cases {
		f := newT2()
		failLeg(f, tc.fail)
		if tc.seed != noSeed {
			f.legStore(tc.seed).seed(t, "ga-1")
		}
		plan, err := NewStores(f.topo, StoresOptions{}).Plan(tc.intent(f))
		if err != nil {
			t.Fatal(err)
		}
		if got := tc.exec(f, plan); got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

// TestResolveOwnerPreferringPrecedence pins which answer wins when a walk
// found a row the predicate rejects AND passed a FatalUnlessResolved leg it
// could not read: the error, because the unread leg may hold the preferred
// copy. (cmd/gc's committedCloseOwner is not this executor: it skips a failed
// read, and falls back to its first store rather than to any row found, and
// stays its own code; see ResolveOwnerPreferring.)
func TestResolveOwnerPreferringPrecedence(t *testing.T) {
	open, broken := newNamedStore("open"), newNamedStore("broken")
	open.seed(t, "ga-1")
	broken.getErr = errDown
	plan := ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{
		{Leg: Leg{Ref: "open", Store: open}, Role: RoleAuthority, OnError: PolicyFatalUnlessResolved},
		{Leg: Leg{Ref: "broken", Store: broken}, Role: RoleV1, OnError: PolicyFatalUnlessResolved},
	}}
	owner, err := ResolveOwnerPreferring(plan, "ga-1", func(b beads.Bead) bool { return b.Status == "closed" })
	if !errors.Is(err, errDown) || owner.Ref != "broken" {
		t.Errorf("ResolveOwnerPreferring = %q, %v; want the unread leg's error, not the open row", owner.Ref, err)
	}
	clean := ResolvedPlan{Mode: ModeFirstOwner, Legs: plan.Legs[:1]}
	if owner, err := ResolveOwnerPreferring(clean, "ga-1", func(b beads.Bead) bool { return b.Status == "closed" }); err != nil || owner.Ref != "open" {
		t.Errorf("with no failed leg the rejected row wins: %q, %v", owner.Ref, err)
	}
}

// TestFirstOwnerRefusesFatalUnlessResolvedWithAWorkAxis: a first-owner plan
// mixing FatalUnlessResolved legs with a by-id work axis is refused.
func TestFirstOwnerRefusesFatalUnlessResolvedWithAWorkAxis(t *testing.T) {
	s := newNamedStore("s")
	for _, role := range []Role{RoleWorkFallback, RoleShadow} {
		plan := ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{
			{Leg: Leg{Ref: "a", Store: s}, Role: RoleAuthority, OnError: PolicyFatalUnlessResolved},
			{Leg: Leg{Ref: "b", Store: newNamedStore("b")}, Role: role, OnError: PolicyFatal},
		}}
		if _, err := ResolveOwnerRow(plan, "ga-1"); err == nil || !strings.Contains(err.Error(), "work axis") {
			t.Errorf("%s: %v, want the combination refused", role, err)
		}
		if _, err := ResolveOwnerPreferring(plan, "ga-1", nil); err == nil {
			t.Errorf("%s: ResolveOwnerPreferring accepted the combination", role)
		}
	}
}
