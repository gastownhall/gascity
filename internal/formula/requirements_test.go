package formula

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTOMLFormulaCompilerRequirement(t *testing.T) {
	p := NewParser()
	f, err := p.ParseTOML([]byte(`
formula = "review"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "review"
title = "Review"
`))
	if err != nil {
		t.Fatalf("ParseTOML failed: %v", err)
	}
	if f.Requires == nil {
		t.Fatal("Requires is nil")
	}
	if got := f.Requires.FormulaCompiler; got != ">=2.0.0" {
		t.Fatalf("FormulaCompiler = %q, want >=2.0.0", got)
	}
}

func TestParseTOMLRejectsUnknownRequirement(t *testing.T) {
	p := NewParser()
	_, err := p.ParseTOML([]byte(`
formula = "review"

[requires]
state_store = ">=2.0.0"

[[steps]]
id = "review"
title = "Review"
`))
	if err == nil {
		t.Fatal("ParseTOML unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.requirement_unknown")
	requireErrorContains(t, err, `unknown formula requirement "state_store"`)
}

func TestCompileRejectsInvalidFormulaCompilerComparator(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, `
formula = "review"

[requires]
formula_compiler = "not-a-comparator"

[[steps]]
id = "review"
title = "Review"
`)

	_, err := Compile(context.Background(), "review", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_invalid")
	requireErrorContains(t, err, "semver comparator")
}

func TestCompileFormulaCompilerRequirementRequiresEnabledV2(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, `
formula = "review"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "review"
title = "Review"
`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	defer SetFormulaV2Enabled(prev)

	_, err := Compile(context.Background(), "review", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_unsatisfied")
	requireErrorContains(t, err, "[daemon] formula_v2 is disabled")
}

func TestCompileComposeExpansionRequirementRequiresEnabledV2(t *testing.T) {
	dir := t.TempDir()
	writeJSONFormula(t, dir, "parent", `{
		"formula": "parent",
		"steps": [{"id": "work", "title": "Work"}],
		"compose": {"expand": [{"target": "work", "with": "needs-v2-expansion"}]}
	}`)
	writeJSONFormula(t, dir, "needs-v2-expansion", `{
		"formula": "needs-v2-expansion",
		"type": "expansion",
		"requires": {"formula_compiler": ">=2.0.0"},
		"template": [{"id": "{target}.child", "title": "Child"}]
	}`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	defer SetFormulaV2Enabled(prev)

	_, err := Compile(context.Background(), "parent", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_unsatisfied")
	requireErrorContains(t, err, `formula "needs-v2-expansion" [requires]`)
}

func TestCompileComposeAspectRequirementRequiresEnabledV2(t *testing.T) {
	dir := t.TempDir()
	writeJSONFormula(t, dir, "parent", `{
		"formula": "parent",
		"steps": [{"id": "work", "title": "Work"}],
		"compose": {"aspects": ["needs-v2-aspect"]}
	}`)
	writeJSONFormula(t, dir, "needs-v2-aspect", `{
		"formula": "needs-v2-aspect",
		"type": "aspect",
		"requires": {"formula_compiler": ">=2.0.0"},
		"advice": [{"target": "work", "after": {"id": "{step.id}.audit", "title": "Audit"}}]
	}`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	defer SetFormulaV2Enabled(prev)

	_, err := Compile(context.Background(), "parent", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_unsatisfied")
	requireErrorContains(t, err, `formula "needs-v2-aspect" [requires]`)
}

func TestCompileComposeRequirementMarksGraphWorkflow(t *testing.T) {
	dir := t.TempDir()
	writeJSONFormula(t, dir, "parent", `{
		"formula": "parent",
		"steps": [{"id": "work", "title": "Work"}],
		"compose": {"expand": [{"target": "work", "with": "needs-v2-expansion"}]}
	}`)
	writeJSONFormula(t, dir, "needs-v2-expansion", `{
		"formula": "needs-v2-expansion",
		"type": "expansion",
		"requires": {"formula_compiler": ">=2.0.0"},
		"template": [{"id": "{target}.child", "title": "Child"}]
	}`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	recipe, err := Compile(context.Background(), "parent", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	root := recipe.RootStep()
	if root == nil {
		t.Fatal("RootStep is nil")
	}
	if got := root.Metadata["gc.kind"]; got != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", got)
	}
	if got := root.Metadata["gc.formula_contract"]; got != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", got)
	}
}

func TestCompileFormulaCompilerRequirementEnablesGraphWorkflow(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, `
formula = "review"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "review"
title = "Review"
metadata = { "gc.on_fail" = "abort_scope" }
`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	recipe, err := Compile(context.Background(), "review", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	root := recipe.RootStep()
	if root == nil {
		t.Fatal("RootStep is nil")
	}
	if got := root.Metadata["gc.kind"]; got != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", got)
	}
	if got := root.Metadata["gc.formula_contract"]; got != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", got)
	}
}

func TestCompileLegacyContractAliasStillRequiresEnabledV2(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, `
formula = "review"
contract = "graph.v2"

[[steps]]
id = "review"
title = "Review"
`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	defer SetFormulaV2Enabled(prev)

	_, err := Compile(context.Background(), "review", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_unsatisfied")
	requireErrorContains(t, err, "contract = \"graph.v2\"")
}

func TestCompileRejectsConflictingLegacyContractAndRequirement(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, `
formula = "review"
contract = "graph.v2"

[requires]
formula_compiler = "<2.0.0"

[[steps]]
id = "review"
title = "Review"
`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	_, err := Compile(context.Background(), "review", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_conflict")
}

func TestResolveInheritsParentFormulaCompilerRequirement(t *testing.T) {
	dir := t.TempDir()
	writeNamedFormula(t, dir, "parent", `
formula = "parent"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "parent-step"
title = "Parent"
`)
	writeNamedFormula(t, dir, "child", `
formula = "child"
extends = ["parent"]

[[steps]]
id = "child-step"
title = "Child"
`)

	resolved := resolveRequirementFormula(t, dir, "child")
	constraints, err := formulaCompilerConstraints(resolved)
	if err != nil {
		t.Fatalf("formulaCompilerConstraints: %v", err)
	}
	requireConstraintSources(t, constraints, `formula "parent" [requires]`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	defer SetFormulaV2Enabled(prev)
	_, err = Compile(context.Background(), "child", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_unsatisfied")
	requireErrorContains(t, err, `formula "parent" [requires]`)
}

func TestResolveChildStrengthensParentFormulaCompilerRequirement(t *testing.T) {
	dir := t.TempDir()
	writeNamedFormula(t, dir, "parent", `
formula = "parent"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "parent-step"
title = "Parent"
`)
	writeNamedFormula(t, dir, "child", `
formula = "child"
extends = ["parent"]

[requires]
formula_compiler = "<3.0.0"

[[steps]]
id = "child-step"
title = "Child"
`)

	resolved := resolveRequirementFormula(t, dir, "child")
	constraints, err := formulaCompilerConstraints(resolved)
	if err != nil {
		t.Fatalf("formulaCompilerConstraints: %v", err)
	}
	requireConstraintSources(t, constraints, `formula "parent" [requires]`, `formula "child" [requires]`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)
	if _, err := Compile(context.Background(), "child", []string{dir}, nil); err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
}

func TestResolveChildCannotWeakenParentFormulaCompilerRequirement(t *testing.T) {
	dir := t.TempDir()
	writeNamedFormula(t, dir, "parent", `
formula = "parent"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "parent-step"
title = "Parent"
`)
	writeNamedFormula(t, dir, "child", `
formula = "child"
extends = ["parent"]

[requires]
formula_compiler = ">=1.0.0"

[[steps]]
id = "child-step"
title = "Child"
`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	defer SetFormulaV2Enabled(prev)
	_, err := Compile(context.Background(), "child", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_unsatisfied")
	requireErrorContains(t, err, ">=2.0.0")
	requireErrorContains(t, err, `formula "parent" [requires]`)
}

func TestResolveMultipleParentFormulaCompilerRequirementsAllApply(t *testing.T) {
	dir := t.TempDir()
	writeNamedFormula(t, dir, "v2-parent", `
formula = "v2-parent"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "v2"
title = "V2"
`)
	writeNamedFormula(t, dir, "upper-parent", `
formula = "upper-parent"

[requires]
formula_compiler = "<3.0.0"

[[steps]]
id = "upper"
title = "Upper"
`)
	writeNamedFormula(t, dir, "child", `
formula = "child"
extends = ["v2-parent", "upper-parent"]

[[steps]]
id = "child-step"
title = "Child"
`)

	resolved := resolveRequirementFormula(t, dir, "child")
	constraints, err := formulaCompilerConstraints(resolved)
	if err != nil {
		t.Fatalf("formulaCompilerConstraints: %v", err)
	}
	requireConstraintSources(t, constraints, `formula "v2-parent" [requires]`, `formula "upper-parent" [requires]`)
}

func TestResolveLegacyParentContractParticipatesAsRequirement(t *testing.T) {
	dir := t.TempDir()
	writeNamedFormula(t, dir, "parent", `
formula = "parent"
contract = "graph.v2"

[[steps]]
id = "parent-step"
title = "Parent"
`)
	writeNamedFormula(t, dir, "child", `
formula = "child"
extends = ["parent"]

[[steps]]
id = "child-step"
title = "Child"
`)

	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	defer SetFormulaV2Enabled(prev)
	_, err := Compile(context.Background(), "child", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_unsatisfied")
	requireErrorContains(t, err, `formula "parent" contract = "graph.v2"`)
}

func TestResolveRejectsParentChildFormulaCompilerRequirementConflict(t *testing.T) {
	dir := t.TempDir()
	writeNamedFormula(t, dir, "parent", `
formula = "parent"

[requires]
formula_compiler = "<2.0.0"

[[steps]]
id = "parent-step"
title = "Parent"
`)
	writeNamedFormula(t, dir, "child", `
formula = "child"
extends = ["parent"]

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "child-step"
title = "Child"
`)

	_, err := Compile(context.Background(), "child", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_conflict")
	requireErrorContains(t, err, `formula "parent" [requires]`)
	requireErrorContains(t, err, `formula "child" [requires]`)
}

func TestResolveRejectsMultiParentFormulaCompilerRequirementConflict(t *testing.T) {
	dir := t.TempDir()
	writeNamedFormula(t, dir, "legacy-parent", `
formula = "legacy-parent"

[requires]
formula_compiler = "<2.0.0"

[[steps]]
id = "legacy"
title = "Legacy"
`)
	writeNamedFormula(t, dir, "v2-parent", `
formula = "v2-parent"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "v2"
title = "V2"
`)
	writeNamedFormula(t, dir, "child", `
formula = "child"
extends = ["legacy-parent", "v2-parent"]

[[steps]]
id = "child-step"
title = "Child"
`)

	_, err := Compile(context.Background(), "child", []string{dir}, nil)
	if err == nil {
		t.Fatal("Compile unexpectedly succeeded")
	}
	requireErrorContains(t, err, "formula.compiler_requirement_conflict")
	requireErrorContains(t, err, `formula "legacy-parent" [requires]`)
	requireErrorContains(t, err, `formula "v2-parent" [requires]`)
}

func writeFormula(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "review.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeNamedFormula(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeJSONFormula(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".formula.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resolveRequirementFormula(t *testing.T, dir, name string) *Formula {
	t.Helper()
	p := NewParser(dir)
	f, err := p.LoadByName(name)
	if err != nil {
		t.Fatalf("LoadByName(%q): %v", name, err)
	}
	resolved, err := p.Resolve(f)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", name, err)
	}
	return resolved
}

func requireConstraintSources(t *testing.T, constraints []formulaCompilerConstraint, want ...string) {
	t.Helper()
	got := make(map[string]bool, len(constraints))
	for _, constraint := range constraints {
		got[constraint.Source] = true
	}
	for _, source := range want {
		if !got[source] {
			t.Fatalf("constraint sources = %v, missing %q", got, source)
		}
	}
}

func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error is nil, want substring %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err.Error(), want)
	}
}
