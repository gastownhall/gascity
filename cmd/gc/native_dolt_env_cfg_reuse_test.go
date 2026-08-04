package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// openStoreResultAtForCityWithAuthority loads the city config, then opens the
// store. Opening the native store with a nil config made the rig-scoped
// projection load that same city config again — pack expansion and builtin
// cache readiness included — for one store open, on a path `gc bd` reaches
// while it is only resolving which store a bead ID belongs to.
//
// The reopen hook in the same closure is deliberately excluded: it fires long
// after the open, on a reconnect where re-reading current config is the point.
func TestOpenNativeStoreReusesTheLoadedCityConfig(t *testing.T) {
	const (
		enclosing = "openStoreResultAtForCityWithConfig"
		field     = "OpenNativeStore"
		callee    = "nativeDoltOpenEnvForScope"
		wantArg   = "cfg"
	)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}

	fn := findFuncDecl(file, enclosing)
	if fn == nil {
		t.Fatalf("%s not found in main.go", enclosing)
	}
	value := compositeLitFieldValue(fn, field)
	if value == nil {
		t.Fatalf("%s field not found in %s", field, enclosing)
	}

	var checked int
	ast.Inspect(value, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != callee {
			return true
		}
		checked++
		if len(call.Args) != 3 {
			t.Fatalf("%s: got %d args, want 3", callee, len(call.Args))
		}
		arg, ok := call.Args[1].(*ast.Ident)
		if !ok || arg.Name != wantArg {
			t.Fatalf("%s in %s.%s passes %s as its config; want the already-loaded %q",
				callee, enclosing, field, exprText(call.Args[1]), wantArg)
		}
		return true
	})
	if checked != 1 {
		t.Fatalf("found %d %s call(s) in %s.%s, want exactly 1", checked, callee, enclosing, field)
	}
}

func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// compositeLitFieldValue returns the value assigned to the named key in any
// composite literal inside fn.
func compositeLitFieldValue(fn *ast.FuncDecl, key string) ast.Node {
	var found ast.Node
	ast.Inspect(fn, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == key {
			found = kv.Value
			return false
		}
		return true
	})
	return found
}

func exprText(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return "a non-identifier expression"
}

// bd scope resolution probes candidate stores only to decide which store an
// invocation is scoped to. Each probe opened a store, and the open re-loaded
// the whole city config that bd scope resolution had already loaded — so on a
// mutating invocation that reaches multiple candidate probes, the city config
// was paid for once per probe on top of the load doBd had already done.
func TestBdBeadExistsProbeReusesTheLoadedCityConfig(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cmd_bd.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing cmd_bd.go: %v", err)
	}

	var probe ast.Node
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != "bdBeadExists" || len(value.Values) != 1 {
				continue
			}
			probe = value.Values[0]
		}
	}
	if probe == nil {
		t.Fatal("bdBeadExists not found in cmd_bd.go")
	}

	var opens int
	ast.Inspect(probe, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch ident.Name {
		case "openStoreAtForCity":
			t.Fatal("bdBeadExists opens the store without a config, which re-loads the city config it was handed")
		case "openStoreAtForCityWithConfig":
			opens++
			if len(call.Args) != 3 {
				t.Fatalf("openStoreAtForCityWithConfig: got %d args, want 3", len(call.Args))
			}
			arg, ok := call.Args[2].(*ast.Ident)
			if !ok || arg.Name != "cfg" {
				t.Fatalf("bdBeadExists passes %s as its config; want the already-loaded \"cfg\"", exprText(call.Args[2]))
			}
		}
		return true
	})
	if opens != 1 {
		t.Fatalf("found %d config-carrying store open(s) in bdBeadExists, want exactly 1", opens)
	}
}

// The work-record close gate opens a store after doBd has already loaded the
// city config. Opening it without that config re-ran the builtin readiness
// pass — the expensive half of a config load — so `gc bd close` paid for it
// twice.
func TestWorkRecordCloseGateReusesTheLoadedCityConfig(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "work_record_gate.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing work_record_gate.go: %v", err)
	}

	fn := findFuncDecl(file, "runWorkRecordCloseGate")
	if fn == nil {
		t.Fatal("runWorkRecordCloseGate not found in work_record_gate.go")
	}

	var opens int
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch ident.Name {
		case "openStoreAtForCity":
			t.Fatal("runWorkRecordCloseGate opens the store without a config, which re-runs the builtin readiness pass doBd already ran")
		case "openStoreAtForCityWithConfig":
			opens++
			if len(call.Args) != 3 {
				t.Fatalf("openStoreAtForCityWithConfig: got %d args, want 3", len(call.Args))
			}
			arg, ok := call.Args[2].(*ast.Ident)
			if !ok || arg.Name != "cfg" {
				t.Fatalf("runWorkRecordCloseGate passes %s as its config; want the already-loaded \"cfg\"", exprText(call.Args[2]))
			}
		}
		return true
	})
	if opens != 1 {
		t.Fatalf("found %d config-carrying store open(s) in runWorkRecordCloseGate, want exactly 1", opens)
	}
}

// The write-ID collision guard reads every bead a mutating `gc bd` invocation
// targets, and the work-record close gate then reads that same set again for
// the same IDs — so `gc bd close` opened the store twice and paid for the same
// store.Get twice. runWorkRecordCloseGate accepts the guard's store and beads
// to skip the second round trip, but accepting them only dedupes if doBd
// actually hands them over: a refactor that drops the arguments back to nil
// would restore the double read with the whole suite still green.
func TestBdCloseGateReusesTheWriteGuardsStoreRead(t *testing.T) {
	const (
		callee        = "runWorkRecordCloseGate"
		wantStoreArg  = "guardStore"
		wantBeadsArg  = "guardBeads"
		storeArgIndex = 4
		beadsArgIndex = 5
	)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cmd_bd.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing cmd_bd.go: %v", err)
	}

	fn := findFuncDecl(file, "doBd")
	if fn == nil {
		t.Fatal("doBd not found in cmd_bd.go")
	}

	var calls int
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != callee {
			return true
		}
		calls++
		if len(call.Args) != 7 {
			t.Fatalf("%s: got %d args, want 7", callee, len(call.Args))
		}
		for _, want := range []struct {
			index int
			name  string
		}{
			{storeArgIndex, wantStoreArg},
			{beadsArgIndex, wantBeadsArg},
		} {
			arg, ok := call.Args[want.index].(*ast.Ident)
			if !ok || arg.Name != want.name {
				t.Fatalf("doBd passes %s as %s arg %d; want the write-ID guard's %q, so the gate reuses the read it already paid for",
					exprText(call.Args[want.index]), callee, want.index, want.name)
			}
		}
		return true
	})
	if calls != 1 {
		t.Fatalf("found %d %s call(s) in doBd, want exactly 1", calls, callee)
	}
}
