package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// TestRunCancelClosesACrossStoreGateAsTheController: POST /runs/{id}/cancel
// closes every open bead of the run's workflow root, a drain's cross-store
// gate among them; the gate must read as closed by the controller (a revived
// drain that finds its blocker open reopens it), not as an operator's final
// close.
func TestRunCancelClosesACrossStoreGateAsTheController(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "run", Type: "task", Metadata: map[string]string{
		"gc.kind":             "workflow",
		"gc.formula_contract": "graph.v2",
	}})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := store.Create(beads.Bead{Title: "gate", Type: "gate", Labels: []string{dispatch.CrossStoreGateLabel}, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:          root.ID,
		beadmeta.CrossStoreGateOwnerMetadataKey: "drain-1",
		beadmeta.CrossStoreBlockerMetadataKey:   "rg-9",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cancelWorkflowRoot(store, root.ID); err != nil {
		t.Fatalf("cancelWorkflowRoot: %v", err)
	}
	got, err := store.Get(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Fatalf("gate = %s; the run cancel does not reach it", got.Status)
	}
	if !dispatch.IsControllerClosedCrossStoreGate(got) {
		t.Fatalf("gate closed by the run cancel with close_reason %q reads as an operator close", got.Metadata["close_reason"])
	}
}
