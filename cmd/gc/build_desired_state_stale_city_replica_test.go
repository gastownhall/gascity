package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestBuildDesiredState_RigPoolIgnoresStaleCityCopyOfClosedRigBead (#5857):
// a bead closed in its owning rig store but still open (a frozen replica) in
// the city store is not work the rig seat will claim. The city-store probe
// must not count it as rig-pool demand, or every tick mints a seat that
// drains no_work.
func TestBuildDesiredState_RigPoolIgnoresStaleCityCopyOfClosedRigBead(t *testing.T) {
	cityPath := t.TempDir()
	cfg := warmCrossStoreCfg(t, cityPath)
	cityStore := beads.NewMemStore()
	cityStore.HonorExplicitIDs = true
	rigStore := beads.NewMemStore()
	rigStore.HonorExplicitIDs = true
	rigStores := map[string]beads.Store{"gascity": rigStore}

	const id = "canary-8a1"
	route := map[string]string{"gc.routed_to": warmWorkerTemplate}
	if _, err := rigStore.Create(beads.Bead{ID: id, Title: "prepare", Type: "task", Status: "open", Metadata: route}); err != nil {
		t.Fatalf("create rig bead: %v", err)
	}
	if err := rigStore.Close(id); err != nil {
		t.Fatalf("close rig bead: %v", err)
	}
	if _, err := cityStore.Create(beads.Bead{ID: id, Title: "prepare", Type: "task", Status: "open", Metadata: route}); err != nil {
		t.Fatalf("create stale city copy: %v", err)
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, rigStores, nil, nil, io.Discard,
	)

	if got := dsResult.ScaleCheckCounts[warmWorkerTemplate]; got != 0 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 0: bead %s is closed in its owning rig store; "+
			"its stale open city-store copy is not demand the rig seat will claim (full=%v)",
			warmWorkerTemplate, got, id, dsResult.ScaleCheckCounts)
	}
}
