package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/molecule"
)

func TestProcessDrainSeparateExpandsConvoyIntoUnitRoots(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	if result.Action != "drain-expanded" || result.Created != 4 {
		t.Fatalf("result = %+v, want drain-expanded with two units and two roots", result)
	}
	drain = mustGetBead(t, store, drain.ID)
	if got := drain.Metadata["gc.drain_state"]; got != "expanded" {
		t.Fatalf("gc.drain_state = %q, want expanded", got)
	}
	manifest := mustDrainManifest(t, drain)
	if len(manifest.Rows) != 2 {
		t.Fatalf("manifest rows = %d, want 2", len(manifest.Rows))
	}
	for _, row := range manifest.Rows {
		if row.UnitConvoyID == "" || row.ItemRootID == "" || row.Status != "wired" {
			t.Fatalf("row = %+v, want wired unit/root", row)
		}
		unit := mustGetBead(t, store, row.UnitConvoyID)
		if unit.Type != "convoy" || unit.Metadata["gc.synthetic_kind"] != "drain-unit-convoy" {
			t.Fatalf("unit = %+v, want drain-unit convoy", unit)
		}
		root := mustGetBead(t, store, row.ItemRootID)
		if root.Metadata["gc.input_convoy_id"] != row.UnitConvoyID {
			t.Fatalf("item root input convoy = %q, want %q", root.Metadata["gc.input_convoy_id"], row.UnitConvoyID)
		}
		if root.Metadata["gc.drain_control_id"] != drain.ID {
			t.Fatalf("item root metadata = %#v, missing drain control", root.Metadata)
		}
		if root.Metadata["gc.graphv2_root_key"] == row.ItemRootKey {
			t.Fatalf("item root graphv2 key = item root key %q, want formula-level key", row.ItemRootKey)
		}
	}
}

func TestProcessDrainSeparateProjectsMemberDependenciesOntoItemWorkflows(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	parentRoot := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	members, err := convoycore.Members(store, parentRoot.Metadata["gc.input_convoy_id"], false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	firstMember := members[0]
	secondMember := members[1]
	mustDepAdd(t, store, secondMember.ID, firstMember.ID, "blocks")

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	if result.Action != "drain-expanded" {
		t.Fatalf("result = %+v, want drain-expanded", result)
	}

	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	rowByMember := make(map[string]drainManifestRow, len(manifest.Rows))
	for _, row := range manifest.Rows {
		rowByMember[row.MemberID] = row
	}
	firstRow := rowByMember[firstMember.ID]
	secondRow := rowByMember[secondMember.ID]
	if firstRow.ItemRootID == "" || secondRow.ItemRootID == "" {
		t.Fatalf("missing item roots: first=%+v second=%+v", firstRow, secondRow)
	}

	assertHasBlockingDep(t, store, secondRow.ItemRootID, firstRow.ItemRootID)
	secondWork := mustFindDrainItemWorkStep(t, store, secondRow.ItemRootID)
	assertHasBlockingDep(t, store, secondWork.ID, firstRow.ItemRootID)

	firstWork := mustFindDrainItemWorkStep(t, store, firstRow.ItemRootID)
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !beadListContainsID(ready, firstWork.ID) {
		t.Fatalf("first item work step %s should be ready; ready=%+v", firstWork.ID, ready)
	}
	if beadListContainsID(ready, secondWork.ID) {
		t.Fatalf("second item work step %s should not be ready while %s is open; ready=%+v", secondWork.ID, firstRow.ItemRootID, ready)
	}
}

func TestProcessDrainSeparateCreatesDependentItemWorkflowsBlocked(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	prevGraphApply := molecule.IsGraphApplyEnabled()
	molecule.SetGraphApplyEnabled(false)
	t.Cleanup(func() { molecule.SetGraphApplyEnabled(prevGraphApply) })

	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	mem, drain := seedDrainWorkflow(t)
	parentRoot := mustGetBead(t, mem, drain.Metadata["gc.root_bead_id"])
	members, err := convoycore.Members(mem, parentRoot.Metadata["gc.input_convoy_id"], false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	firstMember := members[0]
	secondMember := members[1]
	mustDepAdd(t, mem, secondMember.ID, firstMember.ID, "blocks")

	var firstRootID string
	var secondRootID string
	secondRootCreatedBlocked := false
	secondWorkCreatedBlocked := false
	store := &createObservingStore{
		Store: mem,
		onCreate: func(created beads.Bead) {
			if created.Metadata["gc.drain_member_id"] == firstMember.ID && created.Metadata["gc.kind"] == "workflow" {
				firstRootID = created.ID
			}
			if firstRootID == "" {
				return
			}
			if created.Metadata["gc.drain_member_id"] == secondMember.ID && created.Metadata["gc.kind"] == "workflow" {
				secondRootID = created.ID
				secondRootCreatedBlocked = beadNeedsContains(created.Needs, firstRootID)
			}
			if secondRootID != "" && strings.HasPrefix(created.Title, "Work ") && (created.ParentID == secondRootID || created.Metadata["gc.root_bead_id"] == secondRootID) {
				secondWorkCreatedBlocked = beadNeedsContains(created.Needs, firstRootID)
			}
		},
	}

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	if result.Action != "drain-expanded" {
		t.Fatalf("result = %+v, want drain-expanded", result)
	}
	if !secondRootCreatedBlocked {
		t.Fatalf("second item root was created without a blocks need on first root %s", firstRootID)
	}
	if !secondWorkCreatedBlocked {
		t.Fatalf("second item work step was created without a blocks need on first root %s", firstRootID)
	}
}

func TestProcessDrainSeparateTopologicallyOrdersMembersBeforeCreation(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	prevGraphApply := molecule.IsGraphApplyEnabled()
	molecule.SetGraphApplyEnabled(false)
	t.Cleanup(func() { molecule.SetGraphApplyEnabled(prevGraphApply) })

	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	mem, drain, blocker, dependent := seedReverseOrderedDrainWorkflow(t)
	mustDepAdd(t, mem, dependent.ID, blocker.ID, "blocks")

	var blockerRootID string
	var dependentRootID string
	dependentRootCreatedBlocked := false
	dependentWorkCreatedBlocked := false
	store := &createObservingStore{
		Store: mem,
		onCreate: func(created beads.Bead) {
			if created.Metadata["gc.drain_member_id"] == blocker.ID && created.Metadata["gc.kind"] == "workflow" {
				blockerRootID = created.ID
			}
			if created.Metadata["gc.drain_member_id"] == dependent.ID && created.Metadata["gc.kind"] == "workflow" {
				dependentRootID = created.ID
				dependentRootCreatedBlocked = blockerRootID != "" && beadNeedsContains(created.Needs, blockerRootID)
			}
			if dependentRootID != "" && strings.HasPrefix(created.Title, "Work ") && (created.ParentID == dependentRootID || created.Metadata["gc.root_bead_id"] == dependentRootID) {
				dependentWorkCreatedBlocked = blockerRootID != "" && beadNeedsContains(created.Needs, blockerRootID)
			}
		},
	}

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	if result.Action != "drain-expanded" {
		t.Fatalf("result = %+v, want drain-expanded", result)
	}

	manifest := mustDrainManifest(t, mustGetBead(t, mem, drain.ID))
	if len(manifest.Rows) != 2 {
		t.Fatalf("manifest rows = %d, want 2", len(manifest.Rows))
	}
	if manifest.Rows[0].MemberID != blocker.ID || manifest.Rows[1].MemberID != dependent.ID {
		t.Fatalf("manifest order = [%s, %s], want blocker %s before dependent %s", manifest.Rows[0].MemberID, manifest.Rows[1].MemberID, blocker.ID, dependent.ID)
	}
	if !dependentRootCreatedBlocked {
		t.Fatalf("dependent item root was created without a blocks need on blocker root %s", blockerRootID)
	}
	if !dependentWorkCreatedBlocked {
		t.Fatalf("dependent item work step was created without a blocks need on blocker root %s", blockerRootID)
	}
	assertHasBlockingDep(t, mem, manifest.Rows[1].ItemRootID, manifest.Rows[0].ItemRootID)
}

// TestOrderDrainMembersByDependenciesRoutesMemberDepsToOwningStore proves the
// two-store shape: when a drain's convoy members live in a separate per-class
// store (supplied via ProcessOptions.MemberStores), member dependency ordering
// reads each member's edges from the member's OWNING store, not the ambient
// graph store the drain control runs in. A dependency edge is co-resident with
// its source bead, so reverting orderDrainMembersByDependencies to a bare
// store.DepList makes the (empty) ambient store report no blocker and the
// ordering silently collapses to insertion order — this is the regression
// canary for the member-dep cross-store routing.
func TestOrderDrainMembersByDependenciesRoutesMemberDepsToOwningStore(t *testing.T) {
	ambient := beads.NewMemStore() // the drain control's graph store — holds no members
	memberStore := beads.NewMemStore()

	blocker, err := memberStore.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	dependent, err := memberStore.Create(beads.Bead{Title: "dependent", Type: "task"})
	if err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	// dependent blocks-on blocker; the edge lives only in the member store.
	mustDepAdd(t, memberStore, dependent.ID, blocker.ID, "blocks")

	// Pass members in reverse (dependency) order so a no-op ordering would leave
	// them unchanged; correct ordering must move the blocker ahead of the dependent.
	ordered, err := orderDrainMembersByDependencies(ambient, []beads.Bead{dependent, blocker}, ProcessOptions{MemberStores: []beads.Store{memberStore}})
	if err != nil {
		t.Fatalf("orderDrainMembersByDependencies: %v", err)
	}
	if len(ordered) != 2 || ordered[0].ID != blocker.ID || ordered[1].ID != dependent.ID {
		got := make([]string, 0, len(ordered))
		for _, b := range ordered {
			got = append(got, b.ID)
		}
		t.Fatalf("order = %v, want blocker %s before dependent %s (member deps must route to the member store)", got, blocker.ID, dependent.ID)
	}
}

// TestDrainProjectedBlockerIDsRoutesMemberDepsToOwningStore proves
// drainProjectedBlockerIDs reads a member's outbound dependency edges from the
// member's owning store when the member lives in a separate per-class store, and
// projects an in-manifest blocker onto the blocker's item root. Reverting to a
// bare store.DepList makes the empty ambient store report no blockers, so this
// is the regression canary for the projected-blocker cross-store routing.
func TestDrainProjectedBlockerIDsRoutesMemberDepsToOwningStore(t *testing.T) {
	ambient := beads.NewMemStore()
	memberStore := beads.NewMemStore()

	blocker, err := memberStore.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	dependent, err := memberStore.Create(beads.Bead{Title: "dependent", Type: "task"})
	if err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	mustDepAdd(t, memberStore, dependent.ID, blocker.ID, "blocks")

	manifest := drainManifest{
		Version: 1,
		Rows: []drainManifestRow{
			{Index: 0, MemberID: blocker.ID, ItemRootID: "root-blocker"},
			{Index: 1, MemberID: dependent.ID, ItemRootID: "root-dependent"},
		},
	}

	blockerIDs, err := drainProjectedBlockerIDs(ambient, dependent.ID, manifest, ProcessOptions{MemberStores: []beads.Store{memberStore}})
	if err != nil {
		t.Fatalf("drainProjectedBlockerIDs: %v", err)
	}
	if len(blockerIDs) != 1 || blockerIDs[0] != "root-blocker" {
		t.Fatalf("blockerIDs = %v, want [root-blocker] (member deps routed to member store, projected onto the blocker's item root)", blockerIDs)
	}
}

func TestProcessDrainSeparateSucceedsForEmptyInputConvoy(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store := beads.NewMemStore()
	parent, err := store.Create(beads.Bead{Title: "empty parent", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.input_convoy_id":  parent.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain, err := store.Create(beads.Bead{
		Title: "drain",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "drain",
			"gc.root_bead_id":        root.ID,
			"gc.drain_context":       "separate",
			"gc.drain_formula":       "drain-item",
			"gc.drain_member_access": "read",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(empty drain): %v", err)
	}
	if result.Action != "drain-succeeded" {
		t.Fatalf("Action = %q, want drain-succeeded", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("drain = status %q outcome %q, want closed/pass", drain.Status, drain.Metadata["gc.outcome"])
	}
	manifest := mustDrainManifest(t, drain)
	if len(manifest.Rows) != 0 {
		t.Fatalf("manifest rows = %+v, want none", manifest.Rows)
	}
}

func TestProcessDrainSeparateFailsClosedWhenMaxUnitsExceeded(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_max_units", "1"); err != nil {
		t.Fatalf("SetMetadata(max units): %v", err)
	}

	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(limit exceeded): %v", err)
	}
	if result.Action != "drain-limit-exceeded" {
		t.Fatalf("Action = %q, want drain-limit-exceeded", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("drain = status %q outcome %q, want closed/fail", drain.Status, drain.Metadata["gc.outcome"])
	}
	if got := drain.Metadata["gc.failure_reason"]; got != "limit_exceeded" {
		t.Fatalf("gc.failure_reason = %q, want limit_exceeded", got)
	}
}

func TestProcessDrainSharedAdvancesOneItemAtATime(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadataBatch(drain.ID, map[string]string{
		"gc.drain_context":            "shared",
		"gc.drain_member_access":      "exclusive",
		"gc.drain_on_item_failure":    "skip_remaining",
		"gc.drain_item_single_lane":   "true",
		"gc.drain_continuation_group": "impl",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(shared drain): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(shared first): %v", err)
	}
	if result.Action != "drain-shared-advanced" || result.Created != 2 {
		t.Fatalf("result = %+v, want first shared unit/root", result)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	if manifest.Context != "shared" || len(manifest.Rows) != 2 {
		t.Fatalf("manifest = %+v, want two-row shared manifest", manifest)
	}
	if manifest.Rows[0].ItemRootID == "" || manifest.Rows[1].ItemRootID != "" {
		t.Fatalf("rows = %+v, want only first item root materialized", manifest.Rows)
	}
	firstRoot := mustGetBead(t, store, manifest.Rows[0].ItemRootID)
	group := "drain:" + drain.ID + ":impl"
	assertFormulaStepMetadata(t, store, firstRoot.ID, "gc.continuation_group", group)
	assertFormulaStepMetadata(t, store, firstRoot.ID, "gc.session_affinity", "require")
	firstMember := mustGetBead(t, store, manifest.Rows[0].MemberID)
	if got := firstMember.Metadata["gc.exclusive_drain_reservation"]; got != drain.ID {
		t.Fatalf("first member reservation = %q, want %s", got, drain.ID)
	}

	if err := updateMetadataAndClose(store, firstRoot.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("close first root pass: %v", err)
	}
	result, err = ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(shared second): %v", err)
	}
	if result.Action != "drain-shared-advanced" || result.Created != 2 {
		t.Fatalf("result = %+v, want second shared unit/root", result)
	}
	manifest = mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	if manifest.Rows[1].ItemRootID == "" {
		t.Fatalf("second row = %+v, want materialized", manifest.Rows[1])
	}
	secondRootID := manifest.Rows[1].ItemRootID
	if err := updateMetadataAndClose(store, secondRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("close second root pass: %v", err)
	}
	result, err = ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(shared complete): %v", err)
	}
	if result.Action != "drain-succeeded" {
		t.Fatalf("Action = %q, want drain-succeeded", result.Action)
	}
	manifest = mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	for _, row := range manifest.Rows {
		member := mustGetBead(t, store, row.MemberID)
		if got := strings.TrimSpace(member.Metadata["gc.exclusive_drain_reservation"]); got != "" {
			t.Fatalf("member %s reservation = %q, want released", row.MemberID, got)
		}
	}
}

func TestProcessDrainSharedSkipsRemainingAfterFailure(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadataBatch(drain.ID, map[string]string{
		"gc.drain_context":          "shared",
		"gc.drain_on_item_failure":  "skip_remaining",
		"gc.drain_item_single_lane": "true",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(shared drain): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(shared first): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	if err := updateMetadataAndClose(store, manifest.Rows[0].ItemRootID, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_reason": "unit_failed",
	}); err != nil {
		t.Fatalf("close first root fail: %v", err)
	}

	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(shared failure): %v", err)
	}
	if result.Action != "drain-failed" {
		t.Fatalf("Action = %q, want drain-failed", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("drain = %+v, want closed fail", drain)
	}
	manifest = mustDrainManifest(t, drain)
	if manifest.Rows[0].Status != "failed" {
		t.Fatalf("first row status = %q, want failed", manifest.Rows[0].Status)
	}
	if manifest.Rows[1].Status != "skipped" || manifest.Rows[1].ItemRootID != "" {
		t.Fatalf("second row = %+v, want skipped without item root", manifest.Rows[1])
	}
}

func TestProcessDrainSharedContinuesAfterFailure(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadataBatch(drain.ID, map[string]string{
		"gc.drain_context":          "shared",
		"gc.drain_on_item_failure":  "continue",
		"gc.drain_item_single_lane": "true",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(shared drain): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(shared first): %v", err)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	if err := updateMetadataAndClose(store, manifest.Rows[0].ItemRootID, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_reason": "unit_failed",
	}); err != nil {
		t.Fatalf("close first root fail: %v", err)
	}

	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(shared continue): %v", err)
	}
	if result.Action != "drain-shared-advanced" {
		t.Fatalf("Action = %q, want drain-shared-advanced", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status == "closed" {
		t.Fatalf("drain closed after first failed item despite continue policy")
	}
	manifest = mustDrainManifest(t, drain)
	if manifest.Rows[0].Status != "failed" {
		t.Fatalf("first row status = %q, want failed", manifest.Rows[0].Status)
	}
	if manifest.Rows[1].ItemRootID == "" || manifest.Rows[1].Status != "wired" {
		t.Fatalf("second row = %+v, want materialized after first failure", manifest.Rows[1])
	}
	if err := updateMetadataAndClose(store, manifest.Rows[1].ItemRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("close second root pass: %v", err)
	}
	result, err = ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(shared complete after continue): %v", err)
	}
	if result.Action != "drain-failed" {
		t.Fatalf("Action = %q, want drain-failed", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("drain = %+v, want closed fail", drain)
	}
}

func TestCloseOpenDrainItemRootsPreservesSucceededRows(t *testing.T) {
	store := beads.NewMemStore()
	passedRoot, err := store.Create(beads.Bead{Title: "passed root", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := updateMetadataAndClose(store, passedRoot.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("close passed root: %v", err)
	}
	openRoot, err := store.Create(beads.Bead{Title: "open root", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	openChild, err := store.Create(beads.Bead{
		Title:    "open child",
		Type:     "task",
		ParentID: openRoot.ID,
		Metadata: map[string]string{
			"gc.root_bead_id": openRoot.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := drainManifest{Rows: []drainManifestRow{
		{Index: 0, MemberID: "member-1", ItemRootID: passedRoot.ID, Status: "succeeded", OutcomeKind: "pass"},
		{Index: 1, MemberID: "member-2", ItemRootID: openRoot.ID, Status: "wired"},
	}}

	if err := closeOpenDrainItemRoots(store, &manifest, "exclusive_reservation_conflict"); err != nil {
		t.Fatalf("closeOpenDrainItemRoots: %v", err)
	}
	if got := manifest.Rows[0].Status; got != "succeeded" {
		t.Fatalf("first row status = %q, want succeeded", got)
	}
	if got := manifest.Rows[0].OutcomeKind; got != "pass" {
		t.Fatalf("first row outcome = %q, want pass", got)
	}
	if got := manifest.Rows[0].Failure; got != "" {
		t.Fatalf("first row failure = %q, want empty", got)
	}
	if got := manifest.Rows[1].Status; got != "failed" {
		t.Fatalf("second row status = %q, want failed", got)
	}
	openRoot = mustGetBead(t, store, openRoot.ID)
	if openRoot.Status != "closed" || openRoot.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("open root = %+v, want closed fail", openRoot)
	}
	openChild = mustGetBead(t, store, openChild.ID)
	if openChild.Status != "closed" || openChild.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("open child = %+v, want closed skipped with workflow subtree", openChild)
	}
}

func TestProcessDrainExclusiveReservationConflictFailsClosed(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_member_access", "exclusive"); err != nil {
		t.Fatalf("SetMetadata(exclusive): %v", err)
	}
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	members, err := convoycore.Members(store, root.Metadata["gc.input_convoy_id"], false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if err := store.SetMetadata(members[1].ID, "gc.exclusive_drain_reservation", "other-drain"); err != nil {
		t.Fatalf("SetMetadata(reservation): %v", err)
	}

	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(exclusive conflict): %v", err)
	}
	if result.Action != "drain-reservation-failed" {
		t.Fatalf("Action = %q, want drain-reservation-failed", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.failure_reason"] != "exclusive_reservation_conflict" {
		t.Fatalf("drain = %+v, want closed reservation conflict", drain)
	}
	firstMember := mustGetBead(t, store, members[0].ID)
	if got := strings.TrimSpace(firstMember.Metadata["gc.exclusive_drain_reservation"]); got != "" {
		t.Fatalf("first member reservation = %q, want released after partial conflict", got)
	}
	manifest := mustDrainManifest(t, drain)
	for _, row := range manifest.Rows {
		if row.ItemRootID == "" {
			continue
		}
		root := mustGetBead(t, store, row.ItemRootID)
		if root.Status != "closed" {
			t.Fatalf("partial item root %s status = %q, want closed", root.ID, root.Status)
		}
	}
	secondMember := mustGetBead(t, store, members[1].ID)
	if got := secondMember.Metadata["gc.exclusive_drain_reservation"]; got != "other-drain" {
		t.Fatalf("second member reservation = %q, want other-drain", got)
	}
}

func TestProcessDrainSeparateCompletesAfterItemRootsClose(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(drain before item close) error = %v, want pending", err)
	}
	for _, row := range manifest.Rows {
		if err := updateMetadataAndClose(store, row.ItemRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
			t.Fatalf("close item root %s: %v", row.ItemRootID, err)
		}
	}
	drain = mustGetBead(t, store, drain.ID)
	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain complete): %v", err)
	}
	if result.Action != "drain-succeeded" {
		t.Fatalf("Action = %q, want drain-succeeded", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("drain = %+v, want closed pass", drain)
	}
}

func TestProcessDrainSeparateReleasesExclusiveReservationsOnCompletion(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_member_access", "exclusive"); err != nil {
		t.Fatalf("SetMetadata(exclusive): %v", err)
	}

	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	for _, row := range manifest.Rows {
		member := mustGetBead(t, store, row.MemberID)
		if got := member.Metadata["gc.exclusive_drain_reservation"]; got != drain.ID {
			t.Fatalf("member %s reservation = %q, want %s", row.MemberID, got, drain.ID)
		}
		if err := updateMetadataAndClose(store, row.ItemRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
			t.Fatalf("close item root %s: %v", row.ItemRootID, err)
		}
	}
	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain complete): %v", err)
	}
	if result.Action != "drain-succeeded" {
		t.Fatalf("Action = %q, want drain-succeeded", result.Action)
	}
	manifest = mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	for _, row := range manifest.Rows {
		member := mustGetBead(t, store, row.MemberID)
		if got := strings.TrimSpace(member.Metadata["gc.exclusive_drain_reservation"]); got != "" {
			t.Fatalf("member %s reservation = %q, want released", row.MemberID, got)
		}
	}
}

func TestProcessDrainSeparateFailsOnNonPassItemOutcome(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	if err := updateMetadataAndClose(store, manifest.Rows[0].ItemRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("close item root pass: %v", err)
	}
	if err := updateMetadataAndClose(store, manifest.Rows[1].ItemRootID, map[string]string{"gc.outcome": "skipped"}); err != nil {
		t.Fatalf("close item root skipped: %v", err)
	}

	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain complete): %v", err)
	}
	if result.Action != "drain-failed" {
		t.Fatalf("Action = %q, want drain-failed", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("drain = status %q outcome %q, want closed/fail", drain.Status, drain.Metadata["gc.outcome"])
	}
	manifest = mustDrainManifest(t, drain)
	if got := manifest.Rows[1].Status; got != "failed" {
		t.Fatalf("skipped row status = %q, want failed", got)
	}
}

func TestProcessDrainExpandingReusesPersistedManifest(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	if err := store.SetMetadata(drain.ID, "gc.drain_state", "expanding"); err != nil {
		t.Fatalf("rewind drain state: %v", err)
	}
	extra, err := store.Create(beads.Bead{Title: "late member", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	if err := convoycore.TrackItem(store, root.Metadata["gc.input_convoy_id"], extra.ID); err != nil {
		t.Fatalf("track late member: %v", err)
	}

	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain replay): %v", err)
	}
	replayed := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	if len(replayed.Rows) != len(manifest.Rows) {
		t.Fatalf("manifest rows after replay = %d, want original %d", len(replayed.Rows), len(manifest.Rows))
	}
	for i := range manifest.Rows {
		if replayed.Rows[i].MemberID != manifest.Rows[i].MemberID {
			t.Fatalf("row %d member = %q, want snapshot member %q", i, replayed.Rows[i].MemberID, manifest.Rows[i].MemberID)
		}
	}
}

func TestProcessDrainReplayUsesManifestWhenMemberWasDeletedBeforeUnitCreation(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_member_access", "exclusive"); err != nil {
		t.Fatalf("SetMetadata(exclusive): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	parentConvoyID := root.Metadata["gc.input_convoy_id"]
	members, err := convoycore.Members(store, parentConvoyID, false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) == 0 {
		t.Fatal("seed members = 0, want at least one")
	}
	manifest := buildDrainManifest(drain, parentConvoyID, "drain-item", members)
	missingID := manifest.Rows[0].MemberID
	if err := store.Delete(missingID); err != nil {
		t.Fatalf("Delete(%s): %v", missingID, err)
	}
	if err := persistDrainManifest(store, drain.ID, manifest, map[string]string{"gc.drain_state": "expanding"}); err != nil {
		t.Fatalf("persist manifest: %v", err)
	}

	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain replay): %v", err)
	}
	replayed := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	if replayed.Rows[0].UnitConvoyID == "" || replayed.Rows[0].ItemRootID == "" {
		t.Fatalf("row = %+v, want unit/root despite missing member", replayed.Rows[0])
	}
	unit := mustGetBead(t, store, replayed.Rows[0].UnitConvoyID)
	if got := unit.Metadata["gc.drain_member_unresolved"]; got != "true" {
		t.Fatalf("unit metadata = %#v, want unresolved marker", unit.Metadata)
	}
	unitMembers, err := convoycore.Members(store, replayed.Rows[0].UnitConvoyID, true)
	if err != nil {
		t.Fatalf("Members(unit): %v", err)
	}
	if len(unitMembers) != 0 {
		t.Fatalf("unit members = %+v, want no invalid track to missing member %s", unitMembers, missingID)
	}
	deps, err := store.DepList(unit.ID, "down")
	if err != nil {
		t.Fatalf("DepList(unit): %v", err)
	}
	for _, dep := range deps {
		if dep.Type == convoycore.TrackingDepType && dep.DependsOnID == missingID {
			t.Fatalf("unit deps = %+v, want no track dependency to missing member %s", deps, missingID)
		}
	}
}

func TestProcessDrainFailsClosedWhenInitialMembershipHasUnresolvedMember(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	parentConvoyID := root.Metadata["gc.input_convoy_id"]
	members, err := convoycore.Members(store, parentConvoyID, false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) == 0 {
		t.Fatal("seed members = 0, want at least one")
	}
	missingID := members[0].ID
	if err := store.Delete(missingID); err != nil {
		t.Fatalf("Delete(%s): %v", missingID, err)
	}

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain unresolved): %v", err)
	}
	if result.Action != "drain-unresolved-member" {
		t.Fatalf("Action = %q, want drain-unresolved-member", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("drain = status %q outcome %q, want closed/fail", drain.Status, drain.Metadata["gc.outcome"])
	}
	if got := drain.Metadata["gc.drain_state"]; got != "failed" {
		t.Fatalf("gc.drain_state = %q, want failed", got)
	}
	if got := drain.Metadata["gc.failure_reason"]; got != "unresolved_member" {
		t.Fatalf("gc.failure_reason = %q, want unresolved_member", got)
	}
	if got := drain.Metadata["gc.failure_subject"]; got != missingID {
		t.Fatalf("gc.failure_subject = %q, want %s", got, missingID)
	}
}

func TestProcessDrainReplayReusesClosedItemRootByKey(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	closedRootID := manifest.Rows[0].ItemRootID
	if err := updateMetadataAndClose(store, closedRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("close item root: %v", err)
	}
	manifest.Rows[0].ItemRootID = ""
	manifest.Rows[0].Status = "unit-created"
	if err := persistDrainManifest(store, drain.ID, manifest, map[string]string{"gc.drain_state": "expanding"}); err != nil {
		t.Fatalf("persist rewound manifest: %v", err)
	}

	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain replay): %v", err)
	}
	replayed := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	if got := replayed.Rows[0].ItemRootID; got != closedRootID {
		t.Fatalf("replayed item root = %q, want closed root %q", got, closedRootID)
	}
	roots, err := store.ListByMetadata(map[string]string{"gc.item_root_key": replayed.Rows[0].ItemRootKey}, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByMetadata(item root key): %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("roots for item key = %d, want 1", len(roots))
	}
}

func TestProcessDrainReplayClosesFailedItemRootBeforeRecreate(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	failedRootID := manifest.Rows[0].ItemRootID
	if err := store.SetMetadata(failedRootID, "molecule_failed", "true"); err != nil {
		t.Fatalf("mark failed item root: %v", err)
	}
	manifest.Rows[0].ItemRootID = ""
	manifest.Rows[0].Status = "unit-created"
	if err := persistDrainManifest(store, drain.ID, manifest, map[string]string{"gc.drain_state": "expanding"}); err != nil {
		t.Fatalf("persist rewound manifest: %v", err)
	}

	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain replay): %v", err)
	}
	replayed := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	if got := replayed.Rows[0].ItemRootID; got == "" || got == failedRootID {
		t.Fatalf("replayed item root = %q, want replacement distinct from failed %q", got, failedRootID)
	}
	failedRoot := mustGetBead(t, store, failedRootID)
	if failedRoot.Status != "closed" {
		t.Fatalf("failed root status = %q, want closed", failedRoot.Status)
	}
}

func TestProcessDrainReplayConvoyOrderedManifestBlocksOnItemRootNotSourceMember(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain, blocker, dependent := seedReverseOrderedDrainWorkflow(t)
	mustDepAdd(t, store, dependent.ID, blocker.ID, "blocks")
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	parentConvoyID := root.Metadata["gc.input_convoy_id"]
	// Simulate a manifest persisted by a pre-ordering build: rows in convoy
	// order, with the dependent member materializing before its blocker.
	manifest := buildDrainManifest(drain, parentConvoyID, "drain-item",
		[]beads.Bead{mustGetBead(t, store, dependent.ID), mustGetBead(t, store, blocker.ID)})
	if len(manifest.Rows) != 2 || manifest.Rows[0].MemberID != dependent.ID || manifest.Rows[1].MemberID != blocker.ID {
		t.Fatalf("manifest rows = %+v, want convoy order [dependent %s, blocker %s]", manifest.Rows, dependent.ID, blocker.ID)
	}
	if err := persistDrainManifest(store, drain.ID, manifest, map[string]string{"gc.drain_state": "expanding"}); err != nil {
		t.Fatalf("persist manifest: %v", err)
	}

	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain replay): %v", err)
	}
	if result.Action != "drain-expanded" {
		t.Fatalf("result = %+v, want drain-expanded", result)
	}

	replayed := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	rowByMember := make(map[string]drainManifestRow, len(replayed.Rows))
	for _, row := range replayed.Rows {
		rowByMember[row.MemberID] = row
	}
	dependentRow := rowByMember[dependent.ID]
	blockerRow := rowByMember[blocker.ID]
	if dependentRow.ItemRootID == "" || blockerRow.ItemRootID == "" {
		t.Fatalf("missing item roots: dependent=%+v blocker=%+v", dependentRow, blockerRow)
	}

	assertHasBlockingDep(t, store, dependentRow.ItemRootID, blockerRow.ItemRootID)
	dependentWork := mustFindDrainItemWorkStep(t, store, dependentRow.ItemRootID)
	assertHasBlockingDep(t, store, dependentWork.ID, blockerRow.ItemRootID)
	assertNoBlockingDep(t, store, dependentRow.ItemRootID, blocker.ID)
	assertNoBlockingDep(t, store, dependentWork.ID, blocker.ID)

	blockerWork := mustFindDrainItemWorkStep(t, store, blockerRow.ItemRootID)
	if !mustReadyContains(t, store, blockerWork.ID) {
		t.Fatalf("blocker item work step %s should be ready", blockerWork.ID)
	}
	if mustReadyContains(t, store, dependentWork.ID) {
		t.Fatalf("dependent item work step %s should not be ready while blocker root %s is open", dependentWork.ID, blockerRow.ItemRootID)
	}
}

func TestProcessDrainCompleteRepairsEmbeddedSourceMemberDeps(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	parentRoot := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	members, err := convoycore.Members(store, parentRoot.Metadata["gc.input_convoy_id"], false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	firstMember := members[0]
	secondMember := members[1]
	mustDepAdd(t, store, secondMember.ID, firstMember.ID, "blocks")

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	rowByMember := make(map[string]drainManifestRow, len(manifest.Rows))
	for _, row := range manifest.Rows {
		rowByMember[row.MemberID] = row
	}
	firstRow := rowByMember[firstMember.ID]
	secondRow := rowByMember[secondMember.ID]
	secondWork := mustFindDrainItemWorkStep(t, store, secondRow.ItemRootID)
	// Simulate edges embedded by a build that fell back to source members on
	// a convoy-ordered manifest.
	mustDepAdd(t, store, secondRow.ItemRootID, firstMember.ID, "blocks")
	mustDepAdd(t, store, secondWork.ID, firstMember.ID, "blocks")

	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil && !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(drain complete): %v", err)
	}

	assertNoBlockingDep(t, store, secondRow.ItemRootID, firstMember.ID)
	assertNoBlockingDep(t, store, secondWork.ID, firstMember.ID)
	assertHasBlockingDep(t, store, secondRow.ItemRootID, firstRow.ItemRootID)
	assertHasBlockingDep(t, store, secondWork.ID, firstRow.ItemRootID)
}

func TestProcessDrainSharedRepairsEmbeddedSourceMemberDeps(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain, blocker, dependent := seedReverseOrderedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_context", "shared"); err != nil {
		t.Fatalf("SetMetadata(shared): %v", err)
	}
	mustDepAdd(t, store, dependent.ID, blocker.ID, "blocks")
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	parentConvoyID := root.Metadata["gc.input_convoy_id"]
	// Simulate a manifest persisted by a pre-ordering build: rows in convoy
	// order, with the dependent member materializing before its blocker.
	manifest := buildDrainManifest(mustGetBead(t, store, drain.ID), parentConvoyID, "drain-item",
		[]beads.Bead{mustGetBead(t, store, dependent.ID), mustGetBead(t, store, blocker.ID)})
	if manifest.Context != "shared" {
		t.Fatalf("manifest context = %q, want shared", manifest.Context)
	}
	if len(manifest.Rows) != 2 || manifest.Rows[0].MemberID != dependent.ID || manifest.Rows[1].MemberID != blocker.ID {
		t.Fatalf("manifest rows = %+v, want convoy order [dependent %s, blocker %s]", manifest.Rows, dependent.ID, blocker.ID)
	}
	if err := persistDrainManifest(store, drain.ID, manifest, map[string]string{"gc.drain_state": "expanding"}); err != nil {
		t.Fatalf("persist manifest: %v", err)
	}

	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil && !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(shared advance): %v", err)
	}
	replayed := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	dependentRow := replayed.Rows[0]
	if dependentRow.MemberID != dependent.ID || dependentRow.ItemRootID == "" {
		t.Fatalf("row 0 = %+v, want materialized dependent %s", dependentRow, dependent.ID)
	}
	dependentWork := mustFindDrainItemWorkStep(t, store, dependentRow.ItemRootID)
	// Simulate the stall left by a build that embedded the source member: the
	// blocker's item root cannot exist until this row's root closes.
	mustDepAdd(t, store, dependentRow.ItemRootID, blocker.ID, "blocks")
	mustDepAdd(t, store, dependentWork.ID, blocker.ID, "blocks")
	if mustReadyContains(t, store, dependentWork.ID) {
		t.Fatalf("dependent item work step %s should be blocked before repair", dependentWork.ID)
	}

	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil && !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(shared repair): %v", err)
	}

	assertNoBlockingDep(t, store, dependentRow.ItemRootID, blocker.ID)
	assertNoBlockingDep(t, store, dependentWork.ID, blocker.ID)
	if !mustReadyContains(t, store, dependentWork.ID) {
		t.Fatalf("dependent item work step %s should be ready after repair", dependentWork.ID)
	}
}

func TestProcessDrainSharedMaterializesInDependencyOrderAndBlocksOnItemRoot(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain, blocker, dependent := seedReverseOrderedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_context", "shared"); err != nil {
		t.Fatalf("SetMetadata(shared): %v", err)
	}
	mustDepAdd(t, store, dependent.ID, blocker.ID, "blocks")

	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(shared first): %v", err)
	}
	if result.Action != "drain-shared-advanced" {
		t.Fatalf("Action = %q, want drain-shared-advanced", result.Action)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	if len(manifest.Rows) != 2 || manifest.Rows[0].MemberID != blocker.ID || manifest.Rows[1].MemberID != dependent.ID {
		t.Fatalf("manifest rows = %+v, want dependency order [blocker %s, dependent %s]", manifest.Rows, blocker.ID, dependent.ID)
	}
	blockerRow := manifest.Rows[0]
	if blockerRow.ItemRootID == "" || manifest.Rows[1].ItemRootID != "" {
		t.Fatalf("rows = %+v, want only blocker item root materialized", manifest.Rows)
	}

	if err := updateMetadataAndClose(store, blockerRow.ItemRootID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("close blocker root pass: %v", err)
	}
	if _, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil && !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(shared second): %v", err)
	}
	manifest = mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	dependentRow := manifest.Rows[1]
	if dependentRow.MemberID != dependent.ID || dependentRow.ItemRootID == "" {
		t.Fatalf("row 1 = %+v, want materialized dependent %s", dependentRow, dependent.ID)
	}

	dependentWork := mustFindDrainItemWorkStep(t, store, dependentRow.ItemRootID)
	assertHasBlockingDep(t, store, dependentRow.ItemRootID, blockerRow.ItemRootID)
	assertHasBlockingDep(t, store, dependentWork.ID, blockerRow.ItemRootID)
	assertNoBlockingDep(t, store, dependentRow.ItemRootID, blocker.ID)
	assertNoBlockingDep(t, store, dependentWork.ID, blocker.ID)
	if !mustReadyContains(t, store, dependentWork.ID) {
		t.Fatalf("dependent item work step %s should be ready once blocker root %s closed", dependentWork.ID, blockerRow.ItemRootID)
	}
}

func TestProcessDrainPassesParentRuntimeVarsToItemFormula(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormulaWithExtra(t, dir)
	store, drain := seedDrainWorkflow(t)
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	if err := store.SetMetadata(root.ID, graphv2.RuntimeVarsMetadataKey, graphv2.RuntimeVarsMetadata(map[string]string{"extra": "provided"})); err != nil {
		t.Fatalf("SetMetadata(runtime vars): %v", err)
	}

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	itemRoot := mustGetBead(t, store, manifest.Rows[0].ItemRootID)
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List beads: %v", err)
	}
	foundSubstitutedItem := false
	for _, bead := range all {
		if strings.Contains(bead.Title, "provided") {
			foundSubstitutedItem = true
			break
		}
	}
	if !foundSubstitutedItem {
		t.Fatalf("beads = %+v, want an item bead with parent runtime var substituted", all)
	}
	if raw := itemRoot.Metadata[graphv2.RuntimeVarsMetadataKey]; !strings.Contains(raw, "provided") {
		t.Fatalf("item root runtime vars metadata = %q, want inherited var", raw)
	}
}

func TestProcessDrainAppliesItemFormulaDefaultsToRootMetadata(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormulaWithDefault(t, dir)
	store, drain := seedDrainWorkflow(t)

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	row := manifest.Rows[0]
	itemRoot := mustGetBead(t, store, row.ItemRootID)
	runtimeVars, err := graphv2.ParseRuntimeVarsMetadata(itemRoot.Metadata[graphv2.RuntimeVarsMetadataKey])
	if err != nil {
		t.Fatalf("ParseRuntimeVarsMetadata: %v", err)
	}
	if got := runtimeVars["mode"]; got != "defaulted" {
		t.Fatalf("runtime vars = %#v, want item formula default mode", runtimeVars)
	}
	unit := mustGetBead(t, store, row.UnitConvoyID)
	wantKey := graphv2.RootKey(unit.ID, "drain-item", map[string]string{
		graphv2.ConvoyIDVar: unit.ID,
		"mode":              "defaulted",
	}, "drain", drain.ID+":"+row.MemberID)
	if got := itemRoot.Metadata["gc.graphv2_root_key"]; got != wantKey {
		t.Fatalf("gc.graphv2_root_key = %q, want %q", got, wantKey)
	}
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List beads: %v", err)
	}
	foundDefaultedItem := false
	for _, bead := range all {
		if strings.Contains(bead.Title, "defaulted") {
			foundDefaultedItem = true
			break
		}
	}
	if !foundDefaultedItem {
		t.Fatalf("beads = %+v, want item formula default substituted", all)
	}
}

func TestEnsureDrainUnitConvoyRepairsExistingTrack(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{Title: "drain", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Create(beads.Bead{Title: "parent", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.Create(beads.Bead{Title: "member", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	row := drainManifestRow{
		Index:    0,
		MemberID: member.ID,
		UnitKey:  "drain-unit:test:0:" + member.ID,
	}
	existing, err := store.Create(beads.Bead{
		Title: "existing unit",
		Type:  "convoy",
		Metadata: map[string]string{
			"gc.drain_unit_key": row.UnitKey,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	unit, created, err := ensureDrainUnitConvoy(store, control, parent.ID, 1, row, member)
	if err != nil {
		t.Fatalf("ensureDrainUnitConvoy: %v", err)
	}
	if created || unit.ID != existing.ID {
		t.Fatalf("unit=%+v created=%v, want existing %s without create", unit, created, existing.ID)
	}
	members, err := convoycore.Members(store, existing.ID, true)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 || members[0].ID != member.ID {
		t.Fatalf("members = %+v, want repaired member %s", members, member.ID)
	}
}

type recordingDrainUnitStore struct {
	beads.Store
	listMetadataOpts [][]beads.QueryOpt
}

func (s *recordingDrainUnitStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.listMetadataOpts = append(s.listMetadataOpts, opts)
	return s.Store.ListByMetadata(filters, limit, opts...)
}

type createObservingStore struct {
	beads.Store
	onCreate func(beads.Bead)
}

func (s *createObservingStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err == nil && s.onCreate != nil {
		s.onCreate(created)
	}
	return created, err
}

func beadNeedsContains(needs []string, id string) bool {
	for _, need := range needs {
		if need == id || need == "blocks:"+id {
			return true
		}
	}
	return false
}

func TestEnsureDrainUnitConvoyLooksAcrossBothTiers(t *testing.T) {
	store := &recordingDrainUnitStore{Store: beads.NewMemStore()}
	control, err := store.Create(beads.Bead{Title: "drain", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Create(beads.Bead{Title: "parent", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.Create(beads.Bead{Title: "member", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	row := drainManifestRow{
		Index:    0,
		MemberID: member.ID,
		UnitKey:  "drain-unit:test:0:" + member.ID,
	}

	if _, _, err := ensureDrainUnitConvoy(store, control, parent.ID, 1, row, member); err != nil {
		t.Fatalf("ensureDrainUnitConvoy: %v", err)
	}
	if len(store.listMetadataOpts) == 0 {
		t.Fatal("ListByMetadata was not called")
	}
	if got := beads.TierModeFromOpts(store.listMetadataOpts[0]); got != beads.TierBoth {
		t.Fatalf("ListByMetadata tier = %v, want TierBoth", got)
	}
}

func TestProcessDrainExclusiveFailsClosedForInvalidItemFormula(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeLegacyDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_member_access", "exclusive"); err != nil {
		t.Fatalf("SetMetadata(exclusive): %v", err)
	}
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	members, err := convoycore.Members(store, root.Metadata["gc.input_convoy_id"], false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}

	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(non-graph item formula): %v", err)
	}
	if result.Action != "drain-failed" {
		t.Fatalf("Action = %q, want drain-failed", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("drain = status %q outcome %q, want closed/fail", drain.Status, drain.Metadata["gc.outcome"])
	}
	if got := drain.Metadata["gc.failure_reason"]; got != "invalid_drain_item_formula" {
		t.Fatalf("gc.failure_reason = %q, want invalid_drain_item_formula", got)
	}
	manifest := mustDrainManifest(t, drain)
	for _, row := range manifest.Rows {
		if row.Status != "failed" || row.Failure != "invalid_drain_item_formula" {
			t.Fatalf("row = %+v, want failed invalid item formula", row)
		}
	}
	for _, member := range members {
		after := mustGetBead(t, store, member.ID)
		if got := strings.TrimSpace(after.Metadata["gc.exclusive_drain_reservation"]); got != "" {
			t.Fatalf("member %s reservation = %q, want released", member.ID, got)
		}
	}
}

func TestIsSharedDrainExecutableStepExcludesControlAndTopologyKinds(t *testing.T) {
	t.Parallel()
	excluded := append(append([]string{}, beadmeta.ControlKinds...), beadmeta.WorkflowTopologyKinds...)
	for _, kind := range excluded {
		step := &formula.RecipeStep{Metadata: map[string]string{beadmeta.KindMetadataKey: kind}}
		if isSharedDrainExecutableStep(step) {
			t.Errorf("isSharedDrainExecutableStep(kind=%q) = true, want false", kind)
		}
		padded := &formula.RecipeStep{Metadata: map[string]string{beadmeta.KindMetadataKey: " " + kind + " "}}
		if isSharedDrainExecutableStep(padded) {
			t.Errorf("isSharedDrainExecutableStep(kind=%q padded) = true, want false", kind)
		}
	}
	if isSharedDrainExecutableStep(nil) {
		t.Error("isSharedDrainExecutableStep(nil) = true, want false")
	}
	for _, step := range []*formula.RecipeStep{
		{},
		{Metadata: map[string]string{}},
		{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindTask}},
	} {
		if !isSharedDrainExecutableStep(step) {
			t.Errorf("isSharedDrainExecutableStep(%+v) = false, want true", step)
		}
	}
}

func TestStampDrainItemRecipeSharedSkipsControlStep(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	content := `
formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"

[steps.on_complete]
for_each = "output.voters"
bond = "mol-voter"
`
	if err := os.WriteFile(filepath.Join(dir, "drain-item.formula.toml"), []byte(strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	recipe, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), "drain-item", []string{dir}, map[string]string{graphv2.ConvoyIDVar: "unit-1"})
	if err != nil {
		t.Fatalf("CompileWithoutRuntimeVarValidation: %v", err)
	}
	control := beads.Bead{ID: "drain-1", Metadata: map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindDrain,
		beadmeta.DrainContextMetadataKey: beadmeta.DrainContextShared,
	}}
	unit := beads.Bead{ID: "unit-1"}
	member := beads.Bead{ID: "member-1"}
	row := &drainManifestRow{Index: 0, ItemRootKey: "item-key-1"}
	stampDrainItemRecipe(recipe, control, unit, member, 1, row, "drain-item", nil)

	stepsByKind := make(map[string]*formula.RecipeStep)
	var workStep *formula.RecipeStep
	for i := range recipe.Steps {
		step := &recipe.Steps[i]
		if kind := step.Metadata[beadmeta.KindMetadataKey]; kind != "" {
			stepsByKind[kind] = step
		}
		if strings.HasSuffix(step.ID, ".work") || step.ID == "work" {
			workStep = step
		}
	}
	for _, kind := range []string{beadmeta.KindFanout} {
		step, ok := stepsByKind[kind]
		if !ok {
			t.Fatalf("compiled recipe has no %s step; steps=%+v", kind, recipe.Steps)
		}
		if got := step.Metadata[beadmeta.ContinuationGroupMetadataKey]; got != "" {
			t.Errorf("%s step gc.continuation_group = %q, want unset", kind, got)
		}
		if got := step.Metadata[beadmeta.SessionAffinityMetadataKey]; got != "" {
			t.Errorf("%s step gc.session_affinity = %q, want unset", kind, got)
		}
	}
	if workStep == nil {
		t.Fatalf("compiled recipe has no work step; steps=%+v", recipe.Steps)
	}
	if got, want := workStep.Metadata[beadmeta.ContinuationGroupMetadataKey], "drain:drain-1"; got != want {
		t.Errorf("work step gc.continuation_group = %q, want %q", got, want)
	}
	if got, want := workStep.Metadata[beadmeta.SessionAffinityMetadataKey], "require"; got != want {
		t.Errorf("work step gc.session_affinity = %q, want %q", got, want)
	}
}

func seedDrainWorkflow(t *testing.T) (*beads.MemStore, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	parent, err := store.Create(beads.Bead{Title: "parent", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(beads.Bead{Title: "first", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(beads.Bead{Title: "second", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []beads.Bead{first, second} {
		if err := convoycore.TrackItem(store, parent.ID, member.ID); err != nil {
			t.Fatal(err)
		}
	}
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.input_convoy_id":  parent.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain, err := store.Create(beads.Bead{
		Title: "drain",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "drain",
			"gc.root_bead_id":        root.ID,
			"gc.drain_context":       "separate",
			"gc.drain_formula":       "drain-item",
			"gc.drain_member_access": "read",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, drain
}

func seedReverseOrderedDrainWorkflow(t *testing.T) (*beads.MemStore, beads.Bead, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	parent, err := store.Create(beads.Bead{Title: "parent", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := store.Create(beads.Bead{Title: "dependent", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []beads.Bead{dependent, blocker} {
		if err := convoycore.TrackItem(store, parent.ID, member.ID); err != nil {
			t.Fatal(err)
		}
	}
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.input_convoy_id":  parent.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain, err := store.Create(beads.Bead{
		Title: "drain",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "drain",
			"gc.root_bead_id":        root.ID,
			"gc.drain_context":       "separate",
			"gc.drain_formula":       "drain-item",
			"gc.drain_member_access": "read",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, drain, blocker, dependent
}

func writeDrainItemFormula(t *testing.T, dir string) {
	t.Helper()
	content := `
formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"
`
	if err := os.WriteFile(filepath.Join(dir, "drain-item.formula.toml"), []byte(strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
}

func writeDrainItemFormulaWithExtra(t *testing.T, dir string) {
	t.Helper()
	content := `
formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[vars]
[vars.extra]
required = true

[[steps]]
id = "work"
title = "Work {{convoy_id}} {{extra}}"
`
	if err := os.WriteFile(filepath.Join(dir, "drain-item.formula.toml"), []byte(strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
}

func writeDrainItemFormulaWithDefault(t *testing.T, dir string) {
	t.Helper()
	content := `
formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[vars]
[vars.mode]
default = "defaulted"

[[steps]]
id = "work"
title = "Work {{convoy_id}} {{mode}}"
`
	if err := os.WriteFile(filepath.Join(dir, "drain-item.formula.toml"), []byte(strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
}

func writeLegacyDrainItemFormula(t *testing.T, dir string) {
	t.Helper()
	content := `
formula = "drain-item"
version = 1
type = "workflow"

[[steps]]
id = "work"
title = "Legacy work"
`
	if err := os.WriteFile(filepath.Join(dir, "drain-item.formula.toml"), []byte(strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
}

func mustDrainManifest(t *testing.T, bead beads.Bead) drainManifest {
	t.Helper()
	var manifest drainManifest
	if err := json.Unmarshal([]byte(bead.Metadata[drainManifestMetadataKey]), &manifest); err != nil {
		t.Fatalf("parse drain manifest: %v", err)
	}
	return manifest
}

func assertFormulaStepMetadata(t *testing.T, store beads.Store, rootID, key, want string) {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List beads: %v", err)
	}
	for _, bead := range all {
		if bead.ParentID != rootID && bead.Metadata["gc.root_bead_id"] != rootID {
			continue
		}
		if got := bead.Metadata[key]; got == want {
			return
		}
	}
	t.Fatalf("no child of %s has metadata %s=%q; beads=%+v", rootID, key, want, all)
}

// TestProcessDrainTerminalCloseReconcilesEnclosingScope pins the
// fanout/retry/ralph parity fix: when a scoped drain control reaches a
// terminal close and it was the scope's last open member, the drain must
// reconcile its enclosing scope (closing the body) instead of relying on
// another control's close-time backstop.
func TestProcessDrainTerminalCloseReconcilesEnclosingScope(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store := beads.NewMemStore()
	parent, err := store.Create(beads.Bead{Title: "empty parent", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.input_convoy_id":  parent.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := store.Create(beads.Bead{
		Title: "scope body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.iter",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain, err := store.Create(beads.Bead{
		Title: "drain",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "drain",
			"gc.root_bead_id":        root.ID,
			"gc.scope_ref":           "demo.iter",
			"gc.scope_role":          "control",
			"gc.drain_context":       "separate",
			"gc.drain_formula":       "drain-item",
			"gc.drain_member_access": "read",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(scoped empty drain): %v", err)
	}
	if result.Action != "drain-succeeded" {
		t.Fatalf("Action = %q, want drain-succeeded", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("drain = status %q outcome %q, want closed/pass", drain.Status, drain.Metadata["gc.outcome"])
	}
	body = mustGetBead(t, store, body.ID)
	if body.Status != "closed" {
		t.Fatalf("scope body status = %q, want closed (drain terminal close must reconcile the scope)", body.Status)
	}
	if got := body.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("scope body gc.outcome = %q, want pass", got)
	}
}

// TestProcessDrainFailureCloseAbortsEnclosingScope pins the fail half of the
// scope-reconcile parity: a scoped drain that fail-closes (limit exceeded)
// must abort its enclosing scope like any failed scope member — body closed
// with outcome fail — rather than leaving the scope to a backstop.
func TestProcessDrainFailureCloseAbortsEnclosingScope(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	rootID := drain.Metadata["gc.root_bead_id"]
	body, err := store.Create(beads.Bead{
		Title: "scope body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": rootID,
			"gc.step_ref":     "demo.iter",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"gc.scope_ref":       "demo.iter",
		"gc.scope_role":      "control",
		"gc.drain_max_units": "1",
	} {
		if err := store.SetMetadata(drain.ID, k, v); err != nil {
			t.Fatalf("SetMetadata(%s): %v", k, err)
		}
	}

	result, err := ProcessControl(store, mustGetBead(t, store, drain.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(scoped limit exceeded): %v", err)
	}
	if result.Action != "drain-limit-exceeded" {
		t.Fatalf("Action = %q, want drain-limit-exceeded", result.Action)
	}
	body = mustGetBead(t, store, body.ID)
	if body.Status != "closed" {
		t.Fatalf("scope body status = %q, want closed (failed scoped drain must abort the scope)", body.Status)
	}
	if got := body.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("scope body gc.outcome = %q, want fail", got)
	}
}

func mustFindDrainItemWorkStep(t *testing.T, store beads.Store, rootID string) beads.Bead {
	t.Helper()
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		t.Fatalf("listByWorkflowRoot(%s): %v", rootID, err)
	}
	for _, bead := range all {
		if bead.ID != rootID && strings.HasPrefix(bead.Title, "Work ") {
			return bead
		}
	}
	t.Fatalf("no drain item work step found under %s; beads=%+v", rootID, all)
	return beads.Bead{}
}

func assertHasBlockingDep(t *testing.T, store beads.Store, issueID, dependsOnID string) {
	t.Helper()
	deps, err := store.DepList(issueID, "down")
	if err != nil {
		t.Fatalf("DepList(%s): %v", issueID, err)
	}
	for _, dep := range deps {
		if dep.Type == "blocks" && dep.DependsOnID == dependsOnID {
			return
		}
	}
	t.Fatalf("dependencies for %s = %+v, want blocks dependency on %s", issueID, deps, dependsOnID)
}

func assertNoBlockingDep(t *testing.T, store beads.Store, issueID, dependsOnID string) {
	t.Helper()
	deps, err := store.DepList(issueID, "down")
	if err != nil {
		t.Fatalf("DepList(%s): %v", issueID, err)
	}
	for _, dep := range deps {
		if beads.IsReadyBlockingDependencyType(dep.Type) && dep.DependsOnID == dependsOnID {
			t.Fatalf("dependencies for %s = %+v, want no blocking dependency on %s", issueID, deps, dependsOnID)
		}
	}
}
