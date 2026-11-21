package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestAppendOpenAssignedMoleculeWorkUnique_SkipsFutureDeferred is the #7080
// regression: a future-deferred assigned molecule/wisp root must not be
// counted as ready wake demand, the same way the hook
// (isFutureDeferredHookCandidate) already refuses to claim it. Before this
// fix the controller woke an on-demand named session for a root its own
// hook would immediately find unclaimable and drain on.
func TestAppendOpenAssignedMoleculeWorkUnique_SkipsFutureDeferred(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	root := func(id string, deferUntil *time.Time) beads.Bead {
		return beads.Bead{
			ID:         id,
			Status:     "open",
			Assignee:   "worker",
			Type:       "molecule",
			Ephemeral:  true,
			DeferUntil: deferUntil,
		}
	}

	var (
		dst      []beads.Bead
		stores   []beads.Store
		refs     []string
		readyIDs = map[string]bool{}
		seen     = map[string]struct{}{}
	)
	appendOpenAssignedMoleculeWorkUnique(&dst, &stores, &refs, readyIDs, []beads.Bead{
		root("m-future", &future),
		root("m-elapsed", &past),
		root("m-undeferred", nil),
	}, seen, nil, "")

	if readyIDs["m-future"] {
		t.Errorf("future-deferred root counted as ready wake demand")
	}
	for _, id := range []string{"m-elapsed", "m-undeferred"} {
		if !readyIDs[id] {
			t.Errorf("%s not counted as ready wake demand", id)
		}
	}
}

// TestAppendOpenAssignedMoleculeWorkUnique_SkipsIndefinitelyDeferred covers
// the other half of beads.IsDeferred that #7080's own patch did not add a
// case for: a root marked IndefinitelyDeferred must also be skipped, not
// just one with a future DeferUntil.
func TestAppendOpenAssignedMoleculeWorkUnique_SkipsIndefinitelyDeferred(t *testing.T) {
	root := beads.Bead{
		ID:                   "m-indefinite",
		Status:               "open",
		Assignee:             "worker",
		Type:                 "molecule",
		Ephemeral:            true,
		IndefinitelyDeferred: true,
	}

	var (
		dst      []beads.Bead
		stores   []beads.Store
		refs     []string
		readyIDs = map[string]bool{}
		seen     = map[string]struct{}{}
	)
	appendOpenAssignedMoleculeWorkUnique(&dst, &stores, &refs, readyIDs, []beads.Bead{root}, seen, nil, "")

	if readyIDs["m-indefinite"] {
		t.Errorf("indefinitely-deferred root counted as ready wake demand")
	}
}
