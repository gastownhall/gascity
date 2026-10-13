package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// TestExistingGraphV2RootTakesOnlyARoot (architecture B): a task carrying a
// launch's gc.graphv2_root_key neither stands in for its root nor makes the
// launch ambiguous ("multiple live roots"): with none, the launch mints;
// with the real root, it resumes that one.
func TestExistingGraphV2RootTakesOnlyARoot(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{Steps: []formula.RecipeStep{{ID: "root", Metadata: map[string]string{beadmeta.Graphv2RootKeyMetadataKey: "k"}}}}
	if _, err := store.Create(beads.Bead{Title: "forged", Type: "task", Metadata: map[string]string{beadmeta.Graphv2RootKeyMetadataKey: "k"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := existingGraphV2Root(store, recipe); err != nil || got != nil {
		t.Fatalf("existingGraphV2Root = %+v (%v), want none", got, err)
	}
	root, err := store.Create(beads.Bead{Title: "root", Type: "task", Metadata: map[string]string{beadmeta.Graphv2RootKeyMetadataKey: "k", beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := existingGraphV2Root(store, recipe); err != nil || got == nil || got.RootID != root.ID {
		t.Fatalf("existingGraphV2Root = %+v (%v), want %s", got, err, root.ID)
	}
}
