package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

// repoRootDir returns the repository root derived from this test file's
// location so the test works from any working directory.
func repoRootDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

// shippedFormulaDirs returns every bundled and example formula directory that
// ships with the repository. These are the formulas downstream cities receive,
// so all of them must stay compatible with graph.v2 composition.
func shippedFormulaDirs(t *testing.T) []string {
	t.Helper()
	root := repoRootDir(t)
	patterns := []string{
		filepath.Join(root, "internal", "bootstrap", "packs", "*", "formulas"),
		filepath.Join(root, "examples", "*", "formulas"),
		filepath.Join(root, "examples", "*", "packs", "*", "formulas"),
	}
	var dirs []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil || !info.IsDir() {
				continue
			}
			dirs = append(dirs, match)
		}
	}
	if len(dirs) == 0 {
		t.Fatal("no shipped formula directories found")
	}
	return dirs
}

// shippedFormulaNames returns the formula name declared in every TOML file
// under the shipped formula directories.
func shippedFormulaNames(t *testing.T, dirs []string) []string {
	t.Helper()
	seen := make(map[string]bool)
	var names []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".toml")
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatal("no shipped formulas found")
	}
	return names
}

// TestShippedFormulasHaveNoLegacyIssueRefs is the whole-pack regression test
// for gastownhall/gascity#2941: every bundled and example formula must use the
// graph.v2 convoy_id work-bead derivation, never the legacy issue/bead_id
// variables. #2784 reserved those names in graph.v2 but only migrated two of
// the bundled formulas; #3056 then reintroduced the legacy pattern because no
// test guarded the whole pack. This test makes the migration durable.
func TestShippedFormulasHaveNoLegacyIssueRefs(t *testing.T) {
	dirs := shippedFormulaDirs(t)
	parser := formula.NewParser(dirs...)
	for _, name := range shippedFormulaNames(t, dirs) {
		name := name
		t.Run(name, func(t *testing.T) {
			loaded, err := parser.LoadByName(name)
			if err != nil {
				t.Fatalf("load %s: %v", name, err)
			}
			resolved, err := parser.Resolve(loaded)
			if err != nil {
				t.Fatalf("resolve %s: %v", name, err)
			}
			if refs := formula.GraphV2LegacyIssueRefsTransitively(resolved, parser); len(refs) != 0 {
				t.Errorf("formula %s still uses legacy issue/bead_id symbols:\n  %s",
					name, strings.Join(refs, "\n  "))
			}
		})
	}
}

// TestShippedWorkflowFormulasComposeUnderGraphV2 asserts that a graph.v2
// formula extending any shipped workflow formula passes reserved-symbol
// validation. This is the exact contamination path reported in
// gastownhall/gascity#2941: [requires] propagates through extends, so a
// shipped formula carrying legacy issue refs poisons every downstream
// graph.v2 composition that touches it.
func TestShippedWorkflowFormulasComposeUnderGraphV2(t *testing.T) {
	dirs := shippedFormulaDirs(t)
	baseParser := formula.NewParser(dirs...)
	for _, name := range shippedFormulaNames(t, dirs) {
		name := name
		t.Run(name, func(t *testing.T) {
			loaded, err := baseParser.LoadByName(name)
			if err != nil {
				t.Fatalf("load %s: %v", name, err)
			}
			resolved, err := baseParser.Resolve(loaded)
			if err != nil {
				t.Fatalf("resolve %s: %v", name, err)
			}
			if resolved.Type != formula.TypeWorkflow {
				t.Skipf("%s is %s, not a workflow", name, resolved.Type)
			}

			tmpDir := t.TempDir()
			child := fmt.Sprintf(`description = "synthetic graph.v2 extension of %s"
formula = "synthetic-graphv2-extension"
extends = [%q]

[requires]
formula_compiler = ">=2.0.0"
`, name, name)
			childPath := filepath.Join(tmpDir, "synthetic-graphv2-extension.toml")
			if err := os.WriteFile(childPath, []byte(child), 0o644); err != nil {
				t.Fatalf("write synthetic extension: %v", err)
			}

			parser := formula.NewParser(append([]string{tmpDir}, dirs...)...)
			loadedChild, err := parser.LoadByName("synthetic-graphv2-extension")
			if err != nil {
				t.Fatalf("load synthetic extension of %s: %v", name, err)
			}
			resolvedChild, err := parser.Resolve(loadedChild)
			if err != nil {
				t.Fatalf("resolve synthetic extension of %s: %v", name, err)
			}
			if err := formula.ValidateGraphV2ReservedSymbolsTransitively(resolvedChild, parser, true); err != nil {
				t.Errorf("graph.v2 formula extending %s fails validation:\n%v", name, err)
			}
		})
	}
}
