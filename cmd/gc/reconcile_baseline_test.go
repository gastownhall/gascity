package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// Kills a baseline key written beside the builder (v5 M2, I21): v2's arms,
// effects and steps name none of the six baseline keys, nor call legacy's
// hash builders, outside reconcile_baseline.go; the legacy builders the
// reused paths emit (CommitStartedPatch, ConversationResetPatch,
// RestartRequestPatch) are calls, not literals, so they pass.
func TestBaselineKeysOnlyThroughTheBuilder(t *testing.T) {
	keys := map[string]bool{"started_config_hash": true, "started_provision_hash": true, "started_launch_hash": true, "core_hash_breakdown": true, "started_live_hash": true, "live_hash": true}
	builders := map[string]bool{"sessionHashRebaselineMetadata": true, "silentRebaselineSessionHashes": true, "softReloadAcceptedHashMetadata": true}
	var files []string
	for _, glob := range []string{"reconcile_arms_*.go", "reconcile_effect_*.go", "reconcile_steps_*.go", "reconcile_baseline.go"} {
		m, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if filepath.Ext(name) != ".go" || len(name) > 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if v, err := strconv.Unquote(n.Value); err == nil && n.Kind == token.STRING && keys[v] && name != "reconcile_baseline.go" {
					t.Errorf("%s: baseline key %q outside baselinePatch", fset.Position(n.Pos()), v)
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && builders[id.Name] {
					t.Errorf("%s: %s writes the baseline outside baselinePatch", fset.Position(n.Pos()), id.Name)
				}
			}
			return true
		})
	}
	if len(files) < 10 {
		t.Fatalf("scanned %d files: the globs found none of v2's", len(files))
	}
}
