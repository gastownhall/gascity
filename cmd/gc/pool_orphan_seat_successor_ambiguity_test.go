package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestReleaseOrphanedPoolAssignments_NoUniqueSeatSuccessorStillReleases walks
// the AC-4 ambiguity shapes: when the seat's successor is not exactly one
// qualifying row — none, two, only an archived row, or only a row that cannot
// reach the work's store — the hand-over does not apply and the claim releases
// exactly as before. There is no tie-break among successors.
func TestReleaseOrphanedPoolAssignments_NoUniqueSeatSuccessorStillReleases(t *testing.T) {
	t.Run("no open row answers the seat", func(t *testing.T) {
		store, work := seatHandoverFixture(t)
		released := releaseOrphanedPoolAssignments(
			store, beads.SessionStore{Store: store}, seatHandoverConfig(), "",
			nil, []beads.Bead{work}, nil, nil, nil, nil, nil, nil)
		seatHandoverReleaseWanted(t, store, work, released)
	})
	t.Run("two open rows answer the seat", func(t *testing.T) {
		store, work := seatHandoverFixture(t)
		seatRow(t, store, "successor a", "worker-mc-new-a", "baker")
		seatRow(t, store, "successor b", "worker-mc-new-b", "baker")
		released := releaseOrphanedPoolAssignments(
			store, beads.SessionStore{Store: store}, seatHandoverConfig(), "",
			nil, []beads.Bead{work}, nil, nil, nil, nil, nil, nil)
		seatHandoverReleaseWanted(t, store, work, released)
	})
	t.Run("only an archived row answers the seat", func(t *testing.T) {
		store, work := seatHandoverFixture(t)
		archived := seatRow(t, store, "archived successor", "worker-mc-archived", "baker")
		archivedState := "archived"
		if err := store.Update(archived.ID, beads.UpdateOpts{Metadata: map[string]string{"state": archivedState}}); err != nil {
			t.Fatalf("archive successor: %v", err)
		}
		// Project the row as the sweep's snapshot would read it: after the
		// state write, not from the pre-update in-memory value.
		archived, getErr := store.Get(archived.ID)
		if getErr != nil {
			t.Fatalf("re-read archived successor: %v", getErr)
		}
		released := releaseOrphanedPoolAssignments(
			store, beads.SessionStore{Store: store}, seatHandoverConfig(), "",
			sessionInfosFromBeads([]beads.Bead{archived}), []beads.Bead{work},
			nil, nil, nil, nil, nil, nil)
		seatHandoverReleaseWanted(t, store, work, released)
	})
	t.Run("only a row in an unreachable store answers the seat", func(t *testing.T) {
		cfg := testRigScopedPoolReleaseConfig()
		store := beads.NewMemStore()
		predecessor, err := store.Create(beads.Bead{
			Title:  "predecessor worker",
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":               "worker-mc-old",
				"template":                   "repo/worker",
				"state":                      "awake",
				namedSessionMetadataKey:      "true",
				namedSessionIdentityMetadata: "baker",
			},
		})
		if err != nil {
			t.Fatalf("create predecessor: %v", err)
		}
		work, err := store.Create(beads.Bead{
			Title:    "work in another rig's store",
			Type:     "task",
			Assignee: predecessor.ID,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"},
		})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		inProgress := "in_progress"
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("set in_progress: %v", err)
		}
		closed := "closed"
		if err := store.Update(predecessor.ID, beads.UpdateOpts{Status: &closed}); err != nil {
			t.Fatalf("close predecessor: %v", err)
		}
		successor, err := store.Create(beads.Bead{
			Title:  "successor in its own rig",
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":               "worker-mc-new",
				"template":                   "repo/worker",
				"state":                      "awake",
				namedSessionMetadataKey:      "true",
				namedSessionIdentityMetadata: "baker",
			},
		})
		if err != nil {
			t.Fatalf("create successor: %v", err)
		}
		before, err := store.Get(work.ID)
		if err != nil {
			t.Fatalf("re-read work: %v", err)
		}
		released := releaseOrphanedPoolAssignments(
			store, beads.SessionStore{Store: store}, cfg, "",
			sessionInfosFromBeads([]beads.Bead{successor}), []beads.Bead{before},
			[]beads.Store{store}, []string{"other-repo"}, nil, nil, nil, nil)
		seatHandoverReleaseWanted(t, store, before, released)
	})
}

// seatHandoverFixture is the AC-1 shape — closed row P of seat "baker" holding
// in_progress work under its bead ID — with NO successor created, so each
// subtest seeds the ambiguity it names.
func seatHandoverFixture(t *testing.T) (beads.Store, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	predecessor := seatRow(t, store, "predecessor", "worker-mc-old", "baker")
	work, err := store.Create(beads.Bead{
		Title:    "work claimed under the predecessor's bead ID",
		Type:     "task",
		Assignee: predecessor.ID,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:    "worker",
			beadmeta.SessionIDMetadataKey:   predecessor.ID,
			beadmeta.SessionNameMetadataKey: "worker-mc-old",
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	closed := "closed"
	if err := store.Update(predecessor.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close predecessor: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}
	return store, work
}

// seatHandoverReleaseWanted asserts the guard outcome: the claim was released
// (the bead reopened with no assignee) exactly as the base sweep does.
func seatHandoverReleaseWanted(t *testing.T, store beads.Store, work beads.Bead, released []releasedPoolAssignment) {
	t.Helper()
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released %v, want exactly [%s]: no unique qualifying successor means no hand-over, so the claim releases as before",
			released, work.ID)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work after sweep: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("work after sweep = status %q assignee %q, want reopened open work with no assignee", got.Status, got.Assignee)
	}
}
