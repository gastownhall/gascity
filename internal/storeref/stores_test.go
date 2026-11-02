package storeref

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// handleStores reads every class handle back as the store it wraps, keyed by
// class. A test file may unwrap; the point is to name the store each handle
// was minted over.
func handleStores(s Stores) map[coordclass.Class]beads.Store {
	return map[coordclass.Class]beads.Store{
		coordclass.ClassWork:      s.Work().Store,
		coordclass.ClassGraph:     s.Graph().Store,
		coordclass.ClassOrders:    s.Orders().Store,
		coordclass.ClassSessions:  s.Sessions().Store,
		coordclass.ClassMessaging: s.Mail().Store,
		coordclass.ClassNudges:    s.Nudges().Store,
	}
}

// wantClassStore is the oracle the router must agree with, stated without the
// router: planClass's placement (on a refused city, the binding leg, which is
// the refusing store), and the work store for work.
func wantClassStore(t *testing.T, f topoFixture, c coordclass.Class) beads.Store {
	t.Helper()
	if c == coordclass.ClassWork {
		return f.work
	}
	if b, ok := f.topo.BindingFor(c); ok && f.topo.Refused != nil {
		return b.Leg.Store
	}
	placed, err := ResolvePlacement(mustPlan(t, Class{C: c}, f.topo))
	if err != nil {
		t.Fatalf("%s %s: ResolvePlacement: %v", f.name, c, err)
	}
	return placed.Store
}

// TestStoresAgreesWithPlacement is the router's equivalence table over every
// corpus topology and class.
func TestStoresAgreesWithPlacement(t *testing.T) {
	for _, f := range allTopologies() {
		s := NewStores(f.topo, StoresOptions{})
		for c, got := range handleStores(s) {
			if want := wantClassStore(t, f, c); got != want {
				t.Errorf("%s: %s handle wraps %s, want %s", f.name, c, storeNameOf(got), storeNameOf(want))
			}
		}
	}
}

func TestStoresSingleStoreHandlesShareTheWorkStore(t *testing.T) {
	f := newT0()
	for c, got := range handleStores(NewStores(f.topo, StoresOptions{})) {
		if got != beads.Store(f.work) {
			t.Errorf("single-store %s handle wraps %s, want the work store", c, storeNameOf(got))
		}
	}
}

// refusingStore is a per-class refusal of the kind a plane takes itself.
type refusingStore struct{ *beads.MemStore }

func (refusingStore) StorageRefusal() error { return newRefusal() }

func TestStoresClassRefusalWinsRelocatedOrNot(t *testing.T) {
	refusal := refusingStore{beads.NewMemStore()}
	for _, f := range []topoFixture{newT0(), newT1()} {
		s := NewStores(f.topo, StoresOptions{ClassRefusals: map[coordclass.Class]RefusingClassStore{coordclass.ClassGraph: refusal}})
		if got := s.Graph().Store; got != beads.Store(refusal) {
			t.Errorf("%s: graph wraps %s, want the refusal", f.name, storeNameOf(got))
		}
		if got := s.Orders().Store; got == beads.Store(refusal) {
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
	for c, got := range handleStores(s) {
		want := taggedView{f.work, "class"}
		if c == coordclass.ClassWork {
			want = taggedView{f.work, "work"}
		}
		if got != want {
			t.Errorf("%s handle = %v, want %v", c, got, want)
		}
	}
	// A relocated class is the binding itself: the projection only splits one
	// physical store, and a split city already has two.
	split := newT1()
	ss := NewStores(split.topo, StoresOptions{Projection: taggedProjection{}})
	if got := ss.Graph().Store; got != beads.Store(split.bindings[ClassRef(infraClasses)]) {
		t.Errorf("split graph handle = %v, want the bare binding", got)
	}
	// No work store gets no view: there is nothing to project.
	if got := NewStores(Topology{}, StoresOptions{Projection: taggedProjection{}}).Graph().Store; got != nil {
		t.Errorf("a nil work store was projected to %v", got)
	}
}

// TestPlaceIsTheStoreEveryHandleWraps pins the raw form to the mint under
// every option set.
func TestPlaceIsTheStoreEveryHandleWraps(t *testing.T) {
	refusal := refusingStore{beads.NewMemStore()}
	for _, opts := range []StoresOptions{
		{},
		{Projection: taggedProjection{}},
		{ClassRefusals: map[coordclass.Class]RefusingClassStore{coordclass.ClassOrders: refusal}},
	} {
		for _, f := range allTopologies() {
			for c, got := range handleStores(NewStores(f.topo, opts)) {
				if want := Place(f.topo, opts, c); got != want {
					t.Errorf("%s: %s handle wraps %v, Place answers %v", f.name, c, got, want)
				}
			}
		}
	}
}
