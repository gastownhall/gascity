package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/session"
)

func TestPhase0WorkflowRouting_ConcreteSessionAssigneeBeatsTemplateCollision(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(2)},
			{Name: "control-dispatcher", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession, "template:frontend/worker"},
		Metadata: map[string]string{
			"session_name": "s-test-city-frontend-worker",
			"alias":        "frontend/worker-live",
			"template":     "frontend/worker",
			"state":        "active",
		},
	}}, nil)

	recipe := &formula.Recipe{
		Name: "demo",
		Steps: []formula.RecipeStep{
			{
				ID:       "demo",
				Title:    "Root",
				Type:     "task",
				IsRoot:   true,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			},
			{
				ID:       "demo.review",
				Title:    "Review",
				Type:     "task",
				Assignee: "worker",
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "demo.review", DependsOnID: "demo", Type: "parent-child"},
		},
	}

	if err := decorateGraphWorkflowRecipe(recipe, graphWorkflowRouteVars(recipe, nil), "frontend/origin", "s-origin", store, cfg.Workspace.Name, "", cfg); err != nil {
		t.Fatalf("decorateGraphWorkflowRecipe: %v", err)
	}

	review := recipe.StepByID("demo.review")
	if review == nil {
		t.Fatal("review step missing after decorate")
	}
	if review.Assignee != "worker" {
		t.Fatalf("review assignee = %q, want concrete session bead ID worker", review.Assignee)
	}
	if got := review.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("review gc.routed_to = %q, want empty for direct session target", got)
	}
}
