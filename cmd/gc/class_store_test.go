package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// controllerClassAccessor names a controllerState per-class accessor for the
// identity conformance table.
type controllerClassAccessor struct {
	name string
	got  func(cs *controllerState) beads.Store
}

var controllerCityClassAccessors = []controllerClassAccessor{
	// graphBeadStore returns the strongly-typed beads.GraphStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"graphBeadStore", func(cs *controllerState) beads.Store { return cs.graphBeadStore().Store }},
	// sessionsBeadStore returns the strongly-typed beads.SessionStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"sessionsBeadStore", func(cs *controllerState) beads.Store { return cs.sessionsBeadStore().Store }},
	// mailBeadStore returns the strongly-typed beads.MailStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"mailBeadStore", func(cs *controllerState) beads.Store { return cs.mailBeadStore().Store }},
	// nudgesBeadStore returns the strongly-typed beads.NudgesStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"nudgesBeadStore", func(cs *controllerState) beads.Store { return cs.nudgesBeadStore().Store }},
	// ordersBeadStore returns the strongly-typed beads.OrdersStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"ordersBeadStore", func(cs *controllerState) beads.Store { return cs.ordersBeadStore("").Store }},
	// cityWorkStore returns the strongly-typed beads.WorkStore; unwrap its embedded
	// .Store so the identity check compares the underlying store pointer.
	{"cityWorkStore", func(cs *controllerState) beads.Store { return cs.cityWorkStore().Store }},
}

// TestControllerStateClassAccessorsAreIdentity pins that every controllerState
// per-class accessor returns the exact same pointer the call site uses today:
// CityBeadStore() for the city-resident classes and BeadStores() for work.
func TestControllerStateClassAccessorsAreIdentity(t *testing.T) {
	city := beads.NewMemStore()
	rig := beads.NewMemStore()
	cs := &controllerState{
		cityName:      "test-city",
		cityBeadStore: city,
		beadStores:    map[string]beads.Store{"myrig": rig},
	}

	for _, acc := range controllerCityClassAccessors {
		if got := acc.got(cs); !sameStorePtr(got, city) {
			t.Errorf("controllerState.%s() = %p, want CityBeadStore %p", acc.name, got, city)
		}
	}

	work := cs.workBeadStores()
	want := cs.BeadStores()
	if len(work) != len(want) {
		t.Fatalf("workBeadStores() len = %d, want %d", len(work), len(want))
	}
	for name, store := range want {
		// work[name] is a strongly-typed beads.WorkStore; unwrap its embedded .Store
		// so the identity check compares the underlying store pointer.
		if !sameStorePtr(work[name].Store, store) {
			t.Errorf("workBeadStores()[%q] = %p, want %p", name, work[name].Store, store)
		}
	}
}

// TestCityRuntimeClassAccessorsAreIdentity pins that every CityRuntime per-class
// accessor returns the same pointer the runtime call site uses today.
func TestCityRuntimeClassAccessorsAreIdentity(t *testing.T) {
	city := beads.NewMemStore()
	cr := &CityRuntime{
		cityName:            "test-city",
		standaloneCityStore: city,
		standaloneRigStores: map[string]beads.Store{"myrig": beads.NewMemStore()},
	}

	accessors := []struct {
		name string
		got  func() beads.Store
	}{
		// graphBeadStore returns the strongly-typed beads.GraphStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"graphBeadStore", func() beads.Store { return cr.graphBeadStore().Store }},
		// sessionsBeadStore returns the strongly-typed beads.SessionStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"sessionsBeadStore", func() beads.Store { return cr.sessionsBeadStore().Store }},
		// mailBeadStore returns the strongly-typed beads.MailStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"mailBeadStore", func() beads.Store { return cr.mailBeadStore().Store }},
		// nudgesBeadStore returns the strongly-typed beads.NudgesStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"nudgesBeadStore", func() beads.Store { return cr.nudgesBeadStore().Store }},
		// cityWorkStore returns the strongly-typed beads.WorkStore; unwrap its embedded
		// .Store so the identity check compares the underlying store pointer.
		{"cityWorkStore", func() beads.Store { return cr.cityWorkStore().Store }},
	}
	for _, acc := range accessors {
		if got := acc.got(); !sameStorePtr(got, city) {
			t.Errorf("CityRuntime.%s() = %p, want cityBeadStore %p", acc.name, got, city)
		}
	}
	if got := cr.ordersBeadStore("myrig").Store; !sameStorePtr(got, city) {
		t.Errorf("CityRuntime.ordersBeadStore() = %p, want cityBeadStore %p", got, city)
	}

	work := cr.workBeadStores()
	want := cr.rigBeadStores()
	if len(work) != len(want) {
		t.Fatalf("workBeadStores() len = %d, want %d", len(work), len(want))
	}
	for name, store := range want {
		// work[name] is a strongly-typed beads.WorkStore; unwrap its embedded .Store
		// so the identity check compares the underlying store pointer.
		if !sameStorePtr(work[name].Store, store) {
			t.Errorf("workBeadStores()[%q] = %p, want %p", name, work[name].Store, store)
		}
	}
}

// sameStorePtr reports pointer identity between two stores.
func sameStorePtr(a, b beads.Store) bool {
	ka, oka := storePointerKey(a)
	kb, okb := storePointerKey(b)
	return oka && okb && ka == kb
}

// TestCookOnClassRoutedPoursByRecipeClass pins the decision molecule.Cook cannot
// make: it picks its store before the formula has compiled, so a one-store
// caller pours graph-class workflows into the work ledger — and a poured v1
// formula pushed the other way hides its steps from `gc hook`.
func TestCookOnClassRoutedPoursByRecipeClass(t *testing.T) {
	dir := convergenceResidenceFormulaDir(t)

	for _, tt := range []struct {
		name    string
		formula string
		// wantGraph is whether the molecule must land in the graph store.
		wantGraph bool
	}{
		{"vapor formula is graph class", convergenceVaporFormula, true},
		{"poured v1 formula is work class", convergencePouredFormula, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Disjoint ID spaces: two fresh MemStores both mint gc-1, so
			// counting the store that should be EMPTY is the only sound oracle.
			work := beads.NewMemStore()
			graph := beads.NewMemStoreFrom(1000, nil, nil)

			// The parent lives in NEITHER store: Instantiate stamps ParentID and
			// never reads it back, which is the production cross-store shape.
			res, err := cookOnClassRouted(context.Background(), work, graph, tt.formula, []string{dir}, molecule.Options{
				ParentID: "gc-parent-lives-in-a-third-store",
			})
			if err != nil {
				t.Fatalf("cookOnClassRouted: %v", err)
			}
			if res == nil || res.Created == 0 {
				t.Fatalf("cook created no beads; the residence assertions below would be vacuous")
			}

			gotWork, gotGraph := countBeads(t, work), countBeads(t, graph)
			wantStore, otherStore := "graph", "work"
			wantCount, otherCount := gotGraph, gotWork
			if !tt.wantGraph {
				wantStore, otherStore = otherStore, wantStore
				wantCount, otherCount = otherCount, wantCount
			}
			if wantCount != res.Created {
				t.Errorf("the %s store holds %d of the %d beads the cook created; the molecule did not land in its class store", wantStore, wantCount, res.Created)
			}
			if otherCount != 0 {
				t.Errorf("the %s store holds %d beads; the cook leaked across the class boundary", otherStore, otherCount)
			}
		})
	}
}

// TestCookOnClassRoutedRequiresAParent keeps molecule.CookOn's attach-only
// contract: a missing ParentID would detach a molecule no autoclose can reap.
func TestCookOnClassRoutedRequiresAParent(t *testing.T) {
	dir := convergenceResidenceFormulaDir(t)
	_, err := cookOnClassRouted(context.Background(), beads.NewMemStore(), beads.NewMemStore(), convergencePouredFormula, []string{dir}, molecule.Options{})
	if err == nil {
		t.Fatal("cooking with no ParentID succeeded; the attach-only contract is gone")
	}
}

// TestCookOnClassRoutedChoosesItsStoreThroughTheLibraryEntry pins that this
// helper contributes the CHOICE and nothing else: the compile/validate/
// instantiate sequence around it belongs to molecule.CookChoosingStore, which
// calls the chooser exactly once, between validation and the first write. A
// hand-copied Cook body here would be a second implementation of that contract
// and would drift silently the next time Cook grows an invariant.
//
// The chooser is built inside cookOnClassRouted, so a caller cannot count its
// calls; what it CAN observe is the contract only the library entry has. A nil
// choice is refused in the chooser's own vocabulary before anything is written,
// and an inlined body has no such guard — it hands the nil store straight to
// molecule.Instantiate, which dereferences it.
func TestCookOnClassRoutedChoosesItsStoreThroughTheLibraryEntry(t *testing.T) {
	dir := convergenceResidenceFormulaDir(t)
	work := beads.NewMemStore()

	// A vapor formula classifies as graph, so a nil graph store IS the chooser
	// answering nil — the case CookChoosingStore refuses between validation and
	// the first write.
	_, err := cookOnClassRouted(context.Background(), work, nil, convergenceVaporFormula, []string{dir}, molecule.Options{
		ParentID: "gc-parent-lives-in-a-third-store",
	})
	if err == nil {
		t.Fatal("cooking a graph-class formula with no graph store succeeded")
	}
	if !strings.Contains(err.Error(), "store chooser returned nil") {
		t.Errorf("the refusal did not come from molecule.CookChoosingStore's choice guard, so the sequence is inlined here again: %v", err)
	}
	if !strings.Contains(err.Error(), convergenceVaporFormula) {
		t.Errorf("the refusal does not name the formula: %v", err)
	}
	if got := countBeads(t, work); got != 0 {
		t.Errorf("the work store holds %d bead(s) from a cook that never chose a store; the choice happens before the first write", got)
	}

	// The "after validation" half: a formula that never compiles must not reach
	// the choice at all, so the nil graph store above cannot be what a caller
	// with a bad formula name hears about.
	if _, err := cookOnClassRouted(context.Background(), work, nil, "conv-residence-no-such-formula", []string{dir}, molecule.Options{
		ParentID: "gc-parent-lives-in-a-third-store",
	}); err == nil {
		t.Fatal("cooking a formula that does not exist succeeded")
	} else if strings.Contains(err.Error(), "store chooser") {
		t.Errorf("a formula that never compiled reached the store choice: %v", err)
	}
}
