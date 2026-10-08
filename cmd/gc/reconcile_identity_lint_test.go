package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The identity lint (CONTRACT v5 R3, I6, I11): no v2 file reads
// GC_INSTANCE_TOKEN, GC_SESSION_ID or GC_RUNTIME_EPOCH except through the
// identity reader (readRuntimeIdentity, over identityEnvKeys), so
// compareIdentity is the only reader of runtime identity; and no v2 file
// but the comparator's parses a runtime epoch out of a struct field, so the
// own-runtime and StaleSelf tests are never re-derived (ownRuntime).

// identityEnvKeyNames are the identity env keys.
var identityEnvKeyNames = []string{"GC_INSTANCE_TOKEN", "GC_SESSION_ID", "GC_RUNTIME_EPOCH"}

// identityLintAllowed names, per file, the one declaration that may spell
// the keys: the identity reader's key list.
var identityLintAllowed = map[string]string{"runtime_inventory_lane.go": "identityEnvKeys"}

// epochParsers are the calls that parse a number out of a string.
var epochParsers = []string{"Atoi", "ParseInt", "ParseUint", "Sscan", "Sscanf"}

// lintIdentitySource returns one "file:line: what" per violation in src,
// parsed as path.
func lintIdentitySource(t *testing.T, path string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	base := filepath.Base(path)
	var out []string
	for _, decl := range file.Decls {
		allowed := identityLintAllowed[base] != "" && declNames(decl)[identityLintAllowed[base]]
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if v, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && slices.Contains(identityEnvKeyNames, v) && !allowed {
					out = append(out, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), v))
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || !slices.Contains(epochParsers, sel.Sel.Name) || base == "reconcile_identity.go" {
					return true
				}
				for _, arg := range n.Args {
					if readsEpochField(arg) {
						out = append(out, fmt.Sprintf("%s: epoch parsed by %s", fset.Position(n.Pos()), sel.Sel.Name))
					}
				}
			}
			return true
		})
	}
	return out
}

// declNames are the names decl declares.
func declNames(decl ast.Decl) map[string]bool {
	names := make(map[string]bool)
	switch d := decl.(type) {
	case *ast.FuncDecl:
		names[d.Name.Name] = true
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			if v, ok := spec.(*ast.ValueSpec); ok {
				for _, n := range v.Names {
					names[n.Name] = true
				}
			}
		}
	}
	return names
}

// readsEpochField reports whether e reads a field named Epoch.
func readsEpochField(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Epoch" {
			found = true
		}
		return !found
	})
	return found
}

// identityLintFiles are the v2 files: the allocator, the planner and its
// effects and steps, the inventory lane, and the other v2 files.
func identityLintFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pattern := range []string{"allocator_*.go", "reconcile_*.go", "runtime_inventory_*.go", "v2_*.go", "lane_pacing.go"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				files = append(files, m)
			}
		}
	}
	if len(files) == 0 {
		t.Fatal("the identity lint matched no v2 file")
	}
	return files
}

// Kills a lint that matches nothing, an allowance that leaks past the
// identity reader's key list or the comparator's file, and a v2 file that
// reads the identity env or re-parses an epoch: the seeded file trips every
// rule outside the allowances and only the allowed rule inside each, and the
// v2 files trip none.
func TestIdentityLintBansDirectEnvReads(t *testing.T) {
	seeded, err := os.ReadFile(filepath.Join("testdata", "identitylint", "seeded.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		as   string
		want int
	}{
		{"reconcile_effect_seeded.go", 6},
		{"allocator_seeded.go", 6},
		{"runtime_inventory_lane.go", 5}, // identityEnvKeys allowed
		{"reconcile_identity.go", 4},     // epoch parsing allowed
	} {
		if got := lintIdentitySource(t, tc.as, seeded); len(got) != tc.want {
			t.Errorf("seeded as %s: %d findings %v, want %d", tc.as, len(got), got, tc.want)
		}
	}
	for _, f := range identityLintFiles(t) {
		for _, finding := range lintIdentitySource(t, f, nil) {
			t.Errorf("identity read outside the identity reader: %s", finding)
		}
	}
}
