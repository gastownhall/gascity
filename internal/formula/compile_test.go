package formula

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestCompileRalphMarksWorkflowRootAndBlocksOnTopLevelSteps(t *testing.T) {
	FormulaV2Enabled = true
	t.Cleanup(func() { FormulaV2Enabled = false })

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

[steps.ralph]
max_attempts = 2

[steps.ralph.check]
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
	FormulaV2Enabled = true
	t.Cleanup(func() { FormulaV2Enabled = false })

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
	FormulaV2Enabled = true
	t.Cleanup(func() { FormulaV2Enabled = false })

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))
	searchDir := filepath.Join(repoRoot, "cmd", "gc", "formulas")

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

	// The teardown retry control must block on its own attempt.1, matching
	// the invariant in processRetryControl: a retry-manager is only ever
	// processed after its latest attempt has closed. Without this edge,
	// the control bead becomes ready as soon as the body scope closes,
	// and the dispatcher crash-loops with "latest attempt ... is open,
	// not closed (invariant violation)".
	cleanupAttempt := recipe.StepByID("mol-scoped-work.cleanup-worktree.attempt.1")
	if cleanupAttempt == nil {
		t.Fatal("cleanup-worktree.attempt.1 step missing")
	}
	foundAttemptDep := false
	for _, dep := range recipe.Deps {
		if dep.StepID == cleanup.ID && dep.DependsOnID == cleanupAttempt.ID && dep.Type == "blocks" {
			foundAttemptDep = true
			break
		}
	}
	if !foundAttemptDep {
		t.Fatalf("teardown retry %s missing blocks dep on its attempt %s", cleanup.ID, cleanupAttempt.ID)
	}
}

// TestCompileBugReportFlowV2 is an integration-style check that loads
// the real tooling formula used by the bugflow workflow and asserts
// the teardown retry control carries a blocks dep on its attempt.
func TestCompileBugReportFlowV2(t *testing.T) {
	FormulaV2Enabled = true
	t.Cleanup(func() { FormulaV2Enabled = false })

	const toolingPath = "/home/ubuntu/tooling/formulas"
	if _, err := os.Stat(filepath.Join(toolingPath, "mol-bug-report-flow-v2.formula.toml")); err != nil {
		t.Skipf("tooling formula not present: %v", err)
	}

	recipe, err := Compile(context.Background(), "mol-bug-report-flow-v2", []string{toolingPath}, map[string]string{
		"report_ref": "https://example.com/issues/1",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	cleanup := recipe.StepByID("mol-bug-report-flow-v2.cleanup-run-state")
	cleanupAttempt := recipe.StepByID("mol-bug-report-flow-v2.cleanup-run-state.attempt.1")
	if cleanup == nil {
		t.Fatal("cleanup-run-state step missing")
	}
	if cleanupAttempt == nil {
		t.Fatal("cleanup-run-state.attempt.1 step missing")
	}

	foundAttemptDep := false
	var relevant []string
	for _, dep := range recipe.Deps {
		if dep.StepID == cleanup.ID {
			relevant = append(relevant, fmt.Sprintf("  %s -> %s (%s)", dep.StepID, dep.DependsOnID, dep.Type))
		}
		if dep.StepID == cleanup.ID && dep.DependsOnID == cleanupAttempt.ID && dep.Type == "blocks" {
			foundAttemptDep = true
		}
	}
	if !foundAttemptDep {
		t.Fatalf("teardown retry %s missing blocks dep on attempt %s\ncleanup's deps:\n%s",
			cleanup.ID, cleanupAttempt.ID, strings.Join(relevant, "\n"))
	}

	// Also verify a peer body-step retry has its attempt dep, to confirm
	// the check is real and not just passing because no retries work.
	peer := recipe.StepByID("mol-bug-report-flow-v2.verify-run-state")
	peerAttempt := recipe.StepByID("mol-bug-report-flow-v2.verify-run-state.attempt.1")
	if peer == nil || peerAttempt == nil {
		t.Fatalf("verify-run-state or its attempt missing")
	}
	foundPeer := false
	for _, dep := range recipe.Deps {
		if dep.StepID == peer.ID && dep.DependsOnID == peerAttempt.ID && dep.Type == "blocks" {
			foundPeer = true
		}
	}
	if !foundPeer {
		t.Fatalf("peer retry %s missing blocks dep on attempt %s", peer.ID, peerAttempt.ID)
	}
}

// TestCompileTeardownRetryWithDownstreamSibling reproduces the
// mol-bug-report-flow-v2 shape: a teardown-scoped retry step that
// another (later) step `needs = [...]`. A later rewrite step should
// not strip the retry→attempt.1 edge on the teardown control.
func TestCompileTeardownRetryWithDownstreamSibling(t *testing.T) {
	FormulaV2Enabled = true
	t.Cleanup(func() { FormulaV2Enabled = false })

	dir := t.TempDir()
	formulaText := `
formula = "mol-teardown-sibling"
phase = "liquid"
version = 2

[[steps]]
id = "body"
title = "Body scope"
needs = ["work"]
metadata = { "gc.kind" = "scope", "gc.scope_name" = "bugflow", "gc.scope_role" = "body" }

[[steps]]
id = "work"
title = "Do the work"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3

[[steps]]
id = "verify-run-state"
title = "Verify terminal run state"
needs = ["cleanup-run-state"]
metadata = { "gc.continuation_group" = "main" }

[steps.retry]
max_attempts = 3

[[steps]]
id = "cleanup-run-state"
title = "Cleanup run state"
needs = ["body"]
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }

[steps.retry]
max_attempts = 3
`
	if err := os.WriteFile(filepath.Join(dir, "mol-teardown-sibling.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	recipe, err := Compile(context.Background(), "mol-teardown-sibling", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	cleanup := recipe.StepByID("mol-teardown-sibling.cleanup-run-state")
	cleanupAttempt := recipe.StepByID("mol-teardown-sibling.cleanup-run-state.attempt.1")
	if cleanup == nil {
		t.Fatal("cleanup-run-state step missing")
	}
	if cleanupAttempt == nil {
		t.Fatal("cleanup-run-state.attempt.1 step missing")
	}
	if got := cleanup.Metadata["gc.scope_role"]; got != "teardown" {
		t.Fatalf("cleanup gc.scope_role = %q, want teardown", got)
	}
	if got := cleanup.Metadata["gc.kind"]; got != "retry" {
		t.Fatalf("cleanup gc.kind = %q, want retry", got)
	}

	foundAttemptDep := false
	for _, dep := range recipe.Deps {
		if dep.StepID == cleanup.ID && dep.DependsOnID == cleanupAttempt.ID && dep.Type == "blocks" {
			foundAttemptDep = true
			break
		}
	}
	if !foundAttemptDep {
		t.Fatalf("teardown retry %s missing blocks dep on its attempt %s\nall deps referencing %s:\n%s",
			cleanup.ID, cleanupAttempt.ID, cleanup.ID, formatDepsForCleanup(recipe.Deps, cleanup.ID))
	}
}

func formatDepsForCleanup(deps []RecipeDep, stepID string) string {
	var lines []string
	for _, d := range deps {
		if d.StepID == stepID {
			lines = append(lines, fmt.Sprintf("  %s -> %s (%s)", d.StepID, d.DependsOnID, d.Type))
		}
	}
	if len(lines) == 0 {
		return "  (none)"
	}
	return strings.Join(lines, "\n")
}

func TestCompileGraphWorkflowRejectsCycles(t *testing.T) {
	FormulaV2Enabled = true
	t.Cleanup(func() { FormulaV2Enabled = false })

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
