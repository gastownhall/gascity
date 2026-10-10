// Package testrelax bans package-global test relaxations (ARCH-RESTRUCTURE-2
// R11b, V-PROD): no init function, TestMain, or package-level variable
// initializer may reach a production guard setter, a function that weakens a
// production refusal so a test can pass. Such a relaxation would hold for
// every test in the package, the ones that never asked for it included, and
// hide a production path that lost what the guard checks (NEW2-7 hid
// L1b-3 M1 this way). A test that needs one calls the setter itself, and
// restores it when it ends.
//
// A guard setter is named by its types.Func full name in a reviewed list.
// Reaching it counts through any chain of package-level functions, in this
// package or an imported one (an object fact carries it across packages), by
// a call or a function value alike.
package testrelax

import (
	"go/ast"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Config names the guard setters: by types.Func full name
// ("example.com/p.F", "(*example.com/p.T).M"), what each relaxes.
type Config struct {
	Guards map[string]string
}

// relaxes marks a function that reaches a guard setter.
type relaxes struct{ Guard string }

func (*relaxes) AFact() {}

func (r *relaxes) String() string { return "relaxes " + r.Guard }

// New builds the analyzer for cfg.
func New(cfg Config) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name:      "testrelax",
		Doc:       "reports init functions, TestMain and package-level initializers that relax a production guard (R11b)",
		Run:       func(pass *analysis.Pass) (any, error) { return nil, run(pass, cfg) },
		FactTypes: []analysis.Fact{new(relaxes)},
	}
}

func run(pass *analysis.Pass, cfg Config) error {
	// The package's functions, by object, with their bodies.
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
	// guardOf names the guard fn reaches, or "": a listed setter, an imported
	// function's fact, or this package's function found so far.
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
	// via is the first reference in n to a function that reaches a guard, the
	// function, and the guard.
	via := func(n ast.Node) (*ast.Ident, *types.Func, string) {
		var at *ast.Ident
		var by *types.Func
		var guard string
		ast.Inspect(n, func(n ast.Node) bool {
			if guard != "" {
				return false
			}
			if id, ok := n.(*ast.Ident); ok {
				if fn, ok := pass.TypesInfo.Uses[id].(*types.Func); ok {
					if g := guardOf(fn.Origin()); g != "" {
						at, by, guard = id, fn, g
					}
				}
			}
			return true
		})
		return at, by, guard
	}
	// Close the package's functions over their references, to a fixed point.
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
			if _, _, g := via(bodies[fn]); g != "" {
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
	report := func(n ast.Node, where string) {
		at, by, g := via(n)
		if g == "" {
			return
		}
		how := ""
		if by.FullName() != g {
			how = " (via " + by.Name() + ")"
		}
		pass.Reportf(at.Pos(), "testrelax: %s relaxes %s for every test in the package%s: it %s; call it from the test that needs it, and restore it when the test ends", where, g, how, cfg.Guards[g])
	}
	for _, file := range pass.Files {
		test := strings.HasSuffix(pass.Fset.Position(file.Pos()).Filename, "_test.go")
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				switch {
				case d.Body == nil || d.Recv != nil:
				case d.Name.Name == "init":
					report(d.Body, "init")
				case d.Name.Name == "TestMain" && test:
					report(d.Body, "TestMain")
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, v := range vs.Values {
							report(v, "a package-level initializer")
						}
					}
				}
			}
		}
	}
	return nil
}
