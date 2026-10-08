package sling

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// storeStampingRouter models the built-in routers' store contract
// (cmd/gc cliBeadRouter, internal/api apiBeadRouter): stamp gc.routed_to on
// RouteRequest.Store when the core names one, and on the router's own
// configured default store otherwise.
type storeStampingRouter struct {
	fallback beads.Store
	routed   []RouteRequest
}

func (r *storeStampingRouter) Route(_ context.Context, req RouteRequest) error {
	r.routed = append(r.routed, req)
	store := req.Store
	if store == nil {
		store = r.fallback
	}
	return store.SetMetadata(req.BeadID, beadmeta.RoutedToMetadataKey, req.Target)
}

// TestSlingFormulaRoutesTheWispRootThroughTheStoreThatMintedIt is #6054 at the
// core seam: a --formula wisp root is minted in the graph store, so the
// routing stamp and the merge-strategy stamp finalize writes for it must land
// in that store, not in the work store the sling was configured with. On a
// split city the work store has never held the root and the stamp failed with
// "bead not found", leaking an unrouted wisp into the graph binding.
func TestSlingFormulaRoutesTheWispRootThroughTheStoreThatMintedIt(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	deps, work, graph := splitSlingDeps(t, cfg)
	router := &storeStampingRouter{fallback: work}
	deps.Router = router

	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	result, err := DoSling(SlingOpts{
		Target:        a,
		BeadOrFormula: "code-review",
		IsFormula:     true,
		Merge:         "mr",
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling --formula on a split city: %v", err)
	}
	if len(result.MetadataErrors) != 0 {
		t.Errorf("MetadataErrors = %v, want none", result.MetadataErrors)
	}

	root, err := graph.Get(result.BeadID)
	if err != nil {
		t.Fatalf("wisp root %s is not in the graph store that minted it: %v", result.BeadID, err)
	}
	if _, err := work.Get(result.BeadID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("wisp root %s resolves in the work store (err=%v); the fixture no longer separates the legs", result.BeadID, err)
	}

	if len(router.routed) != 1 {
		t.Fatalf("router calls = %d, want 1", len(router.routed))
	}
	if got := router.routed[0]; got.BeadID != root.ID || got.Store != graph {
		t.Errorf("route request = {BeadID:%q Store:%T(%p)}, want {BeadID:%q Store: the graph store %p}", got.BeadID, got.Store, got.Store, root.ID, graph)
	}
	if got := root.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
		t.Errorf("graph-resident root gc.routed_to = %q, want worker", got)
	}
	if got := root.Metadata[beadmeta.MergeStrategyMetadataKey]; got != "mr" {
		t.Errorf("graph-resident root %s = %q, want mr", beadmeta.MergeStrategyMetadataKey, got)
	}
}

// TestSlingFormulaRouteStoreIsTheWorkStoreOnASingleStoreCity pins the
// single-store collapse: with no GraphStore the root is minted in, and routed
// through, the one store the sling was configured with.
func TestSlingFormulaRouteStoreIsTheWorkStoreOnASingleStoreCity(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	deps := testDeps(cfg, nil, newFakeRunner().run)
	router := &storeStampingRouter{fallback: deps.Store}
	deps.Router = router

	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "code-review", IsFormula: true}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling --formula: %v", err)
	}
	if len(router.routed) != 1 || router.routed[0].Store != deps.Store {
		t.Fatalf("route requests = %+v, want one naming the single store", router.routed)
	}
	root, err := deps.Store.Get(result.BeadID)
	if err != nil {
		t.Fatalf("Get(%s): %v", result.BeadID, err)
	}
	if got := root.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
		t.Errorf("gc.routed_to = %q, want worker", got)
	}
}
