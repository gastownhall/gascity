package molecule

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

type graphApplySpyStore struct {
	*beads.MemStore
	plan   *beads.GraphApplyPlan
	result *beads.GraphApplyResult
}

func priorityPtr(v int) *int {
	return &v
}

func (s *graphApplySpyStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	s.plan = plan
	if s.result != nil {
		return s.result, nil
	}
	ids := make(map[string]string, len(plan.Nodes))
	for i, node := range plan.Nodes {
		ids[node.Key] = fmt.Sprintf("bd-%d", i+1)
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

func TestInstantiateSimple(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{
		Name:        "test-formula",
		Description: "A test formula",
		Steps: []formula.RecipeStep{
			{ID: "test-formula", Title: "{{title}}", Type: "molecule", IsRoot: true},
			{ID: "test-formula.step-a", Title: "Step A", Type: "task"},
			{ID: "test-formula.step-b", Title: "Step B: {{feature}}", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "test-formula.step-a", DependsOnID: "test-formula", Type: "parent-child"},
			{StepID: "test-formula.step-b", DependsOnID: "test-formula", Type: "parent-child"},
			{StepID: "test-formula.step-b", DependsOnID: "test-formula.step-a", Type: "blocks"},
		},
		Vars: map[string]*formula.VarDef{
			"title":   {Description: "Title"},
			"feature": {Description: "Feature name"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{
		Title: "My Feature",
		Vars:  map[string]string{"feature": "auth"},
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	if result.Created != 3 {
		t.Errorf("Created = %d, want 3", result.Created)
	}
	if result.RootID == "" {
		t.Fatal("RootID is empty")
	}

	// Verify root bead
	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if root.Title != "My Feature" {
		t.Errorf("root.Title = %q, want %q", root.Title, "My Feature")
	}
	if root.Type != "molecule" {
		t.Errorf("root.Type = %q, want %q", root.Type, "molecule")
	}
	if root.Ref != "test-formula" {
		t.Errorf("root.Ref = %q, want %q", root.Ref, "test-formula")
	}

	// Verify step-b has variable substitution
	stepBID := result.IDMapping["test-formula.step-b"]
	stepB, err := store.Get(stepBID)
	if err != nil {
		t.Fatalf("Get step-b: %v", err)
	}
	if stepB.Title != "Step B: auth" {
		t.Errorf("step-b.Title = %q, want %q", stepB.Title, "Step B: auth")
	}
	if stepB.ParentID != result.RootID {
		t.Errorf("step-b.ParentID = %q, want %q", stepB.ParentID, result.RootID)
	}
}

func TestInstantiateUsesGraphApplyStoreWhenAvailable(t *testing.T) {
	store := &graphApplySpyStore{MemStore: beads.NewMemStore()}
	GraphApplyEnabled = true
	t.Cleanup(func() { GraphApplyEnabled = false })
	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.step", Title: "Work", Type: "task", Assignee: "worker"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if result.RootID != "bd-1" {
		t.Fatalf("RootID = %q, want bd-1", result.RootID)
	}
	if store.plan == nil {
		t.Fatal("ApplyGraphPlan was not called")
	}
	if len(store.plan.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(store.plan.Nodes))
	}
	step := store.plan.Nodes[1]
	if !step.AssignAfterCreate {
		t.Fatalf("step.AssignAfterCreate = false, want true")
	}
	if got := step.MetadataRefs["gc.root_bead_id"]; got != "wf" {
		t.Fatalf("gc.root_bead_id ref = %q, want wf", got)
	}
	hasParentChild := false
	for _, e := range store.plan.Edges {
		if e.Type == "parent-child" {
			hasParentChild = true
		}
	}
	if !hasParentChild {
		t.Fatalf("edges = %+v, want at least one parent-child edge", store.plan.Edges)
	}
}

func TestBuildRecipeApplyPlan_GraphWorkflowOwnershipUsesTracks(t *testing.T) {
	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.body", Title: "Body", Type: "task", Metadata: map[string]string{"gc.kind": "scope"}},
			{ID: "wf.workflow-finalize", Title: "Finalize", Type: "task", Metadata: map[string]string{"gc.kind": "workflow-finalize"}},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf", DependsOnID: "wf.workflow-finalize", Type: "blocks"},
			{StepID: "wf.workflow-finalize", DependsOnID: "wf.body", Type: "blocks"},
		},
	}

	plan, graphWorkflow, rootKey, err := buildRecipeApplyPlan(recipe, Options{})
	if err != nil {
		t.Fatalf("buildRecipeApplyPlan: %v", err)
	}
	if !graphWorkflow {
		t.Fatal("graphWorkflow = false, want true")
	}
	if rootKey != "wf" {
		t.Fatalf("rootKey = %q, want wf", rootKey)
	}

	var rootBlocksFinalize bool
	var bodyTracksRoot bool
	var finalizeTracksRoot bool
	for _, edge := range plan.Edges {
		if edge.Type == "belongs-to" {
			t.Fatalf("unexpected belongs-to edge in plan: %+v", edge)
		}
		if edge.FromKey == "wf" && edge.ToKey == "wf.workflow-finalize" && edge.Type == "blocks" {
			rootBlocksFinalize = true
		}
		if edge.FromKey == "wf.body" && edge.ToKey == "wf" && edge.Type == "tracks" {
			bodyTracksRoot = true
		}
		if edge.FromKey == "wf.workflow-finalize" && edge.ToKey == "wf" && edge.Type == "tracks" {
			finalizeTracksRoot = true
		}
	}
	if !rootBlocksFinalize {
		t.Fatal("missing root -> workflow-finalize blocks edge")
	}
	if !bodyTracksRoot {
		t.Fatal("missing body -> root tracks ownership edge")
	}
	if !finalizeTracksRoot {
		t.Fatal("missing workflow-finalize -> root tracks ownership edge")
	}
}

func TestInstantiateGraphApplyPreservesStepMetadata(t *testing.T) {
	store := &graphApplySpyStore{MemStore: beads.NewMemStore()}
	GraphApplyEnabled = true
	t.Cleanup(func() { GraphApplyEnabled = false })
	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.step", Title: "Work", Type: "task", Assignee: "worker", Metadata: map[string]string{
				"gc.routed_to":      "test-agent",
				"gc.root_store_ref": "store-ref",
			}},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if result.RootID != "bd-1" {
		t.Fatalf("RootID = %q, want bd-1", result.RootID)
	}
	if store.plan == nil {
		t.Fatal("ApplyGraphPlan was not called")
	}
	step := store.plan.Nodes[1]
	if got := step.Metadata["gc.routed_to"]; got != "test-agent" {
		t.Fatalf("gc.routed_to = %q, want test-agent; full metadata = %v", got, step.Metadata)
	}
	if got := step.Metadata["gc.root_store_ref"]; got != "store-ref" {
		t.Fatalf("gc.root_store_ref = %q, want store-ref; full metadata = %v", got, step.Metadata)
	}
}

func TestInstantiateSequentialPathPreservesStepMetadata(t *testing.T) {
	// Verify the NON-graph-apply (sequential) path also preserves step metadata.
	store := beads.NewMemStore() // MemStore does NOT implement GraphApplyStore
	GraphApplyEnabled = false
	t.Cleanup(func() { GraphApplyEnabled = false })
	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.step", Title: "Work", Type: "task", Assignee: "worker", Metadata: map[string]string{
				"gc.routed_to":      "test-agent",
				"gc.root_store_ref": "store-ref",
			}},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	// Find the step bead by looking at all beads except the root.
	stepID := result.IDMapping["wf.step"]
	if stepID == "" {
		t.Fatal("step bead ID not found in IDMapping")
	}
	stepBead, err := store.Get(stepID)
	if err != nil {
		t.Fatalf("Get(%q): %v", stepID, err)
	}
	if got := stepBead.Metadata["gc.routed_to"]; got != "test-agent" {
		t.Fatalf("gc.routed_to = %q, want test-agent; full metadata = %v", got, stepBead.Metadata)
	}
	if got := stepBead.Metadata["gc.root_store_ref"]; got != "store-ref" {
		t.Fatalf("gc.root_store_ref = %q, want store-ref; full metadata = %v", got, stepBead.Metadata)
	}
}

func TestInstantiateUsesGraphApplyStoreForRetryLogicalRefs(t *testing.T) {
	store := &graphApplySpyStore{MemStore: beads.NewMemStore()}
	GraphApplyEnabled = true
	t.Cleanup(func() { GraphApplyEnabled = false })
	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.review", Title: "Review", Type: "task", Metadata: map[string]string{"gc.kind": "retry"}},
			{ID: "wf.review.run.1", Title: "Review attempt 1", Type: "task", Assignee: "polecat", Metadata: map[string]string{"gc.kind": "retry-run", "gc.attempt": "1"}},
			{ID: "wf.review.eval.1", Title: "Evaluate review attempt 1", Type: "task", Metadata: map[string]string{"gc.kind": "retry-eval", "gc.attempt": "1"}},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.review", DependsOnID: "wf", Type: "parent-child"},
			{StepID: "wf.review.run.1", DependsOnID: "wf.review", Type: "blocks"},
			{StepID: "wf.review.eval.1", DependsOnID: "wf.review.run.1", Type: "blocks"},
			{StepID: "wf.review", DependsOnID: "wf.review.eval.1", Type: "blocks"},
		},
	}

	if _, err := Instantiate(context.Background(), store, recipe, Options{}); err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if store.plan == nil {
		t.Fatal("ApplyGraphPlan was not called")
	}

	nodesByKey := make(map[string]beads.GraphApplyNode, len(store.plan.Nodes))
	for _, node := range store.plan.Nodes {
		nodesByKey[node.Key] = node
	}

	run := nodesByKey["wf.review.run.1"]
	if got := run.MetadataRefs["gc.logical_bead_id"]; got != "wf.review" {
		t.Fatalf("run gc.logical_bead_id ref = %q, want wf.review", got)
	}
	eval := nodesByKey["wf.review.eval.1"]
	if got := eval.MetadataRefs["gc.logical_bead_id"]; got != "wf.review" {
		t.Fatalf("eval gc.logical_bead_id ref = %q, want wf.review", got)
	}
}

func TestInstantiatePriorityOverrideCopiesToAllBeads(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{
		Name: "priority-copy",
		Steps: []formula.RecipeStep{
			{ID: "priority-copy", Title: "Root", Type: "molecule", IsRoot: true, Priority: priorityPtr(4)},
			{ID: "priority-copy.step-a", Title: "Step A", Type: "task"},
			{ID: "priority-copy.step-b", Title: "Step B", Type: "task", Priority: priorityPtr(0)},
		},
		Deps: []formula.RecipeDep{
			{StepID: "priority-copy.step-a", DependsOnID: "priority-copy", Type: "parent-child"},
			{StepID: "priority-copy.step-b", DependsOnID: "priority-copy", Type: "parent-child"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{PriorityOverride: priorityPtr(3)})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != result.Created {
		t.Fatalf("created %d beads, store has %d", result.Created, len(all))
	}
	for _, bead := range all {
		if bead.Priority == nil || *bead.Priority != 3 {
			t.Fatalf("bead %s priority = %v, want 3", bead.ID, bead.Priority)
		}
	}
}

func TestInstantiateUsesGraphApplyPriorityOverride(t *testing.T) {
	store := &graphApplySpyStore{MemStore: beads.NewMemStore()}
	GraphApplyEnabled = true
	t.Cleanup(func() { GraphApplyEnabled = false })

	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.step", Title: "Work", Type: "task", Priority: priorityPtr(0)},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}

	if _, err := Instantiate(context.Background(), store, recipe, Options{PriorityOverride: priorityPtr(2)}); err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if store.plan == nil {
		t.Fatal("ApplyGraphPlan was not called")
	}
	for _, node := range store.plan.Nodes {
		if node.Priority == nil || *node.Priority != 2 {
			t.Fatalf("node %s priority = %v, want 2", node.Key, node.Priority)
		}
	}
}

func TestLogicalRecipeStepIDV2AttemptAndIteration(t *testing.T) {
	// Regression: v2 attempt/iteration beads keep their original kind and use
	// .attempt.N / .iteration.N suffixes. logicalRecipeStepID must strip these
	// to find the control bead's step ID.
	tests := []struct {
		name   string
		step   formula.RecipeStep
		wantID string
		wantOK bool
	}{
		{
			name: "v2 retry attempt",
			step: formula.RecipeStep{
				ID:       "mol-feature.review.attempt.2",
				Metadata: map[string]string{"gc.attempt": "2"},
			},
			wantID: "mol-feature.review",
			wantOK: true,
		},
		{
			name: "v2 ralph iteration",
			step: formula.RecipeStep{
				ID:       "mol-feature.design-review-loop.iteration.1",
				Metadata: map[string]string{"gc.attempt": "1"},
			},
			wantID: "mol-feature.design-review-loop",
			wantOK: true,
		},
		{
			name: "v2 nested iteration",
			step: formula.RecipeStep{
				ID:       "mol-arch.converge.iteration.3",
				Metadata: map[string]string{"gc.attempt": "3", "gc.kind": "scope"},
			},
			wantID: "mol-arch.converge",
			wantOK: true,
		},
		{
			name: "v1 retry-run still works",
			step: formula.RecipeStep{
				ID:       "mol-feature.review.run.1",
				Metadata: map[string]string{"gc.kind": "retry-run", "gc.attempt": "1"},
			},
			wantID: "mol-feature.review",
			wantOK: true,
		},
		{
			name: "no attempt metadata returns false",
			step: formula.RecipeStep{
				ID:       "mol-feature.review",
				Metadata: map[string]string{"gc.kind": "retry"},
			},
			wantID: "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotOK := logicalRecipeStepID(tt.step)
			if gotOK != tt.wantOK || gotID != tt.wantID {
				t.Errorf("logicalRecipeStepID(%q) = (%q, %v), want (%q, %v)",
					tt.step.ID, gotID, gotOK, tt.wantID, tt.wantOK)
			}
		})
	}
}

func TestInstantiateRejectsPartialGraphApplyResult(t *testing.T) {
	store := &graphApplySpyStore{
		MemStore: beads.NewMemStore(),
		result: &beads.GraphApplyResult{
			IDs: map[string]string{
				"wf": "bd-1",
			},
		},
	}
	GraphApplyEnabled = true
	t.Cleanup(func() { GraphApplyEnabled = false })
	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true},
			{ID: "wf.step", Title: "Work", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}

	_, err := Instantiate(context.Background(), store, recipe, Options{})
	if err == nil || !strings.Contains(err.Error(), "wf.step") {
		t.Fatalf("Instantiate error = %v, want missing wf.step mapping", err)
	}
}

func TestInstantiateWithParentID(t *testing.T) {
	store := beads.NewMemStore()

	// Create a parent bead first
	parent, err := store.Create(beads.Bead{Title: "Parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}

	recipe := &formula.Recipe{
		Name: "child-formula",
		Steps: []formula.RecipeStep{
			{ID: "child-formula", Title: "Child", Type: "molecule", IsRoot: true},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{
		ParentID: parent.ID,
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	root, _ := store.Get(result.RootID)
	if root.ParentID != parent.ID {
		t.Errorf("root.ParentID = %q, want %q", root.ParentID, parent.ID)
	}
}

func TestInstantiateGraphWorkflowIgnoresParentIDOnRoot(t *testing.T) {
	store := beads.NewMemStore()

	parent, err := store.Create(beads.Bead{Title: "Source", Type: "task"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	recipe := &formula.Recipe{
		Name: "graph-demo",
		Steps: []formula.RecipeStep{
			{
				ID:       "graph-demo",
				Title:    "Graph Demo",
				Type:     "task",
				IsRoot:   true,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			},
			{
				ID:       "graph-demo.step",
				Title:    "Step",
				Type:     "task",
				Metadata: map[string]string{"gc.kind": "run"},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "graph-demo", DependsOnID: "graph-demo.step", Type: "blocks"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if root.ParentID != "" {
		t.Fatalf("graph workflow root ParentID = %q, want empty", root.ParentID)
	}
	if got := root.Metadata["gc.kind"]; got != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", got)
	}
}

type recordingStore struct {
	beads.Store
	created []beads.Bead
	updates []struct {
		ID   string
		Opts beads.UpdateOpts
	}
}

func (r *recordingStore) Create(b beads.Bead) (beads.Bead, error) {
	r.created = append(r.created, b)
	return r.Store.Create(b)
}

func (r *recordingStore) Update(id string, opts beads.UpdateOpts) error {
	r.updates = append(r.updates, struct {
		ID   string
		Opts beads.UpdateOpts
	}{ID: id, Opts: opts})
	return r.Store.Update(id, opts)
}

func TestInstantiateGraphWorkflowDefersAssignmentsOnlyForFutureBlockers(t *testing.T) {
	base := beads.NewMemStore()
	store := &recordingStore{Store: base}

	recipe := &formula.Recipe{
		Name: "graph-assign",
		Steps: []formula.RecipeStep{
			{
				ID:       "graph-assign",
				Title:    "Graph Assign",
				Type:     "task",
				IsRoot:   true,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			},
			{
				ID:       "graph-assign.run",
				Title:    "Run",
				Type:     "task",
				Assignee: "worker",
			},
			{
				ID:       "graph-assign.setup",
				Title:    "Setup",
				Type:     "task",
				Assignee: "worker",
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "graph-assign", DependsOnID: "graph-assign.run", Type: "blocks"},
			{StepID: "graph-assign.run", DependsOnID: "graph-assign.setup", Type: "blocks"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	createdByRef := make(map[string]beads.Bead, len(store.created))
	for _, created := range store.created {
		createdByRef[created.Ref] = created
	}
	if got := createdByRef["graph-assign.setup"].Assignee; got != "worker" {
		t.Fatalf("setup created assignee = %q, want worker", got)
	}
	if got := createdByRef["graph-assign.run"].Assignee; got != "" {
		t.Fatalf("run created assignee = %q, want empty until blocker wiring completes", got)
	}

	setup, err := base.Get(result.IDMapping["graph-assign.setup"])
	if err != nil {
		t.Fatalf("get setup: %v", err)
	}
	run, err := base.Get(result.IDMapping["graph-assign.run"])
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if setup.Assignee != "worker" {
		t.Fatalf("setup assignee = %q, want worker", setup.Assignee)
	}
	if run.Assignee != "worker" {
		t.Fatalf("run assignee = %q, want worker", run.Assignee)
	}

	if len(store.updates) < 1 {
		t.Fatalf("expected deferred assignment update for graph bead, got %d", len(store.updates))
	}
}

func TestInstantiateWithIdempotencyKey(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{
		Name: "idem-formula",
		Steps: []formula.RecipeStep{
			{ID: "idem-formula", Title: "Root", Type: "molecule", IsRoot: true},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{
		IdempotencyKey: "converge:abc:iter:1",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	root, _ := store.Get(result.RootID)
	if root.Metadata["idempotency_key"] != "converge:abc:iter:1" {
		t.Errorf("idempotency_key = %q, want %q", root.Metadata["idempotency_key"], "converge:abc:iter:1")
	}
}

func TestInstantiateFragmentInheritsRootPriority(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:    "Workflow root",
		Type:     "task",
		Priority: priorityPtr(1),
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	recipe := &formula.FragmentRecipe{
		Steps: []formula.RecipeStep{
			{ID: "frag.scope", Title: "Scope", Type: "task"},
			{ID: "frag.work", Title: "Work", Type: "task", Priority: priorityPtr(4)},
		},
		Deps: []formula.RecipeDep{
			{StepID: "frag.work", DependsOnID: "frag.scope", Type: "blocks"},
		},
	}

	result, err := InstantiateFragment(context.Background(), store, recipe, FragmentOptions{RootID: root.ID})
	if err != nil {
		t.Fatalf("InstantiateFragment: %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("Created = %d, want 2", result.Created)
	}
	for _, id := range result.IDMapping {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("get fragment bead %s: %v", id, err)
		}
		if bead.Priority == nil || *bead.Priority != 1 {
			t.Fatalf("fragment bead %s priority = %v, want 1", bead.ID, bead.Priority)
		}
	}
}

func TestBuildFragmentApplyPlan_UsesTracksOwnershipEdges(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:    "Workflow root",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	recipe := &formula.FragmentRecipe{
		Steps: []formula.RecipeStep{
			{ID: "frag.scope", Title: "Scope", Type: "task"},
			{ID: "frag.work", Title: "Work", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "frag.work", DependsOnID: "frag.scope", Type: "blocks"},
		},
	}

	plan, err := buildFragmentApplyPlan(store, recipe, FragmentOptions{RootID: root.ID})
	if err != nil {
		t.Fatalf("buildFragmentApplyPlan: %v", err)
	}

	tracksToRoot := 0
	for _, edge := range plan.Edges {
		if edge.Type == "belongs-to" {
			t.Fatalf("unexpected belongs-to edge in fragment plan: %+v", edge)
		}
		if edge.Type == "tracks" && edge.ToID == root.ID {
			tracksToRoot++
		}
	}
	if tracksToRoot != len(recipe.Steps) {
		t.Fatalf("tracks ownership edges = %d, want %d", tracksToRoot, len(recipe.Steps))
	}
}

func TestInstantiateRootOnly(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{
		Name:     "patrol",
		RootOnly: true,
		Steps: []formula.RecipeStep{
			{ID: "patrol", Title: "Patrol", Type: "molecule", IsRoot: true},
			{ID: "patrol.scan", Title: "Scan", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "patrol.scan", DependsOnID: "patrol", Type: "parent-child"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	if result.Created != 1 {
		t.Errorf("Created = %d, want 1 (root only)", result.Created)
	}

	all, _ := store.ListOpen()
	if len(all) != 1 {
		t.Errorf("store has %d beads, want 1", len(all))
	}
}

func TestInstantiateVarDefaults(t *testing.T) {
	store := beads.NewMemStore()
	defaultVal := "default-branch"
	recipe := &formula.Recipe{
		Name: "var-test",
		Steps: []formula.RecipeStep{
			{ID: "var-test", Title: "{{title}}", Type: "molecule", IsRoot: true},
			{ID: "var-test.step", Title: "Branch: {{branch}}", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "var-test.step", DependsOnID: "var-test", Type: "parent-child"},
		},
		Vars: map[string]*formula.VarDef{
			"title":  {Description: "Title"},
			"branch": {Description: "Branch", Default: &defaultVal},
		},
	}

	// Don't provide "branch" — should use default
	result, err := Instantiate(context.Background(), store, recipe, Options{
		Vars: map[string]string{"title": "My Thing"},
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	stepID := result.IDMapping["var-test.step"]
	step, _ := store.Get(stepID)
	if step.Title != "Branch: default-branch" {
		t.Errorf("step.Title = %q, want %q", step.Title, "Branch: default-branch")
	}
}

func TestInstantiateSubstitutesAssigneeVars(t *testing.T) {
	store := beads.NewMemStore()
	defaultTarget := "codex"
	recipe := &formula.Recipe{
		Name: "assignee-vars",
		Steps: []formula.RecipeStep{
			{ID: "assignee-vars", Title: "Root", Type: "molecule", IsRoot: true},
			{ID: "assignee-vars.step", Title: "Assigned", Type: "task", Assignee: "{{target}}"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "assignee-vars.step", DependsOnID: "assignee-vars", Type: "parent-child"},
		},
		Vars: map[string]*formula.VarDef{
			"target": {Description: "Target", Default: &defaultTarget},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	step, err := store.Get(result.IDMapping["assignee-vars.step"])
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if step.Assignee != "codex" {
		t.Fatalf("step.Assignee = %q, want codex", step.Assignee)
	}
}

func TestInstantiateSubstitutesLabelVars(t *testing.T) {
	store := beads.NewMemStore()
	epicDefault := "CLOUD-100"
	recipe := &formula.Recipe{
		Name: "label-vars",
		Steps: []formula.RecipeStep{
			{ID: "label-vars", Title: "Root", Type: "molecule", IsRoot: true},
			{ID: "label-vars.step", Title: "Tagged", Type: "task", Labels: []string{"{{epic}}", "review"}},
		},
		Deps: []formula.RecipeDep{
			{StepID: "label-vars.step", DependsOnID: "label-vars", Type: "parent-child"},
		},
		Vars: map[string]*formula.VarDef{
			"epic": {Description: "Epic label", Default: &epicDefault},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	step, err := store.Get(result.IDMapping["label-vars.step"])
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if len(step.Labels) != 2 {
		t.Fatalf("step.Labels = %v, want [CLOUD-100 review]", step.Labels)
	}
	if step.Labels[0] != "CLOUD-100" {
		t.Errorf("step.Labels[0] = %q, want CLOUD-100 (substituted)", step.Labels[0])
	}
	if step.Labels[1] != "review" {
		t.Errorf("step.Labels[1] = %q, want review", step.Labels[1])
	}
}

func TestInstantiateNilRecipe(t *testing.T) {
	store := beads.NewMemStore()
	_, err := Instantiate(context.Background(), store, nil, Options{})
	if err == nil {
		t.Fatal("expected error for nil recipe")
	}
}

func TestInstantiateEmptyRecipe(t *testing.T) {
	store := beads.NewMemStore()
	_, err := Instantiate(context.Background(), store, &formula.Recipe{Name: "empty"}, Options{})
	if err == nil {
		t.Fatal("expected error for empty recipe")
	}
}

// errStore fails on the Nth Create call.
type errStore struct {
	beads.Store
	failOnCreate int
	createCount  int
}

func (e *errStore) Create(b beads.Bead) (beads.Bead, error) {
	e.createCount++
	if e.createCount == e.failOnCreate {
		return beads.Bead{}, fmt.Errorf("injected create failure")
	}
	return e.Store.Create(b)
}

func TestInstantiateCreateFailure(t *testing.T) {
	base := beads.NewMemStore()
	store := &errStore{Store: base, failOnCreate: 2} // fail on second create (first step)

	recipe := &formula.Recipe{
		Name: "fail-test",
		Steps: []formula.RecipeStep{
			{ID: "fail-test", Title: "Root", Type: "molecule", IsRoot: true},
			{ID: "fail-test.step", Title: "Step", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "fail-test.step", DependsOnID: "fail-test", Type: "parent-child"},
		},
	}

	_, err := Instantiate(context.Background(), store, recipe, Options{})
	if err == nil {
		t.Fatal("expected error on create failure")
	}

	// Root bead should exist but be marked as failed
	all, _ := base.ListOpen()
	if len(all) != 1 {
		t.Fatalf("expected 1 bead (root), got %d", len(all))
	}
	root, _ := base.Get(all[0].ID)
	if root.Metadata["molecule_failed"] != "true" {
		t.Error("root bead not marked as molecule_failed")
	}
}

// errDepStore fails on DepAdd.
type errDepStore struct {
	beads.Store
}

func (e *errDepStore) DepAdd(_, _, _ string) error {
	return fmt.Errorf("injected dep failure")
}

func TestInstantiateDepFailure(t *testing.T) {
	base := beads.NewMemStore()
	store := &errDepStore{Store: base}

	recipe := &formula.Recipe{
		Name: "dep-fail",
		Steps: []formula.RecipeStep{
			{ID: "dep-fail", Title: "Root", Type: "molecule", IsRoot: true},
			{ID: "dep-fail.b", Title: "B", Type: "task"},
			{ID: "dep-fail.a", Title: "A", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "dep-fail.b", DependsOnID: "dep-fail", Type: "parent-child"},
			{StepID: "dep-fail.a", DependsOnID: "dep-fail", Type: "parent-child"},
			{StepID: "dep-fail.b", DependsOnID: "dep-fail.a", Type: "blocks"},
		},
	}

	_, err := Instantiate(context.Background(), store, recipe, Options{})
	if err == nil {
		t.Fatal("expected error on dep failure")
	}

	// All beads should be marked as failed
	all, _ := base.ListOpen()
	for _, b := range all {
		full, _ := base.Get(b.ID)
		if full.Metadata["molecule_failed"] != "true" {
			t.Errorf("bead %s not marked as molecule_failed", b.ID)
		}
	}
}

func TestCookOnRequiresParentID(t *testing.T) {
	store := beads.NewMemStore()
	_, err := CookOn(context.Background(), store, "x", nil, Options{})
	if err == nil {
		t.Fatal("expected error when ParentID is empty")
	}
}

func TestCookEndToEnd(t *testing.T) {
	// Write a minimal formula TOML to a temp directory.
	dir := t.TempDir()
	toml := `
formula = "e2e-test"
description = "End-to-end Cook test"

[vars.title]
description = "Title"

[[steps]]
id = "implement"
title = "Implement {{title}}"

[[steps]]
id = "verify"
title = "Verify {{title}}"
depends_on = ["implement"]
`
	if err := os.WriteFile(filepath.Join(dir, "e2e-test.formula.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	store := beads.NewMemStore()
	result, err := Cook(context.Background(), store, "e2e-test", []string{dir}, Options{
		Title: "Auth Flow",
		Vars:  map[string]string{"title": "Auth Flow"},
	})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}

	if result.RootID == "" {
		t.Fatal("RootID is empty")
	}
	if result.Created != 3 {
		t.Errorf("Created = %d, want 3 (root + 2 steps)", result.Created)
	}

	// Verify root bead.
	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if root.Title != "Auth Flow" {
		t.Errorf("root.Title = %q, want %q", root.Title, "Auth Flow")
	}
	if root.Type != "molecule" {
		t.Errorf("root.Type = %q, want %q", root.Type, "molecule")
	}

	// Verify step substitution.
	implID := result.IDMapping["e2e-test.implement"]
	impl, err := store.Get(implID)
	if err != nil {
		t.Fatalf("Get implement: %v", err)
	}
	if impl.Title != "Implement Auth Flow" {
		t.Errorf("implement.Title = %q, want %q", impl.Title, "Implement Auth Flow")
	}

	// Verify dependency wiring.
	verifyID := result.IDMapping["e2e-test.verify"]
	verify, err := store.Get(verifyID)
	if err != nil {
		t.Fatalf("Get verify: %v", err)
	}
	if verify.ParentID != result.RootID {
		t.Errorf("verify.ParentID = %q, want %q", verify.ParentID, result.RootID)
	}
}

func TestCookEndToEndRalph(t *testing.T) {
	formula.FormulaV2Enabled = true
	t.Cleanup(func() { formula.FormulaV2Enabled = false })
	dir := t.TempDir()
	toml := `
formula = "ralph-demo"
description = "Ralph cook test"

[[steps]]
id = "design"
title = "Design"

[[steps]]
id = "implement"
title = "Implement"
needs = ["design"]

[steps.metadata]
custom = "value"

[steps.ralph]
max_attempts = 3

[steps.ralph.check]
mode = "exec"
path = ".gascity/checks/widget.sh"
timeout = "2m"
`
	if err := os.WriteFile(filepath.Join(dir, "ralph-demo.formula.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	store := beads.NewMemStore()
	result, err := Cook(context.Background(), store, "ralph-demo", []string{dir}, Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}

	if result.Created != 5 {
		t.Fatalf("Created = %d, want 5 (root + design + control + spec + iteration)", result.Created)
	}

	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	control, err := store.Get(result.IDMapping["ralph-demo.implement"])
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	spec, err := store.Get(result.IDMapping["ralph-demo.implement.spec"])
	if err != nil {
		t.Fatalf("get spec: %v", err)
	}
	iteration, err := store.Get(result.IDMapping["ralph-demo.implement.iteration.1"])
	if err != nil {
		t.Fatalf("get iteration: %v", err)
	}

	if control.Metadata["gc.kind"] != "ralph" {
		t.Fatalf("control gc.kind = %q, want ralph", control.Metadata["gc.kind"])
	}
	if root.Metadata["gc.kind"] != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", root.Metadata["gc.kind"])
	}
	if root.Type != "task" {
		t.Fatalf("root type = %q, want task", root.Type)
	}
	if control.Metadata["gc.check_mode"] != "exec" {
		t.Fatalf("control gc.check_mode = %q, want exec", control.Metadata["gc.check_mode"])
	}
	if control.Metadata["gc.check_path"] != ".gascity/checks/widget.sh" {
		t.Fatalf("control gc.check_path = %q, want .gascity/checks/widget.sh", control.Metadata["gc.check_path"])
	}
	if _, ok := control.Metadata["gc.source_step_spec"]; ok {
		t.Fatalf("control still has inline gc.source_step_spec metadata")
	}
	if spec.Metadata["gc.kind"] != "spec" {
		t.Fatalf("spec gc.kind = %q, want spec", spec.Metadata["gc.kind"])
	}
	if spec.Metadata["gc.spec_for"] != "implement" {
		t.Fatalf("spec gc.spec_for = %q, want implement", spec.Metadata["gc.spec_for"])
	}
	if spec.Metadata["gc.spec_for_ref"] != "ralph-demo.implement" {
		t.Fatalf("spec gc.spec_for_ref = %q, want ralph-demo.implement", spec.Metadata["gc.spec_for_ref"])
	}
	var frozenSpec formula.Step
	if err := json.Unmarshal([]byte(spec.Description), &frozenSpec); err != nil {
		t.Fatalf("unmarshal spec description: %v", err)
	}
	if frozenSpec.ID != "implement" {
		t.Fatalf("frozen spec id = %q, want implement", frozenSpec.ID)
	}
	if iteration.Metadata["gc.ralph_step_id"] != "implement" {
		t.Fatalf("iteration gc.ralph_step_id = %q, want implement", iteration.Metadata["gc.ralph_step_id"])
	}
	if iteration.Metadata["gc.attempt"] != "1" {
		t.Fatalf("iteration gc.attempt = %q, want 1", iteration.Metadata["gc.attempt"])
	}
	if iteration.ParentID != "" {
		t.Fatalf("iteration ParentID = %q, want detached graph node", iteration.ParentID)
	}
	if iteration.Metadata["gc.root_bead_id"] != result.RootID {
		t.Fatalf("iteration gc.root_bead_id = %q, want %q", iteration.Metadata["gc.root_bead_id"], result.RootID)
	}
	if iteration.Metadata["custom"] != "value" {
		t.Fatalf("iteration custom metadata = %q, want value", iteration.Metadata["custom"])
	}

	// Control bead blocks on iteration.
	controlDeps, err := store.DepList(control.ID, "down")
	if err != nil {
		t.Fatalf("dep list control: %v", err)
	}
	foundIterBlock := false
	for _, dep := range controlDeps {
		if dep.Type == "blocks" && dep.DependsOnID == iteration.ID {
			foundIterBlock = true
			break
		}
	}
	if !foundIterBlock {
		t.Fatalf("control bead does not block on iteration bead; deps=%v", controlDeps)
	}

	rootDeps, err := store.DepList(root.ID, "down")
	if err != nil {
		t.Fatalf("dep list root: %v", err)
	}
	foundDesignBlock := false
	foundControlBlock := false
	for _, dep := range rootDeps {
		if dep.Type != "blocks" {
			continue
		}
		switch dep.DependsOnID {
		case result.IDMapping["ralph-demo.design"]:
			foundDesignBlock = true
		case control.ID:
			foundControlBlock = true
		}
	}
	if !foundDesignBlock {
		t.Fatalf("root bead does not block on design bead; deps=%v", rootDeps)
	}
	if !foundControlBlock {
		t.Fatalf("root bead does not block on control bead; deps=%v", rootDeps)
	}
}

func TestCookEndToEndScopedWorkflowStampsRootAndScopeMetadata(t *testing.T) {
	formula.FormulaV2Enabled = true
	t.Cleanup(func() { formula.FormulaV2Enabled = false })
	dir := t.TempDir()
	toml := `
formula = "scoped-demo"
version = 2

[[steps]]
id = "body"
title = "Body"
needs = ["work"]
metadata = { "gc.kind" = "scope", "gc.scope_role" = "body", "gc.scope_name" = "worktree" }

[[steps]]
id = "work"
title = "Work"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope", "gc.continuation_group" = "main" }

[[steps]]
id = "cleanup"
title = "Cleanup"
needs = ["body"]
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "teardown", "gc.kind" = "cleanup" }
`
	if err := os.WriteFile(filepath.Join(dir, "scoped-demo.formula.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	store := beads.NewMemStore()
	result, err := Cook(context.Background(), store, "scoped-demo", []string{dir}, Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}
	if !result.GraphWorkflow {
		t.Fatal("result.GraphWorkflow = false, want true")
	}

	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got := root.Metadata["gc.kind"]; got != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", got)
	}
	if got := root.Metadata["gc.formula_contract"]; got != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", got)
	}

	body, err := store.Get(result.IDMapping["scoped-demo.body"])
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	if got := body.Metadata["gc.step_ref"]; got != "scoped-demo.body" {
		t.Fatalf("body gc.step_ref = %q, want %q", got, "scoped-demo.body")
	}

	work, err := store.Get(result.IDMapping["scoped-demo.work"])
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if got := work.Metadata["gc.root_bead_id"]; got != result.RootID {
		t.Fatalf("work gc.root_bead_id = %q, want %q", got, result.RootID)
	}
	if got := work.Metadata["gc.step_ref"]; got != "scoped-demo.work" {
		t.Fatalf("work gc.step_ref = %q, want %q", got, "scoped-demo.work")
	}
	if got := work.Metadata["gc.scope_ref"]; got != "body" {
		t.Fatalf("work gc.scope_ref = %q, want %q", got, "body")
	}

	cleanup, err := store.Get(result.IDMapping["scoped-demo.cleanup"])
	if err != nil {
		t.Fatalf("get cleanup: %v", err)
	}
	if got := cleanup.Metadata["gc.scope_ref"]; got != "body" {
		t.Fatalf("cleanup gc.scope_ref = %q, want %q", got, "body")
	}

	scopeCheck, err := store.Get(result.IDMapping["scoped-demo.work-scope-check"])
	if err != nil {
		t.Fatalf("get scope-check: %v", err)
	}
	if got := scopeCheck.Metadata["gc.scope_ref"]; got != "body" {
		t.Fatalf("scope-check gc.scope_ref = %q, want %q", got, "body")
	}
	if got := scopeCheck.Metadata["gc.root_bead_id"]; got != result.RootID {
		t.Fatalf("scope-check gc.root_bead_id = %q, want %q", got, result.RootID)
	}
	if got := scopeCheck.Metadata["gc.step_ref"]; got != "scoped-demo.work-scope-check" {
		t.Fatalf("scope-check gc.step_ref = %q, want %q", got, "scoped-demo.work-scope-check")
	}

	finalizer, err := store.Get(result.IDMapping["scoped-demo.workflow-finalize"])
	if err != nil {
		t.Fatalf("get workflow-finalize: %v", err)
	}
	if got := finalizer.Metadata["gc.root_bead_id"]; got != result.RootID {
		t.Fatalf("workflow-finalize gc.root_bead_id = %q, want %q", got, result.RootID)
	}
}

func TestInstantiateRejectsResidualTitleVars(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{
		Name: "residual-check",
		Steps: []formula.RecipeStep{
			{ID: "residual-check", Title: "{{title}}", Type: "molecule", IsRoot: true},
			{ID: "residual-check.step-a", Title: "[{{epic}}] Implement: {{feature}}", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "residual-check.step-a", DependsOnID: "residual-check", Type: "parent-child"},
		},
		Vars: map[string]*formula.VarDef{
			"title":   {Description: "Title"},
			"epic":    {Description: "Epic ID"},
			"feature": {Description: "Feature slug"},
		},
	}

	t.Run("unresolved vars in child title rejected", func(t *testing.T) {
		_, err := Instantiate(context.Background(), store, recipe, Options{
			Title: "My Feature",
			Vars:  map[string]string{"epic": "CLOUD-123"},
		})
		if err == nil {
			t.Fatal("Instantiate should reject unresolved {{feature}} in step title")
		}
		if !strings.Contains(err.Error(), "unresolved variable") {
			t.Errorf("unexpected error: %v", err)
		}
		if !strings.Contains(err.Error(), "feature") {
			t.Errorf("error should mention 'feature': %v", err)
		}
	})

	t.Run("all vars resolved succeeds", func(t *testing.T) {
		result, err := Instantiate(context.Background(), store, recipe, Options{
			Title: "My Feature",
			Vars:  map[string]string{"epic": "CLOUD-123", "feature": "auth"},
		})
		if err != nil {
			t.Fatalf("Instantiate should succeed: %v", err)
		}
		if result.Created != 2 {
			t.Errorf("Created = %d, want 2", result.Created)
		}
	})

	t.Run("root title override bypasses residual check", func(t *testing.T) {
		// Root step has {{title}} but opts.Title overrides it — should succeed
		// even without providing the "title" var.
		result, err := Instantiate(context.Background(), store, &formula.Recipe{
			Name: "root-override",
			Steps: []formula.RecipeStep{
				{ID: "root-override", Title: "{{title}}", Type: "molecule", IsRoot: true},
			},
			Vars: map[string]*formula.VarDef{"title": {Description: "Title"}},
		}, Options{Title: "Overridden"})
		if err != nil {
			t.Fatalf("should succeed with title override: %v", err)
		}
		if result.Created != 1 {
			t.Errorf("Created = %d, want 1", result.Created)
		}
	})

	t.Run("graph-apply path rejects unresolved vars", func(t *testing.T) {
		gaStore := &graphApplySpyStore{MemStore: beads.NewMemStore()}
		GraphApplyEnabled = true
		t.Cleanup(func() { GraphApplyEnabled = false })

		_, err := Instantiate(context.Background(), gaStore, recipe, Options{
			Title: "My Feature",
			Vars:  map[string]string{"epic": "CLOUD-123"},
		})
		if err == nil {
			t.Fatal("graph-apply Instantiate should reject unresolved {{feature}}")
		}
		if !strings.Contains(err.Error(), "unresolved variable") {
			t.Errorf("unexpected error: %v", err)
		}
		if !strings.Contains(err.Error(), "feature") {
			t.Errorf("error should mention 'feature': %v", err)
		}
	})
}

func TestInstantiateFragmentRejectsResidualTitleVars(t *testing.T) {
	fragment := &formula.FragmentRecipe{
		Name: "frag-residual",
		Steps: []formula.RecipeStep{
			{ID: "frag-residual.step-a", Title: "[{{epic}}] Implement: {{feature}}", Type: "task"},
		},
		Vars: map[string]*formula.VarDef{
			"epic":    {Description: "Epic ID"},
			"feature": {Description: "Feature slug"},
		},
	}

	t.Run("sequential path rejects unresolved vars", func(t *testing.T) {
		store := beads.NewMemStore()
		root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
		if err != nil {
			t.Fatal(err)
		}

		GraphApplyEnabled = false
		t.Cleanup(func() { GraphApplyEnabled = false })

		_, err = InstantiateFragment(context.Background(), store, fragment, FragmentOptions{
			RootID: root.ID,
			Vars:   map[string]string{"epic": "CLOUD-123"},
		})
		if err == nil {
			t.Fatal("InstantiateFragment should reject unresolved {{feature}}")
		}
		if !strings.Contains(err.Error(), "unresolved variable") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("graph-apply path rejects unresolved vars", func(t *testing.T) {
		gaStore := &graphApplySpyStore{MemStore: beads.NewMemStore()}
		root, err := gaStore.Create(beads.Bead{Title: "root", Type: "molecule"})
		if err != nil {
			t.Fatal(err)
		}

		GraphApplyEnabled = true
		t.Cleanup(func() { GraphApplyEnabled = false })

		_, err = InstantiateFragment(context.Background(), gaStore, fragment, FragmentOptions{
			RootID: root.ID,
			Vars:   map[string]string{"epic": "CLOUD-123"},
		})
		if err == nil {
			t.Fatal("graph-apply InstantiateFragment should reject unresolved {{feature}}")
		}
		if !strings.Contains(err.Error(), "unresolved variable") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
