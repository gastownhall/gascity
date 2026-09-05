package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// newSourceWorkflowScanFixture builds the directory scan a CLI sling starts
// from: one city store and one rig store, opened as ordinary views. Whether the
// city relocates its classes is the caller's choice, seeded separately.
func newSourceWorkflowScanFixture(t *testing.T) (cfg *config.City, cityPath string, views []convoyStoreView) {
	t.Helper()
	cityPath = t.TempDir()
	rigPath := filepath.Join(cityPath, "alpha")
	cfg = &config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
		Rigs:      []config.Rig{{Name: "alpha", Path: rigPath}},
	}
	views = []convoyStoreView{
		{path: cityPath, store: splittest.NewWorkStore(t, "hq")},
		{path: rigPath, store: splittest.NewWorkStore(t, "al")},
	}
	return cfg, cityPath, views
}

// TestSourceWorkflowStoresFromViewsLeadsWithTheClassBinding is the CLI door's
// half of ga-nqdff: the directory scan enumerates directories, and a relocated
// binding is not one of them, so the singleton guard never saw the store its
// answer lives in.
func TestSourceWorkflowStoresFromViewsLeadsWithTheClassBinding(t *testing.T) {
	cfg, cityPath, views := newSourceWorkflowScanFixture(t)
	binding := splittest.NewWorkStore(t, "in")
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))

	stores, err := sourceWorkflowStoresFromViews(cfg, cityPath, "bright-lights", views)
	if err != nil {
		t.Fatalf("sourceWorkflowStoresFromViews: %v", err)
	}
	if len(stores) != 3 {
		t.Fatalf("projected %d legs, want the binding plus both scanned dirs: %+v", len(stores), stores)
	}
	if stores[0].Store != binding {
		t.Fatalf("stores[0].Store = %p, want the class binding %p to lead", stores[0].Store, binding)
	}
	if stores[0].StoreRef != sourceworkflow.GraphStoreRef("bright-lights") {
		t.Fatalf("stores[0].StoreRef = %q, want %q", stores[0].StoreRef, sourceworkflow.GraphStoreRef("bright-lights"))
	}
	if !stores[0].Strict {
		t.Fatal("the binding leg is not strict; a fault on the store holding the answer would degrade to a warning")
	}
	if stores[1].StoreRef != "city:bright-lights" || stores[2].StoreRef != "rig:alpha" {
		t.Fatalf("work legs = %q, %q; want city:bright-lights then rig:alpha", stores[1].StoreRef, stores[2].StoreRef)
	}
	for _, info := range stores[1:] {
		if info.Strict {
			t.Fatalf("work leg %q is strict; only the binding and the selected source store are", info.StoreRef)
		}
	}
}

// TestSourceWorkflowStoresFromViewsIsUnchangedOnASingleStoreCity is decision
// (4) on the CLI door: a city that relocates nothing projects exactly the
// scanned views, in order, with no strict leg.
func TestSourceWorkflowStoresFromViewsIsUnchangedOnASingleStoreCity(t *testing.T) {
	cfg, cityPath, views := newSourceWorkflowScanFixture(t)
	seedCLIStorageRoutes(t, cityPath, &storageRoutes{stores: map[coordclass.Class]beads.Store{}})

	stores, err := sourceWorkflowStoresFromViews(cfg, cityPath, "bright-lights", views)
	if err != nil {
		t.Fatalf("sourceWorkflowStoresFromViews: %v", err)
	}
	if len(stores) != len(views) {
		t.Fatalf("projected %d legs, want exactly the %d scanned dirs", len(stores), len(views))
	}
	for i, info := range stores {
		if info.Store != views[i].store {
			t.Fatalf("stores[%d].Store = %p, want the scanned view %p in scan order", i, info.Store, views[i].store)
		}
		if info.Strict {
			t.Fatalf("stores[%d] (%q) is strict on a city that relocates nothing", i, info.StoreRef)
		}
		if strings.HasPrefix(info.StoreRef, sourceworkflow.GraphStoreRefPrefix+":") {
			t.Fatalf("stores[%d].StoreRef = %q; a single-store city has no binding leg to name", i, info.StoreRef)
		}
	}
}

// TestSourceWorkflowStoresFromViewsRefusesAnUnreadableBinding pins decision (3)
// at the CLI door. A sling is a mutation: reaching only the work ledger of a
// city whose binding cannot be read would admit a launch on the strength of the
// frozen copies the migration retained.
func TestSourceWorkflowStoresFromViewsRefusesAnUnreadableBinding(t *testing.T) {
	cfg, cityPath, views := newSourceWorkflowScanFixture(t)
	refusal := errors.New("this city is configured for a binding it has not converged on")
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(refusedClassStore{err: refusal}))

	stores, err := sourceWorkflowStoresFromViews(cfg, cityPath, "bright-lights", views)
	if err == nil {
		t.Fatalf("projected %d legs from a refused city; the guard would answer out of the work ledger alone", len(stores))
	}
	if !errors.Is(err, refusal) {
		t.Fatalf("error = %v, want the standing refusal", err)
	}
}
