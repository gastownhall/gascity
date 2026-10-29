package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formulatest"
)

// recordingWorkspaceHook is a SharedDrainWorkspace fake that records every
// drain control it was asked to provision for.
type recordingWorkspaceHook struct {
	path  string
	err   error
	calls []string
}

func (h *recordingWorkspaceHook) provision(control beads.Bead) (string, error) {
	h.calls = append(h.calls, control.ID)
	return h.path, h.err
}

// seedSharedExclusiveDrain returns a shared, exclusive-access drain over two
// task members, in convoy order.
func seedSharedExclusiveDrain(t *testing.T) (*beads.MemStore, beads.Bead, []beads.Bead) {
	t.Helper()
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadataBatch(drain.ID, map[string]string{
		"gc.drain_context":          "shared",
		"gc.drain_member_access":    "exclusive",
		"gc.drain_item_single_lane": "true",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(shared exclusive drain): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	members, err := convoycore.Members(store, root.Metadata["gc.input_convoy_id"], false)
	if err != nil {
		t.Fatalf("convoycore.Members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	return store, drain, members
}

// assertItemWorkDir requires the item root and every step under it to carry
// want in both work_dir keys.
func assertItemWorkDir(t *testing.T, store beads.Store, rootID, want string) {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List beads: %v", err)
	}
	seen := 0
	for _, bead := range all {
		if bead.ID != rootID && bead.ParentID != rootID && bead.Metadata[beadmeta.RootBeadIDMetadataKey] != rootID {
			continue
		}
		seen++
		for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
			if got := bead.Metadata[key]; got != want {
				t.Fatalf("bead %s (%s) %s = %q, want %q", bead.ID, bead.Title, key, got, want)
			}
		}
	}
	if seen < 2 {
		t.Fatalf("item root %s has %d beads (root + steps), want at least 2", rootID, seen)
	}
}

func TestProcessDrainSharedProvisionsWorkspaceOnReservedMember(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain, members := seedSharedExclusiveDrain(t)
	hook := &recordingWorkspaceHook{path: t.TempDir()}

	result, err := ProcessControl(store, drain, ProcessOptions{
		FormulaSearchPaths:   []string{dir},
		SharedDrainWorkspace: hook.provision,
	})
	if err != nil {
		t.Fatalf("ProcessControl(shared first): %v", err)
	}
	if result.Action != "drain-shared-advanced" {
		t.Fatalf("Action = %q, want drain-shared-advanced", result.Action)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	if manifest.Rows[0].MemberID != members[0].ID || manifest.Rows[0].ItemRootID == "" {
		t.Fatalf("rows = %+v, want first member %s materialized", manifest.Rows, members[0].ID)
	}
	first := mustGetBead(t, store, members[0].ID)
	if got := first.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != hook.path {
		t.Fatalf("first member work_dir = %q, want %q", got, hook.path)
	}
	assertItemWorkDir(t, store, manifest.Rows[0].ItemRootID, hook.path)
}
