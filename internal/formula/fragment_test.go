package formula

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileExpansionFragmentRunsInlineExpansionAndConditionFiltering(t *testing.T) {
	dir := t.TempDir()

	leaf := `
formula = "leaf-expand"
type = "expansion"
version = 2

[[template]]
id = "{target}.draft"
title = "Draft"
`
	if err := os.WriteFile(filepath.Join(dir, "leaf-expand.formula.toml"), []byte(leaf), 0o644); err != nil {
		t.Fatalf("write leaf expansion: %v", err)
	}

	parent := `
formula = "parent-expand"
type = "expansion"
version = 2

[[template]]
id = "{target}.worker"
title = "Worker"
expand = "leaf-expand"
`
	if err := os.WriteFile(filepath.Join(dir, "parent-expand.formula.toml"), []byte(parent), 0o644); err != nil {
		t.Fatalf("write parent expansion: %v", err)
	}

	target := &Step{ID: "demo.target", Title: "Target"}
	fragment, err := CompileExpansionFragment(context.Background(), "parent-expand", []string{dir}, target, nil)
	if err != nil {
		t.Fatalf("CompileExpansionFragment: %v", err)
	}

	var sawDraft bool
	for _, step := range fragment.Steps {
		if strings.HasSuffix(step.ID, ".draft") {
			sawDraft = true
		}
	}
	if !sawDraft {
		t.Fatal("expected inline expansion step with .draft suffix in fragment")
	}
}

func TestApplyFragmentRecipeGraphControlsAddsInheritedScopeChecks(t *testing.T) {
	fragment := &FragmentRecipe{
		Name: "expansion-review",
		Steps: []RecipeStep{
			{
				ID:    "expansion-review.review",
				Title: "Review",
				Metadata: map[string]string{
					"gc.scope_ref":  "body",
					"gc.scope_role": "member",
				},
			},
			{
				ID:    "expansion-review.submit",
				Title: "Submit",
				Metadata: map[string]string{
					"gc.scope_ref":  "body",
					"gc.scope_role": "member",
				},
			},
		},
		Deps: []RecipeDep{
			{StepID: "expansion-review.submit", DependsOnID: "expansion-review.review", Type: "blocks"},
		},
	}

	ApplyFragmentRecipeGraphControls(fragment)

	stepByID := make(map[string]RecipeStep, len(fragment.Steps))
	for _, step := range fragment.Steps {
		stepByID[step.ID] = step
	}
	control, ok := stepByID["expansion-review.review-scope-check"]
	if !ok {
		t.Fatal("missing synthesized scope-check")
	}
	if control.Metadata["gc.kind"] != "scope-check" {
		t.Fatalf("control gc.kind = %q, want scope-check", control.Metadata["gc.kind"])
	}
	if control.Metadata["gc.scope_ref"] != "body" {
		t.Fatalf("control gc.scope_ref = %q, want body", control.Metadata["gc.scope_ref"])
	}

	var sawControlDep, sawRewrittenSubmit bool
	for _, dep := range fragment.Deps {
		switch {
		case dep.StepID == "expansion-review.review-scope-check" && dep.DependsOnID == "expansion-review.review" && dep.Type == "blocks":
			sawControlDep = true
		case dep.StepID == "expansion-review.submit" && dep.DependsOnID == "expansion-review.review-scope-check" && dep.Type == "blocks":
			sawRewrittenSubmit = true
		}
	}
	if !sawControlDep {
		t.Fatal("missing review -> scope-check dependency")
	}
	if !sawRewrittenSubmit {
		t.Fatal("submit dependency was not rewritten to scope-check")
	}
}

func TestExpandStepDoesNotMutateSharedTemplateState(t *testing.T) {
	t.Parallel()

	template := []*Step{{
		ID:    "{target}.worker",
		Title: "Worker {target.title}",
		ExpandVars: map[string]string{
			"who": "{target.id}",
		},
		Gate: &Gate{
			Type:    "gh:run",
			ID:      "{target.id}",
			Timeout: "{target.title}",
		},
		Loop: &LoopSpec{
			Until: "{target.id}",
			Range: "{target.title}",
			Var:   "item",
			Body: []*Step{{
				ID:    "{target}.loop",
				Title: "Loop {target.title}",
			}},
		},
	}}

	first, err := expandStep(&Step{ID: "alpha", Title: "Alpha"}, template, 0, nil)
	if err != nil {
		t.Fatalf("expandStep(alpha): %v", err)
	}
	second, err := expandStep(&Step{ID: "beta", Title: "Beta"}, template, 0, nil)
	if err != nil {
		t.Fatalf("expandStep(beta): %v", err)
	}

	if got := first[0].ExpandVars["who"]; got != "alpha" {
		t.Fatalf("first ExpandVars[who] = %q, want alpha", got)
	}
	if got := second[0].ExpandVars["who"]; got != "beta" {
		t.Fatalf("second ExpandVars[who] = %q, want beta", got)
	}
	if got := second[0].Gate.ID; got != "beta" {
		t.Fatalf("second gate id = %q, want beta", got)
	}
	if got := second[0].Loop.Until; got != "beta" {
		t.Fatalf("second loop until = %q, want beta", got)
	}
	if got := second[0].Loop.Body[0].ID; got != "beta.loop" {
		t.Fatalf("second loop body id = %q, want beta.loop", got)
	}

	if got := template[0].ExpandVars["who"]; got != "{target.id}" {
		t.Fatalf("template ExpandVars mutated to %q", got)
	}
	if got := template[0].Gate.ID; got != "{target.id}" {
		t.Fatalf("template gate id mutated to %q", got)
	}
	if got := template[0].Loop.Body[0].ID; got != "{target}.loop" {
		t.Fatalf("template loop body id mutated to %q", got)
	}
}

func TestFragmentSinkStepIDsExcludesSpecBeads(t *testing.T) {
	t.Parallel()

	fragment := &FragmentRecipe{
		Name: "expansion-retry",
		Steps: []RecipeStep{
			{
				ID:    "expansion-retry.control",
				Title: "Retry control",
				Metadata: map[string]string{
					"gc.kind": "retry",
				},
			},
			{
				ID:    "expansion-retry.control.spec",
				Title: "Step spec for retry control",
				Type:  "spec",
				Metadata: map[string]string{
					"gc.kind":         "spec",
					"gc.spec_for":     "control",
					"gc.spec_for_ref": "expansion-retry.control",
				},
			},
			{
				ID:    "expansion-retry.work",
				Title: "Work step",
				Metadata: map[string]string{
					"gc.kind": "",
				},
			},
		},
		Deps: []RecipeDep{
			{StepID: "expansion-retry.work", DependsOnID: "expansion-retry.control", Type: "blocks"},
		},
	}

	sinks := fragmentSinkStepIDs(fragment)

	for _, id := range sinks {
		if id == "expansion-retry.control.spec" {
			t.Fatal("spec bead should not appear in fragment sinks")
		}
	}

	var sawWork bool
	for _, id := range sinks {
		if id == "expansion-retry.work" {
			sawWork = true
		}
	}
	if !sawWork {
		t.Fatal("expected work step in fragment sinks")
	}
}

func TestRecipeStepNeedsScopeCheckExcludesSpec(t *testing.T) {
	t.Parallel()

	step := RecipeStep{
		ID:    "test.spec",
		Title: "Step spec",
		Metadata: map[string]string{
			"gc.kind":      "spec",
			"gc.scope_ref": "body",
		},
	}
	if recipeStepNeedsScopeCheck(step) {
		t.Fatal("spec step should not need scope check")
	}
}
