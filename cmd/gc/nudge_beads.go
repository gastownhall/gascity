package main

import (
	"fmt"
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
)

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
	store, _ := openNudgeBeadStoreErr(cityPath)
	return store
}

// openNudgeBeadStoreErr is openNudgeBeadStore with the open failure kept instead
// of swallowed into a nil-safe zero store.
//
// The zero store is not harmless: every nudge helper below is nil-tolerant, so a
// city whose store will not open reported "opening city store for X" with no
// cause at all — the operator could not tell a missing city from a locked
// database from a storage refusal. Call sites that surface a failure to a human
// use this form and print the reason; the seam above stays for the poll/drain
// helpers whose contract is already "a nil store means do nothing".
func openNudgeBeadStoreErr(cityPath string) (beads.NudgesStore, error) {
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return beads.NudgesStore{}, fmt.Errorf("opening the city store at %q: %w", cityPath, err)
	}
	return beads.NudgesStore{Store: resolveNudgesStore(cliStorageRoutes(cityPath), store, nil, cityPath, nil)}, nil
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

func markQueuedNudgeTerminal(store beads.NudgesStore, item queuedNudge, state, reason, commitBoundary string, now time.Time) error {
	return nudgeFrontDoor(store).Terminalize(item, state, reason, commitBoundary, now)
}

func formatOptionalTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}
