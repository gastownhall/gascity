package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestBeadStoresForIDDefaultBackendIsCityLed pins the single-store invariant:
// when the graph class is NOT relocated (GraphBeadStore() == CityBeadStore()),
// the class-prefix arm never fires, so the unrouted by-id candidate set leads
// with the city store ahead of the per-rig work stores — byte-identical to the
// pre-seam ordering.
func TestBeadStoresForIDDefaultBackendIsCityLed(t *testing.T) {
	st := newFakeState(t)
	city := beads.NewMemStore()
	st.cityBeadStore = city
	// Drop the rig store so the city store is the only by-id candidate.
	st.stores = map[string]beads.Store{}
	st.cfg.Rigs = nil
	s := New(st)

	got := s.beadStoresForID("gcg-1")
	if len(got) != 1 {
		t.Fatalf("beadStoresForID returned %d stores, want 1 (city-led, no graph arm); got %v", len(got), got)
	}
	if got[0] != city {
		t.Errorf("beadStoresForID[0] = %p, want CityBeadStore %p", got[0], city)
	}
}

// TestBeadStoresForIDClassAwareGraphArm pins the relocated-graph behavior: with a
// DISTINCT dedicated graph store, a graph-class id (reserved prefix "gcg") that is
// not reachable via a rig/HQ prefix resolves to [graph, work] — graph-first — so
// the by-id Get-then-mutate handler loop pins the graph store on the first probe.
// On a single-store city (graph == city) the arm is skipped, so this path stays
// byte-identical there (covered by TestBeadStoresForIDDefaultBackendIsCityLed).
func TestBeadStoresForIDClassAwareGraphArm(t *testing.T) {
	work := beads.NewMemStore()
	graph := beads.NewMemStore()

	st := newFakeState(t)
	st.cityBeadStore = work   // plain work store
	st.graphBeadStore = graph // dedicated, distinct graph store
	st.stores = nil
	st.cfg.Rigs = nil

	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatalf("ReservedClassPrefix(graph) returned ok=false; expected a reserved prefix")
	}
	s := New(st)

	got := s.beadStoresForID(prefix + "-1")
	if len(got) != 2 || got[0] != s.state.GraphBeadStore().Store || got[1] != s.state.CityBeadStore() {
		t.Fatalf("beadStoresForID(%s-1) = %v (len %d), want [graph, work]", prefix, got, len(got))
	}
}
