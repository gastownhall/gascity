package workladder

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestPackageIsPure pins WL7 at the source: the package imports only these
// standard-library packages and never reads the clock or randomness. The
// v2purity analyzer checks the same from cmd/gc's //gc:pure roots once one
// calls Decide (W3); this test holds the line in the package itself.
func TestPackageIsPure(t *testing.T) {
	allowed := map[string]bool{"cmp": true, "encoding/json": true, "fmt": true, "slices": true, "strings": true, "time": true}
	forbidden := map[string]bool{
		"time.Now": true, "time.Since": true, "time.Until": true, "time.Sleep": true, "time.After": true,
		"time.Tick": true, "time.NewTimer": true, "time.NewTicker": true, "time.AfterFunc": true,
	}
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, ent := range ents {
		name := ent.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		seen++
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !allowed[path] {
				t.Errorf("%s imports %q: workladder is pure (standard library: %v)", name, path, allowed)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && forbidden[id.Name+"."+sel.Sel.Name] {
				t.Errorf("%s: %s.%s reads the clock", fset.Position(sel.Pos()), id.Name, sel.Sel.Name)
			}
			return true
		})
	}
	if seen == 0 {
		t.Fatal("no package sources found")
	}
}
