package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// convergeCityScopeStore is the CLI half of the convergence-residence split.
// The controller's city scope writes convergence roots into the graph binding
// (buildConvergenceScopes); `gc converge` has to read and write the same one.
// When it does not, the CLI reads the work ledger for relocated graph ids and
// reports every live convergence root as missing — #5125's bug class, and the
// reason a `gc converge status` can look clean while the engine is stranding.
//
// The rig arm is the other half of the same rule: class routing is city-keyed,
// so there is ONE graph binding per city. Routing rig scopes through it would
// merge every rig's convergence loops into a ledger keyed by nothing.
func TestConvergeCityScopeStoreRoutesCityToGraphAndRigToWork(t *testing.T) {
	class := beads.NewMemStore()

	t.Run("split city scope takes the graph binding", func(t *testing.T) {
		cityPath := t.TempDir()
		work := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, splitEnvRoutes(class))

		got := convergeCityScopeStore(work, cityPath, "")
		if !sameStorePtr(got, class) {
			t.Error("the city converge scope did not resolve to the graph binding; " +
				"the CLI would read the work ledger for relocated graph ids and report every live convergence root as missing")
		}
	})

	t.Run("split rig scope keeps its own work ledger", func(t *testing.T) {
		cityPath := t.TempDir()
		rig := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, splitEnvRoutes(class))

		got := convergeCityScopeStore(rig, cityPath, "ra")
		if !sameStorePtr(got, rig) {
			t.Error("the rig converge scope was routed off its own work ledger; " +
				"there is one graph binding per city, so every rig's convergence loops would collapse into it")
		}
	})

	t.Run("single-store city scope is identity", func(t *testing.T) {
		cityPath := t.TempDir()
		work := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, nil)

		got := convergeCityScopeStore(work, cityPath, "")
		if !sameStorePtr(got, work) {
			t.Error("a city that relocates nothing must get its own store back unchanged; " +
				"the class resolver's identity branch is what keeps this fix invisible to every single-store city")
		}
	})
}
