package molecule

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// makeWorkflowRecipe builds a minimal formula recipe with a root and N task steps.
func makeWorkflowRecipe(name string, stepIDs ...string) *formula.Recipe {
	steps := []formula.RecipeStep{
		{ID: name, Title: name, Type: "task", IsRoot: true, Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		}},
	}
	var deps []formula.RecipeDep
	for _, id := range stepIDs {
		steps = append(steps, formula.RecipeStep{
			ID:    name + "." + id,
			Title: "Step " + id,
			Type:  "task",
		})
		deps = append(deps, formula.RecipeDep{
			StepID:      name + "." + id,
			DependsOnID: name,
			Type:        "parent-child",
		})
	}
	return &formula.Recipe{
		Name:  name,
		Steps: steps,
		Deps:  deps,
	}
}

// setupWorkflow creates a workflow root bead and returns its ID.
func setupWorkflow(t *testing.T, store *beads.MemStore) beads.Bead {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title: "Test Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	return root
}

// setupWorkflowChild creates a child bead under a workflow root.
func setupWorkflowChild(t *testing.T, store *beads.MemStore, rootID, title string) beads.Bead {
	t.Helper()
	child, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		ParentID: rootID,
		Metadata: map[string]string{
			"gc.root_bead_id": rootID,
		},
	})
	if err != nil {
		t.Fatalf("create workflow child: %v", err)
	}
	return child
}

// assertBlockingDep verifies that fromID has a blocking dep on toID.
func assertBlockingDep(t *testing.T, store *beads.MemStore, fromID, toID string) {
	t.Helper()
	deps, err := store.DepList(fromID, "down")
	if err != nil {
		t.Fatalf("DepList(%s, down): %v", fromID, err)
	}
	for _, d := range deps {
		if d.DependsOnID == toID && d.Type == "blocks" {
			return
		}
	}
	t.Errorf("expected blocking dep %s -> %s, not found (deps: %+v)", fromID, toID, deps)
}

// assertAllBeadsHaveRootID verifies every bead in the sub-DAG has the expected gc.root_bead_id.
func assertAllBeadsHaveRootID(t *testing.T, store *beads.MemStore, idMapping map[string]string, expectedRootID string) {
	t.Helper()
	for stepID, beadID := range idMapping {
		b, err := store.Get(beadID)
		if err != nil {
			t.Fatalf("Get(%s) for step %s: %v", beadID, stepID, err)
		}
		if got := b.Metadata["gc.root_bead_id"]; got != expectedRootID {
			t.Errorf("step %s (bead %s): gc.root_bead_id = %q, want %q", stepID, beadID, got, expectedRootID)
		}
	}
}

// Test 1: Basic attach creates sub-DAG with correct root_bead_id
func TestAttachBasic(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	leaf := setupWorkflowChild(t, store, root.ID, "Leaf bead")

	recipe := makeWorkflowRecipe("sub-work", "run", "eval")

	result, err := Attach(context.Background(), store, recipe, leaf.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Sub-DAG should have root + 2 steps = 3 beads
	if result.Created != 3 {
		t.Errorf("Created = %d, want 3", result.Created)
	}

	// WorkflowRootID should be the parent workflow root, NOT the sub-DAG root
	if result.WorkflowRootID != root.ID {
		t.Errorf("WorkflowRootID = %q, want %q (parent workflow root)", result.WorkflowRootID, root.ID)
	}

	// All sub-DAG beads should have gc.root_bead_id = workflow root
	assertAllBeadsHaveRootID(t, store, result.IDMapping, root.ID)

	// Leaf should block on sub-DAG root
	assertBlockingDep(t, store, leaf.ID, result.RootID)
}

func TestAttachBasicGraphApplyPreservesWorkflowRootID(t *testing.T) {
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(true)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })

	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	leaf := setupWorkflowChild(t, store, root.ID, "Leaf bead")

	recipe := makeWorkflowRecipe("sub-work", "run", "eval")

	result, err := Attach(context.Background(), store, recipe, leaf.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if result.WorkflowRootID != root.ID {
		t.Errorf("WorkflowRootID = %q, want %q (parent workflow root)", result.WorkflowRootID, root.ID)
	}
	assertAllBeadsHaveRootID(t, store, result.IDMapping, root.ID)
	assertBlockingDep(t, store, leaf.ID, result.RootID)
}

// Test 2: Attach to workflow root itself (bead with no gc.root_bead_id)
func TestAttachToWorkflowRoot(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)

	recipe := makeWorkflowRecipe("sub-work", "task-a")

	result, err := Attach(context.Background(), store, recipe, root.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// When attaching to the root itself, gc.root_bead_id should be the root's own ID
	if result.WorkflowRootID != root.ID {
		t.Errorf("WorkflowRootID = %q, want %q", result.WorkflowRootID, root.ID)
	}

	assertAllBeadsHaveRootID(t, store, result.IDMapping, root.ID)
	assertBlockingDep(t, store, root.ID, result.RootID)
}

// TestAttachResolvesRootFromRunChainNotOwnID is the regression for the
// maintainer-city grafted-wisp incident (gcg-wisp-y785sz). A wisp/source bead
// grafted mid-workflow carries the true top workflow root in its run-chain
// metadata (workflow_id / molecule_id, written by sling) but NOT its own
// gc.root_bead_id. The old Attach fallback checked only gc.root_bead_id and
// then defaulted to the parent's own id, stamping the whole sub-DAG (attempt
// container, scope-check, every child) with the WRONG root. Downstream
// reconciliation then enumerated siblings via beads.DirectMembers(<wrong root>),
// found the wrong set, and burned ralph attempts until abort_scope fired on
// green work. Attach must resolve the root through the canonical run chain
// (beadmeta.ResolveRunID), never the parent's own id.
func TestAttachResolvesRootFromRunChainNotOwnID(t *testing.T) {
	store := beads.NewMemStore()

	// The true top workflow root.
	root := setupWorkflow(t, store)

	// A wisp/source bead grafted mid-workflow: it carries the true root in the
	// run chain (workflow_id) but has no gc.root_bead_id of its own.
	wisp, err := store.Create(beads.Bead{
		Title: "grafted wisp",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":     "wisp",
			"workflow_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("create grafted wisp: %v", err)
	}

	recipe := makeWorkflowRecipe("sub-work", "run", "eval")

	result, err := Attach(context.Background(), store, recipe, wisp.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// The sub-DAG must be rooted at the TRUE workflow root from the run chain,
	// never at the grafted wisp's own id.
	if result.WorkflowRootID != root.ID {
		t.Errorf("WorkflowRootID = %q, want %q (true root from run chain, not the wisp's own id %q)", result.WorkflowRootID, root.ID, wisp.ID)
	}
	assertAllBeadsHaveRootID(t, store, result.IDMapping, root.ID)
}

// TestAttachFallsBackToSelfWhenRunChainRootIsClosed is the regression for
// ga-yov1rr: the common "polecat resume" case where a content bead is
// re-attached to a fresh formula after its PRIOR molecule fully closed (the
// ordinary REQUEST_CHANGES round-trip through a review bead). The content
// bead's run-chain metadata (workflow_id / molecule_id / gc.root_bead_id)
// still points at that now-closed prior molecule -- a dead pointer, not a
// live upstream workflow to honor. Unlike
// TestAttachResolvesRootFromRunChainNotOwnID (gcg-wisp-y785sz), where the
// chain points at a workflow that is still open and must be honored, a
// chain pointing at something CLOSED must be treated as stale and the new
// sub-DAG must self-root at the content bead, exactly like a bead with no
// chain at all.
func TestAttachFallsBackToSelfWhenRunChainRootIsClosed(t *testing.T) {
	store := beads.NewMemStore()

	// A prior molecule wrapper that has since fully closed. Its id is still
	// recorded in the content bead's run-chain metadata below -- exactly
	// the shape left behind by an ordinary attach-complete-close cycle.
	staleRoot, err := store.Create(beads.Bead{
		Title: "prior molecule (now closed)",
		Type:  "task",
	})
	if err != nil {
		t.Fatalf("create stale root: %v", err)
	}
	if err := store.Close(staleRoot.ID); err != nil {
		t.Fatalf("close stale root: %v", err)
	}

	// The content bead being re-attached: its molecule_id still points at
	// the now-closed prior molecule.
	content, err := store.Create(beads.Bead{
		Title: "content bead being re-attached",
		Type:  "bug",
		Metadata: map[string]string{
			"molecule_id": staleRoot.ID,
		},
	})
	if err != nil {
		t.Fatalf("create content bead: %v", err)
	}

	recipe := makeWorkflowRecipe("sub-work", "run", "eval")

	result, err := Attach(context.Background(), store, recipe, content.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// A closed/dead chain must not be honored -- the new sub-DAG must
	// self-root at the content bead, not the stale closed molecule.
	if result.WorkflowRootID != content.ID {
		t.Errorf("WorkflowRootID = %q, want %q (self-root; %q is a closed prior molecule, not a live chain)", result.WorkflowRootID, content.ID, staleRoot.ID)
	}
	assertAllBeadsHaveRootID(t, store, result.IDMapping, content.ID)
}

// Test 3: Blocking dep prevents premature unblock
func TestAttachBlockingDepPreventsClose(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)

	// Create chain: A -> B -> C (A blocks B, B blocks C)
	beadA, _ := store.Create(beads.Bead{
		Title: "A", Type: "task", ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	beadB, _ := store.Create(beads.Bead{
		Title: "B", Type: "task", ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
		Needs:    []string{beadA.ID},
	})
	beadC, _ := store.Create(beads.Bead{
		Title: "C", Type: "task", ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
		Needs:    []string{beadB.ID},
	})
	_ = store.DepAdd(beadB.ID, beadA.ID, "blocks")
	_ = store.DepAdd(beadC.ID, beadB.ID, "blocks")

	// Attach sub-DAG to B
	recipe := makeWorkflowRecipe("sub-work", "task-x")
	result, err := Attach(context.Background(), store, recipe, beadB.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// B now has TWO blocking deps: A and sub-DAG root
	deps, _ := store.DepList(beadB.ID, "down")
	if len(deps) < 2 {
		t.Errorf("B should have at least 2 blocking deps, got %d: %+v", len(deps), deps)
	}
	assertBlockingDep(t, store, beadB.ID, beadA.ID)
	assertBlockingDep(t, store, beadB.ID, result.RootID)

	// Close A — B still blocked on sub-DAG
	_ = store.Close(beadA.ID)

	// Close all sub-DAG beads
	for _, beadID := range result.IDMapping {
		_ = store.Close(beadID)
	}

	// Verify C's dep on B still exists (C is still blocked until B closes)
	assertBlockingDep(t, store, beadC.ID, beadB.ID)

	// Now close B — this should be possible since all its deps are closed
	_ = store.Close(beadB.ID)
	bB, _ := store.Get(beadB.ID)
	if bB.Status != "closed" {
		t.Errorf("B.Status = %q, want closed", bB.Status)
	}

	_ = beadC // C is now unblocked
}

// Test 4: Multiple attaches on same workflow
func TestAttachMultiple(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	b1 := setupWorkflowChild(t, store, root.ID, "Bead 1")
	b2 := setupWorkflowChild(t, store, root.ID, "Bead 2")
	b3 := setupWorkflowChild(t, store, root.ID, "Bead 3")

	recipe1 := makeWorkflowRecipe("sub-1", "run-1")
	recipe2 := makeWorkflowRecipe("sub-2", "run-2")

	result1, err := Attach(context.Background(), store, recipe1, b1.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach to B1: %v", err)
	}

	result2, err := Attach(context.Background(), store, recipe2, b2.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach to B2: %v", err)
	}

	// Both sub-DAGs should share the same workflow root
	if result1.WorkflowRootID != root.ID {
		t.Errorf("result1.WorkflowRootID = %q, want %q", result1.WorkflowRootID, root.ID)
	}
	if result2.WorkflowRootID != root.ID {
		t.Errorf("result2.WorkflowRootID = %q, want %q", result2.WorkflowRootID, root.ID)
	}

	// Each bead has its own sub-DAG
	assertBlockingDep(t, store, b1.ID, result1.RootID)
	assertBlockingDep(t, store, b2.ID, result2.RootID)

	// B3 should have NO blocking deps (unaffected)
	deps, _ := store.DepList(b3.ID, "down")
	if len(deps) != 0 {
		t.Errorf("B3 should have 0 deps, got %d: %+v", len(deps), deps)
	}

	// All beads across both sub-DAGs share gc.root_bead_id
	assertAllBeadsHaveRootID(t, store, result1.IDMapping, root.ID)
	assertAllBeadsHaveRootID(t, store, result2.IDMapping, root.ID)

	// Sub-DAGs should be distinct (no shared bead IDs)
	for stepID, beadID := range result1.IDMapping {
		for stepID2, beadID2 := range result2.IDMapping {
			if beadID == beadID2 {
				t.Errorf("sub-DAGs share bead ID %s (step1=%s, step2=%s)", beadID, stepID, stepID2)
			}
		}
	}
}

// Test 5: gc.root_store_ref propagates when set
func TestAttachPropagatesStoreRef(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	_ = store.SetMetadata(root.ID, "gc.root_store_ref", "rig:test-rig")

	child := setupWorkflowChild(t, store, root.ID, "Child")
	_ = store.SetMetadata(child.ID, "gc.root_store_ref", "rig:test-rig")

	recipe := makeWorkflowRecipe("sub-work", "task-a")
	result, err := Attach(context.Background(), store, recipe, child.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	for stepID, beadID := range result.IDMapping {
		b, _ := store.Get(beadID)
		if got := b.Metadata["gc.root_store_ref"]; got != "rig:test-rig" {
			t.Errorf("step %s (bead %s): gc.root_store_ref = %q, want %q", stepID, beadID, got, "rig:test-rig")
		}
	}
}

// Test 6: Attach with title override
func TestAttachTitleOverride(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	child := setupWorkflowChild(t, store, root.ID, "Child")

	recipe := makeWorkflowRecipe("sub-work", "task-a")
	result, err := Attach(context.Background(), store, recipe, child.ID, AttachOptions{
		Title: "Custom Sub-Workflow Title",
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	subRoot, _ := store.Get(result.RootID)
	if subRoot.Title != "Custom Sub-Workflow Title" {
		t.Errorf("sub-DAG root title = %q, want %q", subRoot.Title, "Custom Sub-Workflow Title")
	}
}

// Test 7: Attach with variable substitution
func TestAttachVarSubstitution(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	child := setupWorkflowChild(t, store, root.ID, "Child")

	recipe := &formula.Recipe{
		Name: "sub-work",
		Steps: []formula.RecipeStep{
			{ID: "sub-work", Title: "Work on {{issue}}", Type: "task", IsRoot: true, Metadata: map[string]string{
				"gc.kind": "workflow",
			}},
			{ID: "sub-work.run", Title: "Run {{issue}}", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "sub-work.run", DependsOnID: "sub-work", Type: "parent-child"},
		},
		Vars: map[string]*formula.VarDef{
			"issue": {Description: "Issue ID"},
		},
	}

	result, err := Attach(context.Background(), store, recipe, child.ID, AttachOptions{
		Vars: map[string]string{"issue": "TASK-42"},
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	subRoot, _ := store.Get(result.RootID)
	if subRoot.Title != "Work on TASK-42" {
		t.Errorf("sub-DAG root title = %q, want %q", subRoot.Title, "Work on TASK-42")
	}

	runID := result.IDMapping["sub-work.run"]
	run, _ := store.Get(runID)
	if run.Title != "Run TASK-42" {
		t.Errorf("run step title = %q, want %q", run.Title, "Run TASK-42")
	}
}

// Test 8: Error — attach to nonexistent bead
func TestAttachNonexistentBead(t *testing.T) {
	store := beads.NewMemStore()
	recipe := makeWorkflowRecipe("sub-work", "task-a")

	_, err := Attach(context.Background(), store, recipe, "nonexistent-id", AttachOptions{})
	if err == nil {
		t.Fatal("expected error for nonexistent bead, got nil")
	}
}

// Test 9: Error — nil recipe
func TestAttachNilRecipe(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)

	_, err := Attach(context.Background(), store, nil, root.ID, AttachOptions{})
	if err == nil {
		t.Fatal("expected error for nil recipe, got nil")
	}
}

// Test 10: Error — empty attach bead ID
func TestAttachEmptyBeadID(t *testing.T) {
	store := beads.NewMemStore()
	recipe := makeWorkflowRecipe("sub-work", "task-a")

	_, err := Attach(context.Background(), store, recipe, "", AttachOptions{})
	if err == nil {
		t.Fatal("expected error for empty bead ID, got nil")
	}
}

// Test 11: Attach with inter-step dependencies in sub-DAG
func TestAttachWithInterStepDeps(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	child := setupWorkflowChild(t, store, root.ID, "Child")

	// Recipe: run -> eval (eval blocks on run)
	recipe := &formula.Recipe{
		Name: "sub-work",
		Steps: []formula.RecipeStep{
			{ID: "sub-work", Title: "Sub Work", Type: "task", IsRoot: true, Metadata: map[string]string{
				"gc.kind": "workflow",
			}},
			{ID: "sub-work.run", Title: "Run", Type: "task"},
			{ID: "sub-work.eval", Title: "Eval", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "sub-work.run", DependsOnID: "sub-work", Type: "parent-child"},
			{StepID: "sub-work.eval", DependsOnID: "sub-work", Type: "parent-child"},
			{StepID: "sub-work.eval", DependsOnID: "sub-work.run", Type: "blocks"},
		},
	}

	result, err := Attach(context.Background(), store, recipe, child.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Verify eval blocks on run within the sub-DAG
	evalID := result.IDMapping["sub-work.eval"]
	runID := result.IDMapping["sub-work.run"]
	assertBlockingDep(t, store, evalID, runID)

	// Verify attach bead blocks on sub-DAG root
	assertBlockingDep(t, store, child.ID, result.RootID)
}

// Test 12: Idempotency — duplicate Attach with same key is a no-op
func TestAttachIdempotency(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")

	recipe := makeWorkflowRecipe("attempt", "run")

	// First attach
	result1, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		IdempotencyKey: "retry:control:attempt:1",
	})
	if err != nil {
		t.Fatalf("Attach 1: %v", err)
	}
	if result1.Duplicate {
		t.Fatal("first attach should not be duplicate")
	}

	allBefore, _ := store.ListOpen()
	countBefore := len(allBefore)

	// Second attach with same key — should be no-op
	result2, err := Attach(context.Background(), store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{
		IdempotencyKey: "retry:control:attempt:1",
	})
	if err != nil {
		t.Fatalf("Attach 2: %v", err)
	}
	if !result2.Duplicate {
		t.Fatal("second attach should be duplicate")
	}
	if result2.RootID != result1.RootID {
		t.Errorf("duplicate RootID = %q, want %q", result2.RootID, result1.RootID)
	}

	// No new beads created
	allAfter, _ := store.ListOpen()
	if len(allAfter) != countBefore {
		t.Errorf("bead count changed: %d → %d (expected no change)", countBefore, len(allAfter))
	}
}

func TestAttachIdempotencyFindsEphemeralExistingRoot(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	existingRoot, err := store.Create(beads.Bead{
		Title:     "attempt",
		Type:      "task",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.kind":            "workflow",
			"gc.idempotency_key": "attempt:1",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "attempt",
		},
	})
	if err != nil {
		t.Fatalf("create existing ephemeral root: %v", err)
	}
	existingRun, err := store.Create(beads.Bead{
		Title:     "run",
		Type:      "task",
		Ephemeral: true,
		ParentID:  existingRoot.ID,
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "attempt.run",
		},
	})
	if err != nil {
		t.Fatalf("create existing ephemeral child: %v", err)
	}

	result, err := Attach(context.Background(), store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{
		IdempotencyKey: "attempt:1",
	})
	if err != nil {
		t.Fatalf("Attach duplicate: %v", err)
	}
	if !result.Duplicate {
		t.Fatal("Attach duplicate should report Duplicate for ephemeral existing root")
	}
	if result.RootID != existingRoot.ID {
		t.Fatalf("RootID = %q, want existing ephemeral root %q", result.RootID, existingRoot.ID)
	}
	if result.IDMapping["attempt"] != existingRoot.ID || result.IDMapping["attempt.run"] != existingRun.ID {
		t.Fatalf("IDMapping = %#v, want existing ephemeral root and child", result.IDMapping)
	}
	assertBlockingDep(t, store, control.ID, existingRoot.ID)
}

// Test 13: Different idempotency keys create separate sub-DAGs
func TestAttachDifferentKeysCreateSeparate(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")

	result1, err := Attach(context.Background(), store, makeWorkflowRecipe("a1", "run"), control.ID, AttachOptions{
		IdempotencyKey: "retry:control:attempt:1",
	})
	if err != nil {
		t.Fatalf("Attach 1: %v", err)
	}

	result2, err := Attach(context.Background(), store, makeWorkflowRecipe("a2", "run"), control.ID, AttachOptions{
		IdempotencyKey: "retry:control:attempt:2",
	})
	if err != nil {
		t.Fatalf("Attach 2: %v", err)
	}

	if result1.RootID == result2.RootID {
		t.Error("different keys should produce different sub-DAGs")
	}
	if result2.Duplicate {
		t.Error("second attach with different key should not be duplicate")
	}
}

func TestAttachPreservesExecutableTaskRootType(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")

	recipe := &formula.Recipe{
		Name: "attempt",
		Steps: []formula.RecipeStep{
			{ID: "attempt", Title: "Attempt", Type: "task", IsRoot: true},
		},
	}

	result, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	attachedRoot, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("Get(attached root): %v", err)
	}
	if attachedRoot.Type != "task" {
		t.Fatalf("attached root type = %q, want task", attachedRoot.Type)
	}
}

// Test 14: Epoch fencing — matching epoch succeeds and increments
func TestAttachEpochSuccess(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "1")

	recipe := makeWorkflowRecipe("attempt", "run")
	_, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		ExpectedEpoch: 1,
	})
	if err != nil {
		t.Fatalf("Attach with matching epoch: %v", err)
	}

	// Epoch should be incremented to 2
	updated, _ := store.Get(control.ID)
	if got := updated.Metadata["gc.control_epoch"]; got != "2" {
		t.Errorf("epoch after attach = %q, want 2", got)
	}
}

// Test 15: Epoch fencing — mismatched epoch returns ErrEpochConflict
func TestAttachEpochConflict(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "3")

	recipe := makeWorkflowRecipe("attempt", "run")
	_, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		ExpectedEpoch: 1, // stale — current is 3
	})
	if err == nil {
		t.Fatal("expected epoch conflict error, got nil")
	}
	if !errors.Is(err, ErrEpochConflict) {
		t.Fatalf("expected ErrEpochConflict, got: %v", err)
	}

	// No beads should have been created
	all, _ := store.ListOpen()
	if len(all) != 2 { // just root + control
		t.Errorf("bead count = %d, want 2 (no sub-DAG created)", len(all))
	}
}

// Test 16: Epoch fencing with idempotency — both work together
func TestAttachEpochWithIdempotency(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "1")

	recipe := makeWorkflowRecipe("attempt", "run")

	// First attach: epoch 1, key "attempt:1"
	result1, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		ExpectedEpoch:  1,
		IdempotencyKey: "attempt:1",
	})
	if err != nil {
		t.Fatalf("Attach 1: %v", err)
	}
	if result1.Duplicate {
		t.Fatal("first should not be duplicate")
	}

	// Epoch is now 2. Duplicate with stale epoch 1 should still work
	// because idempotency check happens before epoch check in the
	// duplicate path (no new beads to create).
	result2, err := Attach(context.Background(), store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{
		ExpectedEpoch:  1, // stale, but idempotency should catch it first
		IdempotencyKey: "attempt:1",
	})
	if err != nil {
		t.Fatalf("Duplicate attach: %v", err)
	}
	if !result2.Duplicate {
		t.Fatal("second should be duplicate")
	}
}

func TestAttachDuplicateRecoveryPopulatesIDMappingAndAdvancesEpoch(t *testing.T) {
	base := beads.NewMemStore()
	root := setupWorkflow(t, base)
	control := setupWorkflowChild(t, base, root.ID, "Control")
	_ = base.SetMetadata(control.ID, "gc.control_epoch", "1")

	store := &attachFailOnceDepStore{
		Store: base,
		err:   errors.New("wiring outer dep: invalid connection: i/o timeout"),
	}
	recipe := makeWorkflowRecipe("attempt")
	_, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		ExpectedEpoch:  1,
		IdempotencyKey: "attempt:1",
	})
	if err == nil {
		t.Fatal("expected first attach to fail during outer dependency wiring")
	}

	afterFailedAttach, err := base.Get(control.ID)
	if err != nil {
		t.Fatalf("get control after failed attach: %v", err)
	}
	if afterFailedAttach.Metadata["gc.control_epoch"] != "1" {
		t.Fatalf("epoch after failed attach = %q, want 1", afterFailedAttach.Metadata["gc.control_epoch"])
	}

	result, err := Attach(context.Background(), base, recipe, control.ID, AttachOptions{
		ExpectedEpoch:  1,
		IdempotencyKey: "attempt:1",
	})
	if err != nil {
		t.Fatalf("duplicate attach recovery: %v", err)
	}
	if !result.Duplicate {
		t.Fatal("duplicate recovery should report Duplicate")
	}
	if result.IDMapping["attempt"] != result.RootID || result.RootID == "" {
		t.Fatalf("duplicate IDMapping = %#v root=%q, want root step mapping", result.IDMapping, result.RootID)
	}
	assertBlockingDep(t, base, control.ID, result.RootID)
	afterRecovery, err := base.Get(control.ID)
	if err != nil {
		t.Fatalf("get control after recovery: %v", err)
	}
	if afterRecovery.Metadata["gc.control_epoch"] != "2" {
		t.Fatalf("epoch after duplicate recovery = %q, want 2", afterRecovery.Metadata["gc.control_epoch"])
	}
}

func TestAttachIdempotencyRejectsFailedPartialSubDAG(t *testing.T) {
	base := beads.NewMemStore()
	root := setupWorkflow(t, base)
	control := setupWorkflowChild(t, base, root.ID, "Control")

	store := &attachFailOnceDepStore{
		Store: base,
		err:   errors.New("wiring dep: lock wait timeout exceeded; try restarting transaction"),
	}
	recipe := makeWorkflowRecipe("attempt", "run")
	_, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		IdempotencyKey: "attempt:1",
	})
	if err == nil {
		t.Fatal("expected first attach to fail during dependency wiring")
	}

	failedRoots, err := base.List(beads.ListQuery{
		Metadata: map[string]string{
			"gc.idempotency_key": "attempt:1",
			"gc.root_bead_id":    root.ID,
			"molecule_failed":    "true",
		},
	})
	if err != nil {
		t.Fatalf("list failed partial roots: %v", err)
	}
	if len(failedRoots) != 1 {
		t.Fatalf("failed partial roots = %d, want 1", len(failedRoots))
	}

	_, err = Attach(context.Background(), base, recipe, control.ID, AttachOptions{
		IdempotencyKey: "attempt:1",
	})
	if err == nil {
		t.Fatal("expected duplicate attach to reject failed partial sub-DAG")
	}
	if !strings.Contains(err.Error(), "molecule_failed") {
		t.Fatalf("duplicate attach error = %v, want molecule_failed context", err)
	}
}

func TestAttachIdempotentDuplicateSkipsRuntimeVarValidation(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")

	recipe := &formula.Recipe{
		Name: "attempt-required-vars",
		Steps: []formula.RecipeStep{
			{ID: "attempt-required-vars", Title: "Attempt {{target_id}}", Type: "task", IsRoot: true},
			{ID: "attempt-required-vars.run", Title: "Run in {{workspace}}", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "attempt-required-vars.run", DependsOnID: "attempt-required-vars", Type: "parent-child"},
		},
		Vars: map[string]*formula.VarDef{
			"target_id": {Description: "Bead being worked on", Required: true},
			"workspace": {Description: "Workspace path", Required: true},
		},
	}

	result1, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		IdempotencyKey: "retry:control:attempt-required-vars:1",
		Vars: map[string]string{
			"target_id": control.ID,
			"workspace": "/tmp/repro",
		},
	})
	if err != nil {
		t.Fatalf("Attach 1: %v", err)
	}
	if result1.Duplicate {
		t.Fatal("first attach should not be duplicate")
	}

	result2, err := Attach(context.Background(), store, &formula.Recipe{
		Name: "attempt-required-vars",
		Steps: []formula.RecipeStep{
			{ID: "attempt-required-vars", Title: "Attempt {{target_id}}", Type: "task", IsRoot: true},
			{ID: "attempt-required-vars.run", Title: "Run in {{workspace}}", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "attempt-required-vars.run", DependsOnID: "attempt-required-vars", Type: "parent-child"},
		},
		Vars: map[string]*formula.VarDef{
			"target_id": {Description: "Bead being worked on", Required: true},
			"workspace": {Description: "Workspace path", Required: true},
		},
	}, control.ID, AttachOptions{
		IdempotencyKey: "retry:control:attempt-required-vars:1",
	})
	if err != nil {
		t.Fatalf("Duplicate attach should return existing sub-DAG before re-validating vars: %v", err)
	}
	if !result2.Duplicate {
		t.Fatal("second attach should be duplicate")
	}
	if result2.RootID != result1.RootID {
		t.Fatalf("duplicate attach root = %q, want %q", result2.RootID, result1.RootID)
	}
}

type attachFailOnceDepStore struct {
	beads.Store
	err    error
	failed bool
}

func (s *attachFailOnceDepStore) DepAdd(issueID, dependsOnID, depType string) error {
	if !s.failed {
		s.failed = true
		return s.err
	}
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

func TestAttachDuplicateReplayMapsStepsWithinSelectedAttachmentOnly(t *testing.T) {
	for _, order := range [][]string{{"a", "b"}, {"b", "a"}} {
		t.Run(strings.Join(order, "-then-"), func(t *testing.T) {
			store := beads.NewMemStore()
			root := setupWorkflow(t, store)
			control := setupWorkflowChild(t, store, root.ID, "Control")
			ctx := context.Background()

			attached := map[string]*AttachResult{}
			for _, key := range order {
				result, err := Attach(ctx, store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{IdempotencyKey: key})
				if err != nil {
					t.Fatalf("attach %s: %v", key, err)
				}
				attached[key] = result
			}
			first := attached[order[0]]
			for _, id := range []string{first.IDMapping["attempt.run"], first.RootID} {
				if err := store.Close(id); err != nil {
					t.Fatalf("close %s: %v", id, err)
				}
			}

			for _, key := range order {
				want := attached[key]
				got, err := Attach(ctx, store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{IdempotencyKey: key})
				if err != nil {
					t.Fatalf("replay %s: %v", key, err)
				}
				if !got.Duplicate || got.RootID != want.RootID {
					t.Fatalf("replay %s: duplicate=%v root=%s, want duplicate of %s", key, got.Duplicate, got.RootID, want.RootID)
				}
				if got.IDMapping["attempt.run"] != want.IDMapping["attempt.run"] {
					t.Errorf("replay %s: attempt.run mapped to %s, want %s from its own attachment %s",
						key, got.IDMapping["attempt.run"], want.IDMapping["attempt.run"], want.RootID)
				}
			}
		})
	}
}

func TestAttachDuplicateReplayLegacyAttachmentNeverAdoptsAnotherAttachmentsStep(t *testing.T) {
	for _, tc := range []struct {
		name        string
		closeLegacy bool
		wantOwn     bool
	}{
		{name: "open legacy step maps to itself", wantOwn: true},
		{name: "closed legacy step stays unmapped", closeLegacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			root := setupWorkflow(t, store)
			control := setupWorkflowChild(t, store, root.ID, "Control")
			ctx := context.Background()

			current, err := Attach(ctx, store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{IdempotencyKey: "current"})
			if err != nil {
				t.Fatalf("attach current: %v", err)
			}
			legacy, err := Attach(ctx, store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{IdempotencyKey: "legacy"})
			if err != nil {
				t.Fatalf("attach legacy: %v", err)
			}
			legacyStep := legacy.IDMapping["attempt.run"]
			if err := store.SetMetadata(legacyStep, beadmeta.AttachRootIDMetadataKey, ""); err != nil {
				t.Fatalf("strip attach root from %s: %v", legacyStep, err)
			}
			if tc.closeLegacy {
				if err := store.Close(legacyStep); err != nil {
					t.Fatalf("close %s: %v", legacyStep, err)
				}
			}

			got, err := Attach(ctx, store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{IdempotencyKey: "legacy"})
			if err != nil {
				t.Fatalf("replay legacy: %v", err)
			}
			if !got.Duplicate || got.RootID != legacy.RootID {
				t.Fatalf("replay legacy: duplicate=%v root=%s, want duplicate of %s", got.Duplicate, got.RootID, legacy.RootID)
			}
			mapped := got.IDMapping["attempt.run"]
			if mapped == current.IDMapping["attempt.run"] {
				t.Fatalf("replay legacy: attempt.run mapped to %s, owned by attachment %s", mapped, current.RootID)
			}
			want := ""
			if tc.wantOwn {
				want = legacyStep
			}
			if mapped != want {
				t.Fatalf("replay legacy: attempt.run mapped to %q, want %q", mapped, want)
			}
		})
	}
}

func TestAttachDuplicateReplayRejectsAmbiguousStepWithinAttachment(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	ctx := context.Background()

	first, err := Attach(ctx, store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{IdempotencyKey: "a"})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title: "stray run",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:   root.ID,
			beadmeta.AttachRootIDMetadataKey: first.RootID,
			beadmeta.StepRefMetadataKey:      "attempt.run",
		},
	}); err != nil {
		t.Fatalf("create stray step: %v", err)
	}

	_, err = Attach(ctx, store, makeWorkflowRecipe("attempt", "run"), control.ID, AttachOptions{IdempotencyKey: "a"})
	if err == nil || !strings.Contains(err.Error(), "matches beads") {
		t.Fatalf("replay with two beads for one step = %v, want ambiguity error", err)
	}
}

func TestAttachStampsAttachRootOnEveryCreatedStep(t *testing.T) {
	store := beads.NewMemStore()
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	result, err := Attach(context.Background(), store, makeWorkflowRecipe("attempt", "build", "run"), control.ID, AttachOptions{IdempotencyKey: "a"})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	for stepID, beadID := range result.IDMapping {
		bead, err := store.Get(beadID)
		if err != nil {
			t.Fatalf("get %s: %v", beadID, err)
		}
		want := result.RootID
		if beadID == result.RootID {
			want = ""
		}
		if got := bead.Metadata[beadmeta.AttachRootIDMetadataKey]; got != want {
			t.Errorf("step %s bead %s %s = %q, want %q", stepID, beadID, beadmeta.AttachRootIDMetadataKey, got, want)
		}
		if got := bead.Metadata[beadmeta.MoleculeIDMetadataKey]; got != "" {
			t.Errorf("step %s bead %s %s = %q, want unset", stepID, beadID, beadmeta.MoleculeIDMetadataKey, got)
		}
		if got := beadmeta.ResolveRunID(bead.Metadata, bead.ID, ""); got != root.ID {
			t.Errorf("step %s bead %s ResolveRunID = %q, want workflow root %s", stepID, beadID, got, root.ID)
		}
	}
}

func TestAttachGraphApplyPlanReferencesAttachRootOnEveryStep(t *testing.T) {
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(true)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })

	store := &graphApplySpyStore{MemStore: beads.NewMemStore()}
	root := setupWorkflow(t, store.MemStore)
	control := setupWorkflowChild(t, store.MemStore, root.ID, "Control")
	if _, err := Attach(context.Background(), store, makeWorkflowRecipe("attempt", "build", "run"), control.ID, AttachOptions{IdempotencyKey: "a"}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if store.calls != 1 || store.plan == nil {
		t.Fatalf("graph apply calls = %d, want 1", store.calls)
	}
	for _, node := range store.plan.Nodes {
		want := "attempt"
		if node.Key == "attempt" {
			want = ""
		}
		if got := node.MetadataRefs[beadmeta.AttachRootIDMetadataKey]; got != want {
			t.Errorf("node %s metadata ref %s = %q, want %q", node.Key, beadmeta.AttachRootIDMetadataKey, got, want)
		}
		if _, ok := node.Metadata[beadmeta.MoleculeIDMetadataKey]; ok {
			t.Errorf("node %s carries %s, want unset", node.Key, beadmeta.MoleculeIDMetadataKey)
		}
	}
}

// setupV1MoleculeAttachment reproduces the production shape gc sling leaves
// behind for a source bead that already has a live v1 attachment: an open
// molecule M (type molecule, no gc.kind) with one open step child and
// gc.var.issue naming the source bead S, and S.molecule_id pointing at M. The
// pointer is a forward pointer from S to its own attachment, never a container
// of S. withEdge additionally wires the S->M blocks edge (the shape PR #6489
// assumed), which must not change the answer.
func setupV1MoleculeAttachment(t *testing.T, store *beads.MemStore, withEdge bool) (source, mol, step beads.Bead) {
	t.Helper()
	source, err := store.Create(beads.Bead{Title: "content bead", Type: "bug"})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	mol, err = store.Create(beads.Bead{
		Title:    "mol-tdd-build",
		Type:     "molecule",
		Metadata: map[string]string{"gc.var.issue": source.ID},
	})
	if err != nil {
		t.Fatalf("create molecule: %v", err)
	}
	step, err = store.Create(beads.Bead{Title: "load-context", Type: "step", ParentID: mol.ID})
	if err != nil {
		t.Fatalf("create molecule step: %v", err)
	}
	if err := store.SetMetadata(source.ID, beadmeta.MoleculeIDMetadataKey, mol.ID); err != nil {
		t.Fatalf("set molecule_id: %v", err)
	}
	if withEdge {
		if err := store.DepAdd(source.ID, mol.ID, "blocks"); err != nil {
			t.Fatalf("dep add source->molecule: %v", err)
		}
	}
	return source, mol, step
}

// TestAttachDoesNotNestUnderMoleculeIDAttachment is the regression for
// ga-crz5ege (be-t5hge / be-1wlhp / be-bl675): gc formula cook --attach on a
// bead whose molecule_id names its own live v1 attachment rooted the new
// sub-DAG at that other molecule and stamped gc.root_bead_id=<molecule> on
// every step. molecule_id is a forward pointer, so the new sub-DAG must root at
// the attach target itself, with or without a blocks edge to the molecule.
func TestAttachDoesNotNestUnderMoleculeIDAttachment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withEdge bool
	}{
		{name: "no source-to-molecule edge (production shape)", withEdge: false},
		{name: "blocks edge source-to-molecule", withEdge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			source, mol, step := setupV1MoleculeAttachment(t, store, tc.withEdge)
			molBefore, stepBefore := mol, step

			result, err := Attach(context.Background(), store, makeWorkflowRecipe("sub-work", "run", "eval"), source.ID, AttachOptions{})
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}

			if result.WorkflowRootID != source.ID {
				t.Errorf("WorkflowRootID = %q, want %q (the attach target; %q is its forward-pointer attachment)", result.WorkflowRootID, source.ID, mol.ID)
			}
			assertAllBeadsHaveRootID(t, store, result.IDMapping, source.ID)
			for stepID, beadID := range result.IDMapping {
				b, err := store.Get(beadID)
				if err != nil {
					t.Fatalf("Get(%s) for step %s: %v", beadID, stepID, err)
				}
				if got := b.Metadata[beadmeta.RootBeadIDMetadataKey]; got == mol.ID {
					t.Errorf("step %s (bead %s) was nested under the molecule_id attachment %s", stepID, beadID, mol.ID)
				}
			}
			assertBlockingDep(t, store, source.ID, result.RootID)

			// The pre-existing attachment is not adopted, burned, or rewritten.
			molAfter, err := store.Get(mol.ID)
			if err != nil {
				t.Fatalf("Get molecule: %v", err)
			}
			stepAfter, err := store.Get(step.ID)
			if err != nil {
				t.Fatalf("Get molecule step: %v", err)
			}
			if molAfter.Status != molBefore.Status || !reflect.DeepEqual(molAfter.Metadata, molBefore.Metadata) {
				t.Errorf("molecule %s changed: before status=%q meta=%v, after status=%q meta=%v", mol.ID, molBefore.Status, molBefore.Metadata, molAfter.Status, molAfter.Metadata)
			}
			if stepAfter.Status != stepBefore.Status || !reflect.DeepEqual(stepAfter.Metadata, stepBefore.Metadata) {
				t.Errorf("molecule step %s changed: before status=%q meta=%v, after status=%q meta=%v", step.ID, stepBefore.Status, stepBefore.Metadata, stepAfter.Status, stepAfter.Metadata)
			}
		})
	}
}

// TestAttachLiveRunChainBeatsMoleculeID pins the precedence of the keys that
// still nest: a live workflow_id, and a live gc.root_bead_id, each win over a
// molecule_id pointing at some other open molecule.
func TestAttachLiveRunChainBeatsMoleculeID(t *testing.T) {
	for _, key := range []string{"workflow_id", beadmeta.RootBeadIDMetadataKey} {
		t.Run(key, func(t *testing.T) {
			store := beads.NewMemStore()
			root := setupWorkflow(t, store)
			other, err := store.Create(beads.Bead{Title: "unrelated open molecule", Type: "molecule"})
			if err != nil {
				t.Fatalf("create other molecule: %v", err)
			}
			source, err := store.Create(beads.Bead{
				Title: "bead inside a live run",
				Type:  "task",
				Metadata: map[string]string{
					key:                            root.ID,
					beadmeta.MoleculeIDMetadataKey: other.ID,
				},
			})
			if err != nil {
				t.Fatalf("create source: %v", err)
			}

			result, err := Attach(context.Background(), store, makeWorkflowRecipe("sub-work", "run"), source.ID, AttachOptions{})
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			if result.WorkflowRootID != root.ID {
				t.Errorf("WorkflowRootID = %q, want %q (live %s), not molecule_id target %q", result.WorkflowRootID, root.ID, key, other.ID)
			}
			assertAllBeadsHaveRootID(t, store, result.IDMapping, root.ID)
		})
	}
}

// TestAttachClosedNestingRootSelfRoots pins the ga-yov1rr guard on the keys
// that still nest. The molecule_id variant of this guard
// (TestAttachFallsBackToSelfWhenRunChainRootIsClosed) now passes without the
// guard, because molecule_id is no longer consulted; these two fail if the
// closed-root guard is removed.
func TestAttachClosedNestingRootSelfRoots(t *testing.T) {
	for _, key := range []string{"workflow_id", beadmeta.RootBeadIDMetadataKey} {
		t.Run(key, func(t *testing.T) {
			store := beads.NewMemStore()
			staleRoot := setupWorkflow(t, store)
			if err := store.Close(staleRoot.ID); err != nil {
				t.Fatalf("close stale root: %v", err)
			}
			source, err := store.Create(beads.Bead{
				Title:    "content bead being re-attached",
				Type:     "bug",
				Metadata: map[string]string{key: staleRoot.ID},
			})
			if err != nil {
				t.Fatalf("create source: %v", err)
			}

			result, err := Attach(context.Background(), store, makeWorkflowRecipe("sub-work", "run"), source.ID, AttachOptions{})
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			if result.WorkflowRootID != source.ID {
				t.Errorf("WorkflowRootID = %q, want %q (self-root; %s %q is closed)", result.WorkflowRootID, source.ID, key, staleRoot.ID)
			}
			assertAllBeadsHaveRootID(t, store, result.IDMapping, source.ID)
		})
	}
}
