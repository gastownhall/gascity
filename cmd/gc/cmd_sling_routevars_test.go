package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

func TestDecorateGraphWorkflowRecipeSubstitutesRouteTargetsWithinRigContext(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "claude", Dir: "frontend"},
			{Name: "codex", Dir: "frontend"},
			{Name: "workflow-control", Dir: "frontend", Pool: &config.PoolConfig{Min: 1, Max: 1}},
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
				ID:       "demo.design",
				Title:    "Design",
				Type:     "task",
				Assignee: "{{design_target}}",
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

	if err := decorateGraphWorkflowRecipe(recipe, graphWorkflowRouteVars(recipe, nil), "", "", "", "", "frontend/claude", claudeSession, store, cfg.Workspace.Name, cfg); err != nil {
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
