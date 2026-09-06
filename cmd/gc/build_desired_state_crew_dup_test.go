package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestBuildDesiredState_RoutedToLiveNamedSession_NoPoolDuplicate is the crew-duplicate
// repro. A bead routed (gc.routed_to) at an on_demand [[named_session]] agent whose
// canonical named session is ALREADY LIVE must NOT register pool demand — otherwise a
// `<name>-1` pool slot spawns beside the resident (double burn, two agents in one rig).
// The live resident claims the routed bead via its own hook. When the resident is
// ASLEEP the pool slot still wakes it (preserved by the sibling scale-from-zero tests).
//
// MUST fail on current code (ScaleCheckCounts==1) and pass after the fix.
func TestBuildDesiredState_RoutedToLiveNamedSession_NoPoolDuplicate(t *testing.T) {
	cfg, cityStore, rigStores, identity := newNoScaleCheckNamedBackingCity(t)

	// Routed, unassigned work in the city store (the demand that spawns the -1).
	if _, err := cityStore.Create(beads.Bead{
		ID:       "bead-city-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": identity},
	}); err != nil {
		t.Fatal(err)
	}

	// A LIVE canonical named session for that identity (the resident crew role).
	liveNamed := beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Status: "open",
		Metadata: map[string]string{
			"configured_named_session":  "true",
			"configured_named_identity": identity,
			"configured_named_mode":     "on_demand",
		},
	}
	snap := newSessionBeadSnapshot([]beads.Bead{liveNamed})

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, snap, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[identity]; got != 0 {
		t.Fatalf("routed-to-LIVE-named-session registered pool demand = %d, want 0 "+
			"(a live resident claims the bead via its hook; registering demand spawns the "+
			"%s-1 duplicate)", got, identity)
	}
	for key, tp := range result.State {
		if tp.TemplateName == identity && tp.Alias != "" && tp.Alias != identity {
			t.Fatalf("spawned pool duplicate %q (alias=%s) beside the live named session %q",
				key, tp.Alias, identity)
		}
	}
}
