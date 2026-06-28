package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const (
	nudgeBeadType = "chore"
	// nudgeBeadLabel is the label applied to queued-nudge beads. coordclass
	// mirrors this string privately (as labelNudge) for store routing; the two
	// must stay in sync.
	nudgeBeadLabel = "gc:nudge"
	// nudgeLookupLimit bounds recovery lookups by the durable nudge ID label.
	// Mirrors nudgequeue.NudgeLookupLimit so cmd/gc adapter tests can assert the
	// bound the front door applies.
	nudgeLookupLimit = nudgequeue.NudgeLookupLimit
)

// nudgeEnqueueRollbackCloseReason is the close_reason metadata value stamped on
// a partially-created nudge bead when the enqueue transaction rolls back. It
// mirrors nudgequeue.EnqueueRollbackCloseReason (the front door owns the write).
const nudgeEnqueueRollbackCloseReason = nudgequeue.EnqueueRollbackCloseReason

type nudgeReference = nudgequeue.Reference

// openNudgeBeadStore is a test seam (mirrors the injectable vars in
// cmd_nudge.go) so tests can substitute a fake store and assert that
// per-tick poll helpers close every store they open. Tests that replace this
// package variable must stay serial; do not use t.Parallel in those tests.
// It routes the opened work store through resolveNudgesStore and returns the
// strongly-typed beads.NudgesStore so the nudges class is statically visible to
// every leaf nudge-bead helper; the wrapper carries the same underlying store
// value (identity to the work store until the nudges class relocates).
var openNudgeBeadStore = func(cityPath string) beads.NudgesStore {
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return beads.NudgesStore{}
	}
	return beads.NudgesStore{Store: resolveNudgesStore(store, nil, cityPath, nil)}
}

// nudgeFrontDoor wraps a strongly-typed nudges store as the nudge object's
// front door (internal/nudgequeue.Store). The bead is a SHADOW of the flock'd
// state.json queue; the front door confines the Item<->Bead codec, leaving these
// cmd/gc helpers as thin adapters that keep the methods callable inside the
// withNudgeQueueState transaction.
func nudgeFrontDoor(store beads.NudgesStore) *nudgequeue.Store {
	return nudgequeue.NewStore(store)
}

func ensureQueuedNudgeBead(store beads.NudgesStore, item queuedNudge) (string, bool, error) {
	return nudgeFrontDoor(store).Save(item)
}

// findQueuedNudgeBead resolves the OPEN nudge shadow bead for nudgeID through
// the front door. Thin adapter retained for cmd/gc callers/tests that inspect
// the raw bead; new logic should prefer nudgeFrontDoor(store).Find.
func findQueuedNudgeBead(store beads.NudgesStore, nudgeID string) (beads.Bead, bool, error) {
	return nudgeFrontDoor(store).FindBead(nudgeID)
}

// findAnyQueuedNudgeBead resolves the nudge shadow bead for nudgeID including
// terminal/closed beads, through the front door.
func findAnyQueuedNudgeBead(store beads.NudgesStore, nudgeID string) (beads.Bead, bool, error) {
	return nudgeFrontDoor(store).FindBeadIncludingTerminal(nudgeID)
}

// nudgeCanonicalCloseReason maps a terminalization state to the canonical
// close_reason. Thin adapter over the front door's codec, retained for the
// cmd/gc test that guards the >=20 char validator floor.
func nudgeCanonicalCloseReason(stateCode string) string {
	return nudgequeue.CanonicalCloseReason(stateCode)
}

func markQueuedNudgeTerminal(store beads.NudgesStore, item queuedNudge, state, reason, commitBoundary string, now time.Time) error {
	return nudgeFrontDoor(store).Terminalize(item, state, reason, commitBoundary, now)
}

func formatOptionalTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}
