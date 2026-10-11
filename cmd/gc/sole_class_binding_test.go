package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// TestSoleClassBindingReportsTheWholeSplitAsOne is the shape this build serves:
// five infrastructure classes, one store, one binding. The grouping is what
// makes the by-id door's "every reserved prefix from one store" argument
// checkable, so the count is the assertion.
func TestSoleClassBindingReportsTheWholeSplitAsOne(t *testing.T) {
	cityPath := t.TempDir()
	infra := splittest.NewWorkStore(t, "hq")
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(infra))

	binding, relocated, err := cliSoleClassBinding(cityPath)
	if err != nil {
		t.Fatalf("resolving the sole binding: %v", err)
	}
	if !relocated {
		t.Fatal("a converged split city reports no relocated binding")
	}
	if binding.Store != infra {
		t.Errorf("the sole binding is %p, want the infra store %p", binding.Store, infra)
	}
	if binding.Name != "infra" {
		t.Errorf("the sole binding is named %q, want the configured binding name", binding.Name)
	}
	if got, want := len(binding.Classes), len(infrastructureClasses()); got != want {
		t.Errorf("the sole binding answers for %d classes, want all %d infrastructure classes — a door that serves every reserved prefix from it must own every one", got, want)
	}
}

// TestSoleClassBindingRefusesAPerClassFanOut is the check that replaced a
// comment.
//
// storageSplitShapeOf refuses a per-class fan-out upstream, so this arrangement
// is unreachable through openStorageRoutes today. That is exactly why it is
// worth pinning here: the by-id door and the claim route both answer for EVERY
// reserved prefix out of the one store they resolve, and the day the upstream
// refusal is relaxed they must stop rather than send a sessions-class read at
// the graph binding and be told, truthfully, that the bead is absent.
func TestSoleClassBindingRefusesAPerClassFanOut(t *testing.T) {
	cityPath := t.TempDir()
	routes := messagingSplitRoutes(splittest.NewWorkStore(t, "hq"))
	routes.stores[coordclass.ClassGraph] = splittest.NewWorkStore(t, "gr")
	seedCLIStorageRoutes(t, cityPath, routes)

	binding, relocated, err := cliSoleClassBinding(cityPath)
	if err == nil {
		t.Fatalf("a two-binding city resolved to %p (relocated=%v); a fan-out has no sole binding to answer every prefix from", binding.Store, relocated)
	}
	if relocated {
		t.Error("a refused fan-out reported relocated=true; a caller that ignored the error would open a door onto one of the two")
	}
	if !strings.Contains(err.Error(), storageSupportedTopologyStatement) {
		t.Errorf("the refusal does not name the supported topology: %v", err)
	}
}

// TestSoleClassBindingCarriesARefusedCityRatherThanHidingIt is the distinction
// the whole residency lane turns on.
//
// A city configured for a binding it has not converged on is served by a store
// whose every operation returns the boot gate's sentence. That city must still
// report as RELOCATED: reading its refusal as "relocates nothing" would send
// every infrastructure read back to the work ledger those beads were migrated
// off, answered from the copy the migration retained, with no error to notice.
func TestSoleClassBindingCarriesARefusedCityRatherThanHidingIt(t *testing.T) {
	cityPath := t.TempDir()
	refusal := errors.New("this city is configured for a binding it has not converged on")
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(refusedClassStore{err: refusal}))

	binding, relocated, err := cliSoleClassBinding(cityPath)
	if err != nil {
		t.Fatalf("a refused city resolved to err=%v; the refusal travels in the store, not in this return", err)
	}
	if !relocated {
		t.Fatal("a refused city reports relocates-nothing; its reads would fall back to the ledger the migration moved them off")
	}
	if _, err := binding.Store.Get("hq-1"); !errors.Is(err, refusal) {
		t.Errorf("reading the carried binding returned %v, want the standing refusal", err)
	}
}

// TestSoleClassBindingIsAbsentOnACityThatRelocatesNothing is the compatibility
// row: no binding, no error, and every caller's existing path stands unchanged.
func TestSoleClassBindingIsAbsentOnACityThatRelocatesNothing(t *testing.T) {
	cityPath := t.TempDir()
	seedCLIStorageRoutes(t, cityPath, &storageRoutes{stores: map[coordclass.Class]beads.Store{}})

	binding, relocated, err := cliSoleClassBinding(cityPath)
	if err != nil {
		t.Fatalf("a single-store city resolved to err=%v", err)
	}
	if relocated {
		t.Errorf("a single-store city reported a binding %p", binding.Store)
	}
}
