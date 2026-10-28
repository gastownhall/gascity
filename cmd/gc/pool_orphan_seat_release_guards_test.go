package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestReleaseOrphanedPoolAssignments_ReleasesUnaliasedPredecessorClaimDespiteProcessingSibling
// pins the ga-r22k2y boundary of the seat hand-over: a predecessor with NO
// seat identity (no configured_named_identity, no alias — an ordinary pool
// row) has no successor to find, and an awake sibling that merely reports the
// bead as currently processing must NOT shield the claim. The triage repro for
// #6362 had exactly this shape and the sweep must keep releasing it: a live
// sibling must never mask a dead unaliased assignee.
func TestReleaseOrphanedPoolAssignments_ReleasesUnaliasedPredecessorClaimDespiteProcessingSibling(t *testing.T) {
	cfg := seatHandoverConfig()
	store := beads.NewMemStore()
	predecessor, err := store.Create(beads.Bead{
		Title:  "predecessor worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-mc-old",
			"template":     "worker",
			"state":        "awake",
		},
	})
	if err != nil {
		t.Fatalf("create predecessor session: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "long bake",
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
	// An awake sibling of the same template reports the bead as currently
	// processing — but carries no seat identity, so it answers to nothing the
	// closed row could be continued by.
	sibling, err := store.Create(beads.Bead{
		Title:  "replacement worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                 "worker-mc-new",
			"template":                     "worker",
			"state":                        "awake",
			"currently_processing_bead_id": work.ID,
		},
	})
	if err != nil {
		t.Fatalf("create replacement session: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		cfg,
		"",
		sessionInfosFromBeads([]beads.Bead{sibling}),
		[]beads.Bead{work},
		nil, nil, nil, nil, nil, nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released %v, want exactly [%s]: the closed row carries no seat identity, so its claim is dead and an unaliased processing sibling must not mask it",
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
