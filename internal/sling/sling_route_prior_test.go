package sling

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestRouteRequestCarriesThePriorRoute pins that the core hands the router each
// bead's gc.routed_to as it stood before the route (PriorRoutedTo), on the
// single-bead and the batch path, so a router can stamp gc.routed_at only when
// the route changes.
func TestRouteRequestCarriesThePriorRoute(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	prior := map[string]string{beadmeta.RoutedToMetadataKey: "old/pool"}

	t.Run("single bead", func(t *testing.T) {
		store := beads.NewMemStore()
		bead, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "open", Metadata: prior})
		if err != nil {
			t.Fatalf("create bead: %v", err)
		}
		router := &fakeBeadRouter{}
		deps := withStore(testDeps(cfg, runtime.NewFake(), newFakeRunner().run), store)
		deps.Router = router
		s, err := New(deps)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RouteBead(context.Background(), bead.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, RouteOpts{}); err != nil {
			t.Fatalf("RouteBead: %v", err)
		}
		if len(router.routed) != 1 || router.routed[0].PriorRoutedTo != "old/pool" {
			t.Fatalf("routed = %+v, want one request with PriorRoutedTo old/pool", router.routed)
		}
	})

	t.Run("batch", func(t *testing.T) {
		store := beads.NewMemStore()
		convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
		if err != nil {
			t.Fatalf("create convoy: %v", err)
		}
		child, err := store.Create(beads.Bead{Title: "child", Type: "task", Status: "open", Metadata: prior})
		if err != nil {
			t.Fatalf("create child: %v", err)
		}
		if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
			t.Fatalf("track child: %v", err)
		}
		router := &fakeBeadRouter{}
		deps := withStore(testDeps(cfg, runtime.NewFake(), newFakeRunner().run), store)
		deps.Router = router
		if _, err := DoSlingBatch(SlingOpts{Target: slingWorker(), BeadOrFormula: convoy.ID}, deps, beadstest.Work(store)); err != nil {
			t.Fatalf("DoSlingBatch: %v", err)
		}
		if len(router.routed) != 1 || router.routed[0].BeadID != child.ID || router.routed[0].PriorRoutedTo != "old/pool" {
			t.Fatalf("routed = %+v, want one request for %s with PriorRoutedTo old/pool", router.routed, child.ID)
		}
	})
}
