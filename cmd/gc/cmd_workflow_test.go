package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

func TestDecorateDynamicFragmentRecipeSupportsExplicitPerStepAgents(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor"},
			{Name: "reviewer"},
		},
	}

	mayorSession := lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "mayor", cfg.Workspace.SessionTemplate)
	reviewerSession := lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "reviewer", cfg.Workspace.SessionTemplate)

	source := beads.Bead{
		ID:       "gc-source",
		Title:    "Source",
		Assignee: mayorSession,
		Metadata: map[string]string{
			"gc.routed_to": "mayor",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:       "expansion-review.review",
				Title:    "Review",
				Assignee: "reviewer",
			},
			{
				ID:    "expansion-review.review-scope-check",
				Title: "Finalize review",
				Metadata: map[string]string{
					"gc.kind":        "scope-check",
					"gc.control_for": "expansion-review.review",
				},
			},
			{
				ID:    "expansion-review.submit",
				Title: "Submit",
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "expansion-review.review-scope-check", DependsOnID: "expansion-review.review", Type: "blocks"},
			{StepID: "expansion-review.submit", DependsOnID: "expansion-review.review-scope-check", Type: "blocks"},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}

	steps := map[string]formula.RecipeStep{}
	for _, step := range fragment.Steps {
		steps[step.ID] = step
	}

	review := steps["expansion-review.review"]
	if review.Assignee != reviewerSession {
		t.Fatalf("review assignee = %q, want %q", review.Assignee, reviewerSession)
	}
	if review.Metadata["gc.routed_to"] != "reviewer" {
		t.Fatalf("review gc.routed_to = %q, want reviewer", review.Metadata["gc.routed_to"])
	}

	control := steps["expansion-review.review-scope-check"]
	if control.Assignee != reviewerSession {
		t.Fatalf("review scope-check assignee = %q, want %q", control.Assignee, reviewerSession)
	}
	if control.Metadata["gc.routed_to"] != "reviewer" {
		t.Fatalf("review scope-check gc.routed_to = %q, want reviewer", control.Metadata["gc.routed_to"])
	}

	submit := steps["expansion-review.submit"]
	if submit.Assignee != mayorSession {
		t.Fatalf("submit assignee = %q, want %q", submit.Assignee, mayorSession)
	}
	if submit.Metadata["gc.routed_to"] != "mayor" {
		t.Fatalf("submit gc.routed_to = %q, want mayor", submit.Metadata["gc.routed_to"])
	}
}

func TestWorkflowFormulaSearchPathsUsesRoutedRigLayers(t *testing.T) {
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"frontend": {"/city/formulas", "/rig/frontend/formulas"},
			},
		},
	}

	paths := workflowFormulaSearchPaths(cfg, beads.Bead{
		Metadata: map[string]string{"gc.routed_to": "frontend/reviewer"},
	})
	if len(paths) != 2 || paths[1] != "/rig/frontend/formulas" {
		t.Fatalf("workflowFormulaSearchPaths(frontend) = %#v, want rig-specific layers", paths)
	}

	fallback := workflowFormulaSearchPaths(cfg, beads.Bead{
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})
	if len(fallback) != 1 || fallback[0] != "/city/formulas" {
		t.Fatalf("workflowFormulaSearchPaths(mayor) = %#v, want city layers", fallback)
	}
}

func TestDecorateDynamicFragmentRecipePreservesPoolFallbackAndScopeMetadata(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "frontend", Pool: &config.PoolConfig{Min: 1, Max: 3}},
		},
	}

	source := beads.Bead{
		ID:    "gc-source",
		Title: "Source",
		Metadata: map[string]string{
			"gc.routed_to": "frontend/reviewer",
			"gc.scope_ref": "body",
			"gc.on_fail":   "abort_scope",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:    "expansion-review.review",
				Title: "Review",
			},
			{
				ID:    "expansion-review.review-scope-check",
				Title: "Finalize review",
				Metadata: map[string]string{
					"gc.kind":        "scope-check",
					"gc.control_for": "expansion-review.review",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "expansion-review.review-scope-check", DependsOnID: "expansion-review.review", Type: "blocks"},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}

	steps := map[string]formula.RecipeStep{}
	for _, step := range fragment.Steps {
		steps[step.ID] = step
	}

	review := steps["expansion-review.review"]
	if review.Assignee != "" {
		t.Fatalf("review assignee = %q, want empty for pool-routed work", review.Assignee)
	}
	if review.Metadata["gc.routed_to"] != "frontend/reviewer" {
		t.Fatalf("review gc.routed_to = %q, want frontend/reviewer", review.Metadata["gc.routed_to"])
	}
	foundPoolLabel := false
	for _, label := range review.Labels {
		if label == "pool:frontend/reviewer" {
			foundPoolLabel = true
		}
	}
	if !foundPoolLabel {
		t.Fatalf("review labels = %#v, want pool label", review.Labels)
	}
	if review.Metadata["gc.scope_ref"] != "body" {
		t.Fatalf("review gc.scope_ref = %q, want body", review.Metadata["gc.scope_ref"])
	}
	if review.Metadata["gc.on_fail"] != "abort_scope" {
		t.Fatalf("review gc.on_fail = %q, want abort_scope", review.Metadata["gc.on_fail"])
	}
	if review.Metadata["gc.scope_role"] != "member" {
		t.Fatalf("review gc.scope_role = %q, want member", review.Metadata["gc.scope_role"])
	}

	control := steps["expansion-review.review-scope-check"]
	if control.Metadata["gc.scope_ref"] != "body" {
		t.Fatalf("control gc.scope_ref = %q, want body", control.Metadata["gc.scope_ref"])
	}
	if control.Metadata["gc.scope_role"] != "control" {
		t.Fatalf("control gc.scope_role = %q, want control", control.Metadata["gc.scope_role"])
	}
}

func TestDecorateDynamicFragmentRecipeUsesSourceRouteRigContextForBareTargets(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "frontend"},
			{Name: "reviewer", Dir: "backend"},
		},
	}

	source := beads.Bead{
		ID:    "gc-source",
		Title: "Source",
		Metadata: map[string]string{
			"gc.routed_to": "frontend/reviewer",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:       "expansion-review.review",
				Title:    "Review",
				Assignee: "reviewer",
			},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}

	review := fragment.Steps[0]
	wantSession := lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "frontend/reviewer", cfg.Workspace.SessionTemplate)
	if review.Assignee != wantSession {
		t.Fatalf("review assignee = %q, want %q", review.Assignee, wantSession)
	}
	if review.Metadata["gc.routed_to"] != "frontend/reviewer" {
		t.Fatalf("review gc.routed_to = %q, want frontend/reviewer", review.Metadata["gc.routed_to"])
	}
}

func TestDecorateDynamicFragmentRecipeSynthesizesInheritedScopeChecks(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "reviewer"},
		},
	}

	source := beads.Bead{
		ID:    "gc-source",
		Title: "Source",
		Metadata: map[string]string{
			"gc.routed_to":     "reviewer",
			"gc.scope_ref":     "body",
			"gc.on_fail":       "abort_scope",
			"gc.step_id":       "review-loop",
			"gc.ralph_step_id": "review-loop",
			"gc.attempt":       "2",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:    "expansion-review.review",
				Title: "Review",
			},
			{
				ID:    "expansion-review.submit",
				Title: "Submit",
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "expansion-review.submit", DependsOnID: "expansion-review.review", Type: "blocks"},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}

	steps := map[string]formula.RecipeStep{}
	for _, step := range fragment.Steps {
		steps[step.ID] = step
	}

	control, ok := steps["expansion-review.review-scope-check"]
	if !ok {
		t.Fatal("missing synthesized review scope-check")
	}
	if control.Metadata["gc.scope_ref"] != "body" {
		t.Fatalf("review scope-check gc.scope_ref = %q, want body", control.Metadata["gc.scope_ref"])
	}
	if control.Metadata["gc.routed_to"] != "reviewer" {
		t.Fatalf("review scope-check gc.routed_to = %q, want reviewer", control.Metadata["gc.routed_to"])
	}
	if control.Metadata["gc.attempt"] != "2" || control.Metadata["gc.ralph_step_id"] != "review-loop" || control.Metadata["gc.step_id"] != "review-loop" {
		t.Fatalf("review scope-check trace metadata = %#v, want inherited attempt/step ids", control.Metadata)
	}

	var sawRewritten bool
	for _, dep := range fragment.Deps {
		if dep.StepID == "expansion-review.submit" && dep.DependsOnID == "expansion-review.review-scope-check" && dep.Type == "blocks" {
			sawRewritten = true
			break
		}
	}
	if !sawRewritten {
		t.Fatal("submit dependency was not rewritten to synthesized scope-check")
	}
}

func TestResolveGraphStepBindingWorkflowFinalizeUsesFallback(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor"},
			{Name: "reviewer"},
		},
	}

	stepByID := map[string]*formula.RecipeStep{
		"demo.review": {
			ID:       "demo.review",
			Title:    "Review",
			Assignee: "reviewer",
		},
		"demo.workflow-finalize": {
			ID:    "demo.workflow-finalize",
			Title: "Finalize workflow",
			Metadata: map[string]string{
				"gc.kind": "workflow-finalize",
			},
		},
	}
	depsByStep := map[string][]string{
		"demo.workflow-finalize": {"demo.review"},
	}
	fallback := graphRouteBinding{
		qualifiedName: "mayor",
		sessionName:   lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "mayor", cfg.Workspace.SessionTemplate),
	}

	binding, err := resolveGraphStepBinding("demo.workflow-finalize", stepByID, nil, depsByStep, map[string]graphRouteBinding{}, map[string]bool{}, fallback, "", store, cfg.Workspace.Name, cfg)
	if err != nil {
		t.Fatalf("resolveGraphStepBinding(workflow-finalize): %v", err)
	}
	if binding.qualifiedName != "mayor" || binding.sessionName != fallback.sessionName {
		t.Fatalf("binding = %+v, want fallback %+v", binding, fallback)
	}
}

func TestResolveGraphStepBindingCheckRejectsInconsistentDeps(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "reviewer-a"},
			{Name: "reviewer-b"},
		},
	}

	stepByID := map[string]*formula.RecipeStep{
		"demo.review-a": {
			ID:       "demo.review-a",
			Title:    "Review A",
			Assignee: "reviewer-a",
		},
		"demo.review-b": {
			ID:       "demo.review-b",
			Title:    "Review B",
			Assignee: "reviewer-b",
		},
		"demo.check": {
			ID:    "demo.check",
			Title: "Check",
			Metadata: map[string]string{
				"gc.kind": "check",
			},
		},
	}
	depsByStep := map[string][]string{
		"demo.check": {"demo.review-a", "demo.review-b"},
	}
	fallback := graphRouteBinding{
		qualifiedName: "reviewer-a",
		sessionName:   lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "reviewer-a", cfg.Workspace.SessionTemplate),
	}

	if _, err := resolveGraphStepBinding("demo.check", stepByID, nil, depsByStep, map[string]graphRouteBinding{}, map[string]bool{}, fallback, "", store, cfg.Workspace.Name, cfg); err == nil || !strings.Contains(err.Error(), "inconsistent check routing") {
		t.Fatalf("resolveGraphStepBinding(check) error = %v, want inconsistent check routing", err)
	}
}
