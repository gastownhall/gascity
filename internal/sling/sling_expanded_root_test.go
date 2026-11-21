package sling

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// recordingRouter stamps gc.routed_to the way the CLI's built-in router does
// (cmd/gc's cliBeadRouter) and records which beads went through it, so tests
// can tell the router's own write apart from the member-routing plan's.
type recordingRouter struct {
	store  beads.Store
	routed []string
}

func (r *recordingRouter) Route(_ context.Context, req RouteRequest) error {
	if err := r.store.SetMetadata(req.BeadID, beadmeta.RoutedToMetadataKey, req.Target); err != nil {
		return err
	}
	r.routed = append(r.routed, req.BeadID)
	return nil
}

// cookedWorkflowStore seeds the store with what `gc formula cook` leaves
// behind: an expanded workflow root and its still-unrouted step beads.
func cookedWorkflowStore(t *testing.T) *beads.MemStore {
	t.Helper()
	root := beads.Bead{
		ID: "wf-1", Title: "cook pancakes", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.WorkflowExpandedMetadataKey: "true",
		},
	}
	steps := []beads.Bead{
		{
			ID: "wf-1.mix", Title: "mix the batter", Type: "task", Status: "open",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		},
		{
			ID: "wf-1.flip", Title: "flip the pancake", Type: "task", Status: "open",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		},
	}
	return beads.NewMemStoreFrom(0, append([]beads.Bead{root}, steps...), nil)
}

func TestSlingExpandedRootRoutesMembers(t *testing.T) {
	store := cookedWorkflowStore(t)
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps = withStore(deps, store)
	router := &recordingRouter{store: store}
	deps.Router = router

	result, err := DoSling(testOpts(worker, "wf-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.RoutedMembers != 2 {
		t.Fatalf("RoutedMembers = %d, want 2 (both unrouted steps of the cooked run)", result.RoutedMembers)
	}
	for _, id := range []string{"wf-1", "wf-1.mix", "wf-1.flip"} {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s after the sling: %v", id, err)
		}
		if got := bead.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
			t.Errorf("%s gc.routed_to = %q, want worker", id, got)
		}
	}
	if len(router.routed) != 1 || router.routed[0] != "wf-1" {
		t.Errorf("router routed %v, want only the root wf-1 (the plan stamps the members)", router.routed)
	}
}
