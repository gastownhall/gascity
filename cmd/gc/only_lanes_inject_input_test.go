package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

// laneInjectMethods are the calls that type input into a runtime.
var laneInjectMethods = map[string]bool{"Nudge": true, "NudgeNow": true, "SendKeys": true}

// laneInjectAllowlist names every non-test cmd/gc site, as <file>:<func> (or
// <file>:var <name> for a package-level declaration), that may type input
// into a runtime, and why (DES-O1 §9). The lane executors are permanent;
// every other entry is a legacy path that OJ-21 deletes or moves behind the
// lane, emptying the list but for them.
var laneInjectAllowlist = map[string]string{
	// The conversation lane's live executor.
	"conversation_lane_exec.go:conversationLaneExec.Deliver": "lane executor",
	// A provider wrapper forwards its base's calls; it produces nothing.
	"status_provider.go:statusProvider.Nudge":    "provider wrapper",
	"status_provider.go:statusProvider.SendKeys": "provider wrapper",
	// Legacy CLI paths.
	"cmd_nudge.go:deliverSessionNudgeLegacy":      "legacy CLI: gc session nudge",
	"cmd_nudge.go:deliverMailNotifyLegacy":        "legacy CLI: mail --notify",
	"cmd_nudge.go:tryDeliverQueuedNudgesByPoller": "legacy poller",
	"cmd_sling.go:deliverSlingNudge":              "legacy CLI: sling --nudge",
	// Planner notices with [ops] authoritative empty, without a lane host
	// for the sessions store, or for a session no lane can type into.
	"nudge_backstop.go:runNudgeBackstop":   "legacy planner notice",
	"warm_bind_nudge.go:nudgeWarmBindSlot": "legacy planner notice",
	"drain_reminder.go:nudgeDrainReminder": "legacy planner notice",
	// The work ladder's one delivery seam; W5 swaps it for a planner notice.
	"work_ladder_lane.go:ladderDeliver": "legacy planner notice (work ladder)",
}

// laneInjectSite is one place a scanned file types input: its site name and
// position, and what it does.
type laneInjectSite struct {
	site string
	pos  token.Position
	what string
}

// TestOnlyLanesInjectInput fails when a non-test cmd/gc site outside
// laneInjectAllowlist types input into a runtime (laneInjectSites), and when
// an allowlisted site no longer does, so the list only shrinks.
func TestOnlyLanesInjectInput(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := gcCallerDir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, s := range laneInjectSites(fset, name, file) {
			found[s.site] = true
			if _, allowed := laneInjectAllowlist[s.site]; !allowed {
				t.Errorf("%s %s outside the conversation lane: propose a message op instead (DES-O1 §9), or allowlist %s with a reason", s.pos, s.what, s.site)
			}
		}
	}
	var stale []string
	for site := range laneInjectAllowlist {
		if !found[site] {
			stale = append(stale, site)
		}
	}
	sort.Strings(stale)
	for _, site := range stale {
		t.Errorf("allowlisted %s no longer types input; remove it from laneInjectAllowlist", site)
	}
}

// TestLaneInjectSitesSeesEveryShape pins what the lint catches, over the
// shapes a producer could type input through, and what it leaves alone.
func TestLaneInjectSitesSeesEveryShape(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"a function's call", `func f(sp runtime.Provider) error { return sp.Nudge("x", nil) }`, []string{"zz.go:f"}},
		{"a method's call", `func (h *host) f(sp runtime.Provider) { _ = sp.SendKeys("x") }`, []string{"zz.go:host.f"}},
		{"a package-level func literal", `var f = func(sp runtime.Provider) error { return sp.Nudge("x", nil) }`, []string{"zz.go:var f"}},
		{"a method value", `func f(sp runtime.Provider) error { n := sp.Nudge; return n("x", nil) }`, []string{"zz.go:f"}},
		{"a method value of a handle passed on", `func f(h worker.Handle) { run(h.NudgeNow) }`, []string{"zz.go:f"}},
		{"a package-level method value", `var sp runtime.Provider
var n = sp.Nudge`, []string{"zz.go:var n"}},
		{"a method expression", `var n = runtime.Provider.Nudge`, []string{"zz.go:var n"}},
		{"a config field read", `func f(a *config.Agent) string { return strings.TrimSpace(a.Nudge) }`, nil},
		{"a field write", `func f(cfg *runtime.Config, s string) { cfg.Nudge = s }`, nil},
		{"a composite literal's field", `var a = config.Agent{Nudge: "x"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "zz.go", "package main\n"+tc.src+"\n", parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing: %v", err)
			}
			var got []string
			for _, s := range laneInjectSites(fset, "zz.go", file) {
				if !slices.Contains(got, s.site) {
					got = append(got, s.site)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("sites = %v, want %v", got, tc.want)
			}
		})
	}
}

// laneInjectSites are the places in file (named name) that type input into
// a runtime, across every declaration: function and method bodies, and
// package-level var and const values (a func literal, a method value). A
// site types input when it calls .Nudge(, .NudgeNow( or .SendKeys( on
// anything, or refers to one of those methods without calling it (a method
// value or method expression, which can be called later) on an operand the
// declaration types as a runtime.Provider or worker.Handle (laneInjectTyped):
// the scan has no type checker, so an uncalled .Nudge on an operand of
// another type reads as a field, as config.Agent.Nudge is.
//
// Without a type checker the scan cannot type every operand, so a method
// value taken without a call is missed when its operand is a field chain
// (e.deps.sp.Nudge) or a variable declared from a call's result
// (sp := h.SessionProvider(); n := sp.Nudge). Calling such a value later
// (n(...)) names no injecting method either. Review catches those; a
// go/types or nogo analyzer would close the gap.
func laneInjectSites(fset *token.FileSet, name string, file *ast.File) []laneInjectSite {
	var out []laneInjectSite
	for _, decl := range file.Decls {
		var site string
		var body ast.Node
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			site, body = name+":"+laneInjectFuncName(d), d
		case *ast.GenDecl:
			if d.Tok != token.VAR && d.Tok != token.CONST {
				continue
			}
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) == 0 {
					continue
				}
				names := make([]string, len(vs.Names))
				for i, n := range vs.Names {
					names[i] = n.Name
				}
				out = append(out, laneInjectScan(fset, name+":var "+strings.Join(names, ","), vs, laneInjectTypedPkg(file))...)
			}
			continue
		default:
			continue
		}
		typed := laneInjectTypedPkg(file)
		for k := range laneInjectTypedIn(body) {
			typed[k] = true
		}
		out = append(out, laneInjectScan(fset, site, body, typed)...)
	}
	return out
}

// laneInjectScan is laneInjectSites over one declaration's node, its sites
// named site, with typed the identifiers it types as a provider or handle.
func laneInjectScan(fset *token.FileSet, site string, node ast.Node, typed map[string]bool) []laneInjectSite {
	var out []laneInjectSite
	called := map[*ast.SelectorExpr]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && laneInjectMethods[sel.Sel.Name] {
				called[sel] = true
				out = append(out, laneInjectSite{site: site, pos: fset.Position(x.Pos()), what: "calls ." + sel.Sel.Name + "("})
			}
		case *ast.SelectorExpr:
			if called[x] || !laneInjectMethods[x.Sel.Name] || !laneInjectTyped(x.X, typed) {
				return true
			}
			out = append(out, laneInjectSite{site: site, pos: fset.Position(x.Pos()), what: "takes ." + x.Sel.Name + " as a value"})
		}
		return true
	})
	return out
}

// laneInjectTyped reports whether operand is, by its declaration, a
// runtime.Provider or worker.Handle (or a type of either package whose name
// ends in Provider or Handle): an identifier typed so in typed, or the type
// itself, as in a method expression.
func laneInjectTyped(operand ast.Expr, typed map[string]bool) bool {
	switch x := operand.(type) {
	case *ast.Ident:
		return typed[x.Name]
	case *ast.ParenExpr:
		return laneInjectTyped(x.X, typed)
	case *ast.StarExpr:
		return laneInjectTyped(x.X, typed)
	case *ast.SelectorExpr:
		return laneInjectTypeExpr(x)
	}
	return false
}

// laneInjectTypeExpr reports whether typ names a provider or handle type of
// the runtime or worker package.
func laneInjectTypeExpr(typ ast.Expr) bool {
	switch x := typ.(type) {
	case *ast.StarExpr:
		return laneInjectTypeExpr(x.X)
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		return ok && (pkg.Name == "runtime" || pkg.Name == "worker") &&
			(strings.HasSuffix(x.Sel.Name, "Provider") || strings.HasSuffix(x.Sel.Name, "Handle"))
	}
	return false
}

// laneInjectTypedIn are the identifiers node declares with a provider or
// handle type: receivers, parameters, results and var declarations,
// including those of nested func literals.
func laneInjectTypedIn(node ast.Node) map[string]bool {
	typed := map[string]bool{}
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			if laneInjectTypeExpr(f.Type) {
				for _, n := range f.Names {
					typed[n.Name] = true
				}
			}
		}
	}
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			addFields(x.Recv)
		case *ast.FuncType:
			addFields(x.Params)
			addFields(x.Results)
		case *ast.ValueSpec:
			if x.Type != nil && laneInjectTypeExpr(x.Type) {
				for _, n := range x.Names {
					typed[n.Name] = true
				}
			}
		}
		return true
	})
	return typed
}

// laneInjectTypedPkg are the package-level identifiers file declares with a
// provider or handle type, and those its package-level func literals'
// signatures declare.
func laneInjectTypedPkg(file *ast.File) map[string]bool {
	typed := map[string]bool{}
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.VAR {
			for k := range laneInjectTypedIn(d) {
				typed[k] = true
			}
		}
	}
	return typed
}

// laneInjectFuncName is fn's name, Type.method for a method.
func laneInjectFuncName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	for {
		switch t := typ.(type) {
		case *ast.StarExpr:
			typ = t.X
			continue
		case *ast.IndexExpr:
			typ = t.X
			continue
		case *ast.IndexListExpr:
			typ = t.X
			continue
		case *ast.Ident:
			return t.Name + "." + fn.Name.Name
		}
		return fn.Name.Name
	}
}
