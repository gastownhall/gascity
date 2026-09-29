package integration

import (
	"go/ast"
	"go/parser"
	"go/token"
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
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "bdstore_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing bdstore_test.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "TestBdStoreConformance" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("TestBdStoreConformance not found in bdstore_test.go — update this test's scoping")
	}

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

// topLevelCall returns the call expression of a bare `f(...)` statement.
func topLevelCall(stmt ast.Stmt) (*ast.CallExpr, bool) {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	call, ok := expr.X.(*ast.CallExpr)
	return call, ok
}
