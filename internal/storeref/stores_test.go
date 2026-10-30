package storeref

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// handleStores reads every class handle of one scope back as stores, keyed by
// class. A test file may unwrap; the point is to name the store each handle
// was minted over.
func handleStores(s Stores, scope StoreRef) map[coordclass.Class]beads.Store {
	out := map[coordclass.Class]beads.Store{
		coordclass.ClassGraph:  s.Graph(scope).Store,
		coordclass.ClassOrders: s.Orders(scope).Store,
	}
	if scope == WorkRef {
		out[coordclass.ClassWork] = s.Work().Store
		out[coordclass.ClassSessions] = s.Sessions().Store
		out[coordclass.ClassMessaging] = s.Mail().Store
		out[coordclass.ClassNudges] = s.Nudges().Store
	}
	return out
}

// wantClassStore is the oracle the router must agree with, stated without the
// router: planClass's placement for the city scope (or the binding leg on a
// refused city, which is the refusing store), and for a rig scope the same
// binding, else the rig's own store. Work is the scope's own store.
func wantClassStore(t *testing.T, f topoFixture, c coordclass.Class, scope StoreRef) beads.Store {
	t.Helper()
	scopeStore := beads.Store(f.legStore(scope))
	if scope != WorkRef && f.rigs[scope.rigName()] == nil {
		scopeStore = nil
	}
	if c == coordclass.ClassWork {
		return scopeStore
	}
	if b, ok := f.topo.BindingFor(c); ok {
		if f.topo.Refused == nil {
			placed, err := ResolvePlacement(mustPlan(t, Class{C: c}, f.topo))
			if err != nil || placed.Store != b.Leg.Store {
				t.Fatalf("%s %s: the oracle disagrees with ResolvePlacement: %v", f.name, c, err)
			}
		}
		return b.Leg.Store
	}
	if scope == WorkRef {
		placed, err := ResolvePlacement(mustPlan(t, Class{C: c}, f.topo))
		if err != nil {
			t.Fatalf("%s %s: ResolvePlacement: %v", f.name, c, err)
		}
		return placed.Store
	}
	return scopeStore
}

func (r StoreRef) rigName() string {
	name, _ := ScopeRigContext(string(r))
	return name
}

// TestStoresAgreesWithPlacementAtEveryScope is the router's equivalence table:
// every corpus topology, every class, at the city, at each rig, and at a rig
// the epoch does not hold.
func TestStoresAgreesWithPlacementAtEveryScope(t *testing.T) {
	for _, f := range allTopologies() {
		s := NewStores(f.topo, StoresOptions{})
		scopes := []StoreRef{WorkRef, RigRef("absent")}
		for name := range f.rigs {
			scopes = append(scopes, RigRef(name))
		}
		for _, scope := range scopes {
			for c, got := range handleStores(s, scope) {
				if want := wantClassStore(t, f, c, scope); got != want {
					t.Errorf("%s at %q: %s handle wraps %s, want %s", f.name, scope, c, storeNameOf(got), storeNameOf(want))
				}
			}
		}
	}
}

func TestStoresSingleStoreHandlesShareTheWorkStore(t *testing.T) {
	f := newT0()
	s := NewStores(f.topo, StoresOptions{})
	for c, got := range handleStores(s, WorkRef) {
		if got != beads.Store(f.work) {
			t.Errorf("single-store %s handle wraps %s, want the work store", c, storeNameOf(got))
		}
	}
}

func TestStoresRigsAndWorkLegs(t *testing.T) {
	f := newT2()
	s := NewStores(f.topo, StoresOptions{})
	legs := s.WorkLegs()
	want := []beads.Store{f.work, f.rigs["alpha"], f.rigs["bravo"]}
	if len(legs) != len(want) {
		t.Fatalf("WorkLegs = %d legs, want %d", len(legs), len(want))
	}
	for i := range want {
		if legs[i].Store != want[i] {
			t.Errorf("WorkLegs[%d] = %s, want %s", i, storeNameOf(legs[i].Store), storeNameOf(want[i]))
		}
	}
	rigs := s.Rigs()
	if len(rigs) != 2 || rigs["alpha"].Store != beads.Store(f.rigs["alpha"]) || rigs["bravo"].Store != beads.Store(f.rigs["bravo"]) {
		t.Errorf("Rigs = %v, want alpha and bravo over their own stores", rigs)
	}
	if h, ok := s.Rig("alpha"); !ok || h.Store != beads.Store(f.rigs["alpha"]) {
		t.Errorf("Rig(alpha) = %v, %v", h, ok)
	}
	if _, ok := s.Rig("absent"); ok {
		t.Error("Rig(absent) reported a rig this epoch does not hold")
	}
	if NewStores(newT0().topo, StoresOptions{}).Rigs() != nil {
		t.Error("a city with no rigs has a non-nil rig map")
	}
}

// refusingStore is a per-class refusal of the kind a plane takes itself.
type refusingStore struct{ *beads.MemStore }

func (refusingStore) StorageRefusal() error { return newRefusal() }

func TestStoresClassRefusalWinsAtEveryScope(t *testing.T) {
	refusal := refusingStore{beads.NewMemStore()}
	for _, f := range []topoFixture{newT0(), newT2()} {
		s := NewStores(f.topo, StoresOptions{ClassRefusals: map[coordclass.Class]RefusingClassStore{coordclass.ClassGraph: refusal}})
		for _, scope := range []StoreRef{WorkRef, RigRef("alpha"), RigRef("absent")} {
			if got := s.Graph(scope).Store; got != beads.Store(refusal) {
				t.Errorf("%s at %q: graph wraps %s, want the refusal", f.name, scope, storeNameOf(got))
			}
		}
		if got := s.Orders(WorkRef).Store; got == beads.Store(refusal) {
			t.Errorf("%s: the graph refusal leaked onto orders", f.name)
		}
		if got := s.Work().Store; got != beads.Store(f.work) {
			t.Errorf("%s: a class refusal moved the work store to %s", f.name, storeNameOf(got))
		}
	}
}

// taggedProjection marks every view it returns, so a test can tell which side
// a handle was given.
type taggedProjection struct{}

type taggedView struct {
	beads.Store
	side string
}

func (taggedProjection) WorkView(s beads.Store) beads.Store  { return taggedView{s, "work"} }
func (taggedProjection) ClassView(s beads.Store) beads.Store { return taggedView{s, "class"} }

func TestStoresProjectionViewsOnlyTheWorkSide(t *testing.T) {
	f := newT0()
	s := NewStores(f.topo, StoresOptions{Projection: taggedProjection{}})
	if got := s.Work().Store; got != (taggedView{f.work, "work"}) {
		t.Errorf("work handle = %v, want the work view", got)
	}
	for c, got := range handleStores(s, WorkRef) {
		if c == coordclass.ClassWork {
			continue
		}
		if got != (taggedView{f.work, "class"}) {
			t.Errorf("%s handle = %v, want the class view of the work store", c, got)
		}
	}
	// A relocated class is the binding itself: the projection only splits one
	// physical store, and a split city already has two.
	split := newT1()
	ss := NewStores(split.topo, StoresOptions{Projection: taggedProjection{}})
	if got := ss.Graph(WorkRef).Store; got != beads.Store(split.bindings[ClassRef(infraClasses)]) {
		t.Errorf("split graph handle = %v, want the bare binding", got)
	}
	// A scope with no store gets no view: there is nothing to project.
	if got := NewStores(Topology{}, StoresOptions{Projection: taggedProjection{}}).Graph(WorkRef).Store; got != nil {
		t.Errorf("a nil work store was projected to %v", got)
	}
}

// TestPlaceIsTheStoreEveryHandleWraps pins the raw form to the mint: the two
// cannot drift, because a raw caller and a handle holder must reach the same
// store.
func TestPlaceIsTheStoreEveryHandleWraps(t *testing.T) {
	refusal := refusingStore{beads.NewMemStore()}
	for _, opts := range []StoresOptions{
		{},
		{Projection: taggedProjection{}},
		{ClassRefusals: map[coordclass.Class]RefusingClassStore{coordclass.ClassOrders: refusal}},
	} {
		for _, f := range allTopologies() {
			s := NewStores(f.topo, opts)
			scopes := []StoreRef{WorkRef, RigRef("absent")}
			for name := range f.rigs {
				scopes = append(scopes, RigRef(name))
			}
			for _, scope := range scopes {
				for c, got := range handleStores(s, scope) {
					if want := Place(f.topo, opts, c, scope); got != want {
						t.Errorf("%s at %q: %s handle wraps %v, Place answers %v", f.name, scope, c, got, want)
					}
				}
				if h, ok := s.Rig(scope.rigName()); ok {
					if want := Place(f.topo, opts, coordclass.ClassWork, scope); h.Store != want {
						t.Errorf("%s: rig %q work handle wraps %v, Place answers %v", f.name, scope, h.Store, want)
					}
				}
			}
		}
	}
}
