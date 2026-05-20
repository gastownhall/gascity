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

func writeFormula(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "review.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
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
