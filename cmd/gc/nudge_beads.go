package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

// openNudgeBeadStoreWithConfig is a test seam (mirrors the injectable vars in
// cmd_nudge.go) so tests can substitute a fake store and assert that
// per-tick poll helpers close every store they open. Tests that replace this
// package variable must stay serial; do not use t.Parallel in those tests.
// It routes the opened work store through resolveNudgesStore and returns the
// strongly-typed beads.NudgesStore so the nudges class is statically visible to
// every leaf nudge-bead helper; the wrapper carries the same underlying store
// value (identity to the work store until the nudges class relocates).
//
// It takes the city config so a caller that already loaded it (the drain path
// resolves its target from the same config) does not pay the full city+pack
// TOML load again inside the store open; nil keeps the loading behavior.
//
// It discards the opened handle, so a caller of this form can never close the
// work store the call opened. Once the nudges class relocates, that work store
// is not the store returned, so it stays open (leaks) for the life of the
// process; only on an unrelocated city is the returned store the opened handle.
// A frame that closes what it opened must use openOwnedNudgeBeadStore and close
// the returned handle, never the class store: closing the class store on a
// relocated city is the bug openOwnedNudgeBeadStore exists to prevent.
var openNudgeBeadStoreWithConfig = func(cityPath string, cfg *config.City) beads.NudgesStore {
	store, _ := openOwnedNudgeBeadStoreWithConfig(cityPath, cfg)
	return store
}

// openNudgeBeadStore is openNudgeBeadStoreWithConfig for callers with no
// config in hand.
func openNudgeBeadStore(cityPath string) beads.NudgesStore {
	return openNudgeBeadStoreWithConfig(cityPath, nil)
}

// openOwnedNudgeBeadStore is the owning-frame form of openNudgeBeadStore: it
// returns the nudges-class store to use AND the handle this call opened, which
// is the only handle the caller may close.
//
// The two differ exactly when the NUDGES class is relocated onto a storage
// binding. resolveNudgesStore then discards the work store this call opened and
// returns the storage routes' process-shared engine, which the routes own and
// closeCLIStorageRoutes releases when the invocation ends. A frame that closes
// "the store it holds" therefore closes the routes' engine without detaching
// their memo, and the memo goes on serving a closed engine: every later class
// read in the process — nudge delivery itself, and the controller's own socket
// handlers — then fails with ErrStoreClosed until the process exits.
//
// Closing by "the handle I opened" is also what keeps the ownership answer in
// one place. The alternative, asking the routes a second time whether this class
// is relocated, is the re-derivation the residency contract reserves to the
// resolver, and is how this bug class reproduces
// (scripts/residency-boundary-patterns.txt, rule b).
//
// It is a seam for the same reason openNudgeBeadStoreWithConfig is, and is the
// one the counting fake replaces: openNudgeBeadStoreWithConfig delegates here,
// so a test that substitutes this var moves both forms at once and the two can
// never disagree.
var openOwnedNudgeBeadStoreWithConfig = func(cityPath string, cfg *config.City) (beads.NudgesStore, beads.Store) {
	store, opened, _ := openNudgeBeadStoreOwned(cityPath, cfg)
	return store, opened
}

// openOwnedNudgeBeadStore is openOwnedNudgeBeadStoreWithConfig for callers with
// no config in hand.
var openOwnedNudgeBeadStore = func(cityPath string) (beads.NudgesStore, beads.Store) {
	return openOwnedNudgeBeadStoreWithConfig(cityPath, nil)
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
	return openNudgeBeadStoreErrWithConfig(cityPath, nil)
}

// openNudgeBeadStoreErrWithConfig is openNudgeBeadStoreErr reusing an
// already-loaded city config (nil loads it).
func openNudgeBeadStoreErrWithConfig(cityPath string, cfg *config.City) (beads.NudgesStore, error) {
	store, _, err := openNudgeBeadStoreOwned(cityPath, cfg)
	return store, err
}

// openNudgeWorkStore opens the work store every nudge opener starts from. It is
// a seam so tests can count, per handle, that each owning frame closes the work
// store it opened exactly once, including on a relocated city where that handle
// is not the class store the frame uses. Tests that replace it must stay serial.
// The cfg is the caller's already-loaded city config (nil loads).
var openNudgeWorkStore = func(storePath, cityPath string, cfg *config.City) (beads.Store, error) {
	return openStoreAtForCityWithConfig(storePath, cityPath, cfg)
}

// openNudgeBeadStoreOwned is the one place the nudges-class store is opened, so
// the class store and the handle the call opened are decided together. Every
// form above is a projection of it.
func openNudgeBeadStoreOwned(cityPath string, cfg *config.City) (beads.NudgesStore, beads.Store, error) {
	store, err := openNudgeWorkStore(cityPath, cityPath, cfg)
	if err != nil {
		return beads.NudgesStore{}, nil, fmt.Errorf("opening the city store at %q: %w", cityPath, err)
	}
	return beads.NudgesStore{Store: resolveNudgesStore(cliStorageRoutes(cityPath), store, nil, cityPath, nil)}, store, nil
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
