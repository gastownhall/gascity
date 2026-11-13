package storeref

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The T7b.1 intents (t7g-design.md §4): Convoy, CloseOwner, Molecule{Roots}
// by census ref, and BindLegs. The corpus rows are in stores_plan_test.go;
// these pin the executor answers the converted cmd/gc sites rest on.

// TestCloseOwnerReplaysTheLiveCloseOwnerTable is cmd/gc's
// TestLiveCloseOwnerTakesTheRowThatReadsClosed run through CloseOwner and
// ResolveOwnerPreferring (the relic rows; without a relic cmd/gc reads
// liveReadOwner, which is not this executor): for an id the relic's binding
// holds beside its prefix route, the owner is the first store whose row
// reads closed; an open row ahead of it does not refute the close; a failed
// read leaves the close unconfirmed; with no closed row the first row found,
// or not-found, refutes it.
func TestCloseOwnerReplaysTheLiveCloseOwnerTable(t *testing.T) {
	const id = "ga-7"
	for _, tc := range []struct {
		work, relic string
		want        string // "closed by <leg>", "open in <leg>", "not found", "unconfirmed"
	}{
		{"open", "closed", "closed by class:gmnos"},
		{"missing", "closed", "closed by class:gmnos"},
		{"closed", "open", "closed by the city work store"},
		{"closed", "closed", "closed by the city work store"},
		{"open", "open", "open in the city work store"},
		{"open", "missing", "open in the city work store"},
		{"missing", "missing", "not found"},
		{"unreadable", "closed", "unconfirmed"},
		{"open", "unreadable", "unconfirmed"},
	} {
		name := fmt.Sprintf("work=%s/relic=%s", tc.work, tc.relic)
		f := newT2()
		stage := func(s *namedStore, status string) {
			switch status {
			case "missing":
			case "unreadable":
				s.getErr = errDown
			default:
				s.seed(t, id)
				if status == "closed" {
					if err := s.Close(id); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		stage(f.work, tc.work)
		stage(f.bindings["class:gmnos"], tc.relic)
		plan, err := NewStores(f.topo, StoresOptions{}).Plan(CloseOwner{ID: id, Route: []StoreRef{WorkRef}, Relic: "class:gmnos"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		owner, err := ResolveOwnerPreferring(plan, id, func(b beads.Bead) bool { return b.Status == "closed" })
		var got string
		switch {
		case errors.Is(err, beads.ErrNotFound):
			got = "not found"
		case err != nil:
			got = "unconfirmed"
		case owner.Bead.Status == "closed":
			got = "closed by " + legName(owner.Ref)
		default:
			got = "open in " + legName(owner.Ref)
		}
		if got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

// TestCloseOwnerReadsLiveAndFederated: the close is confirmed past a cache,
// through a federated leg reader, so a relic's close the cache has not seen
// is read as it is.
func TestCloseOwnerReadsLiveAndFederated(t *testing.T) {
	f := newT2()
	plan, err := NewStores(f.topo, StoresOptions{}).Plan(CloseOwner{ID: "ga-1", Route: []StoreRef{WorkRef, RigRef("alpha")}, Relic: "class:gmnos"})
	if err != nil {
		t.Fatal(err)
	}
	for _, leg := range plan.Legs {
		if leg.Gate != beads.FederatedLegRole || leg.OnError != PolicyFatal {
			t.Errorf("leg %s: gate %v policy %v, want federated and Fatal", legName(leg.Leg.Ref), leg.Gate, leg.OnError)
		}
	}
	if got := plan.String(); got != `FirstOwner: ""[Authority,Fatal,federated] > rig:alpha[Authority,Fatal,federated] > class:gmnos[FederationTail,Fatal,federated]` {
		t.Errorf("plan = %s", got)
	}
}

// TestCloseOwnerRoutesToASuspendedRig: a route names a rig whatever its
// suspension, since the event routed there; a ref the epoch holds no store
// for (an unopened rig) is a plan error, which the router avoids by never
// naming one.
func TestCloseOwnerRoutesToASuspendedRig(t *testing.T) {
	f := newT2()
	for i := range f.topo.Rigs {
		if f.topo.Rigs[i].Ref == RigRef("bravo") {
			f.topo.Rigs[i].Suspended = true
		}
	}
	s := NewStores(f.topo, StoresOptions{})
	plan, err := s.Plan(CloseOwner{ID: "zz-1", Route: []StoreRef{RigRef("bravo")}})
	if err != nil || len(plan.Legs) != 1 || plan.Legs[0].Leg.Ref != RigRef("bravo") {
		t.Fatalf("plan over a suspended rig = %s, %v; want its leg", plan, err)
	}
	if _, err := s.Plan(CloseOwner{ID: "zz-1", Route: []StoreRef{RigRef("charlie")}}); err == nil {
		t.Fatal("a route to a rig this epoch did not open planned")
	}
}

// TestConvoyParentTakesTheLongestRigPrefix: the work home is the opened rig
// (suspended or not) whose configured prefix is the longest the id carries,
// else the city: cmd/gc convoyAutocloseHomes' rule.
func TestConvoyParentTakesTheLongestRigPrefix(t *testing.T) {
	f := buildTopology("nested", map[string]string{"alpha": "ra", "deep": "ra-x"}, []bindingSpec{wholeSplit()}, nil)
	for i := range f.topo.Rigs {
		if f.topo.Rigs[i].Ref == RigRef("deep") {
			f.topo.Rigs[i].Suspended = true
		}
	}
	s := NewStores(f.topo, StoresOptions{})
	binding := f.bindings["class:gmnos"]
	for id, want := range map[string]StoreRef{
		"ra-x-1": RigRef("deep"),
		"ra-1":   RigRef("alpha"),
		"zz-1":   WorkRef,
		"ra":     WorkRef,
	} {
		plan, err := s.Plan(Convoy{Kind: ConvoyParent, ID: id, Near: binding, NearRef: "near"})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if len(plan.Legs) != 2 || plan.Legs[1].Leg.Ref != want {
			t.Errorf("%s: plan %s, want the home %s", id, plan, legName(want))
		}
	}
}

// TestConvoyLegsReadWithTheirGateRole: a typed executor mints each convoy
// leg's reader with the role the kind pins: Tracking's Near strict, its graph
// leg federated; Children's Near strict alone and federated when paired.
func TestConvoyLegsReadWithTheirGateRole(t *testing.T) {
	f := newT2()
	s := NewStores(f.topo, StoresOptions{})
	binding := f.bindings["class:gmnos"]
	roles := func(in Convoy) []beads.LegRole {
		plan, err := s.Plan(in)
		if err != nil {
			t.Fatal(err)
		}
		var out []beads.LegRole
		EachTypedLeg(plan, func(l TypedLeg) { out = append(out, l.Reader.Role()) })
		return out
	}
	check := func(name string, got []beads.LegRole, want ...beads.LegRole) {
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: reader roles %v, want %v", name, got, want)
		}
	}
	check("Tracking", roles(Convoy{Kind: ConvoyTracking, Near: f.work, NearRef: WorkRef}), beads.NoLegRole, beads.FederatedLegRole)
	check("Children alone", roles(Convoy{Kind: ConvoyChildren, Near: f.work, NearRef: WorkRef}), beads.NoLegRole)
	check("Children paired", roles(Convoy{Kind: ConvoyChildren, Near: f.work, NearRef: WorkRef, Elsewhere: binding}), beads.FederatedLegRole, beads.FederatedLegRole)
	check("Parent", roles(Convoy{Kind: ConvoyParent, ID: "ra-1", Near: binding, NearRef: "near"}), beads.FederatedLegRole, beads.FederatedLegRole)
}

// TestRootsReadsTheCensusRefsInOrder: Molecule{Roots} reads the graph
// placement, then the census's legs by ref in the census's order, a
// suspended rig included (the census decided it serves); with no refs it is
// the graph placement alone, on every topology.
func TestRootsReadsTheCensusRefsInOrder(t *testing.T) {
	f := newT2()
	for i := range f.topo.Rigs {
		if f.topo.Rigs[i].Ref == RigRef("alpha") {
			f.topo.Rigs[i].Suspended = true
		}
	}
	s := NewStores(f.topo, StoresOptions{})
	plan, err := s.Plan(Molecule{Kind: MoleculeRoots, Work: []StoreRef{RigRef("bravo"), WorkRef, RigRef("alpha")}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plan.String(), `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > rig:bravo[V1,PartialDegrade,federated] > ""[V1,PartialDegrade,federated] > rig:alpha[V1,PartialDegrade,federated]`; got != want {
		t.Errorf("plan = %s\nwant   %s", got, want)
	}
	for i, ref := range []StoreRef{RigRef("bravo"), WorkRef, RigRef("alpha")} {
		if want := f.legStore(ref); !beads.SameStore(plan.Legs[i+1].Leg.Store, want) {
			t.Errorf("leg %d (%s) is not the epoch's store for its ref", i+1, legName(ref))
		}
	}
}

// TestBindLegsKeepsTheShape: a bound plan reads the bind's stores and keeps
// every leg's ref, role, policy and gate, undeduped; a bind error is the
// leg's.
func TestBindLegsKeepsTheShape(t *testing.T) {
	f := newT2()
	s := NewStores(f.topo, StoresOptions{})
	plan, err := s.Plan(Molecule{Kind: MoleculeActive, Near: f.work, NearRef: WorkRef})
	if err != nil {
		t.Fatal(err)
	}
	clone := newNamedStore("clone")
	bound, err := BindLegs(plan, func(beads.Store) (beads.Store, error) { return clone, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(bound.Legs) != len(plan.Legs) {
		t.Fatalf("bound %d legs, planned %d: BindLegs deduped the one clone", len(bound.Legs), len(plan.Legs))
	}
	for i := range plan.Legs {
		got, want := bound.Legs[i], plan.Legs[i]
		if got.Leg.Ref != want.Leg.Ref || got.Role != want.Role || got.OnError != want.OnError || got.Gate != want.Gate || got.Leg.Store != beads.Store(clone) {
			t.Errorf("leg %d: bound %+v, planned %+v", i, got, want)
		}
		if plan.Legs[i].Leg.Store == beads.Store(clone) {
			t.Errorf("leg %d: BindLegs rewrote the plan it was given", i)
		}
	}
	if _, err := BindLegs(plan, func(beads.Store) (beads.Store, error) { return nil, errDown }); !errors.Is(err, errDown) {
		t.Errorf("a failing bind = %v, want its error", err)
	}
	if _, err := BindLegs(plan, func(beads.Store) (beads.Store, error) { return nil, nil }); err == nil {
		t.Error("a bind that answered no store was accepted")
	}
}
