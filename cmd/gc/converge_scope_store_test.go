package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The routing half of the convergence-residence split, shared with control
// dispatch: the controller writes a scope's convergence roots where the S4
// placement rule says (buildConvergenceScopes), and `gc converge` has to read
// the same place. When it does not, the CLI reports every live root as missing
// — #5125's bug class, and the reason a `gc converge status` can look clean
// while the engine is stranding.
func TestControlGraphStoreFollowsThePlacementRule(t *testing.T) {
	class := beads.NewMemStore()

	t.Run("split city scope takes the graph binding", func(t *testing.T) {
		cityPath := t.TempDir()
		work := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, splitEnvRoutes(class))

		if got := controlGraphStore(cityPath, cityPath, nil, work); !sameStorePtr(got, class) {
			t.Error("the city scope did not resolve to the graph binding")
		}
	})

	t.Run("split rig scope takes the graph binding too", func(t *testing.T) {
		cityPath := t.TempDir()
		rig := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, splitEnvRoutes(class))

		if got := controlGraphStore(cityPath, filepath.Join(cityPath, "rigs", "ra"), nil, rig); !sameStorePtr(got, class) {
			t.Error("the rig scope stayed on its own store; under the S4 placement rule a split city creates every graph root in the binding, at every scope, and rig stores hold only work")
		}
	})

	t.Run("single-store scopes are identity", func(t *testing.T) {
		cityPath := t.TempDir()
		work := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, nil)

		if got := controlGraphStore(cityPath, cityPath, nil, work); !sameStorePtr(got, work) {
			t.Error("a city that relocates nothing must get its own store back unchanged")
		}
		rig := beads.NewMemStore()
		if got := controlGraphStore(cityPath, filepath.Join(cityPath, "rigs", "ra"), nil, rig); !sameStorePtr(got, rig) {
			t.Error("a rig of a city that relocates nothing must get its own store back unchanged")
		}
	})
}

// TestConvergeRigScopeSelectsByNameNotPath pins that converge's rig leg is
// selected by rig NAME. Nothing forbids registering a rig at the city root,
// where its path IS the city path; the controller's scope map keys on the rig
// name, so the CLI must too.
//
// On a single-store city the named rig reads its own store untouched. On a
// split city it reads the binding filtered to rig:<name> — never the city
// scope's rows, which a path-based test would hand it.
func TestConvergeRigScopeSelectsByNameNotPath(t *testing.T) {
	t.Run("single-store: the named rig keeps its own store", func(t *testing.T) {
		cityPath := t.TempDir()
		rigAtCityRoot := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, nil)
		if got := convergeScopeStore(cityPath, "rig-at-city-root", cityPath, rigAtCityRoot); !sameStorePtr(got.(beads.Store), rigAtCityRoot) {
			t.Error("a rig of a single-store city was routed off its own store")
		}
	})

	t.Run("split: the named rig reads only its own scope from the binding", func(t *testing.T) {
		cityPath := t.TempDir()
		class := newSplitEnvClassLeaf(t)
		seedCLIStorageRoutes(t, cityPath, splitEnvRoutes(class))
		cityLoop, err := class.Create(beads.Bead{Title: "city loop", Type: "convergence"})
		if err != nil {
			t.Fatal(err)
		}
		rigLoop, err := class.Create(beads.Bead{Title: "rig loop", Type: "convergence", Metadata: map[string]string{
			"gc.root_store_ref": "rig:rig-at-city-root",
		}})
		if err != nil {
			t.Fatal(err)
		}

		rigReader := convergeScopeStore(cityPath, "rig-at-city-root", cityPath, beads.NewMemStore())
		got, err := rigReader.List(beads.ListQuery{Type: "convergence"})
		if err != nil {
			t.Fatal(err)
		}
		if ids := controlLegIDs(got); len(ids) != 1 || ids[0] != rigLoop.ID {
			t.Errorf("rig scope listed %v, want only its own loop %s (the city loop %s belongs to the city scope)", ids, rigLoop.ID, cityLoop.ID)
		}
		cityReader := convergeScopeStore(cityPath, "", cityPath, beads.NewMemStore())
		got, err = cityReader.List(beads.ListQuery{Type: "convergence"})
		if err != nil {
			t.Fatal(err)
		}
		if ids := controlLegIDs(got); len(ids) != 1 || ids[0] != cityLoop.ID {
			t.Errorf("city scope listed %v, want only the city loop %s", ids, cityLoop.ID)
		}
	})
}
