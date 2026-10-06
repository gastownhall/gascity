package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// A default city relocates nothing, so the API's topology has one leg and every
// plan collapses to the caller's own store. This is the identity gate that
// keeps a single-store city byte-identical.
func TestAPIResidencyTopologyIsSingleStoreOnADefaultCity(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	s := &Server{state: state}

	topo := s.residencyTopology()
	if !topo.IsSingleStore() {
		t.Fatalf("a default city produced %d bindings, want none", len(topo.Bindings))
	}
	if topo.Work.Store != state.cityBeadStore {
		t.Fatal("the work leg is not the city store")
	}
	plan, err := storeref.Plan(storeref.ByID{ID: "ga-1"}, topo)
	if err != nil {
		t.Fatalf("Plan(ByID): %v", err)
	}
	if got, want := plan.String(), `FirstOwner: ""[WorkFallback,Fatal]`; got != want {
		t.Fatalf("ByID plan = %q, want %q", got, want)
	}
}

// A whole-split city: every class accessor the State surface exposes resolves
// to ONE non-work store, and that store becomes one binding carrying all five
// infrastructure classes — messaging included, even though this plane cannot
// observe it (see completeObservedClasses).
func TestAPIResidencyTopologyGroupsTheWholeSplitIntoOneBinding(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	binding := beads.NewMemStore()
	state.graphBeadStore = binding
	state.sessionsBeadStore = binding
	state.ordersBeadStore = binding
	state.nudgesBeadStore = binding
	s := &Server{state: state}

	topo := s.residencyTopology()
	if len(topo.Bindings) != 1 {
		t.Fatalf("got %d bindings, want 1 — the whole split is ONE store", len(topo.Bindings))
	}
	got := topo.Bindings[0]
	if got.Leg.Store != beads.Store(binding) {
		t.Fatal("the binding leg is not the relocated store")
	}
	if got.Leg.Ref != storeref.ClassRef([]coordclass.Class{
		coordclass.ClassGraph, coordclass.ClassMessaging, coordclass.ClassSessions,
		coordclass.ClassOrders, coordclass.ClassNudges,
	}) {
		t.Fatalf("binding ref = %q; the API plane must name the same binding the CLI and controller planes name", got.Leg.Ref)
	}
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() {
			continue
		}
		if !containsClass(got.Classes, class) {
			t.Errorf("binding does not carry class %q", class)
		}
	}
	if got.MintsReserved {
		t.Fatal("MintsReserved is set, but nothing in this build verifies a binding's mint prefix")
	}
	// The prefixes are what decide the by-id namespace gate.
	for _, class := range got.Classes {
		prefix, ok := config.ReservedClassPrefix(class.String())
		if !ok {
			continue
		}
		if !containsString(got.Prefixes, prefix) {
			t.Errorf("binding does not carry the reserved prefix %q for class %q", prefix, class)
		}
	}
}

// A binding carrying only SOME observable classes is described as observed:
// the whole-split completion must not round a per-class arrangement up.
func TestAPIResidencyTopologyDoesNotRoundUpAPartialSplit(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	graph := beads.NewMemStore()
	state.graphBeadStore = graph
	s := &Server{state: state}

	topo := s.residencyTopology()
	if len(topo.Bindings) != 1 {
		t.Fatalf("got %d bindings, want 1", len(topo.Bindings))
	}
	if got := topo.Bindings[0].Classes; len(got) != 1 || got[0] != coordclass.ClassGraph {
		t.Fatalf("classes = %v, want only graph — a partial arrangement must be described honestly", got)
	}
}

// A refused city's class stores refuse, and the topology carries the refusal so
// every plan that touches a binding fails loud with the remedy.
func TestAPIResidencyTopologyCarriesTheStandingRefusal(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	refusing := refusingTestStore{err: standingTestRefusal{}}
	state.graphBeadStore = refusing
	state.sessionsBeadStore = refusing
	state.ordersBeadStore = refusing
	state.nudgesBeadStore = refusing
	s := &Server{state: state}

	topo := s.residencyTopology()
	if topo.Refused == nil {
		t.Fatal("a refusing binding produced a topology with no refusal — the deleted-[storage] trap")
	}
	if _, err := storeref.Plan(storeref.RoutedWork{}, topo); err == nil {
		t.Fatal("RoutedWork planned successfully over a refused topology")
	}
}

type standingTestRefusal struct{}

func (standingTestRefusal) Error() string           { return "storage refused: run `gc storage migrate`" }
func (standingTestRefusal) StandingStorageRefusal() {}

type refusingTestStore struct {
	beads.Store
	err error
}

func (s refusingTestStore) StorageRefusal() error { return s.err }
