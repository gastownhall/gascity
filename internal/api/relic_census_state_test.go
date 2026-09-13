package api

// The relic verdict, on the API plane.
//
// The API holds no storageRoutes and opens no binding of its own, so it cannot
// take the census — but it plans by-id reads against the same bindings the CLI
// does, and a plane that kept probing after the other retired would resolve the
// same id to two different stores. The verdict therefore crosses the State
// surface, and until it arrives the answer stays pessimistic.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// relocatedGraphState wires a distinct graph store onto a fake State, which is
// the smallest shape residencyTopology reads as one binding.
func relocatedGraphState(t *testing.T, graph beads.Store) *fakeState {
	t.Helper()
	st := newFakeState(t)
	st.cityBeadStore = beads.NewMemStore()
	st.graphBeadStore = graph
	st.stores = nil
	st.cfg.Rigs = nil
	return st
}

// mintingGraphStore is a binding that declares the graph namespace it mints
// into, so the mint half of the retirement condition is satisfied and the relic
// half is the only thing left deciding.
func mintingGraphStore(t *testing.T) beads.Store {
	t.Helper()
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("graph has no reserved mint prefix")
	}
	return newPrefixDeclaringStore(prefix)
}

// soleAPIBinding is the one binding a relocated city's API plane observes.
func soleAPIBinding(t *testing.T, st *fakeState) storeref.ClassBinding {
	t.Helper()
	topo := New(st).residencyTopology()
	if len(topo.Bindings) != 1 {
		t.Fatalf("got %d bindings, want the one this state relocates", len(topo.Bindings))
	}
	return topo.Bindings[0]
}

// apiBindingLegRead reports whether a by-id plan for id would read the binding.
func apiBindingLegRead(t *testing.T, st *fakeState, id string) bool {
	t.Helper()
	topo := New(st).residencyTopology()
	plan, err := storeref.Plan(storeref.ByID{ID: id}, topo)
	if err != nil {
		t.Fatalf("planning a by-id read of %q: %v", id, err)
	}
	for _, leg := range plan.Legs {
		for _, binding := range topo.Bindings {
			if leg.Leg.Ref == binding.Leg.Ref {
				return true
			}
		}
	}
	return false
}

// A State that reports the boot census found nothing retires the probe here
// too. This is the whole point of plumbing the verdict across: the CLI plane
// retired on the same city, and the two planes must agree.
func TestAPIRetiresTheProbeWhenTheStateReportsACleanBinding(t *testing.T) {
	graph := mintingGraphStore(t)
	st := relocatedGraphState(t, graph)
	st.bindingRelics = map[beads.Store]bool{graph: false}

	binding := soleAPIBinding(t, st)
	if !binding.MintsReserved {
		t.Fatal("the binding does not mint inside its own namespaces, so the relic bit decides nothing and this row tests no retirement")
	}
	if binding.HasLegacyResidents {
		t.Error("the API kept the relic bit set over a State that reported a clean census")
	}
	if apiBindingLegRead(t, st, "ga-1") {
		t.Error("a work-shaped by-id read still probes a binding the census cleared; the two planes now disagree about where that id lives")
	}
}

// The other direction, and the one that must survive every refactor: a binding
// the census found a carried-across bead in keeps probing.
func TestAPIKeepsTheProbeWhenTheStateReportsRelics(t *testing.T) {
	graph := mintingGraphStore(t)
	st := relocatedGraphState(t, graph)
	st.bindingRelics = map[beads.Store]bool{graph: true}

	if !soleAPIBinding(t, st).HasLegacyResidents {
		t.Error("the API cleared the relic bit over a State that reported relics")
	}
	if !apiBindingLegRead(t, st, "ga-1") {
		t.Error("a work-shaped by-id read skips a binding holding beads reachable no other way")
	}
}

// The default, for every State that has no census to report — a seeded dashport
// city, an API built without a runtime, a controller whose routes never booted.
// An unasked question is not a clean answer.
func TestAPIKeepsTheProbeForAnUncensusedState(t *testing.T) {
	st := relocatedGraphState(t, mintingGraphStore(t))

	if !soleAPIBinding(t, st).HasLegacyResidents {
		t.Error("a State that censused nothing reported a clean binding")
	}
	if !apiBindingLegRead(t, st, "ga-1") {
		t.Error("an uncensused plane retired its probe")
	}
}
