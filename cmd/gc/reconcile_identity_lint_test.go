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

// The identity lint (CONTRACT v5 R3, I6, I11): compareIdentity is the only
// way a v2 file decides whose runtime sits under a name. So no v2 file:
//   - reads GC_INSTANCE_TOKEN, GC_SESSION_ID or GC_RUNTIME_EPOCH except
//     through the identity reader (readRuntimeIdentity, over identityEnvKeys);
//   - but the comparator's parses a runtime epoch out of a struct field, so
//     the own-runtime and StaleSelf tests are never re-derived (ownRuntime);
//   - but the comparator's and the inventory's reads the legacy owner fact
//     (OwnerState and its values, OwnerID), or compares a SessionID or Token
//     field with anything but "" (an emptiness check decides no owner);
//   - calls legacy's own identity readers.

// identityEnvKeyNames are the identity env keys.
var identityEnvKeyNames = []string{"GC_INSTANCE_TOKEN", "GC_SESSION_ID", "GC_RUNTIME_EPOCH"}

// identityLintAllowed names, per file, the one declaration that may spell
// the keys: the identity reader's key list.
var identityLintAllowed = map[string]string{"runtime_inventory_lane.go": "identityEnvKeys"}

// epochParsers are the calls that parse a number out of a string.
var epochParsers = []string{"Atoi", "ParseInt", "ParseUint", "Sscan", "Sscanf"}

// ownerFactNames are the legacy owner fact's names.
var ownerFactNames = []string{"OwnerState", "OwnerID", "OwnerSession", "OwnerNone", "OwnerUnknown"}

// identityFields are the identity fields no v2 file compares itself.
var identityFields = []string{"SessionID", "Token"}

// legacyIdentityReaders are legacy's own ownership readers.
var legacyIdentityReaders = []string{
	"readPendingCreateIdentity", "readPendingCreateIdentityVia", "attributePendingCreateRuntime", "deadRuntimeBelongsToRow",
}

// identityOwnerFile reports whether base is the comparator's file or an
// inventory file, which own the owner fact and the identity read.
func identityOwnerFile(base string) bool {
	return base == "reconcile_identity.go" || strings.HasPrefix(base, "runtime_inventory_") || strings.HasPrefix(base, "runtime_observation_")
}

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
	owner := identityOwnerFile(base)
	var out []string
	report := func(n ast.Node, what string) { out = append(out, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), what)) }
	for _, decl := range file.Decls {
		allowed := identityLintAllowed[base] != "" && declNames(decl)[identityLintAllowed[base]]
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if v, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && slices.Contains(identityEnvKeyNames, v) && !allowed {
					report(n, v)
				}
			case *ast.Ident:
				switch {
				case slices.Contains(ownerFactNames, n.Name) && !owner:
					report(n, "owner fact "+n.Name)
				case slices.Contains(legacyIdentityReaders, n.Name):
					report(n, "legacy identity reader "+n.Name)
				}
			case *ast.BinaryExpr:
				if (n.Op == token.EQL || n.Op == token.NEQ) && !owner && comparesIdentityField(n) {
					report(n, "identity field compared")
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || !slices.Contains(epochParsers, sel.Sel.Name) || base == "reconcile_identity.go" {
					return true
				}
				for _, arg := range n.Args {
					if readsEpochField(arg) {
						report(n, "epoch parsed by "+sel.Sel.Name)
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

// comparesIdentityField reports whether b compares a SessionID or Token
// field with something other than "".
func comparesIdentityField(b *ast.BinaryExpr) bool {
	field := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && slices.Contains(identityFields, sel.Sel.Name)
	}
	empty := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		return ok && lit.Value == `""`
	}
	return (field(b.X) || field(b.Y)) && !empty(b.X) && !empty(b.Y)
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
	for _, pattern := range []string{"allocator_*.go", "reconcile_*.go", "runtime_inventory_*.go", "runtime_observation_*.go", "v2_*.go", "lane_pacing.go"} {
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
// identity reader's key list, the comparator's file or the inventory's, and
// a v2 file that reads the identity env, re-parses an epoch, reads the owner
// fact, compares identity fields itself or calls a legacy identity reader:
// the seeded file trips every rule outside the allowances and only the
// allowed rules inside each, and the v2 files trip none.
func TestIdentityLintBansDirectEnvReads(t *testing.T) {
	seeded, err := os.ReadFile(filepath.Join("testdata", "identitylint", "seeded.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		as   string
		want int
	}{
		{"reconcile_effect_seeded.go", 10},
		{"allocator_seeded.go", 10},
		{"runtime_inventory_lane.go", 6},    // identityEnvKeys, the owner fact and comparisons allowed
		{"runtime_observation_cache.go", 7}, // the owner fact and comparisons allowed
		{"reconcile_identity.go", 5},        // epoch parsing, the owner fact and comparisons allowed
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
