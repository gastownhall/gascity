package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestBeadPriority_NilDefaultsToP2 is the regression test for the
// nil-Priority default mismatch (ra-vsvjlx): native_dolt_store.go persists
// (and round-trips) an unset Priority as P2, so beadPriority must treat nil
// the same way rather than coercing it to P0 (highest) and letting an
// unset-priority bead out-schedule an explicitly-labeled P1 bead.
func TestBeadPriority_NilDefaultsToP2(t *testing.T) {
	got := beadPriority(beads.Bead{ID: "w-nil-priority", Priority: nil})
	if got != 2 {
		t.Fatalf("beadPriority(nil Priority) = %d, want 2 (bd's documented default)", got)
	}
}

func TestBeadPriority_ExplicitPriorityPreserved(t *testing.T) {
	got := beadPriority(beads.Bead{ID: "w-p1", Priority: intPtr(1)})
	if got != 1 {
		t.Fatalf("beadPriority(explicit P1) = %d, want 1", got)
	}

	got = beadPriority(beads.Bead{ID: "w-p0", Priority: intPtr(0)})
	if got != 0 {
		t.Fatalf("beadPriority(explicit P0) = %d, want 0", got)
	}
}

// TestComputePoolDesiredStates_ExplicitP1BeatsNilPriority is the end-to-end
// half of the ra-vsvjlx regression: beadPriority's return value only means
// anything once it has been ranked and sorted by applyNestedCaps. With
// max_active_sessions=1, the explicitly-labeled P1 bead must take the single
// slot even though the unset-priority bead is listed first.
func TestComputePoolDesiredStates_ExplicitP1BeatsNilPriority(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(1), 0)},
	}
	unset := workBead("w-unset", "claude", "s1", "in_progress", 0)
	unset.Priority = nil
	work := []beads.Bead{
		unset,
		workBead("w-p1", "claude", "s2", "in_progress", 1),
	}
	sessions := []beads.Bead{
		sessionBead("s1", "open"),
		sessionBead("s2", "open"),
	}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("expected 1 request under cap=1, got %#v", result)
	}
	if got := result[0].Requests[0].WorkBeadID; got != "w-p1" {
		t.Errorf("accepted work bead = %q, want %q — an unset-priority bead must not out-schedule an explicit P1", got, "w-p1")
	}
}
