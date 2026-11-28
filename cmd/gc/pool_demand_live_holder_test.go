package main

// ga-6ab279: the demand-side twin of the wake filter's ownership arm.
//
// On a city that relocates nothing, filterAssignedWorkBeadsForPoolDemand kept
// a rig agent's assigned work only through rig-equality or a relocated class
// binding, so a live rig-pool session's own in-progress claim on a city-store
// bead (ref "") was dropped. The holder then counted as idle capacity, the
// queued bead's tier=new request was realized onto it, and the pool never ran
// a second session beside a working one — while the wake filter accepted the
// same claim for the same holder (assignedWorkClaimRefs includes the leading
// work arm). The rows here pin the widened demand arm: the bead is kept only
// when its ref is one the wake filter accepts for that holder, its assignee
// is an open session's own exact identity, that session's template is the
// template the bead routes to, and the session claims a live runtime.

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// liveHolderRows runs the v1 wrapper over one work row on the single-store
// fixture with the holder in the given state, and reports the kept beads and
// their refs.
func liveHolderRows(t *testing.T, holderState string, work []beads.Bead, refs []string) ([]beads.Bead, []string) {
	t.Helper()
	cfg, cityPath, infos := rigScopedPoolDemandFixtureWithHolderState(t, holderState)
	seedNoRoutes(t, cityPath)
	return filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, work, refs)
}

// The reported shape: a live (active) holder's own in-progress claim on a
// city-store bead, ref "", is kept with its own ref, index-aligned. Fails on
// the base: the row is dropped and the holder counts as idle.
func TestPoolDemandFilterKeepsALiveHoldersOwnCityStoreClaim(t *testing.T) {
	kept, keptRefs := liveHolderRows(t, "active", []beads.Bead{routedPoolWorkBead()}, []string{""})

	if len(kept) != 1 || kept[0].ID != "gcg-1" {
		t.Fatalf("filtered work = %#v, want the live holder's own city-store claim kept", kept)
	}
	if !slices.Equal(keptRefs, []string{""}) {
		t.Fatalf("kept refs = %q, want [\"\"]: the kept row's own ref, index-aligned", keptRefs)
	}
}

// A holder claiming a live runtime in either of its other spellings — a row
// still creating, one waiting to stop cleanly — is as much a working holder
// as an active one.
func TestPoolDemandFilterKeepsACreatingOrDrainingHoldersClaim(t *testing.T) {
	for _, state := range []string{"creating", "draining"} {
		t.Run(state, func(t *testing.T) {
			kept, _ := liveHolderRows(t, state, []beads.Bead{routedPoolWorkBead()}, []string{""})
			if len(kept) != 1 || kept[0].ID != "gcg-1" {
				t.Fatalf("filtered work = %#v, want the %s holder's own claim kept", kept, state)
			}
		})
	}
}

// A rig-resident row beside a live holder's city-store row: both kept, and
// the returned refs stay aligned with the kept beads.
func TestPoolDemandFilterKeepsALiveHolderClaimBesideARigRow(t *testing.T) {
	rigRow := routedPoolWorkBead()
	rigRow.ID = "gcg-2"
	work := []beads.Bead{rigRow, routedPoolWorkBead()}

	kept, keptRefs := liveHolderRows(t, "active", work, []string{"riga", ""})

	if ids := beadIDs(kept); !slices.Equal(ids, []string{"gcg-2", "gcg-1"}) {
		t.Fatalf("kept IDs = %v, want both the rig row and the live holder's row, in order", ids)
	}
	if !slices.Equal(keptRefs, []string{"riga", ""}) {
		t.Fatalf("kept refs = %q, want [riga \"\"] aligned with the kept beads", keptRefs)
	}
}
