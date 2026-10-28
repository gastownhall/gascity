package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// seatHandoverConfig is the Part B city: one pool-capable template "worker"
// with no named-session configuration at all, so the seat identity the
// hand-over resolves is read off the session beads' configured_named_identity
// metadata rather than leaned out of the config table.
func seatHandoverConfig() *config.City {
	return &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		},
	}
}

// seatRow creates an open, awake session bead of the city's one pool template,
// answering to the named seat identity.
func seatRow(t *testing.T, store beads.Store, title, sessionName, identity string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title:  title,
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"template":                   "worker",
			"state":                      "awake",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatalf("create %s: %v", title, err)
	}
	return bead
}

// TestReleaseOrphanedPoolAssignments_HandsClosedSeatClaimToUniqueSuccessor is
// the Part B regression for issue #6362 (Gap 1): work claimed under a closed
// row's BEAD ID (the only spelling the dead-assignee sweep can see) is moved to
// that seat's unique open successor instead of being reopened underneath the
// replacement that is already running it.
//
// The named-identity spelling of the same claim already survives via the
// named-route gate; the bead-ID spelling names one row, and when that row
// closes, no successor ever answers to the claim without this hand-over.
func TestReleaseOrphanedPoolAssignments_HandsClosedSeatClaimToUniqueSuccessor(t *testing.T) {
	cfg := seatHandoverConfig()
	store := beads.NewMemStore()
	predecessor := seatRow(t, store, "predecessor", "worker-mc-old", "baker")
	successor := seatRow(t, store, "successor", "worker-mc-new", "baker")

	work, err := store.Create(beads.Bead{
		Title:    "work claimed under the predecessor's bead ID",
		Type:     "task",
		Assignee: predecessor.ID,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:    "worker",
			beadmeta.SessionIDMetadataKey:   predecessor.ID,
			beadmeta.SessionNameMetadataKey: "worker-mc-old",
			beadmeta.ClaimedAtMetadataKey:   "2026-10-01T00:00:00Z",
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
	// Refresh the snapshot after the status write: the sweep fences its writes
	// on the snapshot's status and assignee, and a stale pre-update read is
	// refused as "moved" rather than acted on.
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		cfg,
		"",
		sessionInfosFromBeads([]beads.Bead{successor}),
		[]beads.Bead{work},
		nil, nil, nil, nil, nil, nil,
	)
	if len(released) != 0 {
		t.Fatalf("released %v, want none: the closed row's seat has exactly one open successor (%s, same template, same seat identity %q), so the claim must move to it, not reopen the work",
			released, successor.ID, "baker")
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work after sweep: %v", err)
	}
	if got.Assignee != successor.ID {
		t.Fatalf("work assignee after sweep = %q, want the successor's bead ID %q", got.Assignee, successor.ID)
	}
	if got.Status != "in_progress" {
		t.Fatalf("work status after sweep = %q, want in_progress (the hand-over moves the claim; it does not reopen the work)", got.Status)
	}
}
