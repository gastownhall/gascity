package formula

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileExpansionFragmentRunsInlineExpansionAndConditionFiltering(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()

	leaf := `
formula = "leaf-expand"
type = "expansion"
version = 2

[[template]]
id = "{target}.draft"
title = "Draft"
`
	if err := os.WriteFile(filepath.Join(dir, "leaf-expand.toml"), []byte(leaf), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(dir, "parent-expand.toml"), []byte(parent), 0o644); err != nil {
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

func TestCompileExpansionFragmentValidatesRequiredVars(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()

	expansion := `
formula = "expand-required"
type = "expansion"
version = 2

[vars.feature]
description = "Feature slug"
required = true

[[template]]
id = "{target}.implement"
title = "[{target.title}] Implement: {{feature}}"
`
	if err := os.WriteFile(filepath.Join(dir, "expand-required.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion: %v", err)
	}

	target := &Step{ID: "demo.target", Title: "Target"}

	t.Run("missing required var rejected", func(t *testing.T) {
		// Pass a non-empty map (one var provided, one required var missing)
		// to trigger ValidateVars. Empty maps skip validation.
		_, err := CompileExpansionFragment(context.Background(), "expand-required", []string{dir}, target, map[string]string{"unrelated": "value"})
		if err == nil {
			t.Fatal("CompileExpansionFragment should reject missing required var")
		}
		if !strings.Contains(err.Error(), "variable validation failed") {
			t.Errorf("unexpected error: %v", err)
		}
		if !strings.Contains(err.Error(), `"feature" is required`) {
			t.Errorf("error should mention feature: %v", err)
		}
	})

	t.Run("nil vars skips validation", func(t *testing.T) {
		_, err := CompileExpansionFragment(context.Background(), "expand-required", []string{dir}, target, nil)
		if err != nil {
			t.Fatalf("nil vars should skip validation: %v", err)
		}
	})

	t.Run("all required vars provided", func(t *testing.T) {
		fragment, err := CompileExpansionFragment(context.Background(), "expand-required", []string{dir}, target, map[string]string{"feature": "auth"})
		if err != nil {
			t.Fatalf("should succeed with all vars: %v", err)
		}
		if len(fragment.Steps) == 0 {
			t.Fatal("expected at least one step in fragment")
		}
	})
}

func TestCompileExpansionFragmentRejectsImplicitGraphContract(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "implicit-graph-expansion"
type = "expansion"
version = 2

[[template]]
id = "{target}.review"
title = "Review"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member" }

[[template]]
id = "{target}.submit"
title = "Submit"
needs = ["{target}.review"]
`
	if err := os.WriteFile(filepath.Join(dir, "implicit-graph-expansion.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion: %v", err)
	}

	target := &Step{ID: "demo.target", Title: "Target"}
	_, err := CompileExpansionFragment(context.Background(), "implicit-graph-expansion", []string{dir}, target, nil)
	if err == nil {
		t.Fatal("CompileExpansionFragment succeeded, want explicit graph contract error")
	}
	if !strings.Contains(err.Error(), `contract = "graph.v2"`) {
		t.Fatalf("CompileExpansionFragment error = %v, want graph.v2 contract guidance", err)
	}
}

func TestCompileExpansionFragmentRejectsDuplicateParentTemplateIDs(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	parentA := `
formula = "fragment-parent-a"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.attempt"
title = "Attempt A"
`
	if err := os.WriteFile(filepath.Join(dir, "fragment-parent-a.toml"), []byte(parentA), 0o644); err != nil {
		t.Fatalf("write parentA: %v", err)
	}

	parentB := `
formula = "fragment-parent-b"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.attempt"
title = "Attempt B"
`
	if err := os.WriteFile(filepath.Join(dir, "fragment-parent-b.toml"), []byte(parentB), 0o644); err != nil {
		t.Fatalf("write parentB: %v", err)
	}

	child := `
formula = "fragment-expansion-conflict"
type = "expansion"
version = 2
extends = ["fragment-parent-a", "fragment-parent-b"]
`
	if err := os.WriteFile(filepath.Join(dir, "fragment-expansion-conflict.toml"), []byte(child), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}

	target := &Step{ID: "demo.target", Title: "Target"}
	_, err := CompileExpansionFragment(context.Background(), "fragment-expansion-conflict", []string{dir}, target, nil)
	if err == nil {
		t.Fatal("CompileExpansionFragment succeeded, want duplicate step ID error")
	}
	if !strings.Contains(err.Error(), "duplicate step IDs after expansion") {
		t.Fatalf("CompileExpansionFragment error = %v, want duplicate step ID error", err)
	}
}

func TestCompileExpansionFragmentAllowsConditionallyExclusiveDuplicateTemplateIDs(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "fragment-expansion-conditional"
type = "expansion"
version = 2

[[template]]
id = "{target}.attempt"
title = "Fast attempt"
condition = "{{mode}} == fast"

[[template]]
id = "{target}.attempt"
title = "Slow attempt"
condition = "{{mode}} == slow"
`
	if err := os.WriteFile(filepath.Join(dir, "fragment-expansion-conditional.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion: %v", err)
	}

	target := &Step{ID: "demo.target", Title: "Target"}
	fragment, err := CompileExpansionFragment(context.Background(), "fragment-expansion-conditional", []string{dir}, target, map[string]string{"mode": "fast"})
	if err != nil {
		t.Fatalf("CompileExpansionFragment: %v", err)
	}
	if len(fragment.Steps) != 1 {
		t.Fatalf("len(fragment.Steps) = %d, want 1", len(fragment.Steps))
	}
	if got := fragment.Steps[0].ID; got != "fragment-expansion-conditional.demo.target.attempt" {
		t.Fatalf("fragment.Steps[0].ID = %q, want fragment-expansion-conditional.demo.target.attempt", got)
	}
}

func TestExpandStepDoesNotMutateSharedTemplateState(t *testing.T) {
	t.Parallel()

	template := []*Step{{
		ID:      "{target}.worker",
		Title:   "Worker {target.title}",
		Timeout: "{target.id}0s",
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
		Ralph: &RalphSpec{
			MaxAttempts: 2,
			Check: &RalphCheckSpec{
				Mode:    "exec",
				Path:    "checks/{target.id}.sh",
				Timeout: "{target.title}0s",
			},
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
	if got := second[0].Timeout; got != "beta0s" {
		t.Fatalf("second timeout = %q, want beta0s", got)
	}
	if got := second[0].Ralph.Check.Timeout; got != "Beta0s" {
		t.Fatalf("second ralph check timeout = %q, want Beta0s", got)
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
	if got := template[0].Timeout; got != "{target.id}0s" {
		t.Fatalf("template timeout mutated to %q", got)
	}
	if got := template[0].Ralph.Check.Timeout; got != "{target.title}0s" {
		t.Fatalf("template ralph check timeout mutated to %q", got)
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

func TestCompileExpansionFragmentFailsWhenFormulaV2Disabled(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	expansion := `
formula = "needs-v2-fragment"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.work"
title = "Work"
`
	if err := os.WriteFile(filepath.Join(dir, "needs-v2-fragment.formula.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion: %v", err)
	}

	target := &Step{ID: "demo.target", Title: "Target"}
	_, err := CompileExpansionFragment(context.Background(), "needs-v2-fragment", []string{dir}, target, nil)
	if err == nil {
		t.Fatal("CompileExpansionFragment succeeded, want error for v2 expansion with FormulaV2Enabled=false")
	}
	if !strings.Contains(err.Error(), "formula_v2") {
		t.Fatalf("error = %v, want message mentioning formula_v2", err)
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
