package integration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestBdStoreConformanceSkipIsVersionGated statically guards the class of bug
// fixed in ga-zkmcr3: TestBdStoreConformance opened with an unconditional
// t.Skip citing a bd pin gascity had since moved past, so the conformance
// suite stopped running on the default pin and its SKIP failed every deploy
// gate whose diff touched bdstore_test.go. The suite may skip only when the bd
// under test predates the #3691 empty-DB guard, and it must do so through
// helpers.RequireBDAtLeast, whose skip message names the detected version.
//
// bdstore_test.go is integration-tagged, so the suite itself never compiles in
// the default `go test ./...` lane. This test parses its source instead, as
// TestDoltConfigTimeoutsUseNamedConstants does for dolt_config_test.go.
func TestBdStoreConformanceSkipIsVersionGated(t *testing.T) {
	fset, body := bdStoreConformanceBody(t)

	gated := false
	for _, stmt := range body.List {
		call, ok := topLevelCall(stmt)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		recv, _ := sel.X.(*ast.Ident)
		switch {
		case recv != nil && recv.Name == "t" && (sel.Sel.Name == "Skip" || sel.Sel.Name == "Skipf" || sel.Sel.Name == "SkipNow"):
			t.Errorf("%s: TestBdStoreConformance skips unconditionally; gate it on the bd version with helpers.RequireBDAtLeast so the suite runs on a bd that has the #3691 guard", fset.Position(stmt.Pos()))
		case sel.Sel.Name == "RequireBDAtLeast":
			gated = true
		}
	}
	if !gated {
		t.Error("TestBdStoreConformance never calls helpers.RequireBDAtLeast; a bd older than the #3691 empty-DB guard (1.0.4) would run the suite and trip ErrBDSilentFallback instead of skipping")
	}
}

// TestBdStoreConformanceRunsTheForeignParentRow guards ga-6mfvtl: bd resolves
// --parent unconditionally, so BdStore cannot keep a parent bd does not have.
// Its run declares beadstest.Options.RefusesUnresolvableParent, which makes the
// ParentIDNamesARowThisStoreDoesNotHave subtest execute and assert that
// refusal. Opting out of the row instead (any Options field named Skip...)
// would put a SKIP inside a test whose body this file owns, and a skip there
// fails the deploy gate with no waiver path.
func TestBdStoreConformanceRunsTheForeignParentRow(t *testing.T) {
	fset, body := bdStoreConformanceBody(t)

	var options *ast.CompositeLit
	ast.Inspect(body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Options" {
			options = lit
			return false
		}
		return true
	})
	if options == nil {
		t.Fatal("TestBdStoreConformance builds no beadstest.Options; it must declare RefusesUnresolvableParent: true")
	}

	declared := false
	for _, elt := range options.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		value, _ := kv.Value.(*ast.Ident)
		switch {
		case key.Name == "RefusesUnresolvableParent":
			declared = value != nil && value.Name == "true"
		case strings.HasPrefix(key.Name, "Skip"):
			t.Errorf("%s: TestBdStoreConformance sets %s; the ParentID row must execute, so BdStore declares RefusesUnresolvableParent instead of skipping it", fset.Position(kv.Pos()), key.Name)
		}
	}
	if !declared {
		t.Error("TestBdStoreConformance does not set RefusesUnresolvableParent: true; without it the ParentID row demands the full weak-reference contract, which bd's --parent resolution breaks")
	}
}

// TestBdStoreConformanceDropsDeadStoreDatabases guards the root cause of the
// bd init timeouts the un-skipped suite hit (ga-d4nm46). Every store its
// factory creates is a database on ONE shared Dolt server, and both Dolt and
// bd init cost grows with the number of databases the server holds: measured
// on a quiet host, init took 2.7s at one database and 16.8s at twelve, and the
// suite's later stores crossed the 60s bdInitTimeout. A store's database is
// dead once its subtest ends, so the factory must drop it through
// dropDoltDatabase instead of letting the suite go quadratic.
func TestBdStoreConformanceDropsDeadStoreDatabases(t *testing.T) {
	_, body := bdStoreConformanceBody(t)

	dropped := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "dropDoltDatabase" {
				dropped = true
			}
		}
		return true
	})
	if !dropped {
		t.Error("TestBdStoreConformance never calls dropDoltDatabase; every store's database would stay on the shared Dolt server and each later bd init would cost more than the last")
	}
}

// bdStoreConformanceBody parses bdstore_test.go and returns the body of
// TestBdStoreConformance with the file set that positions it.
func bdStoreConformanceBody(t *testing.T) (*token.FileSet, *ast.BlockStmt) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "bdstore_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing bdstore_test.go: %v", err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "TestBdStoreConformance" {
			return fset, fn.Body
		}
	}
	t.Fatal("TestBdStoreConformance not found in bdstore_test.go — update this test's scoping")
	return nil, nil
}

// topLevelCall returns the call expression of a bare `f(...)` statement.
func topLevelCall(stmt ast.Stmt) (*ast.CallExpr, bool) {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	call, ok := expr.X.(*ast.CallExpr)
	return call, ok
}
