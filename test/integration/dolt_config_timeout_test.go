package integration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDoltConfigTimeoutsUseNamedConstants statically guards the class of bug
// fixed in ga-gajll3: runBDInitCompat carried its own hardcoded timeout literal
// that silently drifted out of sync with bdInitTimeout, the constant its
// siblings in bdstore_test.go use for the same class of wait. It parses
// dolt_config_test.go and asserts the call site passes a named constant, not a
// bare literal, so a future edit can't reintroduce the drift without this test
// going red. Server readiness is not budgeted here: dolttest.StartSQLServer
// owns that wait.
func TestDoltConfigTimeoutsUseNamedConstants(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "dolt_config_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing dolt_config_test.go: %v", err)
	}

	var initTimeoutArgs []ast.Expr

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "WithTimeout" && len(call.Args) == 2 {
			// context.WithTimeout(context.Background(), <budget>)
			initTimeoutArgs = append(initTimeoutArgs, call.Args[1])
		}
		return true
	})

	if len(initTimeoutArgs) != 1 {
		t.Fatalf("expected exactly 1 context.WithTimeout call in dolt_config_test.go, found %d — update this test's scoping", len(initTimeoutArgs))
	}

	requireNamedConstant(t, "runBDInitCompat's context.WithTimeout budget", initTimeoutArgs[0], "bdInitTimeout")
}

func requireNamedConstant(t *testing.T, what string, expr ast.Expr, want string) {
	t.Helper()
	ident, ok := expr.(*ast.Ident)
	if !ok {
		t.Fatalf("%s must reference the named constant %s, not a bare literal expression (got %T) — hardcoded timeout literals drift out of sync silently; see ga-gajll3", what, want, expr)
	}
	if ident.Name != want {
		t.Fatalf("%s = %s, want %s", what, ident.Name, want)
	}
}
