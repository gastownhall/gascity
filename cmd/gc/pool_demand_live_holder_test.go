package main

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A rig-scoped pool whose work is filed in the CITY store: on a city that
// relocates nothing, the template-scoped reach test rejects every city-store
// row for a rig agent. The wake filter still accepts a holder's own claim on
// such a row (its exact assignee identity under assignedWorkClaimRefs), so pool
// demand has to as well. Otherwise the busy holder gets no resume request,
// looks reusable, and the next queued bead's new-tier request is realized onto
// it: one session, no create.

// liveHolderSessionName is the holder's exact assignee identity in the filter
// rows below.
const liveHolderSessionName = "test-city--worker-1"

// liveHolderFilterFixture is the single-store rig pool fixture with its one
// open riga/worker session in state.
func liveHolderFilterFixture(t *testing.T, state string) (*config.City, string, []sessionpkg.Info) {
	t.Helper()
	cfg, cityPath, _ := rigScopedPoolDemandFixture(t)
	infos := sessionInfosFromBeads([]beads.Bead{{
		ID:     "gcs-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": liveHolderSessionName,
			"state":        state,
			"pool_managed": "true",
		},
	}})
	seedNoRoutes(t, cityPath)
	return cfg, cityPath, infos
}

// liveHolderWork is an in-progress claim by assignee routed to riga/worker.
func liveHolderWork(id, assignee string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Status:   "in_progress",
		Assignee: assignee,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"},
	}
}

func TestPoolDemandFilterKeepsALiveHoldersCityStoreClaim(t *testing.T) {
	for _, state := range []string{"active", "awake", "creating", "draining"} {
		t.Run(state, func(t *testing.T) {
			cfg, cityPath, infos := liveHolderFilterFixture(t, state)
			work := []beads.Bead{liveHolderWork("gcg-1", liveHolderSessionName)}

			kept, keptRefs := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, work, []string{""})

			if ids := beadIDs(kept); !slices.Equal(ids, []string{"gcg-1"}) || !slices.Equal(keptRefs, []string{""}) {
				t.Fatalf("kept = %v refs %q, want [gcg-1] [\"\"]: a live holder's own city-store claim is pool demand", ids, keptRefs)
			}
		})
	}
}

func TestPoolDemandFilterKeepsALiveHoldersClaimAlignedInAMixedSlice(t *testing.T) {
	cfg, cityPath, infos := liveHolderFilterFixture(t, "active")
	work := []beads.Bead{
		liveHolderWork("gcg-rig", ""),
		liveHolderWork("gcg-unassigned", ""),
		liveHolderWork("gcg-held", liveHolderSessionName),
	}
	work[0].Status = "open"
	work[1].Status = "open"

	kept, keptRefs := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, work, []string{"riga", "", ""})

	if ids := beadIDs(kept); !slices.Equal(ids, []string{"gcg-rig", "gcg-held"}) {
		t.Fatalf("kept IDs = %v, want [gcg-rig gcg-held]", ids)
	}
	if !slices.Equal(keptRefs, []string{"riga", ""}) {
		t.Fatalf("kept refs = %q, want [riga \"\"] aligned with the kept rows", keptRefs)
	}
}
