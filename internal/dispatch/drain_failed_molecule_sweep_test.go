package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// AC-6: a failed drain item root the failed-molecule sweep closed keeps its
// molecule_failed stamp, so the drain's same-key re-entry mints a fresh item
// root instead of adopting the swept one, and a further sweep closes nothing.
func TestEnsureDrainItemRootAfterFailedMoleculeSweep(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	opts := ProcessOptions{Work: beadstest.Work(store), FormulaSearchPaths: []string{dir}}

	if _, err := ProcessControl(beadstest.Graph(store), drain, opts); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	itemRootKey := manifest.Rows[0].ItemRootKey
	failedRootID := manifest.Rows[0].ItemRootID
	if err := store.SetMetadata(failedRootID, beadmeta.MoleculeFailedMetadataKey, "true"); err != nil {
		t.Fatalf("mark failed item root: %v", err)
	}

	swept, err := sourceworkflow.SweepFailedMolecules(context.Background(), beadstest.FederatedLeg(store), beadstest.GraphMolecule(store), time.Now().Add(time.Hour), sourceworkflow.FailedMoleculeSweepBudget{})
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if len(swept.Chains) != 1 || swept.Chains[0] != failedRootID {
		t.Fatalf("sweep = %+v, want the failed item root %s swept", swept, failedRootID)
	}
	if got := mustGetBead(t, store, failedRootID); got.Status != "closed" || got.Metadata[beadmeta.MoleculeFailedMetadataKey] != "true" {
		t.Fatalf("swept root status=%q molecule_failed=%q, want closed and still stamped", got.Status, got.Metadata[beadmeta.MoleculeFailedMetadataKey])
	}

	manifest.Rows[0].ItemRootID = ""
	manifest.Rows[0].Status = "unit-created"
	if err := persistDrainManifest(beadstest.Graph(store), drain.ID, manifest, map[string]string{"gc.drain_state": "expanding"}); err != nil {
		t.Fatalf("persist rewound manifest: %v", err)
	}
	if _, err := ProcessControl(beadstest.Graph(store), mustGetBead(t, store, drain.ID), opts); err != nil {
		t.Fatalf("ProcessControl(drain replay): %v", err)
	}
	freshRootID := mustDrainManifest(t, mustGetBead(t, store, drain.ID)).Rows[0].ItemRootID
	if freshRootID == "" || freshRootID == failedRootID {
		t.Fatalf("re-entered item root = %q, want a fresh root, not the swept %s", freshRootID, failedRootID)
	}
	roots, err := store.ListByMetadata(map[string]string{beadmeta.ItemRootKeyMetadataKey: itemRootKey}, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByMetadata(item root key): %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("roots for item key = %d, want the swept root and one fresh root", len(roots))
	}
	if fresh := mustGetBead(t, store, freshRootID); fresh.Status == "closed" || fresh.Metadata[beadmeta.MoleculeFailedMetadataKey] == "true" {
		t.Fatalf("fresh root status=%q molecule_failed=%q, want open and unstamped", fresh.Status, fresh.Metadata[beadmeta.MoleculeFailedMetadataKey])
	}

	again, err := sourceworkflow.SweepFailedMolecules(context.Background(), beadstest.FederatedLeg(store), beadstest.GraphMolecule(store), time.Now().Add(time.Hour), sourceworkflow.FailedMoleculeSweepBudget{})
	if err != nil || len(again.Chains) != 0 || again.Closed != 0 {
		t.Fatalf("second sweep = %+v, %v; want nothing closed and no error", again, err)
	}
}
