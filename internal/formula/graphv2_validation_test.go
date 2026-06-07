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
	writeGraphV2Formula(t, dir, "bad.formula.toml", `
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
	if !strings.Contains(msg, "bead_id is not available") {
		t.Fatalf("error %q missing %q", msg, "bead_id is not available")
	}
	// issue is a deprecated one-release compat alias (#2941): tolerated by
	// validation, surfaced through GraphV2LegacyIssueRefsTransitively instead.
	if strings.Contains(msg, "issue is not available") {
		t.Fatalf("error %q rejects the deprecated issue alias; it must be tolerated for one release", msg)
	}
}

func TestGraphV2LegacyIssueRefsReportDeprecatedSymbols(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	dir := t.TempDir()
	writeGraphV2Formula(t, dir, "deprecated.formula.toml", `
formula = "deprecated"
version = 1
contract = "graph.v2"
type = "workflow"

[vars]
[vars.issue]
description = "legacy work bead"
required = true

[[steps]]
id = "direct"
title = "Direct {{issue}}"
`)

	parser := NewParser(dir)
	loaded, err := parser.LoadByName("deprecated")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	resolved, err := parser.Resolve(loaded)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	refs := GraphV2LegacyIssueRefsTransitively(resolved, parser)
	if len(refs) != 2 {
		t.Fatalf("GraphV2LegacyIssueRefsTransitively = %v, want declaration + reference", refs)
	}
	for _, want := range []string{"vars.issue: issue", "steps[0].title: issue"} {
		found := false
		for _, ref := range refs {
			if ref == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("refs %v missing %q", refs, want)
		}
	}
}

func TestGraphV2RejectsNonCanonicalConvoyReferences(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	dir := t.TempDir()
	writeGraphV2Formula(t, dir, "bad-convoy.formula.toml", `
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
	writeGraphV2Formula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 2
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
expand = "hidden-legacy"
`)
	writeGraphV2Formula(t, dir, "hidden-legacy.formula.toml", `
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
	for _, want := range []string{"vars.convoy_id", "vars.bead_id"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	// Declaring vars.issue is tolerated as a deprecated compat alias (#2941).
	if strings.Contains(msg, "vars.issue") {
		t.Fatalf("error %q rejects the deprecated vars.issue declaration; it must be tolerated for one release", msg)
	}
}

func TestGraphV2TargetlessRejectsConvoyReferencesAndDrain(t *testing.T) {
	f := &Formula{
		Formula:     "needs-target",
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
	writeGraphV2Formula(t, dir, "drain-demo.formula.toml", `
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
	writeGraphV2Formula(t, dir, "zero-drain.formula.toml", `
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
	writeGraphV2Formula(t, dir, "missing-desc.formula.toml", `
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

func TestParseFileReturnsDescriptionFileErrorsForCompilerRequirement(t *testing.T) {
	dir := t.TempDir()
	writeGraphV2Formula(t, dir, "missing-desc.formula.toml", `
formula = "missing-desc"
version = 1
type = "workflow"

[requires]
formula_compiler = ">=2.0.0"

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
	writeGraphV2Formula(t, dir, "missing-desc.formula.toml", `
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
	writeGraphV2Formula(t, dir, "legacy-desc.formula.toml", `
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
	writeGraphV2Formula(t, dir, "graph-base.formula.toml", `
formula = "graph-base"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "base"
title = "Base"
`)
	writeGraphV2Formula(t, dir, "graph-child.formula.toml", `
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
	writeGraphV2Formula(t, dir, "parent.formula.toml", `
formula = "parent"
version = 2
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work"
expand = "exp"
`)
	writeGraphV2Formula(t, dir, "exp.formula.toml", `
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

func TestCompileRequiresGraphCompilerAcceptsDrain(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	dir := t.TempDir()
	writeGraphV2Formula(t, dir, "drain-demo.formula.toml", `
formula = "drain-demo"
version = 1
type = "workflow"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "drain"
title = "Drain"

[steps.drain]
context = "separate"
formula = "review-one"
`)

	_, err := CompileWithoutRuntimeVarValidation(context.Background(), "drain-demo", []string{dir}, nil)
	if err != nil {
		t.Fatalf("CompileWithoutRuntimeVarValidation: %v", err)
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

func TestValidateGraphV2RecipeRejectsDrainWithGate(t *testing.T) {
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
			},
			Gate: &RecipeGate{Type: "manual"},
		},
	}}

	err := ValidateGraphV2RecipeReservedSymbols(recipe, true)
	if err == nil {
		t.Fatal("ValidateGraphV2RecipeReservedSymbols succeeded, want drain gate error")
	}
	if !strings.Contains(err.Error(), "drain cannot be combined with gate") {
		t.Fatalf("error = %q, want drain gate error", err)
	}
}

func TestGraphV2OutputJSONWarnings(t *testing.T) {
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	defer SetFormulaV2Enabled(prev)

	tests := []struct {
		name      string
		toml      string
		wantCount int
		wantMsg   string
	}{
		{
			name: "graph.v2 step with output_json_required warns",
			toml: `
formula = "legacy-fanout"
version = 1
contract = "graph.v2"
[[steps]]
id = "worker"
prompt = "do work"
[steps.metadata]
"gc.output_json_required" = "true"
`,
			wantCount: 1,
			wantMsg:   "gc.output_json is legacy; use drain in graph.v2 formulas",
		},
		{
			name: "graph.v1 step with output_json_required does not warn",
			toml: `
formula = "v1-fanout"
version = 1
[[steps]]
id = "worker"
prompt = "do work"
[steps.metadata]
"gc.output_json_required" = "true"
`,
			wantCount: 0,
		},
		{
			name: "graph.v2 step using drain does not warn",
			toml: `
formula = "drain-fanout"
version = 1
contract = "graph.v2"
[[steps]]
id = "worker"
prompt = "do work"
[steps.drain]
context = "separate"
formula = "mol-do-work"
member_access = "exclusive"
`,
			wantCount: 0,
		},
		{
			name: "graph.v2 step with no fan-out does not warn",
			toml: `
formula = "no-fanout"
version = 1
contract = "graph.v2"
[[steps]]
id = "worker"
prompt = "do work"
`,
			wantCount: 0,
		},
		{
			name: "warning includes step id and formula name",
			toml: `
formula = "my-formula"
version = 1
contract = "graph.v2"
[[steps]]
id = "my-step"
prompt = "do work"
[steps.metadata]
"gc.output_json_required" = "true"
`,
			wantCount: 1,
			wantMsg:   "formula my-formula step my-step",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGraphV2Formula(t, dir, "f.formula.toml", tt.toml)
			p := NewParser(dir)
			f, err := p.ParseFile(filepath.Join(dir, "f.formula.toml"))
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			got := GraphV2OutputJSONWarnings(f)
			if len(got) != tt.wantCount {
				t.Errorf("warnings count = %d, want %d; got: %v", len(got), tt.wantCount, got)
			}
			if tt.wantMsg != "" && len(got) > 0 && !strings.Contains(got[0], tt.wantMsg) {
				t.Errorf("warning = %q, want to contain %q", got[0], tt.wantMsg)
			}
		})
	}
}

func writeGraphV2Formula(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.TrimSpace(contents)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
}

func intPtr(v int) *int {
	return &v
}
