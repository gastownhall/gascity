// Package testrelax bans package-global test relaxations (ARCH-RESTRUCTURE-2
// R11b, V-PROD). A relaxation weakens a production guard so a test can pass:
// a call of a guard setter, a write of a guarded variable, or a break-glass
// environment variable set. No init function, TestMain, or package-level
// variable initializer may reach one: the relaxation would hold for every test
// in the package, the ones that never asked for it included, and hide a
// production path that lost what the guard checks (NEW2-7 hid L1b-3 M1 this
// way). A test that needs one makes it itself, scoped to t: the guard setters
// take a testing.TB and restore on t.Cleanup, a guarded variable is written
// only by its setter or a reviewed production writer, and a test file sets a
// break-glass variable only with t.Setenv.
//
// Every relaxation is named by type: a setter by its types.Func full name, a
// variable by its package path and name, an environment variable by the
// constant value of os.Setenv's key. A write is an assignment, an increment,
// an address taken (&v), or a pointer-receiver method of v (v.Store), to v or
// to anything under it (v.f, v[i]). Reaching one counts through any chain of
// package-level functions, in this package or an imported one (an object fact
// carries it across packages), by a call or a function value alike.
package testrelax

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Config names the reviewed guards (config.go), each with what it relaxes.
type Config struct {
	Guards map[string]string // guard setters, by types.Func full name ("example.com/p.F", "(*example.com/p.T).M")
	Vars   map[string]string // guarded package variables, "pkgpath.name"; written only by Guards and Writers
	// Writers are the reviewed production writers of a guarded variable, by
	// types.Func full name, each with why its write relaxes nothing.
	Writers map[string]string
	Env     map[string]string // break-glass environment variables, by name
	// Allowed are the package-global relaxations kept, each reviewed with
	// its reason.
	Allowed map[Allowance]string
}

// Allowance is one package-global relaxation kept: Guard (a setter's or a
// variable's name, or an environment variable's) reached at Site in the
// package whose path is Pkg. Site is an init's file ("x_test.go:init", the
// file's every init), "TestMain", or a package-level initializer
// ("var name").
type Allowance struct{ Pkg, Site, Guard string }

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
	// guardedVar names the guarded package variable e is, or is under.
	guardedVar := func(e ast.Expr) (*ast.Ident, string) {
		var id *ast.Ident
		for id == nil {
			switch x := ast.Unparen(e).(type) {
			case *ast.Ident:
				id = x
			case *ast.SelectorExpr:
				if sel, ok := pass.TypesInfo.Selections[x]; ok {
					if sel.Kind() != types.FieldVal {
						return nil, ""
					}
					e = x.X
					continue
				}
				id = x.Sel // a qualified identifier
			case *ast.IndexExpr:
				e = x.X
			case *ast.StarExpr:
				e = x.X
			default:
				return nil, ""
			}
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
	// written names the guarded variable n writes, or "".
	written := func(n ast.Node) (*ast.Ident, string) {
		var targets []ast.Expr
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x.Tok != token.DEFINE {
				targets = x.Lhs
			}
		case *ast.RangeStmt:
			if x.Tok == token.ASSIGN {
				targets = []ast.Expr{x.Key, x.Value}
			}
		case *ast.IncDecStmt:
			targets = []ast.Expr{x.X}
		case *ast.UnaryExpr:
			if x.Op == token.AND {
				targets = []ast.Expr{x.X}
			}
		case *ast.SelectorExpr: // v.M with a pointer receiver takes &v
			sel, ok := pass.TypesInfo.Selections[x]
			if !ok || sel.Kind() != types.MethodVal {
				break
			}
			_, ptrRecv := sel.Obj().Type().(*types.Signature).Recv().Type().(*types.Pointer)
			_, ptrX := sel.Recv().Underlying().(*types.Pointer)
			if ptrRecv && !ptrX {
				targets = []ast.Expr{x.X}
			}
		}
		for _, e := range targets {
			if e == nil {
				continue
			}
			if id, key := guardedVar(e); key != "" {
				return id, key
			}
		}
		return nil, ""
	}
	// writer reports whether fn may write a guarded variable: a guard setter
	// or a reviewed production writer.
	writer := func(fn *types.Func) bool {
		if fn == nil {
			return false
		}
		_, setter := cfg.Guards[fn.FullName()]
		_, listed := cfg.Writers[fn.FullName()]
		return setter || listed
	}
	type body struct {
		fn   *types.Func
		decl *ast.FuncDecl
	}
	var bodies []body
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				if fn, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func); ok {
					bodies = append(bodies, body{fn, fd})
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
	// via finds the first relaxation n, in owner's body (nil: an
	// initializer), reaches: where, the guard, and the function it goes
	// through, if not the guard itself.
	via := func(n ast.Node, owner *types.Func) (at ast.Node, guard, by string) {
		ast.Inspect(n, func(n ast.Node) bool {
			if guard != "" {
				return false
			}
			if id, key := written(n); key != "" && !writer(owner) {
				at, guard = id, key
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
	for changed := true; changed; {
		changed = false
		for _, b := range bodies {
			if _, done := reached[b.fn]; done {
				continue
			}
			if _, g, _ := via(b.decl.Body, b.fn); g != "" {
				reached[b.fn] = g
				changed = true
			}
		}
	}
	for _, b := range bodies {
		if g, ok := reached[b.fn]; ok && b.fn.Name() != "init" {
			pass.ExportObjectFact(b.fn, &relaxes{Guard: g})
		}
	}
	used := map[Allowance]bool{}
	what := func(guard string) string {
		if name, ok := strings.CutPrefix(guard, "$"); ok {
			return cfg.Env[name]
		}
		if w, ok := cfg.Vars[guard]; ok {
			return w
		}
		return cfg.Guards[guard]
	}
	report := func(n ast.Node, owner *types.Func, where, site string) {
		at, g, by := via(n, owner)
		if g == "" {
			return
		}
		if a := (Allowance{pass.Pkg.Path(), site, strings.TrimPrefix(g, "$")}); cfg.Allowed[a] != "" {
			used[a] = true
			return
		}
		how := ""
		if by != "" {
			how = " (via " + by + ")"
		}
		pass.Reportf(at.Pos(), "testrelax: %s relaxes %s for every test in the package%s: it %s; relax it in the test that needs it, scoped to t", where, strings.TrimPrefix(g, "$"), how, what(g))
	}
	// global marks the nodes report covers, so a relaxation there is
	// reported once.
	global := map[ast.Node]bool{}
	for _, b := range bodies {
		switch d := b.decl; {
		case d.Recv != nil:
		case d.Name.Name == "init":
			report(d.Body, b.fn, "init", filepath.Base(pass.Fset.Position(d.Pos()).Filename)+":init")
			global[d] = true
		case d.Name.Name == "TestMain" && isTest(d):
			report(d.Body, b.fn, "TestMain", "TestMain")
			global[d] = true
		}
	}
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			if d, ok := decl.(*ast.GenDecl); ok {
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, v := range vs.Values {
							site := "var " + vs.Names[min(i, len(vs.Names)-1)].Name
							report(v, nil, "a package-level initializer", site)
						}
					}
				}
			}
		}
	}
	// Anywhere else, a guarded variable is written only by its setter or a
	// reviewed writer, and a test file sets a break-glass variable only with
	// t.Setenv.
	for _, b := range bodies {
		if global[b.decl] {
			continue
		}
		test := isTest(b.decl)
		ast.Inspect(b.decl.Body, func(n ast.Node) bool {
			if id, key := written(n); key != "" && !writer(b.fn) {
				pass.Reportf(id.Pos(), "testrelax: %s writes %s, which %s; a test writes it through its setter, scoped to t, and production code only in a writer testrelax lists", b.fn.Name(), key, cfg.Vars[key])
				return false
			}
			if call, ok := n.(*ast.CallExpr); ok && test {
				if name, ok := envSet(pass, call); ok {
					if w, listed := cfg.Env[name]; listed {
						pass.Reportf(call.Pos(), "testrelax: %s sets %s, which %s, past the test that set it; set it with t.Setenv, scoped to t", b.fn.Name(), name, w)
					}
				}
			}
			return true
		})
	}
	// A test file that takes an environment setter as a value sets keys no
	// rule above can read.
	for _, file := range pass.Files {
		if !isTest(file) {
			continue
		}
		called := map[*ast.Ident]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id := funcIdent(call.Fun); id != nil {
					called[id] = true
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && !called[id] {
				if fn, ok := pass.TypesInfo.Uses[id].(*types.Func); ok && slices.Contains(envSetters, fn.FullName()) {
					pass.Reportf(id.Pos(), "testrelax: a test takes %s as a value, so the keys it sets go unchecked; call it with a constant key, or use t.Setenv", fn.FullName())
				}
			}
			return true
		})
	}
	if slices.ContainsFunc(pass.Files, func(f *ast.File) bool { return isTest(f) }) {
		var stale []string
		for a := range cfg.Allowed {
			if a.Pkg == pass.Pkg.Path() && !used[a] {
				stale = append(stale, a.Site+" "+a.Guard)
			}
		}
		sort.Strings(stale)
		for _, s := range stale {
			pass.Reportf(pass.Files[0].Package, "testrelax: allowlisted %s is not relaxed there any more: drop it", s)
		}
	}
	return nil
}

// funcIdent is the identifier naming a called function, or nil.
func funcIdent(fun ast.Expr) *ast.Ident {
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	}
	return nil
}

// envSet reports the constant key of a call that sets an environment
// variable.
func envSet(pass *analysis.Pass, call *ast.CallExpr) (string, bool) {
	id := funcIdent(call.Fun)
	if id == nil {
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
