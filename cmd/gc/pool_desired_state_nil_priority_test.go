package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
