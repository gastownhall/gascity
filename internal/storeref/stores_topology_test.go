package storeref

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// TestStoresTopologyPlansAsTheTopologyItWasMintedFrom: with no projection,
// every existing intent plans over Stores.Topology() exactly as over the
// topology the epoch was minted from, on every corpus topology: the minted
// topology keeps the prefixes, the census and mint bits, the refusal and the
// rig legs, so no plane loses a leg, a role or a policy by reading it.
func TestStoresTopologyPlansAsTheTopologyItWasMintedFrom(t *testing.T) {
	for _, f := range allTopologies() {
		minted := NewStores(f.topo, StoresOptions{}).Topology()
		for _, in := range corpusIntents() {
			if got, want := planString(t, in.intent, minted), planString(t, in.intent, f.topo); got != want {
				t.Errorf("%s x %s: over Stores.Topology() %s, over the minted-from topology %s", in.name, f.name, got, want)
			}
		}
		if got, want := len(minted.PrefixFaults()), len(f.topo.PrefixFaults()); got != want {
			t.Errorf("%s: %d prefix faults over Stores.Topology(), %d over the minted-from topology", f.name, got, want)
		}
	}
}

// TestStoresTopologyCarriesTheHolderBits: the prefixes, census bits and
// relic-free bits a holder completes its topology with come back out of
// Stores.Topology(), on the stores the handles wrap.
func TestStoresTopologyCarriesTheHolderBits(t *testing.T) {
	f := newT2RelicFree()
	f.topo.Bindings[0].MintsReserved, f.topo.Bindings[0].HasLegacyResidents = true, false
	f.topo.Work.RelicFree = true
	s := NewStores(f.topo, StoresOptions{})
	got := s.Topology()
	if got.Work.Prefix != cityPrefix || !got.Work.RelicFree || !beads.SameStore(got.Work.Store, s.Work()) {
		t.Errorf("work leg = %+v, want the work handle's store with prefix %q, relic-free", got.Work, cityPrefix)
	}
	want := map[StoreRef]struct {
		prefix    string
		relicFree bool
	}{RigRef("alpha"): {alphaPrefix, true}, RigRef("bravo"): {bravoPrefix, false}}
	if len(got.Rigs) != len(want) {
		t.Fatalf("rigs = %+v", got.Rigs)
	}
	for _, leg := range got.Rigs {
		w := want[leg.Ref]
		name, _ := ScopeRigContext(string(leg.Ref))
		rig, _ := s.Rig(name)
		if leg.Prefix != w.prefix || leg.RelicFree != w.relicFree || !beads.SameStore(leg.Store, rig) {
			t.Errorf("rig %s = %+v, want prefix %q, relic-free %v, over its handle's store", leg.Ref, leg, w.prefix, w.relicFree)
		}
	}
	if len(got.Bindings) != 1 {
		t.Fatalf("bindings = %+v", got.Bindings)
	}
	b := got.Bindings[0]
	if b.Leg.Ref != "class:gmnos" || !b.MintsReserved || b.HasLegacyResidents || !beads.SameStore(b.Leg.Store, s.Graph()) {
		t.Errorf("binding = %+v, want class:gmnos over the graph handle's store with the plane's census bits", b)
	}
}

// TestStoresTopologyUnderTheGateKeepsTheSplitShape: a single-store topology
// minted under the gate's projection relocates nothing, yet its class handles
// read a class view, so the minted topology names that view as the whole
// split's binding (class:gmnos, as the API plane derives it, never class:gnos)
// with the pessimistic census, over the very store the handles wrap.
func TestStoresTopologyUnderTheGateKeepsTheSplitShape(t *testing.T) {
	f := newT0()
	s := NewStores(f.topo, StoresOptions{Projection: taggedProjection{}})
	got := s.Topology()
	if got.Work.Store != (taggedView{f.work, "work"}) {
		t.Errorf("work leg = %v, want the work view", got.Work.Store)
	}
	if len(got.Bindings) != 1 {
		t.Fatalf("bindings = %+v, want the class view as one binding", got.Bindings)
	}
	b := got.Bindings[0]
	if b.Leg.Ref != "class:gmnos" || b.Leg.Store != (taggedView{f.work, "class"}) || !b.HasLegacyResidents || b.MintsReserved {
		t.Errorf("binding = %+v", b)
	}
	if !reflect.DeepEqual(b.Classes, infrastructureOrder()) {
		t.Errorf("binding classes = %v", b.Classes)
	}
	if plain := NewStores(f.topo, StoresOptions{}).Topology(); !plain.IsSingleStore() {
		t.Errorf("with no projection the single-store city has bindings: %+v", plain.Bindings)
	}
}

// infrastructureOrder is every infrastructure class in coordclass order.
func infrastructureOrder() []coordclass.Class {
	var out []coordclass.Class
	for _, c := range coordclass.Classes() {
		if c.IsInfrastructure() {
			out = append(out, c)
		}
	}
	return out
}

// TestStoresHolds: a binding holds the classes it serves; any other store
// holds work and every class no binding serves; a single-store city answers
// nil (the class guard is silent).
func TestStoresHolds(t *testing.T) {
	work := []coordclass.Class{coordclass.ClassWork}
	for _, tc := range []struct {
		name string
		f    topoFixture
		ref  StoreRef
		want []coordclass.Class
	}{
		{"single-store", newT0(), WorkRef, nil},
		{"whole split, binding", newT1(), "class:gmnos", infraClasses},
		{"whole split, work", newT1(), WorkRef, work},
		{"whole split, rig", newT2(), RigRef("alpha"), work},
		{"per-class, graph binding", newT5(), "class:g", []coordclass.Class{coordclass.ClassGraph}},
		{"per-class, work", newT5(), WorkRef, []coordclass.Class{coordclass.ClassWork, coordclass.ClassMessaging, coordclass.ClassOrders, coordclass.ClassNudges}},
	} {
		got := NewStores(tc.f.topo, StoresOptions{}).Holds(tc.ref)
		if !sameClassSet(got, tc.want) || (got == nil) != (tc.want == nil) {
			t.Errorf("%s: Holds(%q) = %v, want %v", tc.name, tc.ref, got, tc.want)
		}
	}
}

// TestPlanLegDedupeIsPhysicalIdentity: a leg over a class handle and a leg over
// the store it wraps are one leg (beads.SameStore, never ==); two stores are
// two; an uncomparable store is distinct from everything, itself included.
func TestPlanLegDedupeIsPhysicalIdentity(t *testing.T) {
	s, other := newNamedStore("s"), newNamedStore("other")
	plan := dedupeLegs(ResolvedPlan{Mode: ModeUnion, Legs: []PlanLeg{
		{Leg: Leg{Ref: "a", Store: s}},
		{Leg: Leg{Ref: "b", Store: beads.WorkStore{Store: s}}},
		{Leg: Leg{Ref: "c", Store: beads.GraphStore{Store: s}}},
		{Leg: Leg{Ref: "d", Store: beads.MintResident(s, "")}},
		{Leg: Leg{Ref: "e", Store: other}},
	}})
	if got := plan.Refs(); !reflect.DeepEqual(got, []StoreRef{"a", "e"}) {
		t.Errorf("deduped refs = %v, want [a e]: a handle is the store it wraps", got)
	}
	u := uncomparableStore{Store: s.MemStore}
	plan = dedupeLegs(ResolvedPlan{Mode: ModeUnion, Legs: []PlanLeg{{Leg: Leg{Ref: "u1", Store: u}}, {Leg: Leg{Ref: "u2", Store: u}}}})
	if len(plan.Legs) != 2 {
		t.Errorf("an uncomparable store was deduped: %v", plan.Refs())
	}
}

// TestStoresPlanRefusesTheTopologyIntents: an intent the plane's topology
// answers is not Stores.Plan's (OD-T7-5): it is refused here, and a T7 intent
// is refused by the topology Plan, which has no handles to plan it over.
func TestStoresPlanRefusesTheTopologyIntents(t *testing.T) {
	f := newT2()
	s := NewStores(f.topo, StoresOptions{})
	for _, in := range corpusIntents() {
		if _, err := s.Plan(in.intent); err == nil {
			t.Errorf("%s: Stores.Plan answered a topology intent", in.name)
		}
	}
	for _, in := range storesIntents(f) {
		if _, err := Plan(in.intent, f.topo); err == nil {
			t.Errorf("%s: the topology Plan answered a T7 intent", in.name)
		}
	}
}

// TestStoresTopologyIgnoresAPlanesPerClassRefusal: a plane's per-class
// refusal (StoresOptions.ClassRefusals, cmd/gc's worker site) refuses through
// the class's handle only. The minted topology names the binding the routes
// serve and carries no refusal of its own, so every existing intent plans as
// over the plane's topology.
func TestStoresTopologyIgnoresAPlanesPerClassRefusal(t *testing.T) {
	for _, f := range []topoFixture{newT0(), newT2()} {
		s := NewStores(f.topo, StoresOptions{ClassRefusals: map[coordclass.Class]RefusingClassStore{coordclass.ClassGraph: refusingStore{}}})
		if _, refused := beads.As[RefusingStore](s.Graph()); !refused {
			t.Fatalf("%s: fixture: the graph handle does not refuse", f.name)
		}
		minted := s.Topology()
		if minted.Refused != nil {
			t.Errorf("%s: the per-class refusal made the topology refused: %v", f.name, minted.Refused)
		}
		for _, in := range corpusIntents() {
			if got, want := planString(t, in.intent, minted), planString(t, in.intent, f.topo); got != want {
				t.Errorf("%s x %s: minted %s, plane %s", in.name, f.name, got, want)
			}
		}
	}
}

// TestStoresTopologyFailsClosedOnAWorkerSite: when the plane's per-class
// refusals cover every class the topology relocates (a worker site whose
// descriptor attaches none of the binding's classes), the minted topology is
// refused, as the one-shot CLI funnel's routes are there, and still names the
// binding class:gmnos; every existing intent then plans as over the plane's
// topology carrying that refusal. A city that relocates nothing, and a
// refusal of some classes only, leave the topology unrefused.
func TestStoresTopologyFailsClosedOnAWorkerSite(t *testing.T) {
	all := map[coordclass.Class]RefusingClassStore{}
	for _, c := range infraClasses {
		all[c] = refusingStore{}
	}
	f := newT2()
	minted := NewStores(f.topo, StoresOptions{ClassRefusals: all}).Topology()
	if minted.Refused == nil || fmt.Sprint(minted.ClaimRefs()) != "[ class:gmnos]" {
		t.Fatalf("every relocated class refused: refused %v, claim refs %v; want refused, [ class:gmnos]", minted.Refused, minted.ClaimRefs())
	}
	refusedPlane := f.topo
	refusedPlane.Refused = minted.Refused
	for _, in := range corpusIntents() {
		if got, want := planString(t, in.intent, minted), planString(t, in.intent, refusedPlane); got != want {
			t.Errorf("%s: minted %s, the refused plane %s", in.name, got, want)
		}
	}
	if got := NewStores(newT0().topo, StoresOptions{ClassRefusals: all}).Topology(); got.Refused != nil {
		t.Errorf("a city that relocates nothing was refused: %v", got.Refused)
	}
	some := map[coordclass.Class]RefusingClassStore{coordclass.ClassGraph: refusingStore{}, coordclass.ClassSessions: refusingStore{}}
	if got := NewStores(f.topo, StoresOptions{ClassRefusals: some}).Topology(); got.Refused != nil {
		t.Errorf("refusing some relocated classes refused the topology: %v", got.Refused)
	}
}

// refusingGetStore refuses every by-id read with its standing refusal.
type refusingGetStore struct{ refusingStore }

func (s refusingGetStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.StorageRefusal() }

// TestFullyRefusedBindingLegRefuses: a binding whose every class the plane
// refuses is minted over the refusing store, under its own ref, so a by-id
// read that reaches it (inside its namespace, or as the residence probe of a
// work-shaped id) refuses as each class handle does instead of reading the
// binding. A binding refused in part keeps its store.
func TestFullyRefusedBindingLegRefuses(t *testing.T) {
	f := newT2()
	f.legStore(ClassRef(infraClasses)).seed(t, graphNamespacedID)
	refusal := refusingGetStore{refusingStore{beads.NewMemStore()}}
	all := map[coordclass.Class]RefusingClassStore{}
	for _, c := range infraClasses {
		all[c] = refusal
	}
	minted := NewStores(f.topo, StoresOptions{ClassRefusals: all}).Topology()
	if len(minted.Bindings) != 1 || minted.Bindings[0].Leg.Ref != ClassRef(infraClasses) || minted.Bindings[0].Leg.Store != beads.Store(refusal) {
		t.Fatalf("fully refused: bindings %+v, want class:gmnos over the refusing store", minted.Bindings)
	}
	plan := mustPlan(t, ByID{ID: graphNamespacedID}, minted)
	if _, err := ResolveOwnerRow(plan, graphNamespacedID); err == nil || !strings.Contains(err.Error(), newRefusal().Error()) {
		t.Fatalf("a by-id read inside the refused binding's namespace = %v, want its refusal", err)
	}

	some := map[coordclass.Class]RefusingClassStore{coordclass.ClassGraph: refusal}
	partial := NewStores(f.topo, StoresOptions{ClassRefusals: some}).Topology()
	if got := partial.Bindings[0].Leg.Store; got != beads.Store(f.legStore(ClassRef(infraClasses))) {
		t.Fatalf("refused in part: the binding leg reads %s, want the binding", storeNameOf(got))
	}
}

// TestOrderRunsAsksPoursWorkOnlyOnASplitCity: as gateStoresFor, a
// single-store city's plan never asks whether the order pours work (which
// compiles its formula); a split city's asks once.
func TestOrderRunsAsksPoursWorkOnlyOnASplitCity(t *testing.T) {
	for name, tc := range map[string]struct {
		f    topoFixture
		want int
	}{"single store": {newT0(), 0}, "whole split": {newT2(), 1}} {
		asked := 0
		_, err := NewStores(tc.f.topo, StoresOptions{}).Plan(OrderRuns{PoursWork: func() bool { asked++; return true }})
		if err != nil || asked != tc.want {
			t.Errorf("%s: asked %d times (err %v), want %d", name, asked, err, tc.want)
		}
	}
}
