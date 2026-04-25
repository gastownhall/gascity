package cityinit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPackageDoesNotExposeInputOutputWriters(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || filepath.Base(name) == "no_io_boundary_test.go" {
			continue
		}
		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%q): %v", path, err)
		}
		for _, imp := range parsed.Imports {
			if imp.Path.Value == `"io"` {
				t.Fatalf("%s imports io; keep user-facing input/output at cmd/api edges", name)
			}
		}

		parsed, err = parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%q): %v", path, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if ok && ident.Name == "io" {
				t.Fatalf("%s references io.%s; keep input/output wiring outside internal/cityinit", name, sel.Sel.Name)
			}
			return true
		})
	}
}
