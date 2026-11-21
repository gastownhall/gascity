package sling

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// AC-6: a keyed graph.v2 root the failed-molecule sweep closed is not adopted
// on re-entry: the next launch with the same key finds no live root, mints
// one, and resumes exactly that one; a second sweep finds nothing to close.
func TestExistingGraphV2RootAfterFailedMoleculeSweep(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{Steps: []formula.RecipeStep{{ID: "root", Metadata: map[string]string{beadmeta.Graphv2RootKeyMetadataKey: "k"}}}}
	rootMeta := func(extra ...string) map[string]string {
		m := map[string]string{
			beadmeta.Graphv2RootKeyMetadataKey:  "k",
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}
		for i := 0; i+1 < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}
	failed, err := store.Create(beads.Bead{Title: "failed root", Type: "task", Metadata: rootMeta(beadmeta.MoleculeFailedMetadataKey, "true")})
	if err != nil {
		t.Fatal(err)
	}
	step, err := store.Create(beads.Bead{Title: "failed step", Type: "task", ParentID: failed.ID, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:     failed.ID,
		beadmeta.MoleculeFailedMetadataKey: "true",
	}})
	if err != nil {
		t.Fatal(err)
	}

	swept, err := sourceworkflow.SweepFailedMolecules(beadstest.GraphMolecule(store), time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if len(swept.Chains) != 1 || swept.Chains[0] != failed.ID || swept.Closed != 2 {
		t.Fatalf("sweep = %+v, want chain %s with 2 beads closed", swept, failed.ID)
	}
	for _, id := range []string{failed.ID, step.ID} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "closed" || b.Metadata[beadmeta.MoleculeFailedMetadataKey] != "true" {
			t.Fatalf("bead %s = status %q molecule_failed %q, want closed and still stamped", id, b.Status, b.Metadata[beadmeta.MoleculeFailedMetadataKey])
		}
	}

	if got, err := existingGraphV2Root(beadstest.Graph(store), recipe); err != nil || got != nil {
		t.Fatalf("existingGraphV2Root after the sweep = %+v (%v), want none: the swept root is not adopted", got, err)
	}
	fresh, err := store.Create(beads.Bead{Title: "fresh root", Type: "task", Metadata: rootMeta()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := existingGraphV2Root(beadstest.Graph(store), recipe)
	if err != nil || got == nil || got.RootID != fresh.ID {
		t.Fatalf("existingGraphV2Root = %+v (%v), want the one fresh root %s", got, err, fresh.ID)
	}

	again, err := sourceworkflow.SweepFailedMolecules(beadstest.GraphMolecule(store), time.Now().Add(time.Hour), 0)
	if err != nil || len(again.Chains) != 0 || again.Closed != 0 {
		t.Fatalf("second sweep = %+v (%v), want nothing to close", again, err)
	}
}
