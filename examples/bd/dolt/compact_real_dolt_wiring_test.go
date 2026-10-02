//go:build integration || dolt_integration

package dolt_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestCompactRealDoltCallWiring guards the three one-shot call sites. A test
// of doltCallContext alone does not catch a helper that stops using it.
func TestCompactRealDoltCallWiring(t *testing.T) {
	file := compactRealDoltSource(t)
	for _, name := range []string{
		"runDoltForCompactTest",
		"doltServerQueryForCompactTest",
		"runCompactScriptForRealDoltTest",
	} {
		t.Run(name, func(t *testing.T) {
			assertCompactCommandWiring(t, compactFunction(t, file, name), true)
		})
	}
	// The readiness probe intentionally has its own short timeout because it
	// retries, but its child can still hold a command pipe open after exit.
	t.Run("waitForDoltServerQueryForCompactTest", func(t *testing.T) {
		assertCompactCommandWiring(t, compactFunction(t, file, "waitForDoltServerQueryForCompactTest"), false)
	})
}

func TestCompactRealDoltDefaultWaitDelay(t *testing.T) {
	file := compactRealDoltSource(t)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != "doltCallWaitDelay" {
				continue
			}
			if len(value.Values) != 1 || !compactTenSeconds(value.Values[0]) {
				t.Fatal("doltCallWaitDelay must default to 10 seconds")
			}
			return
		}
	}
	t.Fatal("doltCallWaitDelay default is missing")
}

func compactRealDoltSource(t *testing.T) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "compact_real_dolt_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parse compact real-Dolt helpers: %v", err)
	}
	return file
}

func compactFunction(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("compact helper %s is missing", name)
	return nil
}

func assertCompactCommandWiring(t *testing.T, fn *ast.FuncDecl, deadlineBound bool) {
	t.Helper()
	var contextCall, commandCall, waitDelay, outputCall token.Pos
	contextCount, commandCount, waitDelayCount := 0, 0, 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && compactSelector(call.Fun, "cmd", "CombinedOutput") {
			outputCall = call.Pos()
		}
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 {
			return true
		}
		value := assignment.Rhs[0]
		if compactAssignedName(assignment.Lhs, "ctx") {
			contextCount++
			call, ok := value.(*ast.CallExpr)
			if ok && ((deadlineBound && compactIdent(call.Fun, "doltCallContext") &&
				len(call.Args) == 1 && compactIdent(call.Args[0], "t")) ||
				(!deadlineBound && compactSelector(call.Fun, "context", "WithTimeout"))) {
				contextCall = assignment.Pos()
			}
		}
		if compactAssignedName(assignment.Lhs, "cmd") {
			commandCount++
			call, ok := value.(*ast.CallExpr)
			if ok && compactSelector(call.Fun, "exec", "CommandContext") &&
				len(call.Args) > 0 && compactIdent(call.Args[0], "ctx") {
				commandCall = assignment.Pos()
			}
		}
		if len(assignment.Lhs) == 1 && compactSelector(assignment.Lhs[0], "cmd", "WaitDelay") {
			waitDelayCount++
			if compactIdent(value, "doltCallWaitDelay") {
				waitDelay = assignment.Pos()
			}
		}
		return true
	})
	if contextCount != 1 || !contextCall.IsValid() || commandCount != 1 ||
		!commandCall.IsValid() || waitDelayCount != 1 || !waitDelay.IsValid() ||
		!outputCall.IsValid() || !(contextCall < commandCall && commandCall < waitDelay && waitDelay < outputCall) {
		t.Fatalf("%s must pass its %s context to one command and set doltCallWaitDelay before collecting output (ctx=%d cmd=%d delay=%d)",
			fn.Name.Name, compactContextKind(deadlineBound), contextCount, commandCount, waitDelayCount)
	}
}

func compactContextKind(deadlineBound bool) string {
	if deadlineBound {
		return "doltCallContext(t)"
	}
	return "retryable context.WithTimeout"
}

func compactAssignedName(lhs []ast.Expr, name string) bool {
	for _, expr := range lhs {
		if compactIdent(expr, name) {
			return true
		}
	}
	return false
}

func compactIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}

func compactSelector(expr ast.Expr, base, field string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == field && compactIdent(selector.X, base)
}

func compactTenSeconds(expr ast.Expr) bool {
	product, ok := expr.(*ast.BinaryExpr)
	if !ok || product.Op != token.MUL {
		return false
	}
	seconds := func(number, unit ast.Expr) bool {
		literal, ok := number.(*ast.BasicLit)
		return ok && literal.Kind == token.INT && literal.Value == "10" &&
			compactSelector(unit, "time", "Second")
	}
	return seconds(product.X, product.Y) || seconds(product.Y, product.X)
}
