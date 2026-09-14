package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

	projection, err := drainProjectedBlockerIDs(ambient, dependent.ID, manifest, ProcessOptions{MemberStores: []beads.Store{memberStore}})
	if err != nil {
		t.Fatalf("drainProjectedBlockerIDs: %v", err)
	}
	if len(projection.BlockerIDs) != 1 || projection.BlockerIDs[0] != "root-blocker" {
		t.Fatalf("blockerIDs = %v, want [root-blocker] (member deps routed to member store, projected onto the blocker's item root)", projection.BlockerIDs)
	}
	if len(projection.Unprojectable) != 0 {
		t.Fatalf("unprojectable = %v, want none: an in-manifest blocker projects onto an item root, which is co-resident with the item workflow by construction", projection.Unprojectable)
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

func TestProcessDrainPropagatesSourceMemberWorkDirToItemWorkflow(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	members, err := convoycore.Members(store, root.Metadata["gc.input_convoy_id"], false)
	if err != nil {
		t.Fatalf("convoycore.Members: %v", err)
	}
	sourceWorkDir := filepath.Join(t.TempDir(), "source-worktree")
	if err := store.SetMetadata(members[0].ID, beadmeta.LegacyWorkDirMetadataKey, sourceWorkDir); err != nil {
		t.Fatalf("SetMetadata(source legacy work dir): %v", err)
	}
	if err := store.SetMetadata(root.ID, beadmeta.WorkDirMetadataKey, filepath.Join(t.TempDir(), "launcher")); err != nil {
		t.Fatalf("SetMetadata(launcher work dir): %v", err)
	}

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	var itemRootID string
	for _, row := range manifest.Rows {
		if row.MemberID == members[0].ID {
			itemRootID = row.ItemRootID
			break
		}
	}
	if itemRootID == "" {
		t.Fatalf("manifest rows = %+v, want item root for source member %s", manifest.Rows, members[0].ID)
	}
	itemRoot := mustGetBead(t, store, itemRootID)
	for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
		if got := itemRoot.Metadata[key]; got != sourceWorkDir {
			t.Fatalf("item root %s = %q, want source member work dir %q", key, got, sourceWorkDir)
		}
	}
	workStep := mustFindDrainItemWorkStep(t, store, itemRootID)
	for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
		if got := workStep.Metadata[key]; got != sourceWorkDir {
			t.Fatalf("item work step %s = %q, want source member work dir %q", key, got, sourceWorkDir)
		}
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

	unit, created, err := ensureDrainUnitConvoy(store, control, parent.ID, 1, row, member, ProcessOptions{})
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

	if _, _, err := ensureDrainUnitConvoy(store, control, parent.ID, 1, row, member, ProcessOptions{}); err != nil {
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
	all, err := beads.DirectMembers(store, rootID)
	if err != nil {
		t.Fatalf("beads.DirectMembers(%s): %v", rootID, err)
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

// seedSplitClassDrainWorkflow builds the drain fixture a converged split city
// actually has: the graph.v2 input convoy and its work members in the WORK
// store, the workflow root and the drain control in the graph binding.
//
// carryConvoyCopy models what a city migrated under the PREVIOUS classification
// has on disk. Synthetic convoys used to be graph class, so `gc storage migrate`
// copied the convoy ROW into the binding — but importInfraSnapshot re-adds only
// the dep edges whose BOTH endpoints are infra, so the copy carries none of its
// tracks edges to the work members. Synthetic convoys are work class now, so a
// migration run today leaves no such copy; the copies already written are why
// the shape still has to be covered. The two stores mint from disjoint id
// ranges, the way two real bead databases with different prefixes do.
func seedSplitClassDrainWorkflow(t *testing.T, carryConvoyCopy bool) (work *beads.MemStore, graph *beads.MemStore, drain beads.Bead, memberIDs []string) {
	t.Helper()
	work = beads.NewMemStore()
	parent, err := work.Create(beads.Bead{
		Title:    "input convoy",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	})
	if err != nil {
		t.Fatalf("create input convoy: %v", err)
	}
	for _, title := range []string{"first", "second"} {
		member, err := work.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatalf("create member %s: %v", title, err)
		}
		if err := convoycore.TrackItem(work, parent.ID, member.ID); err != nil {
			t.Fatalf("track member %s: %v", title, err)
		}
		memberIDs = append(memberIDs, member.ID)
	}

	var carried []beads.Bead
	if carryConvoyCopy {
		carried = append(carried, parent)
	}
	graph = beads.NewMemStoreFrom(1000, carried, nil)

	root, err := graph.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.input_convoy_id":  parent.ID,
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	drain, err = graph.Create(beads.Bead{
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
		t.Fatalf("create drain control: %v", err)
	}
	return work, graph, drain, memberIDs
}

// TestDrainReadsConvoyMembershipAcrossTheClassBoundary pins the head of the
// membership lookup, which opts.MemberStores does not reach.
//
// MemberStores resolves member BEADS. The convoy's own membership — its legacy
// children and its tracks edges — is read from one store, and reading it from
// the store the drain runs in is what loses the convoy: a store that never saw
// the convoy answers both reads empty instead of erroring, which is
// indistinguishable from a genuinely empty convoy.
//
// Both shapes are covered because the naive repair — pick the one store that
// holds the convoy bead — passes the first and fails the second. On a converged
// city the convoy bead is in BOTH stores and the binding's copy is the empty one.
func TestDrainReadsConvoyMembershipAcrossTheClassBoundary(t *testing.T) {
	for _, tc := range []struct {
		name            string
		carryConvoyCopy bool
	}{
		{name: "convoy only in the work store", carryConvoyCopy: false},
		{name: "edgeless migration copy also in the binding", carryConvoyCopy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, graph, drain, memberIDs := seedSplitClassDrainWorkflow(t, tc.carryConvoyCopy)
			root := mustGetBead(t, graph, drain.Metadata["gc.root_bead_id"])
			convoyID := root.Metadata["gc.input_convoy_id"]

			members, err := drainConvoyMembers(graph, convoyID, ProcessOptions{MemberStores: []beads.Store{work}})
			if err != nil {
				t.Fatalf("drainConvoyMembers: %v", err)
			}
			var got []string
			for _, member := range members {
				got = append(got, member.ID)
			}
			if !slices.Equal(got, memberIDs) {
				t.Fatalf("members = %v, want %v; membership read from a store that does not hold this convoy's edges returns empty instead of failing, and an empty convoy is a drain SUCCESS", got, memberIDs)
			}
		})
	}
}

// TestDrainSingleStoreMembershipIsUnchanged is the compatibility half: with no
// member stores named — every single-store caller — the membership read is the
// one convoycore.Members call it always was, against the one store.
func TestDrainSingleStoreMembershipIsUnchanged(t *testing.T) {
	store, drain := seedDrainWorkflow(t)
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	convoyID := root.Metadata["gc.input_convoy_id"]

	want, err := convoycore.Members(store, convoyID, false)
	if err != nil {
		t.Fatalf("convoycore.Members: %v", err)
	}
	got, err := drainConvoyMembers(store, convoyID, ProcessOptions{})
	if err != nil {
		t.Fatalf("drainConvoyMembers: %v", err)
	}
	if len(got) != len(want) || len(got) != 2 {
		t.Fatalf("members = %d, want the same 2 convoycore.Members returns", len(got))
	}
	for i := range got {
		if got[i].ID != want[i].ID {
			t.Fatalf("member %d = %s, want %s; the single-store read must be order-identical", i, got[i].ID, want[i].ID)
		}
	}
}

// TestProcessDrainNeverPassesAConvoyItCouldNotRead is the invariant that makes
// the failure mode unwritable rather than merely fixed.
//
// A drain over an EMPTY input convoy closes gc.outcome=pass with zero rows, and
// TestProcessDrainSeparateSucceedsForEmptyInputConvoy pins that as intended. That
// is exactly what makes a lost convoy invisible: a drain that could not read its
// membership produces byte-for-byte the same terminal state as a drain that
// correctly found nothing to do, so the whole convoy is left open and
// undispatched while the run reports green and exits 0.
//
// Whatever else a split-class drain does, it may never reach that state over a
// convoy that has members. Failing loudly is allowed; passing is not.
func TestProcessDrainNeverPassesAConvoyItCouldNotRead(t *testing.T) {
	for _, tc := range []struct {
		name            string
		carryConvoyCopy bool
	}{
		{name: "convoy only in the work store", carryConvoyCopy: false},
		{name: "edgeless migration copy also in the binding", carryConvoyCopy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			formulatest.EnableV2ForTest(t)
			dir := t.TempDir()
			writeDrainItemFormula(t, dir)
			work, graph, drain, memberIDs := seedSplitClassDrainWorkflow(t, tc.carryConvoyCopy)

			result, _ := ProcessControl(graph, drain, ProcessOptions{
				FormulaSearchPaths: []string{dir},
				MemberStores:       []beads.Store{work},
			})
			got := mustGetBead(t, graph, drain.ID)
			manifest := mustDrainManifest(t, got)

			if got.Metadata[beadmeta.OutcomeMetadataKey] == beadmeta.OutcomePass && len(manifest.Rows) == 0 {
				t.Fatalf("drain closed gc.outcome=pass over a convoy of %d members with an empty manifest (action=%q); every member is left open and the run reports green", len(memberIDs), result.Action)
			}
			if len(manifest.Rows) != len(memberIDs) {
				t.Fatalf("manifest rows = %d, want %d — one per convoy member", len(manifest.Rows), len(memberIDs))
			}
			for _, id := range memberIDs {
				if !slices.ContainsFunc(manifest.Rows, func(row drainManifestRow) bool { return row.MemberID == id }) {
					t.Fatalf("member %s is missing from the drain manifest %+v", id, manifest.Rows)
				}
			}
		})
	}
}

// TestProcessDrainSplitCityExpandsAndCompletes is the end-to-end acceptance for
// the owner ruling that a synthetic convoy is a WORK bead.
//
// It is the whole point of the ruling stated as an outcome: on a converged split
// city a drain over a non-empty input convoy must reach the same terminal state
// it reaches on a single-store city — a manifest row per member, a unit convoy
// per row that actually TRACKS its member, and an item root per row. Expansion
// alone is not the bar. Before the ruling the expansion happened and then
// ensureDrainUnitConvoy minted the unit convoy in the GRAPH store while its
// member lived in the work store, so the tracks edge convoycore refuses to write
// across a class boundary failed the whole dispatch and the control quarantined.
//
// Both migration shapes are covered because they exercise different halves: a
// convoy that only ever lived in the work store, and one whose edgeless copy
// `gc storage migrate` left in the binding is the first thing a graph-store read
// finds.
func TestProcessDrainSplitCityExpandsAndCompletes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		carryConvoyCopy bool
	}{
		{name: "convoy only in the work store", carryConvoyCopy: false},
		{name: "edgeless migration copy also in the binding", carryConvoyCopy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			formulatest.EnableV2ForTest(t)
			dir := t.TempDir()
			writeDrainItemFormula(t, dir)
			work, graph, drain, memberIDs := seedSplitClassDrainWorkflow(t, tc.carryConvoyCopy)

			result, err := ProcessControl(graph, drain, ProcessOptions{
				FormulaSearchPaths: []string{dir},
				MemberStores:       []beads.Store{work},
			})
			if err != nil {
				t.Fatalf("ProcessControl(split-city drain): %v; a control dispatch that returns a non-transient error is QUARANTINED by the caller, which is the state this ruling exists to remove", err)
			}
			if result.Action != "drain-expanded" {
				t.Fatalf("result = %+v, want drain-expanded", result)
			}
			if result.Created != 2*len(memberIDs) {
				t.Fatalf("created = %d, want %d — one unit convoy and one item root per member", result.Created, 2*len(memberIDs))
			}

			reloaded := mustGetBead(t, graph, drain.ID)
			if got := reloaded.Metadata[beadmeta.DrainStateMetadataKey]; got != beadmeta.DrainStateExpanded {
				t.Fatalf("gc.drain_state = %q, want %q", got, beadmeta.DrainStateExpanded)
			}
			manifest := mustDrainManifest(t, reloaded)
			if len(manifest.Rows) != len(memberIDs) {
				t.Fatalf("manifest rows = %d, want %d", len(manifest.Rows), len(memberIDs))
			}
			for i, row := range manifest.Rows {
				if row.UnitConvoyID == "" {
					t.Fatalf("row %d has no unit convoy", i)
				}
				if row.ItemRootID == "" {
					t.Fatalf("row %d has no item root", i)
				}
				if row.Status != "wired" {
					t.Fatalf("row %d status = %q, want wired", i, row.Status)
				}
				// The unit convoy is a synthetic convoy, so per the ruling it is a
				// work bead: it must be readable from the work store and it must
				// hold the tracks edge to its member there.
				unit, err := work.Get(row.UnitConvoyID)
				if err != nil {
					t.Fatalf("unit convoy %s is not in the work store: %v; a synthetic convoy is a work bead and its tracks edge cannot reference a member the convoy's own store cannot resolve", row.UnitConvoyID, err)
				}
				if unit.Metadata[beadmeta.SyntheticKindMetadataKey] != "drain-unit-convoy" {
					t.Fatalf("unit convoy %s metadata = %v, want gc.synthetic_kind=drain-unit-convoy", unit.ID, unit.Metadata)
				}
				tracked, err := convoycore.HasTrack(work, row.UnitConvoyID, row.MemberID)
				if err != nil {
					t.Fatalf("HasTrack(%s, %s): %v", row.UnitConvoyID, row.MemberID, err)
				}
				if !tracked {
					t.Fatalf("unit convoy %s does not track member %s; the drain item formula resolves its work from convoy_id, so an untracked unit dispatches an empty convoy", row.UnitConvoyID, row.MemberID)
				}
			}
		})
	}
}

// countingGetStore counts Get calls so a test can assert that a code path
// issued no read at all, which is what "byte-identical" means for a probe that
// must not run.
type countingGetStore struct {
	beads.Store
	gets int
}

func (s *countingGetStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

// TestDrainUnitConvoyStoreIsTheAmbientStoreOnASingleStoreCity is the
// byte-identity half of routing unit convoys to the member's owning store.
//
// Every city with no relocated graph class names no member stores, and for those
// the unit convoy's store is not a question: it is the one store there is. The
// owning-store probe must not run for them — not because the answer would be
// wrong (it would be the same store) but because it is a read the drain does not
// make today, once per unit convoy per pass, on the per-tick projection sweep.
//
// The identity assertion alone would not catch a probe, because the probe
// returns the same handle. Counting the reads is what does.
func TestDrainUnitConvoyStoreIsTheAmbientStoreOnASingleStoreCity(t *testing.T) {
	store := &countingGetStore{Store: beads.NewMemStore()}
	member, err := store.Create(beads.Bead{Title: "a member", Type: "task"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	store.gets = 0

	got, err := drainUnitConvoyStore(store, member, ProcessOptions{})
	if err != nil {
		t.Fatalf("drainUnitConvoyStore: %v", err)
	}
	if got != beads.Store(store) {
		t.Fatalf("drainUnitConvoyStore returned a different handle than the ambient store; the optional-capability assertions the drain makes are against that instance")
	}
	if store.gets != 0 {
		t.Fatalf("the single-store path issued %d owning-store probe read(s); with no member stores named there is nothing to probe and the read is pure overhead", store.gets)
	}
}

// TestDrainUnitConvoyStoreFollowsTheMemberAcrossTheClassBoundary is the other
// half: once a work-class member store is named, the unit convoy goes where the
// member is, because convoy.TrackItemIn refuses an edge whose member is owned by
// another class store.
func TestDrainUnitConvoyStoreFollowsTheMemberAcrossTheClassBoundary(t *testing.T) {
	work := beads.NewMemStore()
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	member, err := work.Create(beads.Bead{Title: "a work member", Type: "task"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	got, err := drainUnitConvoyStore(graph, member, ProcessOptions{MemberStores: []beads.Store{work}})
	if err != nil {
		t.Fatalf("drainUnitConvoyStore: %v", err)
	}
	if got != beads.Store(work) {
		t.Fatalf("drainUnitConvoyStore chose the ambient graph store for a work-resident member; the tracks edge it is about to write cannot reference an id that store cannot resolve")
	}
}

// itemWorkflowBeadIDs returns every bead id in the item workflow rooted at
// rootID, so an assertion can talk about the whole molecule rather than guessing
// which step carries the projection.
func itemWorkflowBeadIDs(t *testing.T, store beads.Store, rootID string) []string {
	t.Helper()
	workflowBeads, err := beads.DirectMembers(store, rootID)
	if err != nil {
		t.Fatalf("beads.DirectMembers(%s): %v", rootID, err)
	}
	ids := make([]string, 0, len(workflowBeads))
	for _, bead := range workflowBeads {
		ids = append(ids, bead.ID)
	}
	return ids
}

// assertNoUnresolvableBlockingDeps fails on any ready-blocking dependency in
// store whose target store cannot resolve.
//
// This is the invariant, not a proxy for it: a dependency row lives in one
// store's dep table, and both MemStore.readyLocked and sqliteReadySQL read a
// missing target's status as "" — which is not "closed", so the dependent is
// excluded from Ready forever and no action in any other store can release it.
func assertNoUnresolvableBlockingDeps(t *testing.T, store *beads.MemStore) {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, bead := range all {
		deps, err := store.DepList(bead.ID, "down")
		if err != nil {
			t.Fatalf("DepList(%s): %v", bead.ID, err)
		}
		for _, dep := range deps {
			if !beads.IsReadyBlockingDependencyType(dep.Type) {
				continue
			}
			if _, err := store.Get(dep.DependsOnID); err != nil {
				t.Fatalf("dangling %s edge %s -> %s: the store holding the edge cannot resolve the target, so the dependent never becomes ready and closing the real blocker in its own store cannot release it: %v",
					dep.Type, bead.ID, dep.DependsOnID, err)
			}
		}
	}
}

// TestProcessDrainSplitCityResumeCompletesAfterAMemberVanishes pins the resume
// half of routing unit convoys across the class boundary.
//
// A drain resumes from its persisted manifest, and expandDrain persists after
// every row, so any mid-loop exit — ErrControlPending from the spawn boundary, a
// reservation retry, a transient store error on row i>0 — leaves rows 0..i-1
// with their unit convoy ids recorded and gc.drain_state=expanding. That is a
// designed re-entry, not a crash.
//
// A member hard-deleted between passes is likewise a supported state, pinned for
// single-store cities by TestProcessDrainReplayUsesManifestWhenMemberWasDeleted-
// BeforeUnitCreation: loadDrainManifestMembers substitutes an unresolved
// placeholder and the drain continues. Deriving the unit convoy's store from the
// member breaks that on a split city, because the manifest records the convoy's
// id but not the store that answered when it was minted — so mint and reload
// disagree the moment the member's resolvability changes, and a not-found is not
// a transient controller error, so the caller QUARANTINES the drain instead of
// retrying it.
func TestProcessDrainSplitCityResumeCompletesAfterAMemberVanishes(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	work, graph, drain, memberIDs := seedSplitClassDrainWorkflow(t, false)
	opts := ProcessOptions{FormulaSearchPaths: []string{dir}, MemberStores: []beads.Store{work}}

	if _, err := ProcessControl(graph, drain, opts); err != nil {
		t.Fatalf("ProcessControl(first pass): %v", err)
	}
	expanded := mustDrainManifest(t, mustGetBead(t, graph, drain.ID))

	// The designed re-entry: rows persisted, control still expanding.
	if err := graph.SetMetadata(drain.ID, beadmeta.DrainStateMetadataKey, beadmeta.DrainStateExpanding); err != nil {
		t.Fatalf("rewind gc.drain_state: %v", err)
	}
	if err := work.Delete(memberIDs[0]); err != nil {
		t.Fatalf("Delete(%s): %v", memberIDs[0], err)
	}

	result, err := ProcessControl(graph, mustGetBead(t, graph, drain.ID), opts)
	if err != nil {
		t.Fatalf("ProcessControl(resume after a member vanished): %v; a non-transient control error is quarantined by the caller, and the drain never expands its remaining rows", err)
	}
	if result.Action != "drain-expanded" {
		t.Fatalf("result = %+v, want drain-expanded", result)
	}
	resumed := mustDrainManifest(t, mustGetBead(t, graph, drain.ID))
	if len(resumed.Rows) != len(expanded.Rows) {
		t.Fatalf("manifest rows after resume = %d, want the original %d", len(resumed.Rows), len(expanded.Rows))
	}
	for i, row := range resumed.Rows {
		if row.UnitConvoyID != expanded.Rows[i].UnitConvoyID {
			t.Fatalf("row %d unit convoy = %q, want the already-minted %q; a resume that cannot find the convoy it minted mints a duplicate", i, row.UnitConvoyID, expanded.Rows[i].UnitConvoyID)
		}
		if row.ItemRootID != expanded.Rows[i].ItemRootID {
			t.Fatalf("row %d item root = %q, want the already-created %q", i, row.ItemRootID, expanded.Rows[i].ItemRootID)
		}
		if row.Status != "wired" {
			t.Fatalf("row %d status = %q, want wired", i, row.Status)
		}
	}
}

// TestProcessDrainSplitCityUnitConvoyForAVanishedMember is the mint half, and
// the split-city counterpart of
// TestProcessDrainReplayUsesManifestWhenMemberWasDeletedBeforeUnitCreation.
//
// A drain resumes with row.UnitConvoyID empty either because no pass ever got to
// the mint, or because one crashed between minting and persisting the id — and
// both are the same shape once the member has since been deleted, because the
// resumed pass sees only an unresolved placeholder for it.
//
// Neither may be answered from the ambient graph binding. A unit convoy is a
// synthetic convoy and a synthetic convoy is a WORK bead, so a fresh mint belongs
// in the work store — a member no store can resolve is a reason to have no tracks
// edge (trackDrainMember stamps gc.drain_member_unresolved instead), not a reason
// to file the convoy in the infra ledger, which the migration's own equality
// invariant says work never enters. And the gc.drain_unit_key lookup that decides
// between mint and reuse has to span the same stores, or the crashed-mid-mint
// resume silently mints a second unit convoy for a row that already has one.
func TestProcessDrainSplitCityUnitConvoyForAVanishedMember(t *testing.T) {
	for _, tc := range []struct {
		name     string
		premint  bool
		wantMint bool
	}{
		{name: "no unit convoy minted yet", premint: false, wantMint: true},
		{name: "a previous pass minted one before crashing", premint: true, wantMint: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			formulatest.EnableV2ForTest(t)
			dir := t.TempDir()
			writeDrainItemFormula(t, dir)
			work, graph, drain, _ := seedSplitClassDrainWorkflow(t, false)
			if err := graph.SetMetadata(drain.ID, beadmeta.DrainMemberAccessMetadataKey, "exclusive"); err != nil {
				t.Fatalf("SetMetadata(exclusive): %v", err)
			}
			drain = mustGetBead(t, graph, drain.ID)
			root := mustGetBead(t, graph, drain.Metadata["gc.root_bead_id"])
			parentConvoyID := root.Metadata["gc.input_convoy_id"]
			members, err := convoycore.Members(work, parentConvoyID, false)
			if err != nil {
				t.Fatalf("Members: %v", err)
			}
			manifest := buildDrainManifest(drain, parentConvoyID, "drain-item", members)
			missingID := manifest.Rows[0].MemberID

			preminted := ""
			if tc.premint {
				// The mint the crashed pass completed, while the member was
				// still resolvable. Its id never reached the manifest.
				unit, created, err := ensureDrainUnitConvoy(graph, drain, parentConvoyID, len(members), manifest.Rows[0], members[0], ProcessOptions{MemberStores: []beads.Store{work}})
				if err != nil {
					t.Fatalf("pre-mint unit convoy: %v", err)
				}
				if !created {
					t.Fatal("pre-mint did not create the unit convoy")
				}
				preminted = unit.ID
			}

			if err := work.Delete(missingID); err != nil {
				t.Fatalf("Delete(%s): %v", missingID, err)
			}
			if err := persistDrainManifest(graph, drain.ID, manifest, map[string]string{beadmeta.DrainStateMetadataKey: beadmeta.DrainStateExpanding}); err != nil {
				t.Fatalf("persist manifest: %v", err)
			}

			if _, err := ProcessControl(graph, mustGetBead(t, graph, drain.ID), ProcessOptions{
				FormulaSearchPaths: []string{dir},
				MemberStores:       []beads.Store{work},
			}); err != nil {
				t.Fatalf("ProcessControl(replay with a vanished member): %v", err)
			}

			replayed := mustDrainManifest(t, mustGetBead(t, graph, drain.ID))
			row := replayed.Rows[0]
			if row.UnitConvoyID == "" || row.ItemRootID == "" {
				t.Fatalf("row = %+v, want a unit convoy and an item root despite the missing member", row)
			}
			if !tc.wantMint && row.UnitConvoyID != preminted {
				t.Fatalf("row unit convoy = %s, want the already-minted %s; a gc.drain_unit_key lookup that does not span the store the mint chose mints a duplicate for a row that already has one", row.UnitConvoyID, preminted)
			}
			if _, err := graph.Get(row.UnitConvoyID); err == nil {
				t.Fatalf("unit convoy %s for the vanished member %s is in the GRAPH binding; a synthetic convoy is a work bead, and the migration's own equality invariant says work never crosses into the infra ledger", row.UnitConvoyID, missingID)
			}
			unit, err := work.Get(row.UnitConvoyID)
			if err != nil {
				t.Fatalf("unit convoy %s is not in the work store either: %v", row.UnitConvoyID, err)
			}
			if got := unit.Metadata[beadmeta.DrainMemberUnresolvedMetadataKey]; tc.wantMint && got != "true" {
				t.Fatalf("unit convoy metadata = %#v, want the unresolved marker", unit.Metadata)
			}
			unitMembers, err := convoycore.Members(work, row.UnitConvoyID, true)
			if err != nil {
				t.Fatalf("Members(unit): %v", err)
			}
			for _, member := range unitMembers {
				if member.ID == missingID && !convoycore.IsUnresolvedTrackedItem(member) {
					t.Fatalf("unit members = %+v, want no resolvable track to the missing member %s", unitMembers, missingID)
				}
			}
			// The surviving sibling is unaffected and still lands in the work store.
			sibling := replayed.Rows[1]
			if _, err := work.Get(sibling.UnitConvoyID); err != nil {
				t.Fatalf("sibling unit convoy %s is not in the work store: %v", sibling.UnitConvoyID, err)
			}
		})
	}
}

// TestEnsureDrainUnitConvoyPrefersTheWorkStoreOverAnEdgelessBindingCopy pins the
// probe ORDER of the gc.drain_unit_key idempotence lookup.
//
// The lookup has to span every store the mint could have chosen, or a resume
// mints a duplicate. Spanning them primary-first gets the wrong one on a real
// converged city: a city migrated under the previous classification carries an
// EDGELESS copy of every synthetic convoy in its binding — `gc storage migrate`
// copied the row, and importInfraSnapshot re-added only the edges whose both
// endpoints were infra — so the binding answers with a convoy that has no tracks
// edge and cannot grow one, and the repair attempt fails cross-class.
//
// A unit convoy is a work bead, so the work class answers first.
func TestEnsureDrainUnitConvoyPrefersTheWorkStoreOverAnEdgelessBindingCopy(t *testing.T) {
	work := beads.NewMemStore()
	control, err := work.Create(beads.Bead{Title: "drain", Type: "task"})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	parent, err := work.Create(beads.Bead{Title: "parent", Type: "convoy"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	member, err := work.Create(beads.Bead{Title: "member", Type: "task"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	row := drainManifestRow{Index: 0, MemberID: member.ID, UnitKey: "drain-unit:" + control.ID + ":0:" + member.ID}

	// The real unit convoy, minted in the work store with its tracks edge.
	minted, created, err := ensureDrainUnitConvoy(work, control, parent.ID, 1, row, member, ProcessOptions{})
	if err != nil {
		t.Fatalf("ensureDrainUnitConvoy(single store): %v", err)
	}
	if !created {
		t.Fatal("first ensureDrainUnitConvoy did not create the unit convoy")
	}

	// What `gc storage migrate` left in the binding: the row, none of its edges.
	graph := beads.NewMemStoreFrom(1000, []beads.Bead{minted}, nil)

	reused, createdAgain, err := ensureDrainUnitConvoy(graph, control, parent.ID, 1, row, member, ProcessOptions{MemberStores: []beads.Store{work}})
	if err != nil {
		t.Fatalf("ensureDrainUnitConvoy(split city): %v; the lookup answered with the binding's edgeless copy and then tried to repair its missing track across the class boundary", err)
	}
	if createdAgain {
		t.Fatalf("the lookup missed the unit convoy %s a previous pass already minted and created a duplicate", minted.ID)
	}
	if reused.ID != minted.ID {
		t.Fatalf("reused unit convoy = %s, want the already-minted %s", reused.ID, minted.ID)
	}
	tracked, err := convoycore.HasTrack(work, minted.ID, member.ID)
	if err != nil {
		t.Fatalf("HasTrack: %v", err)
	}
	if !tracked {
		t.Fatalf("unit convoy %s lost its tracks edge to %s", minted.ID, member.ID)
	}
}

// TestProcessDrainSplitCityDoesNotWriteACrossStoreBlockerEdge pins the blocker
// projection to the same co-residence rule the convoy edge has.
//
// A drained member's out-of-convoy `blocks` dependency is an ordinary backlog
// shape, and its target is a WORK bead. The item workflow the drain builds for
// that member is graph-resident, so projecting the raw id writes a dependency row
// into a store that cannot resolve its own target. That does not degrade to an
// unenforced edge: the readiness reader treats a missing target's status as not
// closed, so the item step is excluded from Ready permanently, completeDrain
// returns ErrControlPending forever, and closing the real blocker in the work
// store cannot release it — recovery needs manual DepRemove surgery in the
// binding. There is no error, no quarantine, and no operator signal.
//
// An already-closed blocker is the same shape and far more common (any historical
// `blocks` edge in a real backlog), which is why the classification is by
// residence AND status: a terminal blocker constrains nothing, so omitting its
// edge loses no meaning and is not worth an operator's attention. An open one
// does lose meaning, so it is recorded on the item root.
func TestProcessDrainSplitCityDoesNotWriteACrossStoreBlockerEdge(t *testing.T) {
	for _, tc := range []struct {
		name           string
		closeBlocker   bool
		wantRecordedOn bool
	}{
		{name: "open out-of-convoy blocker is recorded", closeBlocker: false, wantRecordedOn: true},
		{name: "closed out-of-convoy blocker constrains nothing", closeBlocker: true, wantRecordedOn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			formulatest.EnableV2ForTest(t)
			dir := t.TempDir()
			writeDrainItemFormula(t, dir)
			work, graph, drain, memberIDs := seedSplitClassDrainWorkflow(t, false)

			blocker, err := work.Create(beads.Bead{Title: "outside the convoy", Type: "task"})
			if err != nil {
				t.Fatalf("create blocker: %v", err)
			}
			if tc.closeBlocker {
				if err := work.Close(blocker.ID); err != nil {
					t.Fatalf("Close(%s): %v", blocker.ID, err)
				}
			}
			mustDepAdd(t, work, memberIDs[0], blocker.ID, "blocks")

			result, err := ProcessControl(graph, drain, ProcessOptions{
				FormulaSearchPaths: []string{dir},
				MemberStores:       []beads.Store{work},
			})
			if err != nil {
				t.Fatalf("ProcessControl(member with an out-of-convoy blocker): %v", err)
			}
			if result.Action != "drain-expanded" {
				t.Fatalf("result = %+v, want drain-expanded", result)
			}

			assertNoUnresolvableBlockingDeps(t, graph)

			manifest := mustDrainManifest(t, mustGetBead(t, graph, drain.ID))
			ready, err := graph.Ready()
			if err != nil {
				t.Fatalf("Ready: %v", err)
			}
			readyIDs := make(map[string]bool, len(ready))
			for _, b := range ready {
				readyIDs[b.ID] = true
			}
			for i, row := range manifest.Rows {
				runnable := false
				for _, id := range itemWorkflowBeadIDs(t, graph, row.ItemRootID) {
					if readyIDs[id] {
						runnable = true
						break
					}
				}
				if !runnable {
					t.Fatalf("row %d item workflow %s has nothing ready; the drained item can never be dispatched and the drain waits on it forever", i, row.ItemRootID)
				}
			}

			root := mustGetBead(t, graph, manifest.Rows[0].ItemRootID)
			recorded := root.Metadata[beadmeta.DrainUnprojectedBlockersMetadataKey]
			if tc.wantRecordedOn {
				if recorded != blocker.ID {
					t.Fatalf("%s on item root %s = %q, want %q; an item workflow that runs without a constraint its source member had must say so on its own bead, not only in a log line",
						beadmeta.DrainUnprojectedBlockersMetadataKey, root.ID, recorded, blocker.ID)
				}
			} else if recorded != "" {
				t.Fatalf("%s = %q, want empty: a closed blocker constrains nothing, so omitting its edge loses no meaning and recording it would fire on every real backlog",
					beadmeta.DrainUnprojectedBlockersMetadataKey, recorded)
			}

			// The sibling row has no blocker at all and must be untouched.
			sibling := mustGetBead(t, graph, manifest.Rows[1].ItemRootID)
			if got := sibling.Metadata[beadmeta.DrainUnprojectedBlockersMetadataKey]; got != "" {
				t.Fatalf("sibling item root %s recorded %q, want nothing", sibling.ID, got)
			}
		})
	}
}

// TestDrainProjectedBlockerIDsIsProbeFreeOnASingleStoreCity is the byte-identity
// half of the blocker co-residence rule.
//
// Every city with no relocated graph class names no member stores, and for those
// co-residence is not a question: there is one store, so every id it resolves is
// co-resident. The check must not run for them — the projection is a per-row,
// per-pass sweep, and the answer would be the read it already did.
func TestDrainProjectedBlockerIDsIsProbeFreeOnASingleStoreCity(t *testing.T) {
	inner := beads.NewMemStore()
	blocker, err := inner.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	member, err := inner.Create(beads.Bead{Title: "member", Type: "task"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	mustDepAdd(t, inner, member.ID, blocker.ID, "blocks")
	store := &countingGetStore{Store: inner}

	manifest := drainManifest{Version: 1, Rows: []drainManifestRow{{Index: 0, MemberID: member.ID, ItemRootID: "root-member"}}}
	projection, err := drainProjectedBlockerIDs(store, member.ID, manifest, ProcessOptions{})
	if err != nil {
		t.Fatalf("drainProjectedBlockerIDs: %v", err)
	}
	if len(projection.BlockerIDs) != 1 || projection.BlockerIDs[0] != blocker.ID {
		t.Fatalf("blockerIDs = %v, want [%s]", projection.BlockerIDs, blocker.ID)
	}
	if len(projection.Unprojectable) != 0 {
		t.Fatalf("unprojectable = %v, want none on a single-store city", projection.Unprojectable)
	}
	if store.gets != 0 {
		t.Fatalf("the single-store path issued %d co-residence probe read(s); with one store every blocker is co-resident and the read is pure overhead on a per-row, per-pass sweep", store.gets)
	}
}

// TestEnsureDrainUnitConvoyFindsAConvoyMintedBeforeItsMemberVanished pins the
// SPAN of the gc.drain_unit_key idempotence lookup, as
// TestEnsureDrainUnitConvoyPrefersTheWorkStoreOverAnEdgelessBindingCopy pins its
// order.
//
// The key is the token that decides mint-or-reuse, and the manifest persists the
// unit convoy's id but never the store that answered when it was minted. So the
// lookup cannot be re-derived from the member: convoy members may be graph-class
// (convoy.MemberClasses names Graph for exactly that), and a member that resolves
// on the minting pass and not on the resuming pass moves the derived answer. Ask
// one store and the resume misses a convoy that exists and mints a second one for
// a row that already has one.
func TestEnsureDrainUnitConvoyFindsAConvoyMintedBeforeItsMemberVanished(t *testing.T) {
	work := beads.NewMemStore()
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	opts := ProcessOptions{MemberStores: []beads.Store{work}}

	control, err := graph.Create(beads.Bead{Title: "drain", Type: "task"})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	parent, err := work.Create(beads.Bead{Title: "parent", Type: "convoy"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	member, err := graph.Create(beads.Bead{Title: "a graph-class member", Type: "task"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	row := drainManifestRow{Index: 0, MemberID: member.ID, UnitKey: "drain-unit:" + control.ID + ":0:" + member.ID}

	minted, created, err := ensureDrainUnitConvoy(graph, control, parent.ID, 1, row, member, opts)
	if err != nil {
		t.Fatalf("ensureDrainUnitConvoy(minting pass): %v", err)
	}
	if !created {
		t.Fatal("minting pass did not create the unit convoy")
	}
	if _, err := graph.Get(minted.ID); err != nil {
		t.Fatalf("unit convoy %s did not land with its member: %v", minted.ID, err)
	}

	// The member vanishes before the resuming pass, which therefore sees the
	// unresolved placeholder loadDrainManifestMembers synthesizes.
	if err := graph.Delete(member.ID); err != nil {
		t.Fatalf("Delete(%s): %v", member.ID, err)
	}
	placeholder := beads.Bead{ID: member.ID, Title: member.ID, Type: "task", Status: "unknown"}

	reused, createdAgain, err := ensureDrainUnitConvoy(graph, control, parent.ID, 1, row, placeholder, opts)
	if err != nil {
		t.Fatalf("ensureDrainUnitConvoy(resuming pass): %v", err)
	}
	if createdAgain || reused.ID != minted.ID {
		t.Fatalf("resuming pass created unit convoy %s (created=%v), want the already-minted %s; a lookup re-derived from the member moves stores the moment the member stops resolving", reused.ID, createdAgain, minted.ID)
	}
}

// prefixDeclaringStore is a MemStore that DECLARES its mint prefix as a method,
// which is what storeref.PrefixOwner routes on (storeref.HasIDPrefix).
// beads.MemStore carries IDPrefix as a FIELD, so every existing drain fixture
// stands its two stores up as prefix-LESS to storeref and the id-prefix fast
// path never fires. Production stores all declare it (bdstore, native_dolt,
// caching, exec), so a divergence that only appears when it fires is invisible
// to a suite built on bare MemStores.
type prefixDeclaringStore struct {
	*beads.MemStore
}

func (s prefixDeclaringStore) IDPrefix() string { return s.MemStore.IDPrefix }

// seedCoResidentDrainMember builds the shape neither existing fixture has: ONE
// member id present in BOTH stores, over stores that declare their prefixes.
//
// seedSplitClassDrainWorkflow carries a copy of the CONVOY into the binding but
// never of a member, and its stores are bare MemStores. Both gaps have to close
// at once for a resolution disagreement about a member to be observable at all.
func seedCoResidentDrainMember(t *testing.T) (work, graph prefixDeclaringStore, control, member beads.Bead) {
	t.Helper()
	workMem := beads.NewMemStore()
	workMem.IDPrefix = "wrk"
	work = prefixDeclaringStore{MemStore: workMem}

	member, err := work.Create(beads.Bead{Title: "work copy", Type: "task"})
	if err != nil {
		t.Fatalf("create member in the work store: %v", err)
	}

	// The binding's co-resident copy: same id, its own row. This is what a
	// migration leaves behind — the row is copied and the original retained.
	graphMem := beads.NewMemStoreFrom(1000, []beads.Bead{{
		ID:        member.ID,
		Title:     "binding copy",
		Type:      "task",
		Status:    member.Status,
		CreatedAt: member.CreatedAt,
		UpdatedAt: member.UpdatedAt,
	}}, nil)
	graphMem.IDPrefix = "gcb"
	graph = prefixDeclaringStore{MemStore: graphMem}

	control, err = graph.Create(beads.Bead{
		Title: "drain",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "drain",
			"gc.drain_member_access": beadmeta.DrainMemberAccessExclusive,
		},
	})
	if err != nil {
		t.Fatalf("create drain control: %v", err)
	}
	return work, graph, control, member
}

// TestClassifyDrainBlockerFaultIsAnErrorNeverAnOmittedEdge is the fault control
// for the blocker classification's move onto the resolver. classifyDrainBlocker
// answers a question whose "no" SILENTLY DROPS a dependency edge, so a store it
// could not read must reach it as an error: an unread leg reported as
// drainBlockerUnprojectable omits an edge that constrains the workflow, and an
// unread leg reported as drainBlockerSatisfied omits one on the claim that a
// blocker it never saw is terminal.
func TestClassifyDrainBlockerFaultIsAnErrorNeverAnOmittedEdge(t *testing.T) {
	work, graph, _, _ := seedCoResidentDrainMember(t)
	hardErr := errors.New("work store unavailable")
	opts := ProcessOptions{MemberStores: []beads.Store{drainGetErrorStore{Store: work, prefix: "wrk", err: hardErr}}}

	residence, err := classifyDrainBlocker(graph, "wrk-blocker-1", opts)
	if !errors.Is(err, hardErr) {
		t.Fatalf("classifyDrainBlocker error = %v, want %v", err, hardErr)
	}
	if residence != drainBlockerUnclassified {
		t.Fatalf("classifyDrainBlocker residence = %v, want drainBlockerUnclassified; a leg that could not be read must never decide an edge", residence)
	}
}

// drainGetErrorStore is a leg whose every point read fails, with the id prefix
// it declares kept intact so the plan still routes to it.
type drainGetErrorStore struct {
	beads.Store
	prefix string
	err    error
}

func (s drainGetErrorStore) IDPrefix() string { return s.prefix }

func (s drainGetErrorStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

func coResidentDrainManifest(memberID string) drainManifest {
	return drainManifest{
		Version: 1,
		Context: "separate",
		Rows:    []drainManifestRow{{Index: 0, MemberID: memberID, UnitKey: "unit-0"}},
	}
}

// TestManifestLoadAndReservationAgreeOnACoResidentMember pins the agreement the
// two consumers of drainMemberProbeSet do not have: the manifest LOADS a member
// through storeref.Resolve, which tries the id-prefix owner first, while the
// reservation WRITES through drainMemberOwningStore, which hand-walks the same
// list with no such fast path. Same list, different consultation order, so the
// exclusive reservation can land on a row the manifest never serves — and an
// exclusive reservation nothing reads is not a lock.
func TestManifestLoadAndReservationAgreeOnACoResidentMember(t *testing.T) {
	work, graph, control, member := seedCoResidentDrainMember(t)
	opts := ProcessOptions{MemberStores: []beads.Store{work}}

	if err := reserveDrainMember(graph, control, member, opts); err != nil {
		t.Fatalf("reserveDrainMember: %v", err)
	}
	loaded, err := loadDrainManifestMembers(graph, control.ID, coResidentDrainManifest(member.ID), opts)
	if err != nil {
		t.Fatalf("loadDrainManifestMembers: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d members, want 1", len(loaded))
	}
	if got := loaded[0].Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]; got != control.ID {
		t.Fatalf("manifest member reservation = %q, want %q; the reservation was written to a copy the manifest does not serve", got, control.ID)
	}
}

// TestCoResidentDrainMemberAnswersFromTheBinding is the absolute row behind the
// relative one above. A drain member is a FOREIGN bead of statically unknown
// class — the drain reads it, it did not mint it — so a binding copy of a member
// id is the live relocated bead and the work copy is the one the migration
// retained frozen. Answering from work serves the frozen copy, which is the
// stale-read half of the shape storereftest's binding-wins clauses forbid.
//
// It is deliberately not satisfied by making both surfaces agree: a fix that
// converged them on work-first would pass the row above and fail this one.
func TestCoResidentDrainMemberAnswersFromTheBinding(t *testing.T) {
	work, graph, control, member := seedCoResidentDrainMember(t)
	opts := ProcessOptions{MemberStores: []beads.Store{work}}

	if err := reserveDrainMember(graph, control, member, opts); err != nil {
		t.Fatalf("reserveDrainMember: %v", err)
	}
	loaded, err := loadDrainManifestMembers(graph, control.ID, coResidentDrainManifest(member.ID), opts)
	if err != nil {
		t.Fatalf("loadDrainManifestMembers: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Title != "binding copy" {
		t.Fatalf("manifest served %q, want the binding copy", loaded[0].Title)
	}

	bindingRow, err := graph.Get(member.ID)
	if err != nil {
		t.Fatalf("graph.Get(member): %v", err)
	}
	if got := bindingRow.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]; got != control.ID {
		t.Fatalf("binding copy reservation = %q, want %q", got, control.ID)
	}

	workRow, err := work.Get(member.ID)
	if err != nil {
		t.Fatalf("work.Get(member): %v", err)
	}
	if workRow.Title != "work copy" {
		t.Fatalf("retained work copy title = %q, want it untouched", workRow.Title)
	}
	if got := workRow.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey]; got != "" {
		t.Fatalf("retained work copy carries reservation %q, want none; the frozen copy must not be written", got)
	}
}
