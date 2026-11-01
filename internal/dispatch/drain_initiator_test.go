package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formulatest"
)

// TestProcessDrainItemRootsInheritRunInitiator pins that every item workflow a
// drain instantiates carries the launching run's gc.initiator, in both the
// separate and the shared drain contexts. Each item formula is a new
// self-rooted workflow, so without the inherited stamp everything reached
// through the item root reads as unattributed.
func TestProcessDrainItemRootsInheritRunInitiator(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
	}{
		{name: "separate"},
		{name: "shared", metadata: map[string]string{
			"gc.drain_context":       "shared",
			"gc.drain_member_access": "exclusive",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			formulatest.EnableV2ForTest(t)
			dir := t.TempDir()
			writeDrainItemFormula(t, dir)
			store, drain := seedDrainWorkflow(t)
			if err := store.SetMetadata(drain.Metadata[beadmeta.RootBeadIDMetadataKey], beadmeta.InitiatorMetadataKey, "kid-alice"); err != nil {
				t.Fatalf("SetMetadata(root initiator): %v", err)
			}
			if len(tc.metadata) > 0 {
				if err := store.SetMetadataBatch(drain.ID, tc.metadata); err != nil {
					t.Fatalf("SetMetadataBatch(drain): %v", err)
				}
			}

			if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
				t.Fatalf("ProcessControl(drain expand): %v", err)
			}
			created := assertDrainItemRootsInitiator(t, store, drain.ID, "kid-alice")
			if created == 0 {
				t.Fatalf("drain %s materialized no item roots", drain.ID)
			}
		})
	}
}

// TestProcessDrainItemRootsDoNotInventInitiator pins that a drain launched by
// an unattributed run leaves its item roots unattributed: the stamp is only
// ever inherited.
func TestProcessDrainItemRootsDoNotInventInitiator(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, beadmeta.InitiatorMetadataKey, "kid-control-only"); err != nil {
		t.Fatalf("SetMetadata(control initiator): %v", err)
	}

	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	if created := assertDrainItemRootsInitiator(t, store, drain.ID, ""); created == 0 {
		t.Fatalf("drain %s materialized no item roots", drain.ID)
	}
}

func assertDrainItemRootsInitiator(t *testing.T, store beads.Store, drainID, want string) int {
	t.Helper()
	manifest := mustDrainManifest(t, mustGetBead(t, store, drainID))
	created := 0
	for _, row := range manifest.Rows {
		if row.ItemRootID == "" {
			continue
		}
		created++
		itemRoot := mustGetBead(t, store, row.ItemRootID)
		got, ok := itemRoot.Metadata[beadmeta.InitiatorMetadataKey]
		if want == "" {
			if ok {
				t.Fatalf("item root %s gc.initiator = %q, want unset", itemRoot.ID, got)
			}
			continue
		}
		if got != want {
			t.Fatalf("item root %s gc.initiator = %q, want %q", itemRoot.ID, got, want)
		}
	}
	return created
}
