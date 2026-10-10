// Package worklegs is the typed work-location lint (ARCH-RESTRUCTURE-2 R9):
// in cmd/gc a seat's work is read through a WorkLegs, and a WorkLegs is minted
// only from the city's work store, never a sessions store (mc-3ixn3.16,
// NEW2-1). It reports, by type rather than by name:
//   - a field set (a literal with fields, an assignment, an increment, an
//     address taken) on a guarded type outside the file that defines it;
//   - a reference to a mint function (a call, a function value, an alias)
//     outside its reviewed sites, and a reviewed site that no longer mints;
//   - a beads.WorkStore built from a beads.SessionStore, by literal or
//     conversion;
//   - a type, other than the two polarity scopes, that implements the scope
//     interface, embedding included.
//
// Test files are not linted: they mint fixtures on purpose.
package worklegs

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Config names what the lint guards in one package.
type Config struct {
	Package      string                       // the linted package's import path
	DefiningFile string                       // the one file that may set guarded fields
	Guarded      []string                     // type names whose fields only DefiningFile sets
	Mints        map[string]map[string]string // mint function → site ("Func" or "Recv.Method") → why its store is the work store
	Scope        string                       // the scope interface
	ScopeImpls   []string                     // the only types that may implement it
	WorkStore    string                       // "importpath.Name" of the work-class handle
	SessionStore string                       // "importpath.Name" of the sessions-class handle
}

// New builds the analyzer for cfg.
func New(cfg Config) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: "worklegs",
		Doc:  "reports a work leg minted outside its reviewed sites or from a sessions store (R9)",
		Run:  func(pass *analysis.Pass) (any, error) { return nil, run(pass, cfg) },
	}
}

func run(pass *analysis.Pass, cfg Config) error {
	if pass.Pkg.Path() != cfg.Package {
		return nil
	}
	scope := pass.Pkg.Scope()
	guarded := map[*types.Var]string{}
	var guardedTypes []types.Type
	for _, name := range cfg.Guarded {
		obj, ok := scope.Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		guardedTypes = append(guardedTypes, obj.Type())
		if st, ok := obj.Type().Underlying().(*types.Struct); ok {
			for i := 0; i < st.NumFields(); i++ {
				guarded[st.Field(i)] = name
			}
		}
	}
	mints := map[types.Object]string{}
	for name := range cfg.Mints {
		if obj := scope.Lookup(name); obj != nil {
			mints[obj] = name
		}
	}
	isGuarded := func(t types.Type) bool {
		t = types.Unalias(t)
		return slices.ContainsFunc(guardedTypes, func(g types.Type) bool { return types.Identical(t, g) })
	}
	named := func(t types.Type, want string) bool {
		if p, ok := types.Unalias(t).(*types.Pointer); ok {
			t = p.Elem()
		}
		n, ok := types.Unalias(t).(*types.Named)
		return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path()+"."+n.Obj().Name() == want
	}
	fromSession := func(e ast.Expr) bool {
		if named(pass.TypesInfo.TypeOf(e), cfg.SessionStore) {
			return true
		}
		sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Store" && named(pass.TypesInfo.TypeOf(sel.X), cfg.SessionStore)
	}

	used := map[string]bool{}
	var source *ast.File
	for _, file := range pass.Files {
		name := filepath.Base(pass.Fset.Position(file.Pos()).Filename)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if source == nil {
			source = file
		}
		defining := name == cfg.DefiningFile
		fieldWrite := func(e ast.Expr) {
			sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
			if !ok || defining {
				return
			}
			if v, ok := pass.TypesInfo.ObjectOf(sel.Sel).(*types.Var); ok {
				if typ, ok := guarded[v]; ok {
					pass.Reportf(sel.Pos(), "%s.%s set outside %s: mint it there", typ, v.Name(), cfg.DefiningFile)
				}
			}
		}
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch v := n.(type) {
			case *ast.CompositeLit:
				t := pass.TypesInfo.TypeOf(v)
				if !defining && len(v.Elts) > 0 && isGuarded(t) {
					pass.Reportf(v.Pos(), "%s literal with fields outside %s: mint it there", types.TypeString(types.Unalias(t), types.RelativeTo(pass.Pkg)), cfg.DefiningFile)
				}
				if named(t, cfg.WorkStore) && slices.ContainsFunc(v.Elts, func(e ast.Expr) bool {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						e = kv.Value
					}
					return fromSession(e)
				}) {
					pass.Reportf(v.Pos(), "a work store built from a sessions store")
				}
			case *ast.CallExpr:
				if tv, ok := pass.TypesInfo.Types[v.Fun]; ok && tv.IsType() && named(tv.Type, cfg.WorkStore) && len(v.Args) == 1 && fromSession(v.Args[0]) {
					pass.Reportf(v.Pos(), "a work store converted from a sessions store")
				}
			case *ast.AssignStmt:
				for _, lhs := range v.Lhs {
					fieldWrite(lhs)
				}
			case *ast.IncDecStmt:
				fieldWrite(v.X)
			case *ast.UnaryExpr:
				if v.Op == token.AND {
					fieldWrite(v.X)
				}
			case *ast.Ident:
				mint, ok := mints[pass.TypesInfo.Uses[v]]
				if !ok {
					break
				}
				site := enclosing(stack)
				if _, ok := cfg.Mints[mint][site]; !ok {
					pass.Reportf(v.Pos(), "%s referenced in %s, which is not a reviewed mint site", mint, orPackageLevel(site))
				} else {
					used[mint+"\x00"+site] = true
				}
			}
			return true
		})
	}
	if source == nil {
		return nil // a test-only pass
	}
	var stale []string
	for mint, sites := range cfg.Mints {
		for site := range sites {
			if !used[mint+"\x00"+site] {
				stale = append(stale, site+" ("+mint+")")
			}
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		pass.Reportf(source.Package, "reviewed mint site %s mints nothing: drop it", s)
	}
	checkScope(pass, cfg)
	return nil
}

// checkScope reports every type but cfg.ScopeImpls that implements cfg.Scope.
func checkScope(pass *analysis.Pass, cfg Config) {
	obj, ok := pass.Pkg.Scope().Lookup(cfg.Scope).(*types.TypeName)
	if !ok {
		return
	}
	iface, ok := obj.Type().Underlying().(*types.Interface)
	if !ok {
		return
	}
	for id, def := range pass.TypesInfo.Defs {
		tn, ok := def.(*types.TypeName)
		if !ok || tn == obj || slices.Contains(cfg.ScopeImpls, tn.Name()) {
			continue
		}
		if strings.HasSuffix(pass.Fset.Position(id.Pos()).Filename, "_test.go") {
			continue
		}
		t := tn.Type()
		if _, isIface := t.Underlying().(*types.Interface); isIface {
			continue
		}
		if types.Implements(t, iface) || types.Implements(types.NewPointer(t), iface) {
			pass.Reportf(id.Pos(), "%s implements %s: only %s may", tn.Name(), cfg.Scope, strings.Join(cfg.ScopeImpls, " and "))
		}
	}
}

// enclosing is the innermost function declaration on stack, "Func" or
// "Recv.Method", or "" at package level.
func enclosing(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		fn, ok := stack[i].(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			return fn.Name.Name
		}
		t := fn.Recv.List[0].Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		if id, ok := t.(*ast.Ident); ok {
			return id.Name + "." + fn.Name.Name
		}
		return fn.Name.Name
	}
	return ""
}

func orPackageLevel(site string) string {
	if site == "" {
		return "a package-level declaration"
	}
	return site
}
