package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestCanonicalizeLegacyBoundUnassignedRoutedWorkLeavesForeignCityRowAlone
// reproduces gastownhall/gascity#5846: two cities share one rig store. City A
// runs an unbound pool agent "qcore/omp"; city B imports the same agent under
// the "pool" binding, so B's canonical identity is "qcore/pool.omp". City A
// declares no "pool" import, so a row routed to "qcore/pool.omp" is B's
// canonical work, not A's legacy spelling. A's reconcile tick must leave it
// alone; otherwise A and B rewrite the row back and forth on every tick pair.
func TestCanonicalizeLegacyBoundUnassignedRoutedWorkLeavesForeignCityRowAlone(t *testing.T) {
	cityA := &config.City{
		Agents: []config.Agent{poolAgent("omp", "qcore", intPtr(3), 0)},
	}
	bAgent := poolAgent("omp", "qcore", intPtr(3), 0)
	bAgent.BindingName = "pool"
	cityB := &config.City{
		Imports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
		Agents:  []config.Agent{bAgent},
	}
	const foreignCanonical = "qcore/pool.omp"
	if got := bAgent.QualifiedName(); got != foreignCanonical {
		t.Fatalf("city B canonical identity = %q, want %q", got, foreignCanonical)
	}

	wb := workBead("wb-shared", foreignCanonical, "", "open", 2)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
	shared := &updateCountingStore{Store: mem}

	// Alternate the two cities' reconcile ticks over the shared row.
	var trail []string
	for tick := 0; tick < 4; tick++ {
		cfg, city := cityA, "A"
		if tick%2 == 1 {
			cfg, city = cityB, "B"
		}
		cur, err := mem.Get("wb-shared")
		if err != nil {
			t.Fatalf("Get(wb-shared): %v", err)
		}
		canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{cur}, []beads.Store{shared}, []string{"rig:qcore"}, io.Discard)
		after, _ := mem.Get("wb-shared")
		trail = append(trail, city+":"+after.Metadata["gc.routed_to"])
	}

	if shared.updates != 0 {
		t.Errorf("shared rig row rewritten %d times over 4 alternating ticks (trail %v); "+
			"city A must not canonicalize city B's %q route", shared.updates, trail, foreignCanonical)
	}
	got, _ := mem.Get("wb-shared")
	if routed := got.Metadata["gc.routed_to"]; routed != foreignCanonical {
		t.Errorf("gc.routed_to = %q, want %q (foreign city's canonical route left alone)", routed, foreignCanonical)
	}
}

// TestCanonicalizeLegacyRehomeTwoCitiesSharedRigConverges is AC-6 of the
// #5846 design: both cities' both arms run alternately over one shared rig
// store holding each city's row — city A's open unassigned row routed to its
// canonical "qcore/omp" and city B's in_progress row assigned and routed to
// its canonical "qcore/pool.omp". With the guard neither city ever issues an
// Update: every rewrite is either already canonical or is refused, so the two
// cities converge by construction instead of oscillating.
func TestCanonicalizeLegacyRehomeTwoCitiesSharedRigConverges(t *testing.T) {
	cityA := &config.City{
		Agents: []config.Agent{poolAgent("omp", "qcore", intPtr(3), 0)},
	}
	bAgent := poolAgent("omp", "qcore", intPtr(3), 0)
	bAgent.BindingName = "pool"
	cityB := &config.City{
		Imports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
		Agents:  []config.Agent{bAgent},
	}

	rowA := workBead("ga-a-row", "qcore/omp", "", "open", 2)
	rowB := workBead("ga-b-row", "qcore/pool.omp", "qcore/pool.omp", "in_progress", 2)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{rowA, rowB}, nil)
	shared := &updateCountingStore{Store: mem}

	for pass := 0; pass < 10; pass++ {
		cfg := cityA
		if pass%2 == 1 {
			cfg = cityB
		}
		aRow, err := mem.Get("ga-a-row")
		if err != nil {
			t.Fatalf("pass %d: Get(ga-a-row): %v", pass, err)
		}
		bRow, err := mem.Get("ga-b-row")
		if err != nil {
			t.Fatalf("pass %d: Get(ga-b-row): %v", pass, err)
		}
		// The assigned arm's vocabulary is bare rig names; the routed arm's is
		// scoped refs. Each city's census reads every row of the shared store.
		canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{aRow, bRow}, []beads.Store{shared, shared}, []string{"qcore", "qcore"}, newSessionBeadSnapshot(nil), io.Discard)
		canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{aRow}, []beads.Store{shared}, []string{"rig:qcore"}, io.Discard)
	}

	if shared.updates != 0 {
		t.Errorf("shared rows written %d time(s) over 10 alternating two-arm passes; the guard must converge the cities to no writes", shared.updates)
	}
	aRow, _ := mem.Get("ga-a-row")
	if routed := aRow.Metadata["gc.routed_to"]; routed != "qcore/omp" {
		t.Errorf("city A row gc.routed_to = %q, want qcore/omp", routed)
	}
	bRow, _ := mem.Get("ga-b-row")
	if bRow.Assignee != "qcore/pool.omp" || bRow.Metadata["gc.routed_to"] != "qcore/pool.omp" {
		t.Errorf("city B row = (assignee %q, route %q), want both qcore/pool.omp", bRow.Assignee, bRow.Metadata["gc.routed_to"])
	}
}
