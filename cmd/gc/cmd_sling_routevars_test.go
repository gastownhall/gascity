package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sling"
)

func TestDecorateGraphWorkflowRecipeSubstitutesRouteTargetsWithinRigContext(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "claude", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "codex", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "control-dispatcher", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	defaultTarget := "codex"
	recipe := &formula.Recipe{
		Name: "demo",
		Vars: map[string]*formula.VarDef{
			"design_target": {Default: &defaultTarget},
		},
		Steps: []formula.RecipeStep{
			{
				ID:       "demo",
				Title:    "Root",
				Type:     "task",
				IsRoot:   true,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			},
			{
				ID:    "demo.design",
				Title: "Design",
				Type:  "task",
				Metadata: map[string]string{
					"gc.run_target": "{{design_target}}",
				},
			},
			{
				ID:    "demo.review",
				Title: "Review",
				Type:  "task",
				Metadata: map[string]string{
					"gc.run_target": "{{design_target}}",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "demo.design", DependsOnID: "demo", Type: "parent-child"},
			{StepID: "demo.review", DependsOnID: "demo.design", Type: "blocks"},
		},
	}

	claudeSession := lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "frontend/claude", cfg.Workspace.SessionTemplate)
	codexSession := lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "frontend/codex", cfg.Workspace.SessionTemplate)
	if claudeSession == "" || codexSession == "" {
		t.Fatalf("expected non-empty sessions for frontend agents, got claude=%q codex=%q", claudeSession, codexSession)
	}

	if err := decorateGraphWorkflowRecipe(recipe, graphWorkflowRouteVars(recipe, nil), "frontend/claude", claudeSession, store, cfg.Workspace.Name, "", cfg); err != nil {
		t.Fatalf("decorateGraphWorkflowRecipe: %v", err)
	}

	design := recipe.StepByID("demo.design")
	if design == nil {
		t.Fatal("design step missing after decorate")
	}
	if design.Metadata["gc.routed_to"] != "frontend/codex" {
		t.Fatalf("design gc.routed_to = %q, want frontend/codex", design.Metadata["gc.routed_to"])
	}
	if design.Assignee != codexSession {
		t.Fatalf("design assignee = %q, want %q", design.Assignee, codexSession)
	}

	review := recipe.StepByID("demo.review")
	if review == nil {
		t.Fatal("review step missing after decorate")
	}
	if review.Metadata["gc.routed_to"] != "frontend/codex" {
		t.Fatalf("review gc.routed_to = %q, want frontend/codex", review.Metadata["gc.routed_to"])
	}
	if review.Assignee != codexSession {
		t.Fatalf("review assignee = %q, want %q", review.Assignee, codexSession)
	}
}

func TestGraphWorkflowRouteVarsCallerOverridesDefaults(t *testing.T) {
	defaultTarget := "codex"
	recipe := &formula.Recipe{
		Vars: map[string]*formula.VarDef{
			"design_target": {Default: &defaultTarget},
		},
	}

	routeVars := graphWorkflowRouteVars(recipe, map[string]string{"design_target": "claude"})
	if got := routeVars["design_target"]; got != "claude" {
		t.Fatalf("routeVars[design_target] = %q, want claude", got)
	}
}

// graphApplySpyStore wraps a MemStore and captures the graph apply plan
// for inspection. It implements beads.GraphApplyStore.
type graphApplySpyStore struct {
	*beads.MemStore
	plan *beads.GraphApplyPlan
}

func (s *graphApplySpyStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) { //nolint:unparam // interface compliance; error always nil in spy
	s.plan = plan
	ids := make(map[string]string, len(plan.Nodes))
	for i, node := range plan.Nodes {
		ids[node.Key] = fmt.Sprintf("gc-%d", i+1)
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

// TestInstantiateSlingFormulaGraphWorkflowPreservesRoutedTo tests the full
// code path: compile v2 formula -> decorateGraphWorkflowRecipe -> molecule.Instantiate
// -> graph apply plan, verifying gc.routed_to appears in the plan's node metadata.
func TestInstantiateSlingFormulaGraphWorkflowPreservesRoutedTo(t *testing.T) {
	// Create a v2 formula on disk.
	formulaDir := t.TempDir()
	formulaContent := `
formula = "wf-test"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Do work"
type = "task"
`
	if err := os.WriteFile(filepath.Join(formulaDir, "wf-test.formula.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Enable graph workflow features.
	formulatest.EnableV2ForTest(t)
	prevGraphApply := molecule.IsGraphApplyEnabled()
	molecule.SetGraphApplyEnabled(true)
	t.Cleanup(func() {
		molecule.SetGraphApplyEnabled(prevGraphApply)
	})

	store := &graphApplySpyStore{MemStore: beads.NewMemStore()}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	deps := slingDeps{
		CityName: "test-city",
		CityPath: "/city",
		Cfg:      cfg,
		Store:    store,
		StoreRef: "city:test-city",
		Resolver: cliAgentResolver{},
		Notify:   cliNotifier{},
	}

	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	result, err := sling.InstantiateSlingFormula(
		context.Background(),
		"wf-test",
		[]string{formulaDir},
		molecule.Options{},
		"", "", "", a, deps,
	)
	if err != nil {
		t.Fatalf("InstantiateSlingFormula: %v", err)
	}
	if result.RootID == "" {
		t.Fatal("RootID is empty")
	}
	if !result.GraphWorkflow {
		t.Fatal("result.GraphWorkflow = false, want true")
	}
	if store.plan == nil {
		t.Fatal("GraphApplyPlan was not captured — graph apply path not taken")
	}

	// Find the non-root step node in the plan.
	var stepNode *beads.GraphApplyNode
	for i, node := range store.plan.Nodes {
		if node.Key != "wf-test" { // skip root
			stepNode = &store.plan.Nodes[i]
			break
		}
	}
	if stepNode == nil {
		t.Fatal("no non-root step node found in plan")
	}

	// This is the critical assertion: gc.routed_to must be set by
	// decorateGraphWorkflowRecipe and preserved in the graph apply plan.
	if got := stepNode.Metadata["gc.routed_to"]; got != "worker" {
		t.Fatalf("gc.routed_to = %q, want %q; full metadata = %v", got, "worker", stepNode.Metadata)
	}
}
