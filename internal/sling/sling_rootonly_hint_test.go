package sling

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/runtime"
)

func compileHintFixture(t *testing.T, name, content string) *formula.Recipe {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recipe, err := formula.Compile(context.Background(), name, []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile(%s): %v", name, err)
	}
	return recipe
}

// TestRootOnlyVaporPourHint pins the cause (a) vs cause (b) distinction: only a
// vapor formula without pour = true (a) gets the hint; a genuinely step-less
// formula (b), a poured vapor formula, and a normal multi-step formula do not.
func TestRootOnlyVaporPourHint(t *testing.T) {
	tests := []struct {
		name     string
		formula  string
		content  string
		wantHint bool
	}{
		{
			name:     "vapor without pour (cause a)",
			formula:  "patrol",
			content:  "formula = \"patrol\"\nversion = 1\nphase = \"vapor\"\n\n[[steps]]\nid = \"scan\"\ntitle = \"Scan\"\n",
			wantHint: true,
		},
		{
			name:     "step-less formula (cause b)",
			formula:  "router",
			content:  "formula = \"router\"\nversion = 1\n",
			wantHint: false,
		},
		{
			name:     "vapor with pour",
			formula:  "eager",
			content:  "formula = \"eager\"\nversion = 1\nphase = \"vapor\"\npour = true\n\n[[steps]]\nid = \"scan\"\ntitle = \"Scan\"\n",
			wantHint: false,
		},
		{
			name:     "normal multi-step liquid",
			formula:  "build",
			content:  "formula = \"build\"\nversion = 1\n\n[[steps]]\nid = \"a\"\ntitle = \"A\"\n\n[[steps]]\nid = \"b\"\ntitle = \"B\"\n",
			wantHint: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recipe := compileHintFixture(t, tc.formula, tc.content)
			got := rootOnlyVaporPourHint(tc.formula, recipe)
			if tc.wantHint {
				if got == "" {
					t.Fatalf("want hint, got empty (RootOnly=%v Phase=%q Pour=%v)", recipe.RootOnly, recipe.Phase, recipe.Pour)
				}
				if !strings.Contains(got, "pour = true") || !strings.Contains(got, tc.formula) {
					t.Errorf("hint missing key content: %q", got)
				}
				return
			}
			if got != "" {
				t.Errorf("want no hint, got %q (RootOnly=%v Phase=%q Pour=%v)", got, recipe.RootOnly, recipe.Phase, recipe.Pour)
			}
		})
	}
}

func TestRootOnlyVaporPourHintNilRecipe(t *testing.T) {
	if got := rootOnlyVaporPourHint("x", nil); got != "" {
		t.Errorf("nil recipe: want empty, got %q", got)
	}
}

// TestDoSlingFormulaVaporNoPourEmitsHint locks the wiring: a vapor-without-pour
// formula slung via the --formula path surfaces the hint through
// SlingResult.BeadWarnings (which the CLI prints to stderr).
func TestDoSlingFormulaVaporNoPourEmitsHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root-only.toml"), []byte(
		"formula = \"root-only\"\nversion = 1\nphase = \"vapor\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{City: []string{dir}},
	}
	a := config.Agent{Name: "agent-a", MaxActiveSessions: intPtr(3)}
	deps := testDeps(cfg, sp, runner.run)
	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "root-only", IsFormula: true}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	var found bool
	for _, w := range result.BeadWarnings {
		if strings.Contains(w, "pour = true") {
			found = true
		}
	}
	if !found {
		t.Fatalf("vapor-no-pour sling: want pour hint in BeadWarnings, got %v", result.BeadWarnings)
	}
}
