package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/rollout/gate"
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
var openNudgeBeadStoreErr = func(cityPath string) (beads.NudgesStore, error) {
	store, err := openStoreAtForCity(cityPath, cityPath)
	return finishNudgeBeadStoreOpen(cityPath, store, err)
}

// openNudgeBeadStoreWithModeErr opens the nudges store with a caller-supplied
// conditional-writes mode. Long-lived runtimes use their boot-latched mode so
// a config edit cannot change write discipline in only this per-pass handle.
var openNudgeBeadStoreWithModeErr = func(cityPath string, mode gate.Mode) (beads.NudgesStore, error) {
	result, err := openStoreResultAtForCityWithMode(cityPath, cityPath, mode, true, false)
	return finishNudgeBeadStoreOpen(cityPath, result.Store, err)
}

func finishNudgeBeadStoreOpen(cityPath string, store beads.Store, err error) (beads.NudgesStore, error) {
	if err != nil {
		return beads.NudgesStore{}, fmt.Errorf("opening the city store at %q: %w", cityPath, err)
	}
	resolved := resolveNudgesStore(cliStorageRoutes(cityPath), store, nil, cityPath, nil)
	if err := closeDiscardedNudgeWorkStore(cityPath, store, resolved); err != nil {
		return beads.NudgesStore{}, err
	}
	return beads.NudgesStore{Store: resolved}, nil
}

// closeDiscardedNudgeWorkStore closes opened when resolveNudgesStore
// discarded it in favor of the relocated class's shared, process-scoped
// store (resolved != opened). Nothing else owns that discarded handle, so
// this seam must close it rather than leak one per call -- openNudgeBeadStoreErr
// is called once per dispatch pass, far more often than the class-relocation
// case is rare (gc-3javuo: runPass's ownership guard correctly declines to
// close the shared store it got back, but nothing was closing the handle
// resolveNudgesStore dropped to produce it).
func closeDiscardedNudgeWorkStore(cityPath string, opened, resolved beads.Store) error {
	if resolved == opened {
		return nil
	}
	if closeErr := closeBeadStoreHandle(opened); closeErr != nil {
		return fmt.Errorf("closing discarded work-store handle for %q: %w", cityPath, closeErr)
	}
	return nil
}

// nudgeBeadStoreOwned reports whether the store openNudgeBeadStore(cityPath)
// returns is a handle this call opened, safe for the caller to close, versus
// the shared nudges-class binding owned by cliStorageRoutes(cityPath).
//
// When the nudges class is relocated to a split binding, resolveNudgesStore
// above discards the freshly opened work-store handle and returns the
// process-scoped store cliStorageRoutes memoizes instead — the same instance
// every call, closed exactly once at process exit via closeCLIStorageRoutes.
// A per-pass caller that closed it anyway would tear down that shared binding
// out from under every other consumer of the same relocated class group.
func nudgeBeadStoreOwned(cityPath string) bool {
	// Ownership probe, not a fresh store enumeration — mirrors the identical,
	// already-accepted relocation check in cliSessionsRelocated (cli_class_stores.go).
	_, relocated := cliStorageRoutes(cityPath).storeFor(coordclassFor(config.BeadClassNudges)) // residency:allow mirrors cliSessionsRelocated's identical check
	return !relocated
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
