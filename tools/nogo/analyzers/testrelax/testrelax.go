// Package testrelax bans package-global test relaxations (ARCH-RESTRUCTURE-2
// R11b, V-PROD). A relaxation weakens a production guard so a test can pass:
// a call of a guard setter, a test's write of a guarded variable, or a
// break-glass environment variable set. No init function, TestMain, or
// package-level variable initializer may reach one: the relaxation would hold
// for every test in the package, the ones that never asked for it included,
// and hide a production path that lost what the guard checks (NEW2-7 hid
// L1b-3 M1 this way). A test that needs one makes it itself, scoped to t: the
// guard setters take a testing.TB and restore on t.Cleanup, and a test file
// writes a guarded variable only through its setter.
//
// Every relaxation is named by type: a setter by its types.Func full name, a
// variable by its package path and name, an environment variable by the
// constant value of os.Setenv's key. Reaching one counts through any chain of
// package-level functions, in this package or an imported one (an object fact
// carries it across packages), by a call or a function value alike.
package testrelax

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Config names the reviewed guards (config.go), each with what it relaxes.
type Config struct {
	Guards map[string]string // guard setters, by types.Func full name ("example.com/p.F", "(*example.com/p.T).M")
	Vars   map[string]string // guarded package variables, "pkgpath.name"; their setters are Guards
	Env    map[string]string // break-glass environment variables, by name
	// Allowed are the package-global relaxations a package keeps, reviewed:
	// "pkgpath:guard" to the reason.
	Allowed map[string]string
}

// relaxes marks a function that reaches a relaxation.
type relaxes struct{ Guard string }

func (*relaxes) AFact() {}

func (r *relaxes) String() string { return "relaxes " + r.Guard }

// New builds the analyzer for cfg.
func New(cfg Config) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name:      "testrelax",
		Doc:       "reports package-global test relaxations of production guards (R11b)",
		Run:       func(pass *analysis.Pass) (any, error) { return nil, run(pass, cfg) },
		FactTypes: []analysis.Fact{new(relaxes)},
	}
}

// envSetters are the functions that set a process environment variable.
var envSetters = []string{"os.Setenv", "syscall.Setenv"}

func run(pass *analysis.Pass, cfg Config) error {
	isTest := func(n ast.Node) bool {
		return strings.HasSuffix(pass.Fset.Position(n.Pos()).Filename, "_test.go")
	}
	allowed := func(guard string) bool {
		_, ok := cfg.Allowed[pass.Pkg.Path()+":"+guard]
		return ok
	}
	// guardedVar names the guarded package variable e writes, or "".
	guardedVar := func(e ast.Expr) (*ast.Ident, string) {
		var id *ast.Ident
		switch x := ast.Unparen(e).(type) {
		case *ast.Ident:
			id = x
		case *ast.SelectorExpr:
			id = x.Sel
		default:
			return nil, ""
		}
		v, ok := pass.TypesInfo.ObjectOf(id).(*types.Var)
		if !ok || v.Pkg() == nil || v.Parent() != v.Pkg().Scope() {
			return nil, ""
		}
		key := v.Pkg().Path() + "." + v.Name()
		if _, ok := cfg.Vars[key]; ok {
			return id, key
		}
		return nil, ""
	}
	bodies := map[*types.Func]*ast.BlockStmt{}
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				if fn, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func); ok {
					bodies[fn] = fd.Body
				}
			}
		}
	}
	reached := map[*types.Func]string{}
	guardOf := func(fn *types.Func) string {
		if _, ok := cfg.Guards[fn.FullName()]; ok {
			return fn.FullName()
		}
		if g, ok := reached[fn]; ok {
			return g
		}
		var fact relaxes
		if fn.Pkg() != nil && fn.Pkg() != pass.Pkg && pass.ImportObjectFact(fn, &fact) {
			return fact.Guard
		}
		return ""
	}
	// via finds the first relaxation n reaches: where, the guard, and the
	// function it goes through, if not the guard itself.
	via := func(n ast.Node) (at ast.Node, guard, by string) {
		ast.Inspect(n, func(n ast.Node) bool {
			if guard != "" {
				return false
			}
			switch x := n.(type) {
			case *ast.Ident:
				if fn, ok := pass.TypesInfo.Uses[x].(*types.Func); ok {
					if g := guardOf(fn.Origin()); g != "" {
						at, guard = x, g
						if fn.FullName() != g {
							by = fn.Name()
						}
					}
				}
			case *ast.AssignStmt:
				if isTest(x) && x.Tok != token.DEFINE {
					for _, lhs := range x.Lhs {
						if id, key := guardedVar(lhs); key != "" {
							at, guard = id, key
							return false
						}
					}
				}
			case *ast.IncDecStmt:
				if isTest(x) {
					if id, key := guardedVar(x.X); key != "" {
						at, guard = id, key
						return false
					}
				}
			case *ast.CallExpr:
				if name, ok := envSet(pass, x); ok {
					if _, listed := cfg.Env[name]; listed {
						at, guard = x, "$"+name
						return false
					}
				}
			}
			return true
		})
		return at, guard, by
	}
	fns := make([]*types.Func, 0, len(bodies))
	for fn := range bodies {
		fns = append(fns, fn)
	}
	sort.Slice(fns, func(i, j int) bool { return fns[i].Pos() < fns[j].Pos() })
	for changed := true; changed; {
		changed = false
		for _, fn := range fns {
			if _, done := reached[fn]; done {
				continue
			}
			if _, g, _ := via(bodies[fn]); g != "" {
				reached[fn] = g
				changed = true
			}
		}
	}
	for _, fn := range fns {
		if g, ok := reached[fn]; ok && fn.Name() != "init" {
			pass.ExportObjectFact(fn, &relaxes{Guard: g})
		}
	}
	used := map[string]bool{}
	what := func(guard string) string {
		if name, ok := strings.CutPrefix(guard, "$"); ok {
			return cfg.Env[name]
		}
		if w, ok := cfg.Vars[guard]; ok {
			return w
		}
		return cfg.Guards[guard]
	}
	report := func(n ast.Node, where string) {
		at, g, by := via(n)
		if g == "" {
			return
		}
		if allowed(g) {
			used[g] = true
			return
		}
		how := ""
		if by != "" {
			how = " (via " + by + ")"
		}
		pass.Reportf(at.Pos(), "testrelax: %s relaxes %s for every test in the package%s: it %s; relax it in the test that needs it, scoped to t", where, strings.TrimPrefix(g, "$"), how, what(g))
	}
	// global marks the nodes report covers, so a write there is reported once.
	global := map[ast.Node]bool{}
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				switch {
				case d.Body == nil || d.Recv != nil:
				case d.Name.Name == "init":
					report(d.Body, "init")
					global[d.Body] = true
				case d.Name.Name == "TestMain" && isTest(d):
					report(d.Body, "TestMain")
					global[d.Body] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, v := range vs.Values {
							report(v, "a package-level initializer")
							global[v] = true
						}
					}
				}
			}
		}
	}
	// A test writes a guarded variable only through its t-scoped setter.
	for _, file := range pass.Files {
		if !isTest(file) {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if global[n] {
				return false
			}
			var lhs []ast.Expr
			switch x := n.(type) {
			case *ast.AssignStmt:
				if x.Tok != token.DEFINE {
					lhs = x.Lhs
				}
			case *ast.IncDecStmt:
				lhs = []ast.Expr{x.X}
			}
			for _, e := range lhs {
				if id, key := guardedVar(e); key != "" {
					if allowed(key) {
						used[key] = true
						continue
					}
					pass.Reportf(id.Pos(), "testrelax: a test writes %s, which %s; write it through its setter, scoped to t", key, cfg.Vars[key])
				}
			}
			return true
		})
	}
	if slices.ContainsFunc(pass.Files, func(f *ast.File) bool { return isTest(f) }) {
		var stale []string
		for key := range cfg.Allowed {
			if g, ok := strings.CutPrefix(key, pass.Pkg.Path()+":"); ok && !used[g] {
				stale = append(stale, g)
			}
		}
		sort.Strings(stale)
		for _, g := range stale {
			pass.Reportf(pass.Files[0].Package, "testrelax: allowlisted %s is not relaxed in this package any more: drop it", g)
		}
	}
	return nil
}

// envSet reports the constant key of a call that sets an environment
// variable.
func envSet(pass *analysis.Pass, call *ast.CallExpr) (string, bool) {
	var id *ast.Ident
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return "", false
	}
	fn, ok := pass.TypesInfo.Uses[id].(*types.Func)
	if !ok || !slices.Contains(envSetters, fn.FullName()) || len(call.Args) == 0 {
		return "", false
	}
	tv := pass.TypesInfo.Types[call.Args[0]]
	if tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}
