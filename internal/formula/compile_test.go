package formula

import (
	"context"
	"fmt"
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

func TestCompileWithoutRuntimeVarValidationReportsMissingCompileTimeRangeVar(t *testing.T) {
	dir := t.TempDir()
	formulaContent := `
formula = "range-demo"
version = 1

[vars.n]
description = "Loop count"
required = true

[[steps]]
id = "loop"
title = "Loop"

[steps.loop]
range = "1..{n}"

[[steps.loop.body]]
id = "work"
title = "Work {i}"
`
	if err := os.WriteFile(filepath.Join(dir, "range-demo.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := CompileWithoutRuntimeVarValidation(context.Background(), "range-demo", []string{dir}, nil)
	if err == nil {
		t.Fatal("CompileWithoutRuntimeVarValidation should reject missing compile-time range var")
	}
	if !strings.Contains(err.Error(), `variable "n" is required`) {
		t.Fatalf("error = %v, want required n", err)
	}

	recipe, err := CompileWithoutRuntimeVarValidation(context.Background(), "range-demo", []string{dir}, map[string]string{"n": "2"})
	if err != nil {
		t.Fatalf("CompileWithoutRuntimeVarValidation with n: %v", err)
	}
	if len(recipe.Steps) != 3 {
		t.Fatalf("len(recipe.Steps) = %d, want root plus two loop iterations", len(recipe.Steps))
	}
}

func TestCompileWithoutRuntimeVarValidationValidatesCompileTimeRangeVarDefs(t *testing.T) {
	dir := t.TempDir()
	formulaContent := `
formula = "range-demo"
version = 1

[vars.n]
description = "Loop count"
default = "2"
enum = ["1", "2", "3"]

[[steps]]
id = "loop"
title = "Loop"

[steps.loop]
range = "1..{n}"

[[steps.loop.body]]
id = "work"
title = "Work {i}"
`
	if err := os.WriteFile(filepath.Join(dir, "range-demo.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := CompileWithoutRuntimeVarValidation(context.Background(), "range-demo", []string{dir}, nil)
	if err != nil {
		t.Fatalf("CompileWithoutRuntimeVarValidation with default n: %v", err)
	}
	if len(recipe.Steps) != 3 {
		t.Fatalf("len(recipe.Steps) = %d, want root plus two loop iterations", len(recipe.Steps))
	}

	_, err = CompileWithoutRuntimeVarValidation(context.Background(), "range-demo", []string{dir}, map[string]string{"n": "4"})
	if err == nil {
		t.Fatal("CompileWithoutRuntimeVarValidation should reject invalid compile-time range var")
	}
	if !strings.Contains(err.Error(), `variable "n": value "4" not in allowed values`) {
		t.Fatalf("error = %v, want enum validation for n", err)
	}
}

func TestCompileWithoutRuntimeVarValidationReportsMissingCompileTimeConditionVar(t *testing.T) {
	dir := t.TempDir()
	formulaContent := `
formula = "condition-demo"
version = 1

[vars.mode]
description = "Build mode"
required = true

[[steps]]
id = "always"
title = "Always"

[[steps]]
id = "slow"
title = "Slow path"
condition = "{{mode}} == slow"
`
	if err := os.WriteFile(filepath.Join(dir, "condition-demo.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := CompileWithoutRuntimeVarValidation(context.Background(), "condition-demo", []string{dir}, nil)
	if err == nil {
		t.Fatal("CompileWithoutRuntimeVarValidation should reject missing compile-time condition var")
	}
	if !strings.Contains(err.Error(), `variable "mode" is required`) {
		t.Fatalf("error = %v, want required mode", err)
	}

	recipe, err := CompileWithoutRuntimeVarValidation(context.Background(), "condition-demo", []string{dir}, map[string]string{"mode": "slow"})
	if err != nil {
		t.Fatalf("CompileWithoutRuntimeVarValidation with mode: %v", err)
	}
	if len(recipe.Steps) != 3 {
		t.Fatalf("len(recipe.Steps) = %d, want root plus two included steps", len(recipe.Steps))
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

// TestCompileExtendsPhasePour regresses the merge bug where the 'extends'
// resolver dropped Phase and Pour from the merged formula, silently coercing
// vapor formulas to persistent and pour formulas back to root-only.
func TestCompileExtendsPhasePour(t *testing.T) {
	cases := []struct {
		name         string
		parent       string
		child        string
		wantPhase    string
		wantPour     bool
		wantRootOnly bool
	}{
		{
			name: "parent_sets_phase_child_inherits",
			parent: `
formula = "base"
version = 1
phase = "vapor"

[[steps]]
id = "scan"
title = "Scan"
`,
			child: `
formula = "derived"
version = 1
extends = ["base"]
`,
			wantPhase:    "vapor",
			wantPour:     false,
			wantRootOnly: true,
		},
		{
			name: "child_sets_phase_overrides_parent",
			parent: `
formula = "base"
version = 1
phase = "liquid"

[[steps]]
id = "scan"
title = "Scan"
`,
			child: `
formula = "derived"
version = 1
extends = ["base"]
phase = "vapor"
`,
			wantPhase:    "vapor",
			wantPour:     false,
			wantRootOnly: true,
		},
		{
			name: "parent_sets_pour_child_inherits",
			parent: `
formula = "base"
version = 1
pour = true

[[steps]]
id = "scan"
title = "Scan"
`,
			child: `
formula = "derived"
version = 1
extends = ["base"]
`,
			wantPhase:    "",
			wantPour:     true,
			wantRootOnly: false,
		},
		{
			name: "parent_sets_both_child_inherits",
			parent: `
formula = "base"
version = 1
phase = "vapor"
pour = true

[[steps]]
id = "scan"
title = "Scan"
`,
			child: `
formula = "derived"
version = 1
extends = ["base"]
`,
			wantPhase: "vapor",
			wantPour:  true,
			// RootOnly is gated on !Pour && Phase=="vapor"; Pour=true overrides.
			wantRootOnly: false,
		},
		{
			// Guards against a future refactor that stops seeding merged
			// from the child: without seeding, a child-only Pour=true would
			// be dropped because the parent loop only propagates true values.
			name: "child_sets_pour_parent_unset",
			parent: `
formula = "base"
version = 1

[[steps]]
id = "scan"
title = "Scan"
`,
			child: `
formula = "derived"
version = 1
extends = ["base"]
pour = true
`,
			wantPhase:    "",
			wantPour:     true,
			wantRootOnly: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "base.toml"), []byte(tc.parent), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "derived.toml"), []byte(tc.child), 0o644); err != nil {
				t.Fatal(err)
			}

			recipe, err := Compile(context.Background(), "derived", []string{dir}, nil)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			if recipe.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", recipe.Phase, tc.wantPhase)
			}
			if recipe.Pour != tc.wantPour {
				t.Errorf("Pour = %v, want %v", recipe.Pour, tc.wantPour)
			}
			if recipe.RootOnly != tc.wantRootOnly {
				t.Errorf("RootOnly = %v, want %v", recipe.RootOnly, tc.wantRootOnly)
			}
		})
	}
}

// TestCompileExtendsMultiParentPhaseFirstWins verifies that when multiple
// parents declare Phase, the first non-empty parent wins — matching the
// inheritance semantics already used for Vars and Contract.
func TestCompileExtendsMultiParentPhaseFirstWins(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"parentA.toml": `
formula = "parentA"
version = 1
phase = "vapor"

[[steps]]
id = "a"
title = "A"
`,
		"parentB.toml": `
formula = "parentB"
version = 1
phase = "liquid"

[[steps]]
id = "b"
title = "B"
`,
		"child.toml": `
formula = "child"
version = 1
extends = ["parentA", "parentB"]
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	recipe, err := Compile(context.Background(), "child", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if recipe.Phase != "vapor" {
		t.Errorf("Phase = %q, want %q (first non-empty parent wins)", recipe.Phase, "vapor")
	}
}

// TestCompileExtendsMultiParentPourAnyParentWins verifies OR semantics for
// Pour across multiple parents — unlike Phase (first-non-empty-wins), any
// parent declaring pour=true promotes the merged formula regardless of
// parent order. Pins the Phase-vs-Pour semantic asymmetry so future
// refactors don't silently align them.
func TestCompileExtendsMultiParentPourAnyParentWins(t *testing.T) {
	cases := []struct {
		name    string
		extends string
	}{
		{name: "pour_first", extends: `["pourParent", "plainParent"]`},
		{name: "pour_second", extends: `["plainParent", "pourParent"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{
				"pourParent.toml": `
formula = "pourParent"
version = 1
pour = true

[[steps]]
id = "a"
title = "A"
`,
				"plainParent.toml": `
formula = "plainParent"
version = 1

[[steps]]
id = "b"
title = "B"
`,
				"child.toml": `
formula = "child"
version = 1
extends = ` + tc.extends + `
`,
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			recipe, err := Compile(context.Background(), "child", []string{dir}, nil)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if !recipe.Pour {
				t.Errorf("Pour = false, want true (any parent with pour=true promotes)")
			}
		})
	}
}

func TestCompileCheckSyntaxWithoutGraphContractKeepsMoleculeRoot(t *testing.T) {
	enableV2ForTest(t)

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
	if got := root.Metadata["gc.kind"]; got != "" {
		t.Fatalf("root gc.kind = %q, want empty", got)
	}
	if got := root.Metadata["gc.formula_contract"]; got != "" {
		t.Fatalf("root gc.formula_contract = %q, want empty", got)
	}
	if root.Type != "molecule" {
		t.Fatalf("root type = %q, want molecule", root.Type)
	}
}

func TestCompileCheckSyntaxWithGraphContractMarksWorkflowRoot(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	formulaContent := `
formula = "ralph-demo"
version = 1
contract = "graph.v2"

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
	if got := root.Metadata["gc.formula_contract"]; got != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", got)
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

	if finalizer := recipe.StepByID("ralph-demo.workflow-finalize"); finalizer == nil {
		t.Fatal("missing workflow finalizer")
	}

	assertHasDep("ralph-demo", "ralph-demo.workflow-finalize", "blocks")
	assertLacksDep("ralph-demo", "ralph-demo.design", "blocks")
	assertLacksDep("ralph-demo", "ralph-demo.implement", "blocks")
	assertLacksDep("ralph-demo", "ralph-demo.implement.run.1", "blocks")
	assertLacksDep("ralph-demo", "ralph-demo.implement.check.1", "blocks")
}

func TestCompileExpansionFormulaSubstitutesTimeoutsFromFile(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	formulaContent := `
formula = "exp-timeout"
version = 1
type = "expansion"

[vars.step_timeout]
default = "10m"

[vars.check_timeout]
default = "30s"

[[template]]
id = "{target}.check"
title = "Check"
timeout = "{step_timeout}"

[template.check]
max_attempts = 1

[template.check.check]
mode = "exec"
path = "checks/pass.sh"
timeout = "{check_timeout}"
`
	if err := os.WriteFile(filepath.Join(dir, "exp-timeout.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "exp-timeout", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	for _, step := range recipe.Steps {
		if step.Metadata["gc.kind"] != "ralph" {
			continue
		}
		if got := step.Metadata["gc.step_timeout"]; got != "10m" {
			t.Fatalf("gc.step_timeout = %q, want 10m", got)
		}
		if got := step.Metadata["gc.check_timeout"]; got != "30s" {
			t.Fatalf("gc.check_timeout = %q, want 30s", got)
		}
		return
	}
	t.Fatal("ralph control step not found")
}

func TestCompileExpansionFormulaAllowsUnresolvedTimeoutVars(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	formulaContent := `
formula = "exp-timeout"
version = 1
type = "expansion"

[[template]]
id = "{target}.check"
title = "Check"
timeout = "{step_timeout}"

[template.check]
max_attempts = 1

[template.check.check]
mode = "exec"
path = "checks/pass.sh"
timeout = "{check_timeout}"
`
	if err := os.WriteFile(filepath.Join(dir, "exp-timeout.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, vars := range []map[string]string{nil, {}} {
		recipe, err := Compile(context.Background(), "exp-timeout", []string{dir}, vars)
		if err != nil {
			t.Fatalf("Compile with vars %#v: %v", vars, err)
		}
		found := false
		for _, step := range recipe.Steps {
			if step.Metadata["gc.kind"] != "ralph" {
				continue
			}
			found = true
			if got := step.Metadata["gc.step_timeout"]; got != "{step_timeout}" {
				t.Fatalf("gc.step_timeout = %q, want unresolved placeholder", got)
			}
			if got := step.Metadata["gc.check_timeout"]; got != "{check_timeout}" {
				t.Fatalf("gc.check_timeout = %q, want unresolved placeholder", got)
			}
		}
		if !found {
			t.Fatal("ralph control step not found")
		}
	}
}

func TestCompileVersion2UsesGraphWorkflowRootAndNoParentChild(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	formulaContent := `
formula = "graph-demo"
version = 2
contract = "graph.v2"

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

func TestCompileLegacyFormulaRevisionDoesNotUseGraphWorkflow(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	formulaContent := `
formula = "legacy-revision"
version = 8

[[steps]]
id = "work"
title = "Work"
`
	if err := os.WriteFile(filepath.Join(dir, "legacy-revision.toml"), []byte(formulaContent), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "legacy-revision", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	root := recipe.RootStep()
	if root == nil {
		t.Fatal("root step missing")
	}
	if root.Type != "molecule" {
		t.Fatalf("root type = %q, want molecule", root.Type)
	}
	if got := root.Metadata["gc.formula_contract"]; got != "" {
		t.Fatalf("root gc.formula_contract = %q, want empty", got)
	}
}

func TestCompileScopedWorkCarriesScopeAndCleanupMetadata(t *testing.T) {
	enableV2ForTest(t)

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
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

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
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	formulaText := `
formula = "mol-teardown-sibling"
phase = "liquid"
version = 2
contract = "graph.v2"

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

func TestCompileGraphRetryWorkflowRequiresExplicitGraphContract(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	formulaText := `
formula = "implicit-v2"
phase = "liquid"
version = 2

[[steps]]
id = "work"
title = "Do the work"

[steps.retry]
max_attempts = 2
`
	if err := os.WriteFile(filepath.Join(dir, "implicit-v2.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	_, err := Compile(context.Background(), "implicit-v2", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile succeeded, want explicit contract error")
	}
	if !strings.Contains(err.Error(), `contract = "graph.v2"`) {
		t.Fatalf("Compile error = %v, want graph.v2 contract guidance", err)
	}
}

func TestCompileVersion1DetachedGraphMetadataRequiresExplicitGraphContract(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	formulaText := `
formula = "implicit-v1-detached"
phase = "liquid"
version = 1

[[steps]]
id = "work"
title = "Do the work"
metadata = { "gc.kind" = "retry" }
`
	if err := os.WriteFile(filepath.Join(dir, "implicit-v1-detached.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	_, err := Compile(context.Background(), "implicit-v1-detached", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile succeeded, want explicit contract error")
	}
	if !strings.Contains(err.Error(), `contract = "graph.v2"`) {
		t.Fatalf("Compile error = %v, want graph.v2 contract guidance", err)
	}
}

func TestCompileGraphOnCompleteWorkflowRequiresExplicitGraphContract(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	t.Cleanup(func() { SetFormulaV2Enabled(prev) })

	dir := t.TempDir()
	formulaText := `
formula = "implicit-v2-fanout"
phase = "liquid"
version = 2

[[steps]]
id = "survey"
title = "Survey"

[steps.on_complete]
for_each = "output.items"
bond = "mol-item"
`
	if err := os.WriteFile(filepath.Join(dir, "implicit-v2-fanout.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	_, err := Compile(context.Background(), "implicit-v2-fanout", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile succeeded, want explicit contract error")
	}
	if !strings.Contains(err.Error(), `contract = "graph.v2"`) {
		t.Fatalf("Compile error = %v, want graph.v2 contract guidance", err)
	}
}

func TestCompileStandaloneExpansionRejectsDuplicateParentTemplateIDs(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	parentA := `
formula = "standalone-parent-a"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.attempt"
title = "Attempt A"
`
	if err := os.WriteFile(filepath.Join(dir, "standalone-parent-a.toml"), []byte(parentA), 0o644); err != nil {
		t.Fatalf("write parentA: %v", err)
	}

	parentB := `
formula = "standalone-parent-b"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.attempt"
title = "Attempt B"
`
	if err := os.WriteFile(filepath.Join(dir, "standalone-parent-b.toml"), []byte(parentB), 0o644); err != nil {
		t.Fatalf("write parentB: %v", err)
	}

	child := `
formula = "standalone-expansion-conflict"
type = "expansion"
version = 2
extends = ["standalone-parent-a", "standalone-parent-b"]
`
	if err := os.WriteFile(filepath.Join(dir, "standalone-expansion-conflict.toml"), []byte(child), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}

	_, err := Compile(context.Background(), "standalone-expansion-conflict", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile succeeded, want duplicate step ID error")
	}
	if !strings.Contains(err.Error(), "duplicate step IDs after expansion") {
		t.Fatalf("Compile error = %v, want duplicate step ID error", err)
	}
}

func TestCompileStandaloneExpansionAllowsConditionallyExclusiveDuplicateTemplateIDs(t *testing.T) {
	enableV2ForTest(t)

	dir := t.TempDir()
	formulaText := `
formula = "standalone-expansion-conditional"
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
	if err := os.WriteFile(filepath.Join(dir, "standalone-expansion-conditional.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	recipe, err := Compile(context.Background(), "standalone-expansion-conditional", []string{dir}, map[string]string{"mode": "fast"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(recipe.Steps) != 2 {
		t.Fatalf("len(recipe.Steps) = %d, want 2", len(recipe.Steps))
	}
	if got := recipe.Steps[1].ID; got != "standalone-expansion-conditional.main.attempt" {
		t.Fatalf("recipe.Steps[1].ID = %q, want standalone-expansion-conditional.main.attempt", got)
	}
}

func TestCompileAllowsConditionallyExclusiveDuplicateComposeExpansionTemplateIDs(t *testing.T) {
	dir := t.TempDir()

	expansion := `{
		"formula": "compose-conditional-duplicate",
		"type": "expansion",
		"version": 1,
		"template": [
			{"id": "{target}.attempt", "title": "Fast attempt", "condition": "{{mode}} == fast"},
			{"id": "{target}.attempt", "title": "Slow attempt", "condition": "{{mode}} == slow"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "compose-conditional-duplicate.formula.json"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion: %v", err)
	}

	formulaText := `{
		"formula": "compose-conditional-parent",
		"version": 1,
		"steps": [
			{"id": "release", "title": "Release"}
		],
		"compose": {
			"expand": [
				{"target": "release", "with": "compose-conditional-duplicate"}
			]
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "compose-conditional-parent.formula.json"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	recipe, err := Compile(context.Background(), "compose-conditional-parent", []string{dir}, map[string]string{"mode": "fast"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(recipe.Steps) != 2 {
		t.Fatalf("len(recipe.Steps) = %d, want 2", len(recipe.Steps))
	}
	if got := recipe.Steps[1].ID; got != "compose-conditional-parent.release.attempt" {
		t.Fatalf("recipe.Steps[1].ID = %q, want compose-conditional-parent.release.attempt", got)
	}
}

func TestCompileComposeExpansionUsesRuleVarsForConditionalTemplateSelection(t *testing.T) {
	dir := t.TempDir()

	expansion := `{
		"formula": "compose-override-conditional",
		"type": "expansion",
		"version": 1,
		"vars": {
			"mode": {"default": "slow"}
		},
		"template": [
			{"id": "{target}.attempt", "title": "Fast attempt", "condition": "{{mode}} == fast"},
			{"id": "{target}.attempt", "title": "Slow attempt", "condition": "{{mode}} == slow"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "compose-override-conditional.formula.json"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion: %v", err)
	}

	formulaText := `{
		"formula": "compose-override-parent",
		"version": 1,
		"steps": [
			{"id": "release", "title": "Release"}
		],
		"compose": {
			"expand": [
				{"target": "release", "with": "compose-override-conditional", "vars": {"mode": "fast"}}
			]
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "compose-override-parent.formula.json"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	recipe, err := Compile(context.Background(), "compose-override-parent", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(recipe.Steps) != 2 {
		t.Fatalf("len(recipe.Steps) = %d, want 2", len(recipe.Steps))
	}
	if got := recipe.Steps[1].ID; got != "compose-override-parent.release.attempt" {
		t.Fatalf("recipe.Steps[1].ID = %q, want compose-override-parent.release.attempt", got)
	}
}

func TestCompileAllowsConditionallyExclusiveDuplicateInlineExpansionTemplateIDs(t *testing.T) {
	dir := t.TempDir()

	expansion := `{
		"formula": "inline-conditional-duplicate",
		"type": "expansion",
		"version": 1,
		"template": [
			{"id": "{target}.attempt", "title": "Fast attempt", "condition": "{{mode}} == fast"},
			{"id": "{target}.attempt", "title": "Slow attempt", "condition": "{{mode}} == slow"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "inline-conditional-duplicate.formula.json"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion: %v", err)
	}

	formulaText := `{
		"formula": "inline-conditional-parent",
		"version": 1,
		"steps": [
			{"id": "work", "title": "Work", "expand": "inline-conditional-duplicate"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "inline-conditional-parent.formula.json"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	recipe, err := Compile(context.Background(), "inline-conditional-parent", []string{dir}, map[string]string{"mode": "fast"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(recipe.Steps) != 2 {
		t.Fatalf("len(recipe.Steps) = %d, want 2", len(recipe.Steps))
	}
	if got := recipe.Steps[1].ID; got != "inline-conditional-parent.work.attempt" {
		t.Fatalf("recipe.Steps[1].ID = %q, want inline-conditional-parent.work.attempt", got)
	}
}

func TestCompileInlineExpansionUsesExpandVarsForConditionalTemplateSelection(t *testing.T) {
	dir := t.TempDir()

	expansion := `{
		"formula": "inline-override-conditional",
		"type": "expansion",
		"version": 1,
		"vars": {
			"mode": {"default": "slow"}
		},
		"template": [
			{"id": "{target}.attempt", "title": "Fast attempt", "condition": "{{mode}} == fast"},
			{"id": "{target}.attempt", "title": "Slow attempt", "condition": "{{mode}} == slow"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "inline-override-conditional.formula.json"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion: %v", err)
	}

	formulaText := `{
		"formula": "inline-override-parent",
		"version": 1,
		"steps": [
			{"id": "work", "title": "Work", "expand": "inline-override-conditional", "expand_vars": {"mode": "fast"}}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "inline-override-parent.formula.json"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	recipe, err := Compile(context.Background(), "inline-override-parent", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(recipe.Steps) != 2 {
		t.Fatalf("len(recipe.Steps) = %d, want 2", len(recipe.Steps))
	}
	if got := recipe.Steps[1].ID; got != "inline-override-parent.work.attempt" {
		t.Fatalf("recipe.Steps[1].ID = %q, want inline-override-parent.work.attempt", got)
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
	enableV2ForTest(t)

	dir := t.TempDir()
	formulaText := `
formula = "graph-cycle"
phase = "liquid"
version = 2
contract = "graph.v2"

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
	enableV2ForTest(t)

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
	enableV2ForTest(t)

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
contract = "graph.v2"

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

	t.Run("legacy revision formula stays on molecule contract", func(t *testing.T) {
		formulaContent := `
formula = "legacy-v8"
version = 8

[[steps]]
id = "work"
title = "Do work"
`
		if err := os.WriteFile(filepath.Join(dir, "legacy-v8.formula.toml"), []byte(formulaContent), 0o644); err != nil {
			t.Fatal(err)
		}

		recipe, err := Compile(context.Background(), "legacy-v8", []string{dir}, nil)
		if err != nil {
			t.Fatalf("Compile(legacy-v8): %v", err)
		}
		if recipe.RootStep().Type != "molecule" {
			t.Fatalf("root type = %q, want molecule", recipe.RootStep().Type)
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

	t.Run("check syntax without graph contract stays on molecule contract", func(t *testing.T) {
		formulaContent := `
formula = "legacy-check"
version = 1

[[steps]]
id = "work"
title = "Do work"

[steps.check]
max_attempts = 1

[steps.check.check]
mode = "exec"
path = "check.sh"
`
		if err := os.WriteFile(filepath.Join(dir, "legacy-check.formula.toml"), []byte(formulaContent), 0o644); err != nil {
			t.Fatal(err)
		}

		recipe, err := Compile(context.Background(), "legacy-check", []string{dir}, nil)
		if err != nil {
			t.Fatalf("Compile(legacy-check): %v", err)
		}
		root := recipe.RootStep()
		if root == nil {
			t.Fatal("root step missing")
		}
		if root.Type != "molecule" || root.Metadata["gc.kind"] != "" {
			t.Fatalf("root = %+v, want legacy molecule root", root)
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
