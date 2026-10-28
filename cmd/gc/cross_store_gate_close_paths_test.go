package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// TestWispGCRepairClosesACrossStoreGateAsTheController: wisp GC's repair of a
// closed workflow root closes the root's still-open members, a cross-store
// gate among them; the gate must read as closed by the controller (a drain
// that finds its blocker open reopens it), not as an operator's final close.
func TestWispGCRepairClosesACrossStoreGateAsTheController(t *testing.T) {
	now := time.Now()
	gate := makeGCBeadWithMetadata("gcg-xsg-1", now.Add(-30*time.Minute), "open", "gate", map[string]string{
		beadmeta.RootBeadIDMetadataKey:          "closed-root",
		beadmeta.CrossStoreGateOwnerMetadataKey: "drain-1",
		beadmeta.CrossStoreBlockerMetadataKey:   "rg-9",
	})
	gate.Labels = []string{dispatch.CrossStoreGateLabel}
	store := newGCStore([]beads.Bead{
		makeGCBeadWithMetadata("closed-root", now.Add(-30*time.Minute), "closed", "task", map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.outcome":          "pass",
		}),
		gate,
	})
	if _, err := closeGeneratedMembersForClosedRoots(store); err != nil {
		t.Fatalf("closeGeneratedMembersForClosedRoots: %v", err)
	}
	got, err := store.Get(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Fatalf("gate = %s; wisp GC's repair does not reach it", got.Status)
	}
	if !dispatch.IsControllerClosedCrossStoreGate(got) {
		t.Fatalf("gate closed by wisp GC with close_reason %q reads as an operator close", got.Metadata["close_reason"])
	}
}
