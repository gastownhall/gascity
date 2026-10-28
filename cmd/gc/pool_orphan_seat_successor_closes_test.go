package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestReleaseOrphanedPoolAssignments_ReleasesWhenSuccessorAlsoCloses pins the
// exit of the seat hand-over: the claim moves to the closed row's unique
// successor, and when that successor closes too — with no open row left
// answering the seat — the next sweep releases the bead. A hand-over that
// could not eventually release would strand the claim behind a seat that no
// live session holds, the exact dead-assignee state the sweep exists to clear.
func TestReleaseOrphanedPoolAssignments_ReleasesWhenSuccessorAlsoCloses(t *testing.T) {
	store, _, successor, work := seatHandoverClaimedFixture(t)

	// Sweep 1: the closed predecessor's claim moves to the seat's one open
	// successor instead of reopening the work.
	released := releaseOrphanedPoolAssignments(
		store, beads.SessionStore{Store: store}, seatHandoverConfig(), "",
		sessionInfosFromBeads([]beads.Bead{successor}), []beads.Bead{work},
		nil, nil, nil, nil, nil, nil)
	if len(released) != 0 {
		t.Fatalf("sweep 1 released %v, want none: the claim must move to the successor first", released)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work after sweep 1: %v", err)
	}
	if got.Assignee != successor.ID {
		t.Fatalf("work assignee after sweep 1 = %q, want the successor's bead ID %q", got.Assignee, successor.ID)
	}

	// The successor closes as well; the sweep fences on a fresh snapshot.
	closed := "closed"
	if err := store.Update(successor.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close successor: %v", err)
	}
	snapshot, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work after closing the successor: %v", err)
	}

	// Sweep 2: no open row answers the seat, so the claim is dead and the
	// work is released back to the pool.
	released = releaseOrphanedPoolAssignments(
		store, beads.SessionStore{Store: store}, seatHandoverConfig(), "",
		nil, []beads.Bead{snapshot},
		nil, nil, nil, nil, nil, nil)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("sweep 2 released %v, want exactly [%s]: the successor closed and no open row answers the seat", released, work.ID)
	}
	after, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work after sweep 2: %v", err)
	}
	if after.Status != "open" || after.Assignee != "" {
		t.Fatalf("work after sweep 2 = status %q assignee %q, want reopened open work with no assignee", after.Status, after.Assignee)
	}
}
