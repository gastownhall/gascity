package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestBuildDesiredState_RoutedToLiveNamedSession_NoPoolDuplicate ensures routed
// work does not create a pool slot alongside an already-live on-demand named
// session. The resident session can claim the routed work itself; when it is
// asleep, the existing scale-from-zero coverage retains the wake path.
func TestBuildDesiredState_RoutedToLiveNamedSession_NoPoolDuplicate(t *testing.T) {
	cfg, cityStore, rigStores, identity := newNoScaleCheckNamedBackingCity(t)

	if _, err := cityStore.Create(beads.Bead{
		ID:       "bead-city-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": identity},
	}); err != nil {
		t.Fatal(err)
	}

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
		t.Fatalf("routed-to-LIVE-named-session registered pool demand = %d, want 0", got)
	}
	for key, tp := range result.State {
		if tp.TemplateName == identity && tp.Alias != "" && tp.Alias != identity {
			t.Fatalf("spawned pool duplicate %q beside the live named session %q", key, identity)
		}
	}
}
