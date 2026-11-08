package main

import (
	"context"
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

// openNudgeBeadStore is the verify-only nudges-store seam of the observation
// doors (nudge status, poll and drain's pre-claim reads, prime --hook): it
// opens the city's store under the observation policy, so a stopped, stale or
// unreachable store is never started or recovered here. A failed open returns
// the zero store; a caller that must tell the reason asks
// openObservationNudgeBeadStoreErr. The paths that deliver nudges and so may
// recover the store use openNudgeBeadStoreRecovering instead.
//
// It is a test seam (mirrors the injectable vars in cmd_nudge.go) so tests can
// substitute a fake store and assert that per-tick poll helpers close every
// store they open. Tests that replace this package variable must stay serial;
// do not use t.Parallel in those tests. It returns the strongly-typed
// beads.NudgesStore so the nudges class is statically visible to every leaf
// nudge-bead helper.
//
// It discards the opened handle, so a caller of this form can never close the
// work store the call opened. Once the nudges class relocates, that work store
// is not the store returned, so it stays open (leaks) for the life of the
// process; only on an unrelocated city is the returned store the opened handle.
// A frame that closes what it opened must use openOwnedNudgeBeadStore and close
// the returned handle, never the class store: closing the class store on a
// relocated city is the bug openOwnedNudgeBeadStore exists to prevent.
var openNudgeBeadStore = func(cityPath string) beads.NudgesStore {
	store, _, _ := openObservationNudgeBeadStoreOwned(cityPath)
	return store
}

// openNudgeBeadStoreRecovering is openNudgeBeadStore for a frame that is not an
// observation door: nudge delivery after a claim, and the session-runtime
// resolution. It opens through openOwnedNudgeBeadStore, so the store is
// recovered when it is down, and a test that substitutes that var moves it too.
var openNudgeBeadStoreRecovering = func(cityPath string) beads.NudgesStore {
	store, _ := openOwnedNudgeBeadStore(cityPath)
	return store
}

// openObservationNudgeBeadStoreOwned is openNudgeBeadStoreOwned for a
// verify-only caller: the work store opens under the observation policy and
// the nudges class resolves through the observation routes, so nothing here
// starts or recovers the city's Dolt. An unavailable store returns the typed
// *storeUnavailableError unwrapped. The opened handle carries no deadline of
// its own: a poller keeps it for the life of the process.
func openObservationNudgeBeadStoreOwned(cityPath string) (beads.NudgesStore, beads.Store, error) {
	store, err := openObservationStoreAt(context.Background(), cityPath, cityPath, nil)
	if err != nil {
		if _, ok := asStoreUnavailable(err); ok {
			return beads.NudgesStore{}, nil, err
		}
		return beads.NudgesStore{}, nil, fmt.Errorf("opening the city store at %q: %w", cityPath, err)
	}
	return observationStores(cityPath, store).Nudges(), store, nil
}

// openObservationNudgeBeadStoreErr is openNudgeBeadStoreErr for a verify-only
// caller: the open failure is kept, and an unavailable store is the typed
// *storeUnavailableError.
func openObservationNudgeBeadStoreErr(cityPath string) (beads.NudgesStore, error) {
	store, _, err := openObservationNudgeBeadStoreOwned(cityPath)
	return store, err
}

// observationNudgeStoreFailure answers why the verify-only open of cityPath's
// nudges store failed: the typed unavailable error, another open error, or nil
// when the store opens after all (the handle it opened is closed).
func observationNudgeStoreFailure(cityPath string) error {
	_, opened, err := openObservationNudgeBeadStoreOwned(cityPath)
	if err != nil {
		return err
	}
	return closeBeadStoreHandle(opened)
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
// It is a seam for the same reason openNudgeBeadStore is, and is the one the
// counting fake replaces: openNudgeBeadStoreRecovering delegates here, so a test
// that substitutes this var moves both forms at once and the two can never
// disagree.
var openOwnedNudgeBeadStore = func(cityPath string) (beads.NudgesStore, beads.Store) {
	store, opened, _ := openNudgeBeadStoreOwned(cityPath)
	return store, opened
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
	store, _, err := openNudgeBeadStoreOwned(cityPath)
	return store, err
}

// openNudgeWorkStore opens the work store every nudge opener starts from. It is
// a seam so tests can count, per handle, that each owning frame closes the work
// store it opened exactly once, including on a relocated city where that handle
// is not the class store the frame uses. Tests that replace it must stay serial.
var openNudgeWorkStore = openStoreAtForCity

// openNudgeBeadStoreOwned is the one place the nudges-class store is opened, so
// the class store and the handle the call opened are decided together. Every
// form above is a projection of it.
func openNudgeBeadStoreOwned(cityPath string) (beads.NudgesStore, beads.Store, error) {
	store, err := openNudgeWorkStore(cityPath, cityPath)
	if err != nil {
		return beads.NudgesStore{}, nil, fmt.Errorf("opening the city store at %q: %w", cityPath, err)
	}
	return cliStores(cityPath, store).Nudges(), store, nil
}

// nudgeFrontDoor wraps a strongly-typed nudges store as the nudge object's
// front door (internal/nudgequeue.Store). The bead is a SHADOW of the flock'd
// state.json queue; the front door confines the Item<->Bead codec, leaving these
// cmd/gc helpers as thin adapters. None is called inside the queue's flock.
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
