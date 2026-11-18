package proctable

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// proctableDir returns the directory holding this package's sources: a test
// runs with it as the working directory, and under `bazel test` the target's
// data glob carries the package files into the runfiles copy.
func proctableDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return dir
}

// funcDeclCount returns how many package-level functions named name the
// non-test files that GOOS selects declare.
func funcDeclCount(t *testing.T, dir, goos, name string) int {
	t.Helper()
	ctx := build.Default
	ctx.GOOS = goos
	ctx.GOARCH = "amd64"
	ctx.CgoEnabled = false
	pkg, err := ctx.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("ImportDir(%q) for GOOS=%s: %v", dir, goos, err)
	}
	count := 0
	for _, file := range pkg.GoFiles {
		f, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, file), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s for GOOS=%s: %v", file, goos, perr)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				count++
			}
		}
	}
	return count
}

// TestScanEntryPointsDefinedExactlyOncePerGOOS pins the cross-platform
// contract of the scan entry points that untagged callers (for example
// internal/runtime/exec) use: every GOOS the repository builds for selects
// exactly one definition. A GOOS that selects none stops `GOOS=<os> go build
// ./cmd/gc/...` with "undefined: proctable.<name>"; one that selects two
// stops it with "redeclared".
func TestScanEntryPointsDefinedExactlyOncePerGOOS(t *testing.T) {
	dir := proctableDir(t)
	for _, goos := range []string{"linux", "darwin", "windows", "freebsd"} {
		for _, name := range []string{"ScanBySessionID", "ScanBySessionIDOutsideTmux"} {
			t.Run(goos+"/"+name, func(t *testing.T) {
				if got := funcDeclCount(t, dir, goos, name); got != 1 {
					t.Fatalf("GOOS=%s selects %d definitions of %s, want exactly 1", goos, got, name)
				}
			})
		}
	}
}
