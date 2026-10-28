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

// TestSeatHandover_PreservesEveryFieldExceptAssignee pins the AC-2 contract:
// the hand-over writes Assignee and nothing else. Status, the claim projection
// (gc.session_id, gc.session_name, gc.claimed_at), the routing keys and every
// other metadata field must survive byte for byte — the successor rebinds the
// claim projection itself, on its next `gc hook --claim`, and a release that
// rewrote anything else would race that adoption. (Revision and UpdatedAt are
// store bookkeeping that any Update bumps, including the hand-over's own.)
func TestSeatHandover_PreservesEveryFieldExceptAssignee(t *testing.T) {
	cfg := seatHandoverConfig()
	store := beads.NewMemStore()
	predecessor := seatRow(t, store, "predecessor", "worker-mc-old", "corner")
	successor := seatRow(t, store, "successor", "worker-mc-new", "corner")

	priority := 2
	work, err := store.Create(beads.Bead{
		Title:    "field-preserving work",
		Type:     "task",
		Priority: &priority,
		Labels:   []string{"convoy:abc"},
		Assignee: predecessor.ID,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:    "worker",
			beadmeta.SessionIDMetadataKey:   predecessor.ID,
			beadmeta.SessionNameMetadataKey: "worker-mc-old",
			beadmeta.ClaimedAtMetadataKey:   "2026-10-01T00:00:00Z",
			"gc.workflow.id":                "wf-1",
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
	before, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		cfg,
		"",
		sessionInfosFromBeads([]beads.Bead{successor}),
		[]beads.Bead{before},
		nil, nil, nil, nil, nil, nil,
	)
	if len(released) != 0 {
		t.Fatalf("released %v, want none", released)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work after sweep: %v", err)
	}
	if got.Assignee != successor.ID {
		t.Fatalf("work assignee after sweep = %q, want the successor's bead ID %q (the one field the hand-over writes)", got.Assignee, successor.ID)
	}
	if got.Status != before.Status {
		t.Fatalf("work status = %q, want %q", got.Status, before.Status)
	}
	if got.Title != before.Title {
		t.Fatalf("work title = %q, want %q", got.Title, before.Title)
	}
	if got.Type != before.Type {
		t.Fatalf("work type = %q, want %q", got.Type, before.Type)
	}
	if !sameIntPtr(got.Priority, before.Priority) {
		t.Fatalf("work priority = %v, want %v", got.Priority, before.Priority)
	}
	if len(got.Labels) != len(before.Labels) {
		t.Fatalf("work labels = %v, want %v", got.Labels, before.Labels)
	}
	for i, label := range before.Labels {
		if got.Labels[i] != label {
			t.Fatalf("work labels = %v, want %v", got.Labels, before.Labels)
		}
	}
	if got.CreatedAt != before.CreatedAt {
		t.Fatalf("work created_at changed: %v -> %v", before.CreatedAt, got.CreatedAt)
	}
	if len(got.Metadata) != len(before.Metadata) {
		t.Fatalf("work metadata = %v, want %v", got.Metadata, before.Metadata)
	}
	for key, want := range before.Metadata {
		if got.Metadata[key] != want {
			t.Fatalf("work metadata %q = %q, want %q", key, got.Metadata[key], want)
		}
	}
}

// sameIntPtr compares two optional bead priorities for equality.
func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
