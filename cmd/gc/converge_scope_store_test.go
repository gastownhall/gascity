package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// scopeGraphStore is the routing half of the convergence-residence split, shared
// with control dispatch. The controller's city scope writes convergence roots
// into the graph binding (buildConvergenceScopes); `gc converge` has to read and
// write the same one. When it does not, the CLI reads the work ledger for
// relocated graph ids and reports every live convergence root as missing —
// #5125's bug class, and the reason a `gc converge status` can look clean while
// the engine is stranding.
//
// The rig arm is the other half of the same rule: class routing is city-keyed,
// so there is ONE graph binding per city. Routing rig scopes through it would
// merge every rig's convergence loops into a ledger keyed by nothing.
func TestScopeGraphStoreRoutesCityToGraphAndRigToWork(t *testing.T) {
	class := beads.NewMemStore()

	t.Run("split city scope takes the graph binding", func(t *testing.T) {
		cityPath := t.TempDir()
		work := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, splitEnvRoutes(class))

		got := scopeGraphStore(cityPath, cityPath, nil, work)
		if !sameStorePtr(got, class) {
			t.Error("the city scope did not resolve to the graph binding; " +
				"the CLI would read the work ledger for relocated graph ids and report every live convergence root as missing")
		}
	})

	t.Run("split rig scope keeps its own work ledger", func(t *testing.T) {
		cityPath := t.TempDir()
		rig := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, splitEnvRoutes(class))

		got := scopeGraphStore(cityPath, filepath.Join(cityPath, "rigs", "ra"), nil, rig)
		if !sameStorePtr(got, rig) {
			t.Error("the rig scope was routed off its own work ledger; " +
				"there is one graph binding per city, so every rig's convergence loops would collapse into it")
		}
	})

	t.Run("single-store city scope is identity", func(t *testing.T) {
		cityPath := t.TempDir()
		work := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, nil)

		got := scopeGraphStore(cityPath, cityPath, nil, work)
		if !sameStorePtr(got, work) {
			t.Error("a city that relocates nothing must get its own store back unchanged; " +
				"the class resolver's identity branch is what keeps this fix invisible to every single-store city")
		}
	})
}

// TestConvergeRigScopeIgnoresPathBasedCityTest pins the one place converge's
// scope test is deliberately NOT scopeGraphStore's.
//
// scopeGraphStore decides city-ness by PATH, which is right for control
// dispatch. Convergence has a second constraint: the controller's scope map
// keys on rig NAME (buildConvergenceScopes), and nothing forbids registering a
// rig at the city root. For such a rig the path test says "city" — so routing
// the CLI's rig leg by path alone would read the graph binding while the
// controller writes that rig's roots to its own store. That is the exact
// read/write asymmetry this change removes, reintroduced through the back door.
//
// This is the assertion that fails if someone later "simplifies" the rig
// short-circuit in openConvergeStore away as redundant.
func TestConvergeRigScopeIgnoresPathBasedCityTest(t *testing.T) {
	cityPath := t.TempDir()
	class := beads.NewMemStore()
	rigAtCityRoot := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, splitEnvRoutes(class))

	// The premise: by path alone this scope reads as the city, so the shared
	// helper routes it. Without this control the test below could pass because
	// the routes were never seeded rather than because the guard held.
	if routed := scopeGraphStore(cityPath, cityPath, nil, rigAtCityRoot); !sameStorePtr(routed, class) {
		t.Fatal("premise failed: scopeGraphStore did not route a city-path scope to the binding, " +
			"so this test cannot distinguish the rig guard from an unseeded route")
	}

	// openConvergeStore's rig leg returns the scope store untouched for any
	// non-empty rig name, whatever its path resolves to.
	if got := convergeScopeStore(cityPath, "rig-at-city-root", cityPath, rigAtCityRoot); !sameStorePtr(got, rigAtCityRoot) {
		t.Error("a rig registered at the city root was routed to the city graph binding; " +
			"the controller keys convergence scopes by rig name and writes that rig's roots to its own store, " +
			"so the CLI would read a database the controller never writes for it")
	}
}
