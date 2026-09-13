package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestGCNonTestFilesUsePooledDoltConnections forbids raw database/sql
// Open calls in cmd/gc and internal/api non-test files (style of
// TestGCNonTestFilesStayOnWorkerBoundary). Every Go-native Dolt
// connection must go through the shared internal/doltpool registry: one
// pooled *sql.DB per endpoint with bounded open/idle/lifetime caps.
// Per-call sql.Open+Close churns connections — the
// dolt_project_id.go pattern alone produced 2,618 TIME_WAIT sockets —
// and bypasses the connection budget the supervisor doctor enforces
// (city-scale plan items 1.2 and 2.7).
//
// The guard resolves calls through the AST instead of matching the text
// "sql.Open(", so it also holds for an aliased import (import stdsql
// "database/sql"; stdsql.Open) and a dot import, ignores the string in
// comments and literals, and does not mistake sql.OpenDB for sql.Open.
// It walks both trees recursively, so the internal/api subpackages
// (apierr, dashboardbff, genclient) are covered too.
//
// If you need a deliberately fresh, unpooled connection (e.g. a health
// probe that must exercise new-connection setup), put it in a dedicated
// package outside cmd/gc and internal/api, or extend doltpool with an
// explicit fresh-dial API — do not add a raw Open here.
func TestGCNonTestFilesUsePooledDoltConnections(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	gcDir := filepath.Dir(currentFile)
	roots := []string{
		gcDir,
		filepath.Join(gcDir, "..", "..", "internal", "api"),
	}

	scanned := 0
	for _, root := range roots {
		for _, path := range sqlOpenGuardGoFiles(t, root) {
			scanned++
			for _, line := range sqlOpenGuardRawOpenLines(t, path) {
				t.Errorf("%s:%d calls database/sql Open directly; route the connection through internal/doltpool (shared pooled *sql.DB per endpoint)", path, line)
			}
		}
	}
	// A walk that matched no files at all would make this guard vacuous.
	if scanned == 0 {
		t.Fatal("scanned 0 non-test Go files; the guard is not looking at anything")
	}
}

// sqlOpenGuardGoFiles returns every non-test .go file under root.
func sqlOpenGuardGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "vendor", "testdata", ".git":
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", root, err)
	}
	return out
}

// sqlOpenGuardRawOpenLines reports the line of every call to
// database/sql's Open in the file at path, resolved through that file's
// own import declarations so aliased and dot imports are caught.
func sqlOpenGuardRawOpenLines(t *testing.T, path string) []int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %q: %v", path, err)
	}

	// The local names database/sql is bound to in this file. A blank
	// import only registers a driver and cannot call Open, so it is not
	// a binding.
	locals := make(map[string]bool)
	dotImported := false
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil || importPath != "database/sql" {
			continue
		}
		switch {
		case imp.Name == nil:
			locals["sql"] = true
		case imp.Name.Name == ".":
			dotImported = true
		case imp.Name.Name == "_":
		default:
			locals[imp.Name.Name] = true
		}
	}
	if len(locals) == 0 && !dotImported {
		return nil
	}

	var lines []int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel.Name != "Open" {
				return true
			}
			if ident, ok := fn.X.(*ast.Ident); ok && locals[ident.Name] {
				lines = append(lines, fset.Position(call.Pos()).Line)
			}
		case *ast.Ident:
			if dotImported && fn.Name == "Open" {
				lines = append(lines, fset.Position(call.Pos()).Line)
			}
		}
		return true
	})
	return lines
}
