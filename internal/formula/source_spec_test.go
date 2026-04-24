package formula

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyRetriesEmitsSpecBeadInsteadOfInlineSourceSpec(t *testing.T) {
	steps := []*Step{
		{
			ID:       "review",
			Title:    "Review change",
			Type:     "task",
			Assignee: "polecat",
			Retry:    &RetrySpec{MaxAttempts: 3, OnExhausted: "soft_fail"},
		},
	}

	got, err := ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (control + spec + attempt)", len(got))
	}

	control := findGraphStepByID(got, "review")
	spec := findGraphStepByID(got, "review.spec")
	attempt := findGraphStepByID(got, "review.attempt.1")
	if control == nil || spec == nil || attempt == nil {
		t.Fatalf("missing retry expansion steps: control=%t spec=%t attempt=%t", control != nil, spec != nil, attempt != nil)
	}
	if got := control.Metadata["gc.source_step_spec"]; got != "" {
		t.Fatalf("control gc.source_step_spec = %q, want empty; source spec must live in spec bead", got)
	}
	assertFrozenSpecStep(t, spec, "review", func(frozen Step) {
		if frozen.Retry == nil || frozen.Retry.MaxAttempts != 3 {
			t.Fatalf("frozen retry = %+v, want max_attempts=3", frozen.Retry)
		}
	})
	if attempt.Metadata["gc.source_step_spec"] != "" {
		t.Fatalf("attempt gc.source_step_spec = %q, want empty", attempt.Metadata["gc.source_step_spec"])
	}
}

func TestApplyRalphEmitsSpecBeadInsteadOfInlineSourceSpec(t *testing.T) {
	steps := []*Step{
		{
			ID:    "converge",
			Title: "Converge changes",
			Type:  "task",
			Ralph: &RalphSpec{
				MaxAttempts: 5,
				Check:       &RalphCheckSpec{Mode: "exec", Path: "check.sh"},
			},
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (control + spec + iteration)", len(got))
	}

	control := findGraphStepByID(got, "converge")
	spec := findGraphStepByID(got, "converge.spec")
	iteration := findGraphStepByID(got, "converge.iteration.1")
	if control == nil || spec == nil || iteration == nil {
		t.Fatalf("missing ralph expansion steps: control=%t spec=%t iteration=%t", control != nil, spec != nil, iteration != nil)
	}
	if got := control.Metadata["gc.source_step_spec"]; got != "" {
		t.Fatalf("control gc.source_step_spec = %q, want empty; source spec must live in spec bead", got)
	}
	var frozenRaw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(spec.Description), &frozenRaw); err != nil {
		t.Fatalf("unmarshal frozen raw spec: %v", err)
	}
	if _, ok := frozenRaw["ralph"]; !ok {
		t.Fatalf("frozen raw spec = %v, want legacy ralph key for compatibility", frozenRaw)
	}
	if _, ok := frozenRaw["check"]; ok {
		t.Fatalf("frozen raw spec unexpectedly wrote canonical check key")
	}
	assertFrozenSpecStep(t, spec, "converge", func(frozen Step) {
		if frozen.Ralph == nil || frozen.Ralph.MaxAttempts != 5 {
			t.Fatalf("frozen ralph = %+v, want max_attempts=5", frozen.Ralph)
		}
	})
}

func TestApplyRalphNestedRetrySpecBeadsRemainMetadataOnly(t *testing.T) {
	children, err := ApplyRetries([]*Step{
		{
			ID:    "work",
			Title: "Do work",
			Retry: &RetrySpec{MaxAttempts: 2, OnExhausted: "hard_fail"},
		},
	})
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}

	got, err := ApplyRalph([]*Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &RalphSpec{
				MaxAttempts: 5,
				Check:       &RalphCheckSpec{Mode: "exec", Path: "check.sh"},
			},
			Children: children,
		},
	})
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	iteration := findGraphStepByID(got, "outer.iteration.1")
	spec := findGraphStepByID(got, "outer.iteration.1.work.spec")
	if iteration == nil || spec == nil {
		t.Fatalf("missing nested iteration/spec: iteration=%t spec=%t", iteration != nil, spec != nil)
	}
	if containsString(iteration.Needs, spec.ID) {
		t.Fatalf("iteration needs = %v, should not block on metadata-only spec bead %s", iteration.Needs, spec.ID)
	}
	assertFrozenSpecStepWithRef(t, spec, "work", "outer.iteration.1.work", func(frozen Step) {
		if frozen.Retry == nil || frozen.Retry.MaxAttempts != 2 {
			t.Fatalf("frozen retry = %+v, want max_attempts=2", frozen.Retry)
		}
	})
	for _, key := range []string{"gc.scope_ref", "gc.scope_role", "gc.on_fail", "gc.step_id", "gc.ralph_step_id", "gc.attempt", "gc.step_ref"} {
		if spec.Metadata[key] != "" {
			t.Fatalf("nested spec metadata %s = %q, want empty; full metadata: %#v", key, spec.Metadata[key], spec.Metadata)
		}
	}
}

func TestNamespaceSourceSpecStepPreservesNestedRef(t *testing.T) {
	// Simulate a spec step that has already been namespaced once (inner ralph),
	// then namespaced again (outer ralph). The gc.spec_for_ref should accumulate
	// both namespace prefixes.
	step := &Step{
		ID:    "inner.iteration.1.work.spec",
		Title: "Step spec for work",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.spec_for":     "work",
			"gc.spec_for_ref": "inner.iteration.1.work",
		},
	}

	namespaced := namespaceSourceSpecStep(step, "outer.iteration.1")

	wantRef := "outer.iteration.1.inner.iteration.1.work"
	if got := namespaced.Metadata["gc.spec_for_ref"]; got != wantRef {
		t.Fatalf("gc.spec_for_ref = %q, want %q", got, wantRef)
	}
	// gc.spec_for should remain the original logical step ID
	if got := namespaced.Metadata["gc.spec_for"]; got != "work" {
		t.Fatalf("gc.spec_for = %q, want %q", got, "work")
	}
}

func TestCompileControlSpecBeadsAreNotWorkflowSinks(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	formulaContent := `
formula = "control-spec-demo"
version = 2
contract = "graph.v2"

[[steps]]
id = "review"
title = "Review"
metadata = { "gc.run_target" = "polecat" }
type = "task"

[steps.retry]
max_attempts = 3
on_exhausted = "soft_fail"

[[steps]]
id = "converge"
title = "Converge"
type = "task"
needs = ["review"]

[steps.check]
max_attempts = 2

[steps.check.check]
mode = "exec"
path = "check.sh"
`
	if err := os.WriteFile(filepath.Join(dir, "control-spec-demo.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "control-spec-demo", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	review := findRecipeStepByID(recipe.Steps, "control-spec-demo.review")
	reviewSpec := findRecipeStepByID(recipe.Steps, "control-spec-demo.review.spec")
	converge := findRecipeStepByID(recipe.Steps, "control-spec-demo.converge")
	convergeSpec := findRecipeStepByID(recipe.Steps, "control-spec-demo.converge.spec")
	finalizer := findRecipeStepByID(recipe.Steps, "control-spec-demo.workflow-finalize")
	if review == nil || reviewSpec == nil || converge == nil || convergeSpec == nil || finalizer == nil {
		t.Fatalf("missing compiled steps: review=%t reviewSpec=%t converge=%t convergeSpec=%t finalizer=%t",
			review != nil, reviewSpec != nil, converge != nil, convergeSpec != nil, finalizer != nil)
	}
	if review.Metadata["gc.source_step_spec"] != "" {
		t.Fatalf("retry control still has inline source spec")
	}
	if converge.Metadata["gc.source_step_spec"] != "" {
		t.Fatalf("ralph control still has inline source spec")
	}
	if hasRecipeDep(recipe.Deps, finalizer.ID, reviewSpec.ID, "blocks") {
		t.Fatalf("workflow finalizer should not block on retry spec bead %s", reviewSpec.ID)
	}
	if hasRecipeDep(recipe.Deps, finalizer.ID, convergeSpec.ID, "blocks") {
		t.Fatalf("workflow finalizer should not block on ralph spec bead %s", convergeSpec.ID)
	}
}

func assertFrozenSpecStep(t *testing.T, spec *Step, specFor string, assert func(Step)) {
	t.Helper()
	assertFrozenSpecStepWithRef(t, spec, specFor, "", assert)
}

func assertFrozenSpecStepWithRef(t *testing.T, spec *Step, specFor, specForRef string, assert func(Step)) {
	t.Helper()
	if spec.Type != "spec" {
		t.Fatalf("spec Type = %q, want spec", spec.Type)
	}
	if spec.Assignee != "" {
		t.Fatalf("spec Assignee = %q, want empty", spec.Assignee)
	}
	if len(spec.Needs) != 0 || len(spec.DependsOn) != 0 {
		t.Fatalf("spec deps = depends_on %v needs %v, want none", spec.DependsOn, spec.Needs)
	}
	if spec.Metadata["gc.kind"] != "spec" {
		t.Fatalf("spec gc.kind = %q, want spec", spec.Metadata["gc.kind"])
	}
	if spec.Metadata["gc.spec_for"] != specFor {
		t.Fatalf("spec gc.spec_for = %q, want %q", spec.Metadata["gc.spec_for"], specFor)
	}
	if specForRef != "" && spec.Metadata["gc.spec_for_ref"] != specForRef {
		t.Fatalf("spec gc.spec_for_ref = %q, want %q", spec.Metadata["gc.spec_for_ref"], specForRef)
	}
	if spec.Description == "" {
		t.Fatal("spec Description is empty")
	}
	var frozen Step
	if err := json.Unmarshal([]byte(spec.Description), &frozen); err != nil {
		t.Fatalf("unmarshal frozen spec: %v", err)
	}
	if frozen.ID != specFor {
		t.Fatalf("frozen ID = %q, want %q", frozen.ID, specFor)
	}
	if assert != nil {
		assert(frozen)
	}
}

func findRecipeStepByID(steps []RecipeStep, id string) *RecipeStep {
	for i := range steps {
		if steps[i].ID == id {
			return &steps[i]
		}
	}
	return nil
}

func hasRecipeDep(deps []RecipeDep, stepID, dependsOnID, depType string) bool {
	for _, dep := range deps {
		if dep.StepID == stepID && dep.DependsOnID == dependsOnID && dep.Type == depType {
			return true
		}
	}
	return false
}
