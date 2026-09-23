package main

import (
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// TestNudgeBeadStoreOwnedReportsRelocation pins the ownership signal runPass
// relies on to decide whether it may close the store openNudgeBeadStore
// returned.
//
// When the nudges class is not relocated, openNudgeBeadStoreErr hands back
// the fresh handle it opened itself, and the caller owns it. When the
// nudges class IS relocated (a split-topology city), resolveNudgesStore
// discards that fresh handle and returns the shared, process-scoped store
// cliStorageRoutes memoizes for the whole binding — a handle only
// closeCLIStorageRoutes may close, at process exit, once. A caller that
// closed it per-pass would tear the shared binding down for every other
// class routed onto it (messaging, orders, sessions, graph) after the first
// nudge dispatch pass.
func TestNudgeBeadStoreOwnedReportsRelocation(t *testing.T) {
	unrouted := t.TempDir()
	if !nudgeBeadStoreOwned(unrouted) {
		t.Fatalf("nudgeBeadStoreOwned(%q) = false with no routes memoized, want true (identity to the work store)", unrouted)
	}

	cityPath := t.TempDir()
	entry := cliStorageRoutesEntryFor(cityPath)
	shared := beads.NewMemStore()
	// once.Do must fire on ITS first call for this entry: cliStorageRoutes
	// (called by nudgeBeadStoreOwned) would otherwise win the race and
	// memoize resolveCLIStorageRoutes's real (unrouted) answer first, after
	// which this Do is a silent no-op and the fixture never takes hold.
	entry.once.Do(func() {
		entry.routes = &storageRoutes{
			binding: "infra",
			stores: map[coordclass.Class]beads.Store{
				coordclass.ClassNudges: shared,
			},
		}
	})
	t.Cleanup(func() {
		cliStorageRoutesMu.Lock()
		delete(cliStorageRoutesByCity, cityPath)
		cliStorageRoutesMu.Unlock()
	})

	if nudgeBeadStoreOwned(cityPath) {
		t.Fatalf("nudgeBeadStoreOwned(%q) = true with the nudges class relocated onto a shared binding, want false", cityPath)
	}

	got, relocated := entry.routes.storeFor(coordclass.ClassNudges)
	if !relocated || got != beads.Store(shared) {
		t.Fatalf("routes.storeFor(ClassNudges) = (%p, %v), want (%p, true) -- test fixture is wired wrong", got, relocated, shared)
	}
}

// TestNudgeEventDispatcherRunPassClosesBeadStore guards specifically against
// closing the SHARED relocated-class store across passes: with the nudges
// class relocated onto a shared binding, runPass must run twice and the
// second pass must still be able to read the store — a bare Close on the
// first pass would tear the shared binding down for the second.
func TestNudgeEventDispatcherRunPassClosesBeadStore(t *testing.T) {
	cityPath := t.TempDir()
	entry := cliStorageRoutesEntryFor(cityPath)
	var closes atomic.Int64
	shared := &runPassCloseCountingStore{Store: beads.NewMemStore(), closes: &closes}
	entry.once.Do(func() {
		entry.routes = &storageRoutes{
			binding: "infra",
			stores: map[coordclass.Class]beads.Store{
				coordclass.ClassNudges: shared,
			},
		}
	})
	t.Cleanup(func() {
		cliStorageRoutesMu.Lock()
		delete(cliStorageRoutesByCity, cityPath)
		cliStorageRoutesMu.Unlock()
	})

	if nudgeBeadStoreOwned(cityPath) {
		t.Fatalf("fixture wiring: nudgeBeadStoreOwned(%q) = true with the nudges class relocated, want false", cityPath)
	}

	// Simulate two dispatch passes against the relocated binding the way
	// runPass does: open, conditionally close on ownership, repeat.
	for i, pass := range []string{"first", "second"} {
		store, err := openNudgeBeadStoreErr(cityPath)
		if err != nil {
			t.Fatalf("pass %d (%s): openNudgeBeadStoreErr: %v", i, pass, err)
		}
		if nudgeBeadStoreOwned(cityPath) {
			if err := closeBeadStoreHandle(store.Store); err != nil {
				t.Fatalf("pass %d (%s): closeBeadStoreHandle: %v", i, pass, err)
			}
		}
	}

	// The shared relocated-class binding must survive both passes: only
	// closeCLIStorageRoutes may close it, at process exit. A pass that
	// (wrongly) closed it per-dispatch would tear the binding down for
	// every other class routed onto it.
	if got := closes.Load(); got != 0 {
		t.Fatalf("shared relocated store was closed %d time(s) across dispatch passes, want 0", got)
	}
}

// TestCloseDiscardedNudgeWorkStoreClosesOnlyWhenDiscarded pins
// closeDiscardedNudgeWorkStore's two branches: resolveNudgesStore handing
// back the SAME handle it was given (identity, not relocated) leaves it
// open for the caller to use and close itself, while a DIFFERENT resolved
// store (relocated: the just-opened work-store handle was discarded in
// favor of the shared binding) must have that discarded handle closed here,
// at the open seam -- gc-3javuo's defect class, where nothing closed it.
func TestCloseDiscardedNudgeWorkStoreClosesOnlyWhenDiscarded(t *testing.T) {
	var identityCloses, discardedCloses atomic.Int64
	opened := &runPassCloseCountingStore{Store: beads.NewMemStore(), closes: &identityCloses}

	if err := closeDiscardedNudgeWorkStore("city-not-relocated", opened, opened); err != nil {
		t.Fatalf("closeDiscardedNudgeWorkStore (identity): %v", err)
	}
	if got := identityCloses.Load(); got != 0 {
		t.Fatalf("closeDiscardedNudgeWorkStore closed the opened handle %d time(s) when resolved == opened, want 0 (caller's own handle, not discarded)", got)
	}

	opened = &runPassCloseCountingStore{Store: beads.NewMemStore(), closes: &discardedCloses}
	shared := beads.NewMemStore()
	if err := closeDiscardedNudgeWorkStore("city-relocated", opened, shared); err != nil {
		t.Fatalf("closeDiscardedNudgeWorkStore (discarded): %v", err)
	}
	if got := discardedCloses.Load(); got != 1 {
		t.Fatalf("closeDiscardedNudgeWorkStore closed the discarded work-store handle %d time(s) when resolved != opened, want exactly 1 (leaked handle, gc-3javuo)", got)
	}
}
