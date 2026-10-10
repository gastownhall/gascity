// Package testunit tells library compile units from test compile units.
//
// Bazel compiles a Go package twice when it has internal tests: the library
// alone (go_library) and the library plus its _test.go files (go_test's
// internal archive). nogo analyzes both. golangci-lint, like `go vet`, analyzes
// only the test variant of a package that has tests, so an unexported helper
// or parameter used only from tests is not "unused" there. Analyzers whose
// verdict depends on every caller in the package (unused, unparam) therefore
// report only from units that contain test files.
//
// A package with no internal test files has no such unit, so those analyzers
// do not check it under nogo (they did under golangci-lint).
package testunit

import (
	"strings"

	"golang.org/x/tools/go/analysis"
)

// HasTestFiles reports whether the pass's compile unit includes _test.go files.
func HasTestFiles(pass *analysis.Pass) bool {
	for _, f := range pass.Files {
		if strings.HasSuffix(pass.Fset.File(f.Pos()).Name(), "_test.go") {
			return true
		}
	}
	return false
}
