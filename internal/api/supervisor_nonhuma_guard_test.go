package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestSupervisorNonHumaSurfacesAreSanctioned pins the set of raw (non-Huma)
// handler registrations on the supervisor mux to the three carved-out surfaces
// documented in engdocs/architecture/api-control-plane.md §3.9: the /svc/*
// workspace-service proxy, the embedded dashboard SPA at "/", and the host-side
// dashboard "/api/" plane. A new humaMux.Handle/HandleFunc registration fails
// this test until it is added here AND documented as a sanctioned exception, so
// an untyped wire surface cannot slip in under internal/api silently —
// TestOpenAPISpecInSync only covers Huma-registered operations and would not
// catch a new non-Huma carve-out.
func TestSupervisorNonHumaSurfacesAreSanctioned(t *testing.T) {
	const src = "supervisor.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	sanctioned := map[string]bool{
		"/v0/city/{cityName}/svc/":  true, // workspace-service pass-through
		"/v0/city/{cityName}/hook/": true, // webhook receiver (E3) — raw body for HMAC/ed25519
		"/":                         true, // embedded dashboard SPA (WithStaticHandler)
		"/api/":                     true, // host-side dashboard plane (WithAPIPlane)
	}

	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isHumaMuxRegister(call.Fun) || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("humaMux registration with a non-literal path at %s; the guard can only vet string-literal mounts", fset.Position(call.Pos()))
			return true
		}
		path, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Errorf("unquote %q: %v", lit.Value, err)
			return true
		}
		found[path] = true
		if !sanctioned[path] {
			t.Errorf("unsanctioned non-Huma registration %q at %s: every raw mount under internal/api must be a §3.9 carve-out. Register it through Huma, or add it to api-control-plane.md §3.9 and this guard.", path, fset.Position(call.Pos()))
		}
		return true
	})

	for path := range sanctioned {
		if !found[path] {
			t.Errorf("expected sanctioned non-Huma registration %q not found in %s; if it moved or was renamed, update this guard and §3.9 together", path, src)
		}
	}
}

// isHumaMuxRegister reports whether fun is a humaMux.Handle / humaMux.HandleFunc
// selector — receiver "humaMux" (local var) or "<x>.humaMux" (field access).
func isHumaMuxRegister(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
		return false
	}
	switch recv := sel.X.(type) {
	case *ast.Ident:
		return recv.Name == "humaMux"
	case *ast.SelectorExpr:
		return recv.Sel.Name == "humaMux"
	}
	return false
}
