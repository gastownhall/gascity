package rollout

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRolloutImportBoundary is the structural half of the general-Auto guarantee
// and the "no capability flags" line: internal/rollout non-test files may import
// ONLY the standard library, internal/config, internal/deps, and its own
// dependency-leaf subpackage internal/rollout/gate. Importing any consumer
// package — internal/beads above all, but also beads-adjacent
// internal/beadmeta, internal/dispatch, internal/molecule, internal/events —
// fails this test naming the file and package. The capability model is general;
// beads CAS is merely its first consumer and lives on the OTHER side of this line.
// The gate subpackage is held to a stricter bar: stdlib only, nothing else —
// it is the half consumers import, so any non-stdlib import could reopen the
// config→orders→beads cycle that forced the split.
func TestRolloutImportBoundary(t *testing.T) {
	t.Parallel()
	const self = "github.com/gastownhall/gascity/internal/rollout"
	const gatePkg = self + "/gate"
	allowedInternal := map[string]bool{
		"github.com/gastownhall/gascity/internal/config": true,
		"github.com/gastownhall/gascity/internal/deps":   true,
		gatePkg: true,
	}

	checkDir := func(dir string, allowed func(path string) bool, rule string) int {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read package dir %s: %v", dir, err)
		}
		fset := token.NewFileSet()
		checked := 0
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			checked++
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				if allowed(p) {
					continue
				}
				t.Errorf("%s imports disallowed package %q; %s", path, p, rule)
			}
		}
		return checked
	}

	checked := checkDir(".", func(p string) bool {
		return isStdlibImport(p) || allowedInternal[p] || p == self
	}, "internal/rollout must import only stdlib + internal/config + internal/deps + "+
		"internal/rollout/gate (it must never reach a consumer like internal/beads)")
	checked += checkDir("gate", isStdlibImport,
		"internal/rollout/gate is the dependency-leaf consumers import and must stay stdlib-only")
	if checked == 0 {
		t.Fatal("import-boundary test scanned zero non-test package files")
	}
}

// isStdlibImport reports whether an import path is a standard-library package:
// its first path segment carries no dot, i.e. no module domain.
func isStdlibImport(p string) bool {
	seg := p
	if i := strings.IndexByte(p, '/'); i >= 0 {
		seg = p[:i]
	}
	return !strings.Contains(seg, ".")
}
