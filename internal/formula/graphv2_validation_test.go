package formula

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGraphV2RejectsLegacyReservedReferences(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	dir := t.TempDir()
	writeFormula(t, dir, "bad.formula.toml", `
formula = "bad"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "direct"
title = "Direct {{issue}}"

[[steps]]
id = "spaced"
title = "Spaced {{ issue }}"

[[steps]]
id = "trimmed"
title = "Trimmed {{- issue -}}"

[[steps]]
id = "dotted"
title = "Dotted {{.bead_id}}"

[[steps]]
id = "indexed"
title = "Indexed {{ index . \"issue\" }}"
`)

	_, err := Compile(context.Background(), "bad", []string{dir}, map[string]string{"convoy_id": "convoy-1"})
	if err == nil {
		t.Fatal("Compile succeeded, want reserved-variable error")
	}
	msg := err.Error()
	for _, want := range []string{"issue is not available", "bead_id is not available"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func TestGraphV2RejectsNonCanonicalConvoyReferences(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	dir := t.TempDir()
	writeFormula(t, dir, "bad-convoy.formula.toml", `
formula = "bad-convoy"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "spaced"
title = "Spaced {{ convoy_id }}"

[[steps]]
id = "dotted"
title = "Dotted {{ .convoy_id }}"

[[steps]]
id = "trimmed"
title = "Trimmed {{- convoy_id -}}"

[[steps]]
id = "piped"
title = "Piped {{convoy_id | quote}}"

[[steps]]
id = "indexed"
title = "Indexed {{ index . \"convoy_id\" }}"
`)

	_, err := Compile(context.Background(), "bad-convoy", []string{dir}, map[string]string{"convoy_id": "convoy-1"})
	if err == nil {
		t.Fatal("Compile succeeded, want non-canonical convoy_id error")
	}
	if !strings.Contains(err.Error(), "convoy_id references must use {{convoy_id}} exactly") {
		t.Fatalf("error = %q, want canonical convoy_id reference message", err)
	}
}

func TestGraphV2RejectsLegacyReservedReferencesInExpansionBeforeConditionFiltering(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	dir := t.TempDir()
	writeFormula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 2
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
expand = "hidden-legacy"
`)
	writeFormula(t, dir, "hidden-legacy.formula.toml", `
formula = "hidden-legacy"
version = 2
type = "expansion"

[[template]]
id = "{target}.hidden"
title = "Hidden {{bead_id}}"
condition = "!{{convoy_id}}"
`)

	_, err := Compile(context.Background(), "parent", []string{dir}, map[string]string{"convoy_id": "convoy-1"})
	if err == nil {
		t.Fatal("Compile succeeded, want transitive reserved-variable error")
	}
	if !strings.Contains(err.Error(), "bead_id is not available") {
		t.Fatalf("error = %q, want bead_id reserved-variable error", err)
	}
}

func TestGraphV2RejectsReservedVariableDeclarations(t *testing.T) {
	f := &Formula{
		Formula:  "bad-vars",
		Version:  1,
		Contract: "graph.v2",
		Type:     TypeWorkflow,
		Vars: map[string]*VarDef{
			"convoy_id": {},
			"issue":     {},
			"bead_id":   {},
		},
	}

	err := ValidateGraphV2ReservedSymbols(f, true)
	if err == nil {
		t.Fatal("ValidateGraphV2ReservedSymbols succeeded, want error")
	}
	msg := err.Error()
	for _, want := range []string{"vars.convoy_id", "vars.issue", "vars.bead_id"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func TestGraphV2TargetlessRejectsConvoyReferencesAndDrain(t *testing.T) {
	f := &Formula{
		Formula:     "needs-target",
		Version:     1,
		Contract:    "graph.v2",
		Type:        TypeWorkflow,
		Description: "Work on {{convoy_id}}",
		Steps: []*Step{{
			ID:    "drain",
			Title: "Drain",
			Drain: &DrainSpec{Context: "separate", Formula: "item"},
		}},
	}

	err := ValidateGraphV2ReservedSymbols(f, false)
	if err == nil {
		t.Fatal("ValidateGraphV2ReservedSymbols succeeded, want targetless error")
	}
	if !strings.Contains(err.Error(), "convoy_id requires a targeted graph.v2 invocation") {
		t.Fatalf("error = %q, want convoy target message", err)
	}
	if !GraphV2FormulaReferencesInputConvoy(f) {
		t.Fatal("GraphV2FormulaReferencesInputConvoy = false, want true")
	}
}

func TestGraphV2DrainV0AcceptsSharedAndExclusive(t *testing.T) {
	f := &Formula{
		Formula:  "shared-drain",
		Version:  1,
		Contract: "graph.v2",
		Type:     TypeWorkflow,
		Steps: []*Step{{
			ID:    "drain",
			Title: "Drain",
			Drain: &DrainSpec{
				Context:       "shared",
				Formula:       "item",
				MemberAccess:  "exclusive",
				OnItemFailure: "skip_remaining",
				Item:          &DrainItemSpec{SingleLane: true},
			},
		}},
	}

	if err := ValidateGraphV2ReservedSymbols(f, true); err != nil {
		t.Fatalf("ValidateGraphV2ReservedSymbols(shared exclusive drain): %v", err)
	}
}

func TestGraphV2DrainV0RejectsInvalidModes(t *testing.T) {
	cases := []struct {
		name string
		spec DrainSpec
		want string
	}{
		{
			name: "too many units",
			spec: DrainSpec{Context: "separate", Formula: "item", MaxUnits: intPtr(101)},
			want: "max_units must be <= 100",
		},
		{
			name: "zero units",
			spec: DrainSpec{Context: "separate", Formula: "item", MaxUnits: intPtr(0)},
			want: "max_units must be >= 1",
		},
		{
			name: "templated item formula",
			spec: DrainSpec{Context: "separate", Formula: "{{item_formula}}"},
			want: "templated item formula names are not supported in v0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Formula{
				Formula:  "bad-drain",
				Version:  1,
				Contract: "graph.v2",
				Type:     TypeWorkflow,
				Steps: []*Step{{
					ID:    "drain",
					Title: "Drain",
					Drain: &tc.spec,
				}},
			}

			err := f.Validate()
			if err == nil {
				t.Fatal("Validate succeeded, want drain validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want %q", err, tc.want)
			}
		})
	}
}

func TestParseGraphV2DrainStep(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "drain-demo.formula.toml", `
formula = "drain-demo"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "review-members"
title = "Review members"

[steps.drain]
context = "separate"
formula = "review-one"
member_access = "read"
max_units = 50
on_item_failure = "continue"
`)

	parsed, err := NewParser(dir).LoadByName("drain-demo")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	resolved, err := NewParser(dir).Resolve(parsed)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := resolved.Steps[0].Drain; got == nil || got.Context != "separate" || got.Formula != "review-one" {
		t.Fatalf("parsed drain = %#v", got)
	}
}

func TestResolveGraphV2DrainRejectsExplicitZeroMaxUnits(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "zero-drain.formula.toml", `
formula = "zero-drain"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "drain"
title = "Drain"

[steps.drain]
context = "separate"
formula = "review-one"
max_units = 0
`)

	parser := NewParser(dir)
	parsed, err := parser.LoadByName("zero-drain")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	_, err = parser.Resolve(parsed)
	if err == nil {
		t.Fatal("Resolve succeeded, want explicit zero max_units error")
	}
	if !strings.Contains(err.Error(), "max_units must be >= 1") {
		t.Fatalf("error = %q, want explicit zero max_units error", err)
	}
}

func TestParseFileReturnsDescriptionFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "missing-desc.formula.toml", `
formula = "missing-desc"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
description_file = "does-not-exist.md"
`)

	_, err := NewParser(dir).LoadByName("missing-desc")
	if err == nil {
		t.Fatal("LoadByName succeeded, want description_file error")
	}
	if !strings.Contains(err.Error(), "does-not-exist.md") {
		t.Fatalf("error = %q, want missing path", err)
	}
}

func TestParseFileKeepsLegacyDescriptionFileTolerance(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "missing-desc.formula.toml", `
formula = "missing-desc"
version = 1
type = "workflow"

[[steps]]
id = "work"
title = "Work"
description_file = "does-not-exist.md"
`)

	loaded, err := NewParser(dir).LoadByName("missing-desc")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	if got := loaded.Steps[0].DescriptionFile; got != "does-not-exist.md" {
		t.Fatalf("DescriptionFile = %q, want unresolved legacy value", got)
	}
}

func TestParseFileResolvesGraphDescriptionFileThroughSource(t *testing.T) {
	gitOK(t)
	root := initRepo(t)
	formulaDir := filepath.Join(root, "formulas")
	commitFile(t, root, "formulas/graph-desc.formula.toml", `
formula = "graph-desc"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
description_file = "desc.md"
`)
	commitFile(t, root, "formulas/desc.md", "committed description\n")
	commitOnBranch(t, root, "main", "graph desc")
	if err := os.Remove(filepath.Join(formulaDir, "desc.md")); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_FORMULA_REF", "main")
	loaded, err := NewParser(formulaDir).SetSource(SourceFromEnv()).LoadByName("graph-desc")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	if got := loaded.Steps[0].Description; got != "committed description\n" {
		t.Fatalf("Description = %q, want committed description from source", got)
	}
}

func TestParseFileLegacyDescriptionFileMissStillResolvesChildren(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "legacy-desc.formula.toml", `
formula = "legacy-desc"
version = 1
type = "workflow"

[[steps]]
id = "parent"
title = "Parent"
description_file = "missing-parent.md"

[[steps.children]]
id = "child"
title = "Child"
description_file = "child.md"
`)
	if err := os.WriteFile(filepath.Join(dir, "child.md"), []byte("child description\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := NewParser(dir).LoadByName("legacy-desc")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	if got := loaded.Steps[0].Children[0].Description; got != "child description\n" {
		t.Fatalf("child Description = %q, want child description resolved after parent miss", got)
	}
	if got := loaded.Steps[0].DescriptionFile; got != "missing-parent.md" {
		t.Fatalf("parent DescriptionFile = %q, want unresolved missing parent file", got)
	}
}

func TestResolveInheritedGraphV2RejectsUnresolvedChildDescriptionFile(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "graph-base.formula.toml", `
formula = "graph-base"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "base"
title = "Base"
`)
	writeFormula(t, dir, "graph-child.formula.toml", `
formula = "graph-child"
version = 1
type = "workflow"
extends = ["graph-base"]

[[steps]]
id = "child"
title = "Child"
description_file = "missing-child.md"
`)

	parser := NewParser(dir)
	loaded, err := parser.LoadByName("graph-child")
	if err != nil {
		t.Fatalf("LoadByName: %v", err)
	}
	_, err = parser.Resolve(loaded)
	if err == nil {
		t.Fatal("Resolve succeeded, want inherited graph.v2 description_file error")
	}
	if !strings.Contains(err.Error(), "missing-child.md") {
		t.Fatalf("error = %q, want missing child description_file", err)
	}
}

func TestCompileGraphV2RejectsUnresolvedExpansionDescriptionFile(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	dir := t.TempDir()
	writeFormula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 2
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
expand = "exp"
`)
	writeFormula(t, dir, "exp.formula.toml", `
formula = "exp"
version = 2
type = "expansion"

[[template]]
id = "{target}.expanded"
title = "Expanded"
description_file = "missing-expansion.md"
`)

	_, err := CompileWithoutRuntimeVarValidation(context.Background(), "parent", []string{dir}, nil)
	if err == nil {
		t.Fatal("CompileWithoutRuntimeVarValidation succeeded, want expansion description_file error")
	}
	if !strings.Contains(err.Error(), "missing-expansion.md") {
		t.Fatalf("error = %q, want missing expansion description_file", err)
	}
}

func TestValidateGraphV2RecipeRejectsZeroDrainMaxUnits(t *testing.T) {
	recipe := &Recipe{Steps: []RecipeStep{
		{
			ID:     "root",
			IsRoot: true,
			Metadata: map[string]string{
				"gc.formula_contract": "graph.v2",
			},
		},
		{
			ID: "root.drain",
			Metadata: map[string]string{
				"gc.kind":                "drain",
				"gc.drain_context":       "separate",
				"gc.drain_formula":       "item",
				"gc.drain_member_access": "read",
				"gc.drain_max_units":     "0",
			},
		},
	}}

	err := ValidateGraphV2RecipeReservedSymbols(recipe, true)
	if err == nil {
		t.Fatal("ValidateGraphV2RecipeReservedSymbols succeeded, want zero max_units error")
	}
	if !strings.Contains(err.Error(), "max_units must be >= 1") {
		t.Fatalf("error = %q, want zero max_units error", err)
	}
}

func writeFormula(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.TrimSpace(contents)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
}

func intPtr(v int) *int {
	return &v
}
