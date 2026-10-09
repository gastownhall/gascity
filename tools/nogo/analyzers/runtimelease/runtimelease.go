// Package runtimelease is the runtime lease's provider-verb lint
// (ARCH-RESTRUCTURE §3.2, invariant I-LEASE): in the packages that start and
// stop session runtimes, every runtime verb (a provider's Start, Stop or
// Relaunch, a process, object or unattended kill, a server teardown,
// StopForCleanup) runs in a function allowlisted with the lease it runs under
// (and, where that lease is its caller's, only from the listed callers), and
// every worker-boundary starter or stopper is handed a city path, so its
// Manager can take the lease. A verb is found by its method's name and
// signature, so a call through any type or narrow interface, or a method
// value, counts.
package runtimelease

import (
	"go/ast"
	"go/constant"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Allowance is why a function may call a runtime provider's Start, Stop or
// process kill directly. Callers, when set, are the only functions that may
// call it: it runs under their lease.
type Allowance struct {
	Reason  string
	Callers []string
}

// Config scopes the lint and names its allowlist.
type Config struct {
	Packages    []string             // the import paths linted
	RuntimePkg  string               // declares the verbs' interfaces (verbs) and StopForCleanup
	SkipFiles   []string             // file-name prefixes another lint covers
	CityHelpers map[string]int       // a function handed a city path, by its argument index
	SweepCtx    string               // the ctx constructor that marks a stop sweep
	Allowed     map[string]Allowance // by function: "Name" or "Recv.Name", per package path prefix "pkg:"
}

// New builds the analyzer for cfg.
func New(cfg Config) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: "runtimelease",
		Doc:  "reports runtime starts and stops outside the runtime lease (I-LEASE)",
		Run:  func(pass *analysis.Pass) (any, error) { return nil, run(pass, cfg) },
	}
}

func run(pass *analysis.Pass, cfg Config) error {
	if !slices.Contains(cfg.Packages, pass.Pkg.Path()) {
		return nil
	}
	refs, stopForCleanup := runtimeObjects(pass.Pkg, cfg.RuntimePkg)
	allowed := func(fn string) (Allowance, bool) {
		a, ok := cfg.Allowed[pass.Pkg.Path()+":"+fn]
		return a, ok
	}
	used := map[string]bool{}
	var source *ast.File // a linted non-test file: a test-only pass has none
	for _, file := range pass.Files {
		name := pass.Fset.Position(file.Pos()).Filename
		base := name[strings.LastIndex(name, "/")+1:]
		if strings.HasSuffix(base, "_test.go") || slices.ContainsFunc(cfg.SkipFiles, func(p string) bool { return strings.HasPrefix(base, p) }) {
			continue
		}
		if source == nil {
			source = file
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := funcName(fd)
			_, ok = allowed(fn)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if id, isIdent := n.(*ast.Ident); isIdent {
					if verb := runtimeVerb(pass, id, refs, stopForCleanup); verb != "" {
						used[fn] = true
						if !ok {
							pass.Reportf(id.Pos(), "runtime lease: %s calls a provider's %s outside the lease; take it, or allowlist %s with the lease it runs under", fn, verb, fn)
						}
					}
					return true
				}
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				callee := calleeName(pass, call)
				if a, listed := allowed(callee); listed && len(a.Callers) > 0 && !slices.Contains(a.Callers, fn) {
					pass.Reportf(call.Pos(), "runtime lease: %s runs under its caller's lease (%s), and %s is not one of its callers %v", callee, a.Reason, fn, a.Callers)
				}
				if i, helper := cfg.CityHelpers[callee]; helper && i < len(call.Args) && emptyString(pass, fd, call.Args[i]) && !sweep(call, cfg.SweepCtx) {
					pass.Reportf(call.Args[i].Pos(), "runtime lease: %s is handed no city path, so its Manager cannot take the runtime lease", callee)
				}
				return true
			})
		}
	}
	for key := range cfg.Allowed {
		if fn, ok := strings.CutPrefix(key, pass.Pkg.Path()+":"); ok && !used[fn] && source != nil {
			pass.Reportf(source.Package, "runtime lease: allowlisted %s calls no provider Start, Stop or process kill: drop it", fn)
		}
	}
	return nil
}

// verbs are the runtime verbs the lint enforces, each by the runtime
// interface that declares it.
var verbs = map[string]string{
	"Start":                 "Provider",
	"Stop":                  "Provider",
	"Relaunch":              "RelaunchProvider",
	"TerminateRuntime":      "ProcessTableScanner",
	"StopUnattendedSession": "UnattendedSessionStopper",
	"KillCorpseObject":      "SessionObjectKiller",
	"KillZombieObject":      "SessionObjectKiller",
	"TeardownServer":        "ServerLifecycleProvider",
}

// runtimeObjects finds, among pkg's imports or in pkg, each verb's method
// signature in the runtime package's interfaces, and its StopForCleanup.
func runtimeObjects(pkg *types.Package, path string) (refs map[string]*types.Signature, stop types.Object) {
	rt := pkg
	if pkg.Path() != path {
		rt = nil
		for _, imp := range pkg.Imports() {
			if imp.Path() == path {
				rt = imp
			}
		}
	}
	if rt == nil {
		return nil, nil
	}
	refs = map[string]*types.Signature{}
	for verb, ifaceName := range verbs {
		obj := rt.Scope().Lookup(ifaceName)
		if obj == nil {
			continue
		}
		iface, ok := obj.Type().Underlying().(*types.Interface)
		if !ok {
			continue
		}
		for k := 0; k < iface.NumMethods(); k++ {
			m := iface.Method(k)
			if m.Name() == verb {
				refs[verb] = m.Type().(*types.Signature)
			}
		}
	}
	return refs, rt.Scope().Lookup("StopForCleanup")
}

// runtimeVerb names the runtime verb id uses, or "": StopForCleanup, or a
// method named for a verb whose signature is the verb's, on any type.
func runtimeVerb(pass *analysis.Pass, id *ast.Ident, refs map[string]*types.Signature, stopForCleanup types.Object) string {
	obj := pass.TypesInfo.Uses[id]
	if obj == nil {
		return ""
	}
	if stopForCleanup != nil && obj == stopForCleanup {
		return "StopForCleanup"
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return ""
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return ""
	}
	if ref, ok := refs[fn.Name()]; ok && types.Identical(sig, ref) {
		return fn.Name()
	}
	return ""
}

// calleeName is the package-local function a call names, as funcName spells
// it, or "".
func calleeName(pass *analysis.Pass, call *ast.CallExpr) string {
	var id *ast.Ident
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	default:
		return ""
	}
	fn, ok := pass.TypesInfo.Uses[id].(*types.Func)
	if !ok || fn.Pkg() != pass.Pkg {
		return ""
	}
	if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
		t := recv.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		if n, ok := t.(*types.Named); ok {
			return n.Obj().Name() + "." + fn.Name()
		}
	}
	return fn.Name()
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// emptyString reports whether e is the empty string: a constant, or a local
// variable of fd that is only ever assigned the empty string.
func emptyString(pass *analysis.Pass, fd *ast.FuncDecl, e ast.Expr) bool {
	if tv, ok := pass.TypesInfo.Types[e]; ok && tv.Value != nil {
		return tv.Value.Kind() == constant.String && constant.StringVal(tv.Value) == ""
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	v, ok := pass.TypesInfo.Uses[id].(*types.Var)
	if !ok || v.Parent() == nil || v.Parent() == pass.Pkg.Scope() || v.Pos() < fd.Body.Pos() || v.Pos() > fd.Body.End() {
		return false
	}
	empty := true
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		var lhs, rhs []ast.Expr
		switch s := n.(type) {
		case *ast.AssignStmt:
			lhs, rhs = s.Lhs, s.Rhs
		case *ast.ValueSpec:
			for _, name := range s.Names {
				lhs = append(lhs, name)
			}
			rhs = s.Values
		case *ast.UnaryExpr:
			if x, ok := s.X.(*ast.Ident); ok && pass.TypesInfo.Uses[x] == v {
				empty = false // its address escapes
			}
			return true
		default:
			return true
		}
		for i, l := range lhs {
			lid, ok := l.(*ast.Ident)
			if !ok || (pass.TypesInfo.Defs[lid] != v && pass.TypesInfo.Uses[lid] != v) {
				continue
			}
			if len(rhs) != len(lhs) {
				empty = false
				continue
			}
			tv, ok := pass.TypesInfo.Types[rhs[i]]
			if !ok || tv.Value == nil || constant.StringVal(tv.Value) != "" {
				empty = false
			}
		}
		return true
	})
	return empty
}

// sweep reports whether one of call's arguments is a stop sweep's ctx.
func sweep(call *ast.CallExpr, ctor string) bool {
	for _, a := range call.Args {
		c, ok := a.(*ast.CallExpr)
		if !ok {
			continue
		}
		switch f := c.Fun.(type) {
		case *ast.SelectorExpr:
			if f.Sel.Name == ctor {
				return true
			}
		case *ast.Ident:
			if f.Name == ctor {
				return true
			}
		}
	}
	return false
}
