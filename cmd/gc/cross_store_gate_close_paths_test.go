package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
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

// TestCmdGCRootClosesReadAsControllerClosesOnACrossStoreGate: gc hook
// --claim's skip of a done workflow and the stale order wisp sweep close by
// gc.root_bead_id, so they reach a drain's gate; each reason is the shared
// constant dispatch reads, and the real close leaves the gate controller-closed.
func TestCmdGCRootClosesReadAsControllerClosesOnACrossStoreGate(t *testing.T) {
	if hookSkipDoneWorkflowCloseReason != sourceworkflow.HookSkipDoneWorkflowCloseReason {
		t.Fatalf("hookSkipDoneWorkflowCloseReason %q drifted from sourceworkflow's %q", hookSkipDoneWorkflowCloseReason, sourceworkflow.HookSkipDoneWorkflowCloseReason)
	}
	if staleOrderWispCloseReason != sourceworkflow.StaleOrderWispCloseReason {
		t.Fatalf("staleOrderWispCloseReason %q drifted from sourceworkflow's %q", staleOrderWispCloseReason, sourceworkflow.StaleOrderWispCloseReason)
	}
	for name, closeRoot := range map[string]func(store beads.Store, rootID, gateID string) error{
		"gc hook --claim skipDoneWorkflow": func(store beads.Store, rootID, _ string) error {
			return skipDoneWorkflow(store, rootID)
		},
		"stale order wisp sweep": func(store beads.Store, rootID, gateID string) error {
			_, err := closeStaleOrderWispIDs(store, []string{gateID, rootID}, "")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			root, err := store.Create(beads.Bead{Title: "workflow", Type: "task"})
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
			if err := closeRoot(store, root.ID, gate.ID); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(gate.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "closed" || !dispatch.IsControllerClosedCrossStoreGate(got) {
				t.Fatalf("gate = %s, close_reason %q; want closed by the controller", got.Status, got.Metadata["close_reason"])
			}
		})
	}
}
