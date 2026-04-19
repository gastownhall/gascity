package formula

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testfixtures/reviewworkflows"
)

func TestCompileSimpleFormula(t *testing.T) {
	dir := t.TempDir()
	formulaContent := `
formula = "pancakes"
description = "Make pancakes"
version = 1

[[steps]]
id = "dry"
title = "Mix dry ingredients"
type = "task"

[[steps]]
id = "wet"
title = "Mix wet ingredients"
type = "task"

[[steps]]
id = "cook"
title = "Cook pancakes"
needs = ["dry", "wet"]
`
	if err := os.WriteFile(filepath.Join(dir, "pancakes.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "pancakes", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if recipe.Name != "pancakes" {
		t.Errorf("Name = %q, want %q", recipe.Name, "pancakes")
	}

	// Root + 3 steps = 4 total
	if len(recipe.Steps) != 4 {
		t.Errorf("len(Steps) = %d, want 4", len(recipe.Steps))
	}

	// Root should be first and marked
	if !recipe.Steps[0].IsRoot {
		t.Error("Steps[0] should be root")
	}
	if recipe.Steps[0].ID != "pancakes" {
		t.Errorf("root ID = %q, want %q", recipe.Steps[0].ID, "pancakes")
	}
	if recipe.RootStep().Type != "molecule" {
		t.Errorf("root Type = %q, want %q", recipe.RootStep().Type, "molecule")
	}

	// Check step IDs are namespaced
	if recipe.Steps[1].ID != "pancakes.dry" {
		t.Errorf("step 1 ID = %q, want %q", recipe.Steps[1].ID, "pancakes.dry")
	}

	// Check deps include the needs -> blocks edge
	foundNeedsDep := false
	for _, dep := range recipe.Deps {
		if dep.StepID == "pancakes.cook" && dep.DependsOnID == "pancakes.dry" && dep.Type == "blocks" {
			foundNeedsDep = true
		}
	}
	if !foundNeedsDep {
		t.Error("missing blocks dependency from cook -> dry")
	}
}

func TestCompileWithVarsAndConditions(t *testing.T) {
	dir := t.TempDir()
	formulaContent := `
formula = "conditional"
version = 1

[vars.mode]
description = "Execution mode"
default = "fast"

[[steps]]
id = "always"
title = "Always runs"

[[steps]]
id = "slow-only"
title = "Only in slow mode"
condition = "{{mode}} == slow"
`
	if err := os.WriteFile(filepath.Join(dir, "conditional.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// With default vars (mode=fast), slow-only should be filtered out
	recipe, err := Compile(context.Background(), "conditional", []string{dir}, map[string]string{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// Root + always = 2 (slow-only filtered out by condition)
	if len(recipe.Steps) != 2 {
		t.Errorf("len(Steps) = %d, want 2 (slow-only filtered)", len(recipe.Steps))
	}

	// With mode=slow, both should be present
	recipe2, err := Compile(context.Background(), "conditional", []string{dir}, map[string]string{"mode": "slow"})
	if err != nil {
		t.Fatalf("Compile with mode=slow: %v", err)
	}

	if len(recipe2.Steps) != 3 {
		t.Errorf("len(Steps) = %d, want 3 (all included)", len(recipe2.Steps))
	}
}

func TestCompileNilVarsAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	formulaContent := `
formula = "nil-vars"
version = 1

[vars.env]
description = "Target environment"
default = "dev"

[[steps]]
id = "always"
title = "Always runs"

[[steps]]
id = "staging-only"
title = "Only in staging"
condition = "{{env}} == staging"

[[steps]]
id = "dev-only"
title = "Only in dev"
condition = "{{env}} == dev"
`
	if err := os.WriteFile(filepath.Join(dir, "nil-vars.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// With nil vars, formula defaults (env=dev) should still drive condition filtering
	recipe, err := Compile(context.Background(), "nil-vars", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile with nil vars: %v", err)
	}

	// Root + always + dev-only = 3 (staging-only filtered out by default env=dev)
	if len(recipe.Steps) != 3 {
		t.Errorf("len(Steps) = %d, want 3 (staging-only filtered by default vars)", len(recipe.Steps))
	}

	// Verify the right steps survived
	foundAlways := false
	foundDevOnly := false
	for _, step := range recipe.Steps {
		switch step.ID {
		case "nil-vars.always":
			foundAlways = true
		case "nil-vars.dev-only":
			foundDevOnly = true
		case "nil-vars.staging-only":
			t.Error("staging-only step should be filtered when env defaults to dev")
		}
	}
	if !foundAlways {
		t.Error("always step missing from result")
	}
	if !foundDevOnly {
		t.Error("dev-only step missing from result")
	}
}

func TestCompileWithChildren(t *testing.T) {
	dir := t.TempDir()
	formulaContent := `
formula = "nested"
version = 1

[[steps]]
id = "parent"
title = "Parent step"

[[steps.children]]
id = "child-a"
title = "Child A"

[[steps.children]]
id = "child-b"
title = "Child B"
needs = ["child-a"]
`
	if err := os.WriteFile(filepath.Join(dir, "nested.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "nested", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// Root + parent (promoted to epic) + child-a + child-b = 4
	if len(recipe.Steps) != 4 {
		t.Errorf("len(Steps) = %d, want 4", len(recipe.Steps))
	}

	// Parent should be promoted to epic
	parentStep := recipe.StepByID("nested.parent")
	if parentStep == nil {
		t.Fatal("parent step not found")
	}
	if parentStep.Type != "epic" {
		t.Errorf("parent.Type = %q, want %q", parentStep.Type, "epic")
	}

	// Child IDs should be nested
	childA := recipe.StepByID("nested.parent.child-a")
	if childA == nil {
		t.Fatal("child-a not found at nested.parent.child-a")
	}
}

func TestCompileNotFound(t *testing.T) {
	_, err := Compile(context.Background(), "nonexistent", nil, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent formula")
	}
}

func TestCompileVaporPhase(t *testing.T) {
	dir := t.TempDir()
	formulaContent := `
formula = "patrol"
version = 1
phase = "vapor"

[[steps]]
id = "scan"
title = "Scan"
`
	if err := os.WriteFile(filepath.Join(dir, "patrol.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "patrol", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if recipe.Phase != "vapor" {
		t.Errorf("Phase = %q, want %q", recipe.Phase, "vapor")
	}
	if !recipe.RootOnly {
		t.Error("vapor formula should be RootOnly by default")
	}
}

func TestCompileCheckSyntaxMarksWorkflowRootAndBlocksOnTopLevelSteps(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	formulaContent := `
formula = "ralph-demo"
version = 1

[[steps]]
id = "design"
title = "Design"

[[steps]]
id = "implement"
title = "Implement"
needs = ["design"]

[steps.check]
max_attempts = 2

[steps.check.check]
mode = "exec"
path = ".gascity/checks/widget.sh"
timeout = "30s"
`
	if err := os.WriteFile(filepath.Join(dir, "ralph-demo.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "ralph-demo", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	root := recipe.RootStep()
	if root == nil {
		t.Fatal("root step missing")
	}
	if got := root.Metadata["gc.kind"]; got != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", got)
	}
	if root.Type != "task" {
		t.Fatalf("root type = %q, want task", root.Type)
	}

	assertHasDep := func(stepID, dependsOnID, depType string) {
		t.Helper()
		for _, dep := range recipe.Deps {
			if dep.StepID == stepID && dep.DependsOnID == dependsOnID && dep.Type == depType {
				return
			}
		}
		t.Fatalf("missing dep %s --%s--> %s", stepID, depType, dependsOnID)
	}
	assertLacksDep := func(stepID, dependsOnID, depType string) {
		t.Helper()
		for _, dep := range recipe.Deps {
			if dep.StepID == stepID && dep.DependsOnID == dependsOnID && dep.Type == depType {
				t.Fatalf("unexpected dep %s --%s--> %s", stepID, depType, dependsOnID)
			}
		}
	}

	assertHasDep("ralph-demo", "ralph-demo.design", "blocks")
	assertHasDep("ralph-demo", "ralph-demo.implement", "blocks")
	assertLacksDep("ralph-demo", "ralph-demo.implement.run.1", "blocks")
	assertLacksDep("ralph-demo", "ralph-demo.implement.check.1", "blocks")
}

func TestCompileVersion2UsesGraphWorkflowRootAndNoParentChild(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	formulaContent := `
formula = "graph-demo"
version = 2

[[steps]]
id = "setup"
title = "Setup"

[[steps]]
id = "work"
title = "Work"
needs = ["setup"]
`
	if err := os.WriteFile(filepath.Join(dir, "graph-demo.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "graph-demo", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	root := recipe.RootStep()
	if root == nil {
		t.Fatal("root step missing")
	}
	if root.Type != "task" {
		t.Fatalf("root type = %q, want task", root.Type)
	}
	if got := root.Metadata["gc.kind"]; got != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", got)
	}
	if got := root.Metadata["gc.formula_contract"]; got != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", got)
	}
	finalizer := recipe.StepByID("graph-demo.workflow-finalize")
	if finalizer == nil {
		t.Fatal("workflow-finalize step missing")
	}
	if got := finalizer.Metadata["gc.kind"]; got != "workflow-finalize" {
		t.Fatalf("workflow-finalize gc.kind = %q, want workflow-finalize", got)
	}

	for _, dep := range recipe.Deps {
		if dep.Type == "parent-child" {
			t.Fatalf("unexpected parent-child dep in v2 recipe: %+v", dep)
		}
	}

	foundBlocks := false
	foundRootFinalize := false
	for _, dep := range recipe.Deps {
		if dep.StepID == "graph-demo.work" && dep.DependsOnID == "graph-demo.setup" && dep.Type == "blocks" {
			foundBlocks = true
		}
		if dep.StepID == "graph-demo" && dep.DependsOnID == "graph-demo.workflow-finalize" && dep.Type == "blocks" {
			foundRootFinalize = true
		}
	}
	if !foundBlocks {
		t.Fatal("missing work -> setup blocks dep")
	}
	if !foundRootFinalize {
		t.Fatal("missing root -> workflow-finalize blocks dep")
	}
}

func TestCompileScopedWorkCarriesScopeAndCleanupMetadata(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))
	searchDir := filepath.Join(repoRoot, "internal", "bootstrap", "packs", "core", "formulas")

	recipe, err := Compile(context.Background(), "mol-scoped-work", []string{searchDir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	root := recipe.RootStep()
	if root == nil {
		t.Fatal("root step missing")
	}
	if got := root.Metadata["gc.formula_contract"]; got != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", got)
	}

	body := recipe.StepByID("mol-scoped-work.body")
	if body == nil {
		t.Fatal("body step missing")
	}
	if got := body.Metadata["gc.kind"]; got != "scope" {
		t.Fatalf("body gc.kind = %q, want scope", got)
	}
	if got := body.Metadata["gc.scope_name"]; got != "worktree" {
		t.Fatalf("body gc.scope_name = %q, want worktree", got)
	}

	cleanup := recipe.StepByID("mol-scoped-work.cleanup-worktree")
	if cleanup == nil {
		t.Fatal("cleanup step missing")
	}
	if got := cleanup.Metadata["gc.scope_role"]; got != "teardown" {
		t.Fatalf("cleanup gc.scope_role = %q, want teardown", got)
	}
	if got := cleanup.Metadata["gc.kind"]; got != "retry" {
		t.Fatalf("cleanup gc.kind = %q, want retry", got)
	}
	if got := cleanup.Metadata["gc.original_kind"]; got != "cleanup" {
		t.Fatalf("cleanup gc.original_kind = %q, want cleanup", got)
	}
	scopeCheck := recipe.StepByID("mol-scoped-work.implement-scope-check")
	if scopeCheck == nil {
		t.Fatal("implement scope-check step missing")
	}
	if got := scopeCheck.Metadata["gc.kind"]; got != "scope-check" {
		t.Fatalf("scope-check gc.kind = %q, want scope-check", got)
	}
	finalizer := recipe.StepByID("mol-scoped-work.workflow-finalize")
	if finalizer == nil {
		t.Fatal("workflow-finalize step missing")
	}
	if got := finalizer.Metadata["gc.kind"]; got != "workflow-finalize" {
		t.Fatalf("workflow-finalize gc.kind = %q, want workflow-finalize", got)
	}

	foundCleanupDep := false
	foundRootFinalize := false
	foundFinalizeBody := false
	for _, dep := range recipe.Deps {
		if dep.StepID == cleanup.ID && dep.DependsOnID == body.ID && dep.Type == "blocks" {
			foundCleanupDep = true
		}
		if dep.StepID == root.ID && dep.DependsOnID == finalizer.ID && dep.Type == "blocks" {
			foundRootFinalize = true
		}
		if dep.StepID == finalizer.ID && dep.DependsOnID == body.ID && dep.Type == "blocks" {
			foundFinalizeBody = true
		}
	}
	if !foundCleanupDep {
		t.Fatalf("missing cleanup -> body blocks dep")
	}
	if !foundRootFinalize {
		t.Fatalf("missing workflow root -> workflow-finalize blocks dep")
	}
	if !foundFinalizeBody {
		t.Fatalf("missing workflow-finalize -> body blocks dep")
	}

	indexByID := make(map[string]int, len(recipe.Steps))
	for i, step := range recipe.Steps {
		indexByID[step.ID] = i
	}
	assertBefore := func(first, second string) {
		t.Helper()
		if indexByID[first] >= indexByID[second] {
			t.Fatalf("step order %s (%d) should come before %s (%d)", first, indexByID[first], second, indexByID[second])
		}
	}

	assertBefore("mol-scoped-work.load-context", "mol-scoped-work.workspace-setup")
	assertBefore("mol-scoped-work.workspace-setup", "mol-scoped-work.workspace-setup-scope-check")
	assertBefore("mol-scoped-work.preflight-tests", "mol-scoped-work.preflight-tests-scope-check")
	assertBefore("mol-scoped-work.submit-scope-check", "mol-scoped-work.body")
	assertBefore("mol-scoped-work.body", "mol-scoped-work.cleanup-worktree")
	assertBefore("mol-scoped-work.cleanup-worktree", "mol-scoped-work.workflow-finalize")
}

func TestCompileGraphWorkflowRejectsCycles(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	formulaText := `
formula = "graph-cycle"
phase = "liquid"
version = 2

[[steps]]
id = "a"
title = "A"
needs = ["b"]

[[steps]]
id = "b"
title = "B"
needs = ["a"]
`
	if err := os.WriteFile(filepath.Join(dir, "graph-cycle.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write graph-cycle formula: %v", err)
	}

	_, err := Compile(context.Background(), "graph-cycle", []string{dir}, nil)
	if err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("Compile(graph-cycle) error = %v, want dependency cycle", err)
	}
}

func TestCompileReviewWorkflowSkipGeminiFiltersExpansionLane(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	writeReviewWorkflowFixtures(t, dir)

	recipe, err := Compile(context.Background(), "mol-adopt-pr-v2", []string{dir}, map[string]string{
		"issue":       "GC-1",
		"pr_ref":      "refs/heads/test",
		"skip_gemini": "true",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	for _, step := range recipe.Steps {
		if strings.Contains(step.ID, "review-gemini") {
			t.Fatalf("compiled recipe unexpectedly retained Gemini lane with skip_gemini=true: %s", step.ID)
		}
	}
	for _, dep := range recipe.Deps {
		if strings.Contains(dep.StepID, "review-gemini") || strings.Contains(dep.DependsOnID, "review-gemini") {
			t.Fatalf("compiled recipe unexpectedly retained Gemini dependency with skip_gemini=true: %+v", dep)
		}
	}

	for _, want := range []string{
		"mol-adopt-pr-v2.review-loop.iteration.1.review-pipeline.review-claude",
		"mol-adopt-pr-v2.review-loop.iteration.1.review-pipeline.review-codex",
		"mol-adopt-pr-v2.review-loop.iteration.1.review-pipeline.synthesize",
		"mol-adopt-pr-v2.review-loop.iteration.1.apply-fixes",
	} {
		if recipe.StepByID(want) == nil {
			t.Fatalf("compiled recipe missing expected step %q", want)
		}
	}
}

func TestCompileReviewWorkflowAnnotatesNestedReviewerRetries(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	writeReviewWorkflowFixtures(t, dir)

	recipe, err := Compile(context.Background(), "mol-adopt-pr-v2", []string{dir}, map[string]string{
		"issue":       "GC-1",
		"pr_ref":      "refs/heads/test",
		"skip_gemini": "false",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	assertRetryStep := func(stepID, onExhausted string) {
		t.Helper()
		step := recipe.StepByID(stepID)
		if step == nil {
			t.Fatalf("missing retry control step %q", stepID)
		}
		if got := step.Metadata["gc.kind"]; got != "retry" {
			t.Fatalf("%s gc.kind = %q, want retry", stepID, got)
		}
		if got := step.Metadata["gc.on_exhausted"]; got != onExhausted {
			t.Fatalf("%s gc.on_exhausted = %q, want %q", stepID, got, onExhausted)
		}
		if got := step.Metadata["gc.max_attempts"]; got != "3" {
			t.Fatalf("%s gc.max_attempts = %q, want 3", stepID, got)
		}

		attempt := recipe.StepByID(stepID + ".attempt.1")
		if attempt == nil {
			t.Fatalf("missing first retry attempt for %q", stepID)
		}
		if got := attempt.Metadata["gc.attempt"]; got != "1" {
			t.Fatalf("%s gc.attempt = %q, want 1", attempt.ID, got)
		}
		if got := attempt.Metadata["gc.step_id"]; got == "" {
			t.Fatalf("%s gc.step_id should be populated for retry attempts", attempt.ID)
		}
	}

	assertRetryStep("mol-adopt-pr-v2.review-loop.iteration.1.review-pipeline.review-codex", "hard_fail")
	assertRetryStep("mol-adopt-pr-v2.review-loop.iteration.1.review-pipeline.review-gemini", "soft_fail")
}

func writeReviewWorkflowFixtures(t *testing.T, dir string) {
	t.Helper()
	for name, content := range map[string]string{
		"expansion-review-pr.toml": reviewworkflows.ExpansionReviewPR,
		"mol-adopt-pr-v2.toml":     reviewworkflows.AdoptPR,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestCompileV2FormulaFailsWhenFormulaV2Disabled(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()

	t.Run("version 2 formula errors", func(t *testing.T) {
		formulaContent := `
formula = "needs-v2"
version = 2

[[steps]]
id = "work"
title = "Do work"
`
		if err := os.WriteFile(filepath.Join(dir, "needs-v2.formula.toml"), []byte(formulaContent), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := Compile(context.Background(), "needs-v2", []string{dir}, nil)
		if err == nil {
			t.Fatal("Compile(needs-v2) succeeded, want error for v2 formula with FormulaV2Enabled=false")
		}
		if !strings.Contains(err.Error(), "formula_v2") {
			t.Fatalf("error = %v, want message mentioning formula_v2", err)
		}
	})

	t.Run("version 8 formula errors", func(t *testing.T) {
		formulaContent := `
formula = "needs-v8"
version = 8

[[steps]]
id = "work"
title = "Do work"
`
		if err := os.WriteFile(filepath.Join(dir, "needs-v8.formula.toml"), []byte(formulaContent), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := Compile(context.Background(), "needs-v8", []string{dir}, nil)
		if err == nil {
			t.Fatal("Compile(needs-v8) succeeded, want error for v8 formula with FormulaV2Enabled=false")
		}
		if !strings.Contains(err.Error(), "formula_v2") {
			t.Fatalf("error = %v, want message mentioning formula_v2", err)
		}
	})

	t.Run("version 1 formula still compiles", func(t *testing.T) {
		formulaContent := `
formula = "still-v1"
version = 1

[[steps]]
id = "work"
title = "Do work"
`
		if err := os.WriteFile(filepath.Join(dir, "still-v1.formula.toml"), []byte(formulaContent), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := Compile(context.Background(), "still-v1", []string{dir}, nil)
		if err != nil {
			t.Fatalf("Compile(still-v1) = %v, want nil for v1 formula", err)
		}
	})
}

func TestCompileValidatesRequiredVars(t *testing.T) {
	dir := t.TempDir()
	formulaContent := `
formula = "repro-unresolved"
description = "Repro: unresolved template variables survive into bead titles."
version = 1

[vars.epic]
description = "Epic ticket ID"
required = true

[vars.feature]
description = "Feature slug"
required = true

[[steps]]
id = "implement"
title = "[{{epic}}] Implement: {{feature}}"
tags = ["implement", "{{epic}}"]
description = "Implement the {{feature}} feature for {{epic}}."

[[steps]]
id = "review"
title = "[{{epic}}] Review: {{feature}}"
needs = ["implement"]
tags = ["review", "{{epic}}"]
description = "Review the {{feature}} implementation."
`
	if err := os.WriteFile(filepath.Join(dir, "repro-unresolved.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("empty vars skips validation", func(t *testing.T) {
		// Empty map = read-only display (formula show). Validation is
		// deferred to instantiation-time residual checks.
		recipe, err := Compile(context.Background(), "repro-unresolved", []string{dir}, map[string]string{})
		if err != nil {
			t.Fatalf("Compile with empty vars should skip validation: %v", err)
		}
		if recipe.Name != "repro-unresolved" {
			t.Errorf("Name = %q, want %q", recipe.Name, "repro-unresolved")
		}
	})

	t.Run("missing one required var", func(t *testing.T) {
		_, err := Compile(context.Background(), "repro-unresolved", []string{dir}, map[string]string{"epic": "CLOUD-99999"})
		if err == nil {
			t.Fatal("Compile should reject missing feature var")
		}
		if !strings.Contains(err.Error(), `"feature" is required`) {
			t.Errorf("error should mention feature: %v", err)
		}
	})

	t.Run("all required vars provided", func(t *testing.T) {
		recipe, err := Compile(context.Background(), "repro-unresolved", []string{dir}, map[string]string{
			"epic":    "CLOUD-99999",
			"feature": "auth",
		})
		if err != nil {
			t.Fatalf("Compile should succeed with all vars: %v", err)
		}
		if recipe.Name != "repro-unresolved" {
			t.Errorf("Name = %q, want %q", recipe.Name, "repro-unresolved")
		}
	})

	t.Run("nil vars skips validation", func(t *testing.T) {
		recipe, err := Compile(context.Background(), "repro-unresolved", []string{dir}, nil)
		if err != nil {
			t.Fatalf("Compile with nil vars should skip validation: %v", err)
		}
		if recipe.Name != "repro-unresolved" {
			t.Errorf("Name = %q, want %q", recipe.Name, "repro-unresolved")
		}
	})
}
