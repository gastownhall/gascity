package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestRootStoreRefIsNeverMappedToAStoreWithoutAResidenceProbe is the design
// §2.1 pin: gc.root_store_ref is a run's SCOPE, never its location. On a split
// city every graph bead lives in the binding whatever its scope, so a reader
// that turns the key into a store (workflowStoreByRef) must first probe the
// graph binding (workflowGraphStoreInfo) in the same function. A new reader
// that maps the key straight to a store fails here.
func TestRootStoreRefIsNeverMappedToAStoreWithoutAResidenceProbe(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			probeAt := token.NoPos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch callName(call) {
				case "workflowGraphStoreInfo":
					if probeAt == token.NoPos {
						probeAt = call.Pos()
					}
				case "workflowStoreByRef":
					if len(call.Args) < 2 || !mentions(call.Args[1], "RootStoreRefMetadataKey") {
						return true
					}
					checked++
					if probeAt == token.NoPos || probeAt > call.Pos() {
						t.Errorf("%s: %s maps gc.root_store_ref to a store without first probing the graph binding (workflowGraphStoreInfo); the key is scope, never location",
							fset.Position(call.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("found no reader mapping gc.root_store_ref to a store; the guard is checking nothing (was workflowStoreByRef renamed?)")
	}
}

func callName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func mentions(expr ast.Expr, ident string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == ident {
			found = true
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == ident {
			found = true
		}
		return !found
	})
	return found
}
