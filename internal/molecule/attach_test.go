package molecule

import (
	"context"
	"testing"

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

	allBefore, _ := store.List()
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
	allAfter, _ := store.List()
	if len(allAfter) != countBefore {
		t.Errorf("bead count changed: %d → %d (expected no change)", countBefore, len(allAfter))
	}
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
	if err != ErrEpochConflict {
		t.Fatalf("expected ErrEpochConflict, got: %v", err)
	}

	// No beads should have been created
	all, _ := store.List()
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
