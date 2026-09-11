package runtime

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stagingExemption is the one root-package file allowed to import other
// gascity internal packages. staging.go depends on internal/overlay; it
// rides along until workspace staging is relocated or overlay becomes
// protocol data (tracked under the runtime-provider-packs initiative,
// ga-1symz6).
const stagingExemption = "staging.go"

// TestRuntimeContractPackageStaysStdlibOnly pins the root internal/runtime
// package to standard-library imports. The package is the in-process
// expression of the Runtime Provider Protocol contract
// (engdocs/design/runtime-provider-packs.md, RUNTIME-INV-001 in
// REQUIREMENTS.md): it must not accrete dependencies on the rest of the
// SDK, or providers and the conformance suite drag the world with them.
func TestRuntimeContractPackageStaysStdlibOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == stagingExemption {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !isStdlibImport(path) {
				t.Errorf("%s imports %q; the runtime contract package must stay stdlib-only (move the dependency behind the provider or into a subpackage)", name, path)
			}
		}
	}
}

// isStdlibImport reports whether the import path is part of the Go
// standard library: its first path element contains no dot (no domain).
func isStdlibImport(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func TestStagingExemptionFileStillExists(t *testing.T) {
	if _, err := os.Stat(filepath.Clean(stagingExemption)); err != nil {
		t.Fatalf("exempted file %s is gone — delete the exemption in this test: %v", stagingExemption, err)
	}
}
