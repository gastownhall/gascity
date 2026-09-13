package cityinit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPackageDoesNotExposeInputOutputWriters(t *testing.T) {
	for _, file := range packageGoFiles(t) {
		name := filepath.Base(file)
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%q): %v", file, err)
		}
		for _, imp := range parsed.Imports {
			if imp.Path.Value == `"io"` {
				t.Fatalf("%s imports io; keep user-facing input/output at cmd/api edges", name)
			}
		}

		parsed, err = parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%q): %v", file, err)
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

func TestPackageBoundary_NoDataIO(t *testing.T) {
	for _, file := range packageGoFiles(t) {
		name := filepath.Base(file)
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%q): %v", file, err)
		}
		for _, imp := range parsed.Imports {
			if imp.Path.Value == `"os"` {
				t.Fatalf("%s imports os; route filesystem I/O through ScaffoldFS port", name)
			}
		}

		parsed, err = parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%q): %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name == "filepath" && sel.Sel.Name == "Walk" {
				t.Fatalf("%s references filepath.Walk; use ScaffoldFS.Walk instead", name)
			}
			return true
		})
	}
}

func packageGoFiles(t *testing.T) []string {
	t.Helper()
	_, caller, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(caller)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files
}
