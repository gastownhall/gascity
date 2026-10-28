package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestReleaseOrphanedPoolAssignments_KeepsWorkCurrentlyProcessedByReplacement
// reproduces gastownhall/gascity#6362: a predecessor session drains while its
// replacement adopts the assigned work, but the claim projection (assignee,
// gc.session_id) still names the drained predecessor. The replacement is awake,
// answers to the same seat identity, and reports the bead as currently
// processing, so the dead-assignee sweep must not reopen it: it moves the
// claim to the replacement instead.
func TestReleaseOrphanedPoolAssignments_KeepsWorkCurrentlyProcessedByReplacement(t *testing.T) {
	cfg := seatHandoverConfig()
	store := beads.NewMemStore()
	predecessor := seatRow(t, store, "predecessor worker", "worker-mc-old", "baker")

	work, err := store.Create(beads.Bead{
		Title:    "long bake",
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
	// Predecessor drains and its session bead closes.
	closed := "closed"
	if err := store.Update(predecessor.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close predecessor: %v", err)
	}
	// Replacement adopts the work and reports it as currently processing.
	replacement, err := store.Create(beads.Bead{
		Title:  "replacement worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                 "worker-mc-new",
			"template":                     "worker",
			"state":                        "awake",
			namedSessionMetadataKey:        "true",
			namedSessionIdentityMetadata:   "baker",
			"currently_processing_bead_id": work.ID,
		},
	})
	if err != nil {
		t.Fatalf("create replacement session: %v", err)
	}
	// Refresh the snapshot after the writes: the sweep fences on it.
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		cfg,
		"",
		sessionInfosFromBeads([]beads.Bead{replacement}),
		[]beads.Bead{work},
		nil, nil, nil, nil, nil, nil,
	)
	if len(released) != 0 {
		t.Fatalf("released %v: work %s is currently processed by awake replacement %s but was reopened as dead-assignee %s",
			released, work.ID, replacement.ID, predecessor.ID)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work after sweep: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("work status = %q, want in_progress (live replacement still processing it)", got.Status)
	}
	if got.Assignee != replacement.ID {
		t.Fatalf("work assignee = %q, want the replacement's bead ID %q (the stale claim names the drained predecessor)",
			got.Assignee, replacement.ID)
	}
}
