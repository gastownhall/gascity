package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// TestNudgeBeadStoreOwnedReportsRelocation pins the ownership signal
// runPass relies on to decide whether it may close the store
// openNudgeBeadStore returned.
//
// When the nudges class is not relocated, openNudgeBeadStoreErr hands back
// the fresh handle it opened itself, and the caller owns it. When the
// nudges class IS relocated (a split-topology city), resolveNudgesStore
// discards that fresh handle and returns the shared, process-scoped store
// cliStorageRoutes memoizes for the whole binding — a handle only
// closeCLIStorageRoutes may close, at process exit, once. A caller that
// closed it per-pass would tear the shared binding down for every other
// class routed onto it (messaging, orders, sessions, graph) after the
// first nudge dispatch pass.
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
