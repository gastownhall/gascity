package formula

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// writeLifecycleFormulas writes each name -> TOML body into a fresh search
// directory and returns it.
func writeLifecycleFormulas(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// lifecycleStamps returns the gc.input_lifecycle value of every recipe step
// that carries one, keyed by step id.
func lifecycleStamps(recipe *Recipe) map[string]string {
	stamps := map[string]string{}
	for _, step := range recipe.Steps {
		if v, ok := step.Metadata[beadmeta.InputLifecycleMetadataKey]; ok {
			stamps[step.ID] = v
		}
	}
	return stamps
}

const graphLifecycleParent = `
formula = "base"
version = 1
input_lifecycle = "graph"

[requires]
formula_compiler = ">=2.1.0"

[[steps]]
id = "settle"
title = "Settle the input"

[[steps]]
id = "report"
title = "Report"
needs = ["settle"]
`

// TestCompileInputLifecycleStampsOnlyTheRoot: a graph-owned declaration
// reaches the compiled workflow root and no other step, directly and through
// inheritance; an explicit external declaration overrides an inherited graph
// one and, like an absent declaration, stamps nothing.
func TestCompileInputLifecycleStampsOnlyTheRoot(t *testing.T) {
	enableV2ForTest(t)
	cases := []struct {
		name  string
		files map[string]string
		want  map[string]string
	}{
		{
			name:  "declared_graph",
			files: map[string]string{"base": graphLifecycleParent},
			want:  map[string]string{"base": beadmeta.InputLifecycleGraph},
		},
		{
			name: "inherited_graph",
			files: map[string]string{"base": graphLifecycleParent, "derived": `
formula = "derived"
version = 1
extends = ["base"]
`},
			want: map[string]string{"derived": beadmeta.InputLifecycleGraph},
		},
		{
			name: "external_overrides_inherited_graph",
			files: map[string]string{"base": graphLifecycleParent, "derived": `
formula = "derived"
version = 1
extends = ["base"]
input_lifecycle = "external"
`},
			want: map[string]string{},
		},
		{
			name: "absent_is_external",
			files: map[string]string{"base": `
formula = "base"
version = 1

[requires]
formula_compiler = ">=2.1.0"

[[steps]]
id = "settle"
title = "Settle the input"
`},
			want: map[string]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeLifecycleFormulas(t, tc.files)
			name := "base"
			if _, ok := tc.files["derived"]; ok {
				name = "derived"
			}
			recipe, err := Compile(context.Background(), name, []string{dir}, nil)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if root := recipe.RootStep(); root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow {
				t.Fatalf("root gc.kind = %q, want a graph workflow root", root.Metadata[beadmeta.KindMetadataKey])
			}
			got := lifecycleStamps(recipe)
			if len(got) != len(tc.want) {
				t.Fatalf("gc.input_lifecycle stamps = %v, want %v", got, tc.want)
			}
			for id, value := range tc.want {
				if got[id] != value {
					t.Fatalf("gc.input_lifecycle stamps = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestCompileInputLifecycleRejectsInvalidDeclarations: an unknown value, a
// declaration on an expansion or aspect formula, a declaration on a v1
// formula, and graph ownership without a compiler requirement that excludes
// the 2.0.0 compiler (which would silently drop it) all fail compilation with
// an error naming input_lifecycle.
func TestCompileInputLifecycleRejectsInvalidDeclarations(t *testing.T) {
	enableV2ForTest(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown_value",
			body: `
formula = "f"
version = 1
input_lifecycle = "shared"

[requires]
formula_compiler = ">=2.1.0"

[[steps]]
id = "a"
title = "A"
`,
			want: `input_lifecycle: invalid value "shared"`,
		},
		{
			name: "expansion_formula",
			body: `
formula = "f"
version = 1
type = "expansion"
input_lifecycle = "graph"

[requires]
formula_compiler = ">=2.1.0"

[[template]]
id = "{target}.a"
title = "A"
`,
			want: `input_lifecycle: not allowed on expansion formulas`,
		},
		{
			name: "aspect_formula",
			body: `
formula = "f"
version = 1
type = "aspect"
input_lifecycle = "external"

[requires]
formula_compiler = ">=2.1.0"
`,
			want: `input_lifecycle: not allowed on aspect formulas`,
		},
		{
			name: "v1_formula",
			body: `
formula = "f"
version = 1
input_lifecycle = "graph"

[[steps]]
id = "a"
title = "A"
`,
			want: `input_lifecycle requires a formulas v2 graph workflow`,
		},
		{
			name: "graph_without_2_1_requirement",
			body: `
formula = "f"
version = 1
input_lifecycle = "graph"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "a"
title = "A"
`,
			want: `input_lifecycle = "graph" requires [requires] formula_compiler = ">=2.1.0"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeLifecycleFormulas(t, map[string]string{"f": tc.body})
			_, err := Compile(context.Background(), "f", []string{dir}, nil)
			requireErrorContains(t, err, tc.want)
		})
	}
}

// TestResolveInputLifecycleFirstDeclaringParentWins: a child that declares no
// input_lifecycle takes the value of its first declaring parent, so a later
// parent's external declaration does not override an earlier parent's graph.
func TestResolveInputLifecycleFirstDeclaringParentWins(t *testing.T) {
	dir := t.TempDir()
	writeNamedFormula(t, dir, "graph-parent", `
formula = "graph-parent"
input_lifecycle = "graph"

[requires]
formula_compiler = ">=2.1.0"

[[steps]]
id = "work"
title = "Work"
`)
	writeNamedFormula(t, dir, "external-parent", `
formula = "external-parent"
input_lifecycle = "external"

[requires]
formula_compiler = ">=2.1.0"

[[steps]]
id = "audit"
title = "Audit"
`)
	writeNamedFormula(t, dir, "two-parent-child", `
formula = "two-parent-child"
extends = ["graph-parent", "external-parent"]

[requires]
formula_compiler = ">=2.1.0"

[[steps]]
id = "tail"
title = "Tail"
`)
	resolved := resolveRequirementFormula(t, dir, "two-parent-child")
	if resolved.InputLifecycle != beadmeta.InputLifecycleGraph {
		t.Fatalf("InputLifecycle = %q, want the first declaring parent's graph", resolved.InputLifecycle)
	}
}
