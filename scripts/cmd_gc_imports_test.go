package scripts_test

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"
)

// Kills a reintroduction of the per-key workqueue (C3): no package cmd/gc
// builds from, nor cmd/gc's own tests, imports internal/workqueue.
func TestCmdGcDoesNotImportWorkqueue(t *testing.T) {
	const module = "github.com/gastownhall/gascity/"
	const banned = module + "internal/workqueue"
	root := repoRoot(t)
	seen := map[string]bool{}
	var walk func(path string, tests bool, from string)
	walk = func(path string, tests bool, from string) {
		if path == banned {
			t.Errorf("%s imports %s", from, banned)
			return
		}
		if seen[path] || !strings.HasPrefix(path, module) {
			return
		}
		seen[path] = true
		pkg, err := build.Default.ImportDir(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, module))), 0)
		if err != nil {
			t.Fatalf("resolving %s: %v", path, err)
		}
		imports := pkg.Imports
		if tests {
			imports = append(imports, append(pkg.TestImports, pkg.XTestImports...)...)
		}
		for _, imp := range imports {
			walk(imp, false, path)
		}
	}
	walk(module+"cmd/gc", true, "")
	if len(seen) < 10 {
		t.Fatalf("walked only %d packages, want cmd/gc's module dependencies", len(seen))
	}
}
