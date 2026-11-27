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
