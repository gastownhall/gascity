package dispatch

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
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

func TestProcessDrainSharedReusesWorkspaceAndKeepsExistingWorkDir(t *testing.T) {
	t.Run("second member gets the same workspace", func(t *testing.T) {
		formulatest.EnableV2ForTest(t)
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		hook := &recordingWorkspaceHook{path: t.TempDir()}
		opts := ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}

		if _, err := ProcessControl(store, drain, opts); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
		if err := updateMetadataAndClose(store, manifest.Rows[0].ItemRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
			t.Fatalf("close first root pass: %v", err)
		}
		result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), opts)
		if err != nil {
			t.Fatalf("ProcessControl(shared second): %v", err)
		}
		if result.Action != "drain-shared-advanced" {
			t.Fatalf("Action = %q, want drain-shared-advanced", result.Action)
		}
		manifest = mustDrainManifest(t, mustGetBead(t, store, drain.ID))
		if manifest.Rows[1].ItemRootID == "" {
			t.Fatalf("second row = %+v, want materialized", manifest.Rows[1])
		}
		second := mustGetBead(t, store, members[1].ID)
		if got := second.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != hook.path {
			t.Fatalf("second member work_dir = %q, want %q", got, hook.path)
		}
		assertItemWorkDir(t, store, manifest.Rows[1].ItemRootID, hook.path)
		if want := []string{drain.ID, drain.ID}; !slices.Equal(hook.calls, want) {
			t.Fatalf("hook calls = %v, want one per reserved member %v", hook.calls, want)
		}
	})

	t.Run("existing work_dir wins and skips the hook", func(t *testing.T) {
		formulatest.EnableV2ForTest(t)
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		existing := t.TempDir()
		if err := store.SetMetadata(members[0].ID, beadmeta.LegacyWorkDirMetadataKey, existing); err != nil {
			t.Fatalf("SetMetadata(existing work_dir): %v", err)
		}
		hook := &recordingWorkspaceHook{path: t.TempDir()}

		if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		first := mustGetBead(t, store, members[0].ID)
		if got := first.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != existing {
			t.Fatalf("first member work_dir = %q, want existing %q", got, existing)
		}
		manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
		assertItemWorkDir(t, store, manifest.Rows[0].ItemRootID, existing)
		if len(hook.calls) != 0 {
			t.Fatalf("hook calls = %v, want none for a member that already has work_dir", hook.calls)
		}
	})

	t.Run("crash resume after the write reuses the recorded workspace", func(t *testing.T) {
		formulatest.EnableV2ForTest(t)
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		recorded := t.TempDir()
		if err := store.SetMetadataBatch(members[0].ID, map[string]string{
			beadmeta.LegacyWorkDirMetadataKey:             recorded,
			beadmeta.ExclusiveDrainReservationMetadataKey: drain.ID,
		}); err != nil {
			t.Fatalf("SetMetadataBatch(resume state): %v", err)
		}
		hook := &recordingWorkspaceHook{path: t.TempDir()}

		if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}); err != nil {
			t.Fatalf("ProcessControl(resume): %v", err)
		}
		manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
		assertItemWorkDir(t, store, manifest.Rows[0].ItemRootID, recorded)
		if len(hook.calls) != 0 {
			t.Fatalf("hook calls = %v, want none on resume", hook.calls)
		}
	})

	t.Run("observability mirror alone does not count as a workspace", func(t *testing.T) {
		formulatest.EnableV2ForTest(t)
		dir := t.TempDir()
		writeDrainItemFormula(t, dir)
		store, drain, members := seedSharedExclusiveDrain(t)
		if err := store.SetMetadata(members[0].ID, beadmeta.WorkDirMetadataKey, t.TempDir()); err != nil {
			t.Fatalf("SetMetadata(gc.work_dir mirror): %v", err)
		}
		hook := &recordingWorkspaceHook{path: t.TempDir()}

		if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision}); err != nil {
			t.Fatalf("ProcessControl(shared first): %v", err)
		}
		if len(hook.calls) != 1 {
			t.Fatalf("hook calls = %v, want one for a member with only gc.work_dir", hook.calls)
		}
		first := mustGetBead(t, store, members[0].ID)
		if got := first.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != hook.path {
			t.Fatalf("first member work_dir = %q, want %q", got, hook.path)
		}
	})
}

func TestProcessDrainWorkspaceHookSkippedOutsideSharedExclusive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		nilHook  bool
	}{
		{name: "shared read access", metadata: map[string]string{"gc.drain_member_access": "read"}},
		{name: "separate context", metadata: map[string]string{"gc.drain_context": "separate"}},
		{name: "nil hook", nilHook: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			formulatest.EnableV2ForTest(t)
			dir := t.TempDir()
			writeDrainItemFormula(t, dir)
			store, drain, members := seedSharedExclusiveDrain(t)
			if len(tc.metadata) > 0 {
				if err := store.SetMetadataBatch(drain.ID, tc.metadata); err != nil {
					t.Fatalf("SetMetadataBatch(%s): %v", tc.name, err)
				}
				drain = mustGetBead(t, store, drain.ID)
			}
			hook := &recordingWorkspaceHook{path: t.TempDir()}
			opts := ProcessOptions{FormulaSearchPaths: []string{dir}}
			if !tc.nilHook {
				opts.SharedDrainWorkspace = hook.provision
			}

			if _, err := ProcessControl(store, drain, opts); err != nil {
				t.Fatalf("ProcessControl(%s): %v", tc.name, err)
			}
			if len(hook.calls) != 0 {
				t.Fatalf("hook calls = %v, want none", hook.calls)
			}
			for _, member := range members {
				after := mustGetBead(t, store, member.ID)
				if got := after.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != "" {
					t.Fatalf("member %s work_dir = %q, want unwritten", member.ID, got)
				}
			}
		})
	}
}

func TestProcessDrainSharedWorkspaceFailureClosesHardAndReleases(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain, members := seedSharedExclusiveDrain(t)
	hook := &recordingWorkspaceHook{err: errors.New("worktree root is inside the rig checkout")}

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision})
	if err != nil {
		t.Fatalf("ProcessControl(workspace failure): %v", err)
	}
	if result.Action != "drain-failed" {
		t.Fatalf("Action = %q, want drain-failed", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" {
		t.Fatalf("drain status = %q, want closed", drain.Status)
	}
	for key, want := range map[string]string{
		"gc.outcome":         "fail",
		"gc.failure_class":   "hard",
		"gc.failure_reason":  "shared_workspace_unavailable",
		"gc.failure_subject": members[0].ID,
	} {
		if got := drain.Metadata[key]; got != want {
			t.Fatalf("drain %s = %q, want %q", key, got, want)
		}
	}
	if got := drain.Metadata[beadmeta.ControllerErrorMetadataKey]; !strings.Contains(got, "worktree root is inside the rig checkout") {
		t.Fatalf("gc.controller_error = %q, want the hook error", got)
	}
	if got := drain.Metadata[beadmeta.ControllerErrorClassMetadataKey]; got != beadmeta.FailureClassHard {
		t.Fatalf("gc.controller_error_class = %q, want hard", got)
	}
	manifest := mustDrainManifest(t, drain)
	for _, row := range manifest.Rows {
		if row.ItemRootID != "" {
			t.Fatalf("row = %+v, want no item root", row)
		}
		if row.Status != "failed" || row.Failure != "shared_workspace_unavailable" {
			t.Fatalf("row = %+v, want failed shared_workspace_unavailable", row)
		}
	}
	for _, member := range members {
		after := mustGetBead(t, store, member.ID)
		if got := strings.TrimSpace(after.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]); got != "" {
			t.Fatalf("member %s reservation = %q, want released", member.ID, got)
		}
		if got := after.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != "" {
			t.Fatalf("member %s work_dir = %q, want unwritten", member.ID, got)
		}
	}
}

func TestProcessDrainSharedWorkspaceMemberWriteErrorKeepsDrainOpen(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	mem, drain, members := seedSharedExclusiveDrain(t)
	writeErr := errors.New("store unavailable")
	store := &failOnceMetadataKeyStore{Store: mem, key: beadmeta.LegacyWorkDirMetadataKey, err: writeErr}
	hook := &recordingWorkspaceHook{path: t.TempDir()}

	_, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision})
	if !errors.Is(err, writeErr) {
		t.Fatalf("ProcessControl err = %v, want wrapped %v", err, writeErr)
	}
	if !strings.Contains(err.Error(), members[0].ID) {
		t.Fatalf("ProcessControl err = %v, want it to name member %s", err, members[0].ID)
	}
	drain = mustGetBead(t, mem, drain.ID)
	if drain.Status == "closed" {
		t.Fatalf("drain closed with outcome %q, want it open for retry", drain.Metadata["gc.outcome"])
	}
	if raw := drain.Metadata[drainManifestMetadataKey]; raw != "" {
		for _, row := range mustDrainManifest(t, drain).Rows {
			if row.ItemRootID != "" {
				t.Fatalf("row = %+v, want no item root", row)
			}
		}
	}
	if got := mustGetBead(t, mem, members[0].ID).Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != "" {
		t.Fatalf("first member work_dir = %q, want unwritten", got)
	}

	result, err := ProcessControl(store, mustGetBead(t, mem, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}, SharedDrainWorkspace: hook.provision})
	if err != nil {
		t.Fatalf("ProcessControl(retry): %v", err)
	}
	if result.Action != "drain-shared-advanced" {
		t.Fatalf("retry Action = %q, want drain-shared-advanced", result.Action)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, mem, drain.ID))
	assertItemWorkDir(t, mem, manifest.Rows[0].ItemRootID, hook.path)
}

func TestStampDrainItemRecipePrefersLegacyWorkDir(t *testing.T) {
	recipe := &formula.Recipe{Steps: []formula.RecipeStep{
		{ID: "drain-item", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
		{ID: "drain-item.work"},
	}}
	control := beads.Bead{ID: "gc-drain", Metadata: map[string]string{"gc.drain_member_access": "exclusive"}}
	member := beads.Bead{ID: "gc-member", Metadata: map[string]string{
		beadmeta.LegacyWorkDirMetadataKey: "/work/creator",
		beadmeta.WorkDirMetadataKey:       "/work/mirror",
	}}
	row := &drainManifestRow{Index: 0, MemberID: member.ID}

	stampDrainItemRecipe(recipe, control, beads.Bead{ID: "gc-unit"}, member, 1, row, "drain-item", nil)

	for _, step := range recipe.Steps {
		for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
			if got := step.Metadata[key]; got != "/work/creator" {
				t.Fatalf("step %s %s = %q, want the creator's work_dir %q", step.ID, key, got, "/work/creator")
			}
		}
	}
}
