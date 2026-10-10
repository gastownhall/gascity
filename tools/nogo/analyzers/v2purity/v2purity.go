// Package v2purity is the v2 reconciler's typed rules (EFFECT-STRUCTURE §4;
// ARCH-RESTRUCTURE R6.2), checked on every compile through nogo:
//
//   - Purity: a //gc:pure root may not reach a forbidden function, a
//     function or variable of a forbidden package, or a method of a
//     forbidden interface. The roots are functions marked //gc:pure
//     (the decide, admission, the dirty filters), every function stored in
//     a struct field marked //gc:pure (a section's Decide), every argument
//     a function marked //gc:pure-param passes for that parameter, and
//     every function literal of a variable marked //gc:pure.
//   - Reach is static and conservative. It follows calls and references:
//     a function named as a value, a package variable read (its
//     initializer and every assignment to it), a concrete method, and an
//     interface method call to every method of a type in scope that
//     implements the interface. A function stored in a struct field is
//     reached only when that field is read. Out of the package, a function
//     of a Module package carries a Reaches fact when it reaches a target;
//     other packages (the standard library, third-party modules) are leaves,
//     checked against the forbidden sets.
//   - A function on the Exempt list, with its reason, is a leaf: the
//     reviewed baseline, which only shrinks, and holds no live entry once
//     the Cutover constant is true (C9).
//   - The seal: a value of a sealed type (a proof only a fresh read mints)
//     is built, or a field of one written, only in its minting file
//     (seal.go).
package v2purity

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

// Config is what the rules check, by fully qualified name.
type Config struct {
	// Package is the one package path the rules apply to.
	Package string
	// Module is the path prefix of the packages whose functions export a
	// Reaches fact.
	Module string
	// ForbiddenPackages: any function, method or package variable of one
	// is forbidden, but an error variable (a sentinel compared, not read
	// from the world).
	ForbiddenPackages []string
	// ForbiddenFuncs are package functions, "path.Name".
	ForbiddenFuncs []string
	// ForbiddenInterfaces are interface types, "path.Name": calling a
	// method of one is forbidden, however it is reached.
	ForbiddenInterfaces []string
	// Roots, when set, pins the roots: a //gc:pure mark missing or added
	// fails. A function root is its types.Func.FullName, a field "T.F", a
	// parameter "F(p)", a variable its name.
	Roots []string
	// Exempt is the reviewed baseline of functions reach does not enter,
	// by types.Func.FullName: the package's own, or an imported one with a
	// Reaches fact. Each must be reached from a root and reach a target
	// itself, or it is stale.
	Exempt map[string]Exemption
	// Cutover names a package bool constant (v2EffectsReal) that is true
	// from C9: then a live exemption fails the build.
	Cutover string
	// Sealed maps a sealed type's name, in Package, to its minting file.
	Sealed map[string]string
}

// Exemption is one baseline entry. Class is "live" (reachable in v2 at run
// time) or "dead" (provably unreachable at run time); Bead tracks its
// removal. A dead entry names in Via the functions every root path to it
// passes, which the analyzer checks, and its Reason cites the line in one
// that keeps it from running.
type Exemption struct {
	Class, Bead, Reason string
	Via                 []string
}

// Reaches is the fact on a function of a Module package that reaches a
// forbidden target.
type Reaches struct {
	Target string // the forbidden function, variable or interface method
	Path   string // the functions from this one to the target
}

// AFact marks Reaches a fact.
func (*Reaches) AFact() {}

func (r *Reaches) String() string { return "reaches " + r.Target + " via " + r.Path }

// New is the analyzer for c.
func New(c Config) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name:      "v2purity",
		Doc:       "the v2 reconciler's //gc:pure reachability and sealed-type rules",
		Run:       func(pass *analysis.Pass) (any, error) { return nil, c.run(pass) },
		FactTypes: []analysis.Fact{new(Reaches), new(Methods)},
	}
}

func (c Config) run(pass *analysis.Pass) error {
	switch path := pass.Pkg.Path(); {
	case path == c.Package:
		g := c.build(pass, true)
		g.purity()
		c.seal(pass)
		c.cutover(pass)
	case c.Module != "" && strings.HasPrefix(path, c.Module) && !strings.HasSuffix(path, "_test"):
		c.build(pass, false).export()
	}
	return nil
}

// cutover reports a missing Cutover constant, and the live exemptions left
// once it is true (C9: no live reach from a //gc:pure root at cutover).
func (c Config) cutover(pass *analysis.Pass) {
	if c.Cutover == "" {
		return
	}
	k, ok := pass.Pkg.Scope().Lookup(c.Cutover).(*types.Const)
	if !ok || k.Val().Kind() != constant.Bool {
		pass.Reportf(pass.Files[0].Package, "v2purity cutover constant %s is not a bool constant of the package: restore it, or rename it in the config", c.Cutover)
		return
	}
	if !constant.BoolVal(k.Val()) {
		return
	}
	var live []string
	for name, e := range c.Exempt {
		if e.Class == "live" {
			live = append(live, name)
		}
	}
	if len(live) > 0 {
		sort.Strings(live)
		pass.Reportf(k.Pos(), "%s is true, but %d v2purity exemptions are live: %s", c.Cutover, len(live), strings.Join(live, ", "))
	}
}

// node is one reachable unit: a function's body, a package variable's
// initializer and assignments, the functions stored in a struct field, or
// the arguments for a //gc:pure-param parameter.
type node struct {
	name   string
	bodies []ast.Node
	links  []types.Object // nodes it holds too: a converted struct's fields
}

// graph is one package's reach.
type graph struct {
	c        Config
	pass     *analysis.Pass
	rules    bool // the rules' package: roots, exemptions
	nodes    map[types.Object]*node
	exempt   map[types.Object]ast.Node // the package's exempt functions' bodies
	deferred map[ast.Node]bool         // stored in a field: reached through a read of it
	roots    []types.Object
	params   map[*types.Func][]int // //gc:pure-param parameter indexes
	scope    []*types.TypeName     // named types in scope, for interface calls
	impls    map[*types.Func][]impl
	names    map[string]bool        // the exempt methods of other packages reach reached, by name
	pureFlds map[*types.Var]bool    // the //gc:pure fields
	banned   map[*types.Func]string // forbidden interfaces' methods
}

func (c Config) build(pass *analysis.Pass, rules bool) *graph {
	g := &graph{
		c: c, pass: pass, rules: rules, nodes: map[types.Object]*node{}, exempt: map[types.Object]ast.Node{},
		deferred: map[ast.Node]bool{}, params: map[*types.Func][]int{}, impls: map[*types.Func][]impl{},
		pureFlds: map[*types.Var]bool{}, names: map[string]bool{},
	}
	var files []*ast.File
	for _, f := range pass.Files {
		if !rules || !strings.HasSuffix(pass.Fset.File(f.Pos()).Name(), "_test.go") {
			files = append(files, f)
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				g.addFunc(d)
			case *ast.GenDecl:
				g.addGen(d)
			}
		}
	}
	for _, f := range files {
		ast.Inspect(f, g.collect)
	}
	return g
}

func (g *graph) addFunc(d *ast.FuncDecl) {
	fn, ok := g.pass.TypesInfo.Defs[d.Name].(*types.Func)
	if !ok || d.Body == nil {
		return
	}
	for _, p := range directives(d.Doc, "//gc:pure-param ") {
		sig := fn.Type().(*types.Signature)
		for i := range sig.Params().Len() {
			if sig.Params().At(i).Name() == p {
				g.params[fn] = append(g.params[fn], i)
				g.root(sig.Params().At(i), fn.Name()+"("+p+")")
			}
		}
	}
	if _, ok := g.c.Exempt[fn.FullName()]; ok && g.rules {
		g.exempt[fn] = d.Body
		return
	}
	g.nodes[fn] = &node{name: funcName(d), bodies: []ast.Node{d.Body}}
	if directive(d.Doc, "//gc:pure") {
		g.roots = append(g.roots, fn)
	}
}

func (g *graph) addGen(d *ast.GenDecl) {
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			for i, id := range s.Names {
				v, ok := g.pass.TypesInfo.Defs[id].(*types.Var)
				if !ok || d.Tok != token.VAR {
					continue
				}
				n := g.node(v, id.Name)
				if i < len(s.Values) {
					n.bodies = append(n.bodies, s.Values[i])
				}
				if directive(d.Doc, "//gc:pure") || directive(s.Doc, "//gc:pure") {
					g.roots = append(g.roots, v)
				}
			}
		case *ast.TypeSpec:
			st, ok := s.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, f := range st.Fields.List {
				if !directive(f.Doc, "//gc:pure") {
					continue
				}
				for _, id := range f.Names {
					g.root(g.pass.TypesInfo.Defs[id], s.Name.Name+"."+id.Name)
					if v, ok := g.pass.TypesInfo.Defs[id].(*types.Var); ok {
						g.pureFlds[v] = true
					}
				}
			}
		}
	}
}

// root makes obj, a field or a parameter, a root named name.
func (g *graph) root(obj types.Object, name string) {
	g.node(obj, name)
	g.roots = append(g.roots, obj)
}

func (g *graph) node(obj types.Object, name string) *node {
	n := g.nodes[obj]
	if n == nil {
		n = &node{name: name}
		g.nodes[obj] = n
	}
	return n
}

// collect records, over the package's syntax, what a later read reaches:
// every assignment to a package variable, every function stored in a struct
// field, and every argument for a //gc:pure-param parameter.
func (g *graph) collect(n ast.Node) bool {
	info := g.pass.TypesInfo
	switch n := n.(type) {
	case *ast.AssignStmt:
		for i, lhs := range n.Lhs {
			rhs := n.Rhs[0]
			if len(n.Rhs) == len(n.Lhs) {
				rhs = n.Rhs[i]
			}
			switch lhs := ast.Unparen(lhs).(type) {
			case *ast.Ident:
				if v, ok := info.Uses[lhs].(*types.Var); ok && v.Pkg() == g.pass.Pkg && v.Parent() == g.pass.Pkg.Scope() {
					g.node(v, v.Name()).bodies = append(g.node(v, v.Name()).bodies, rhs)
				}
			case *ast.SelectorExpr:
				if sel := info.Selections[lhs]; sel != nil && sel.Kind() == types.FieldVal {
					g.store(sel.Obj().(*types.Var), rhs)
				}
			case *ast.IndexExpr: // a registry filled by index: reg[k] = f, s.m[k] = f
				switch x := ast.Unparen(lhs.X).(type) {
				case *ast.Ident:
					if v, ok := info.Uses[x].(*types.Var); ok && v.Pkg() == g.pass.Pkg && v.Parent() == g.pass.Pkg.Scope() {
						g.node(v, v.Name()).bodies = append(g.node(v, v.Name()).bodies, rhs)
					}
				case *ast.SelectorExpr:
					if sel := info.Selections[x]; sel != nil && sel.Kind() == types.FieldVal {
						g.store(sel.Obj().(*types.Var), rhs)
					}
				}
			}
		}
	case *ast.CompositeLit:
		t := info.TypeOf(n)
		if ptr, ok := t.(*types.Pointer); ok {
			t = ptr.Elem()
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok {
			return true
		}
		for i, e := range n.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if f, ok := info.Uses[kv.Key.(*ast.Ident)].(*types.Var); ok {
					g.store(f, kv.Value)
				}
			} else if i < st.NumFields() {
				g.store(st.Field(i), e)
			}
		}
	case *ast.CallExpr:
		if tv, ok := info.Types[n.Fun]; ok && tv.IsType() && len(n.Args) == 1 {
			g.convert(tv.Type, info.TypeOf(n.Args[0])) // T(u): u's fields' functions are T's
			return true
		}
		fn, ok := typeutilCallee(info, n).(*types.Func)
		if !ok {
			return true
		}
		for _, i := range g.params[fn.Origin()] {
			if i < len(n.Args) {
				p := fn.Origin().Type().(*types.Signature).Params().At(i)
				g.nodes[p].bodies = append(g.nodes[p].bodies, n.Args[i])
			}
		}
	}
	return true
}

// convert links each field of to, a struct type, to the same field of from,
// one converted into it: a function stored in from's field is stored in
// to's, and a //gc:pure field of to roots it.
func (g *graph) convert(to, from types.Type) {
	ts, ok1 := to.Underlying().(*types.Struct)
	fs, ok2 := from.Underlying().(*types.Struct)
	if !ok1 || !ok2 || ts.NumFields() != fs.NumFields() {
		return
	}
	for i := range ts.NumFields() {
		t, f := ts.Field(i), fs.Field(i)
		if t.Pkg() == g.pass.Pkg && holdsFuncs(t.Type(), map[types.Type]bool{}) {
			g.node(t, t.Name()).links = append(g.node(t, t.Name()).links, f)
		}
	}
}

// store records the functions e stores in field f, a function literal or a
// named function, or the ones of a slice or map literal of them.
func (g *graph) store(f *types.Var, e ast.Expr) {
	if !holdsFuncs(f.Type(), map[types.Type]bool{}) || f.Pkg() != g.pass.Pkg {
		return // another package's field: reached where it is stored
	}
	switch e := ast.Unparen(e).(type) {
	case *ast.FuncLit:
	case *ast.Ident, *ast.SelectorExpr:
		if _, ok := g.pass.TypesInfo.Uses[ident(e)].(*types.Func); !ok {
			g.unfollowable(f, e)
			return
		}
	case *ast.CallExpr: // s.fs = append(s.fs, f...)
		if b, ok := g.pass.TypesInfo.Uses[ident(e.Fun)].(*types.Builtin); ok && b.Name() == "append" {
			for _, a := range e.Args[1:] {
				g.store(f, a)
			}
			return
		}
		g.unfollowable(f, e)
		return
	case *ast.CompositeLit:
		if _, ok := g.pass.TypesInfo.TypeOf(e).Underlying().(*types.Struct); !ok {
			for _, el := range e.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					el = kv.Value
				}
				g.store(f, el)
			}
		}
		return
	default:
		g.unfollowable(f, e)
		return
	}
	g.deferred[e] = true
	g.node(f, f.Name()).bodies = append(g.node(f, f.Name()).bodies, e)
}

// unfollowable fails closed on e, a value v2purity cannot follow, stored in
// a //gc:pure field: its roots would go unchecked.
func (g *graph) unfollowable(f *types.Var, e ast.Expr) {
	if id, ok := e.(*ast.Ident); ok && id.Name == "nil" || !g.rules || !g.pureFlds[f] {
		return
	}
	g.pass.Reportf(e.Pos(), "v2purity cannot follow this value into //gc:pure field %s: store a function literal or a named function", f.Name())
}

// holdsFuncs reports a type that can hold a function value.
func holdsFuncs(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t := t.Underlying().(type) {
	case *types.Signature, *types.Interface:
		return true
	case *types.Pointer:
		return holdsFuncs(t.Elem(), seen)
	case *types.Slice:
		return holdsFuncs(t.Elem(), seen)
	case *types.Array:
		return holdsFuncs(t.Elem(), seen)
	case *types.Map:
		return holdsFuncs(t.Elem(), seen)
	case *types.Struct:
		for i := range t.NumFields() {
			if holdsFuncs(t.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// edge is one thing a body reaches: a forbidden target at pos, or an
// object (a function, variable or field) to follow.
type edge struct {
	pos            token.Pos
	target, exempt string // exempt: another package's method the baseline exempts
	obj            types.Object
}

// edges calls visit for each edge of body.
func (g *graph) edges(body ast.Node, visit func(edge)) {
	info := g.pass.TypesInfo
	skip := map[*ast.Ident]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if n != body && g.deferred[n] {
			return false
		}
		switch n := n.(type) {
		case *ast.SelectorExpr:
			sel := info.Selections[n]
			if sel == nil {
				return true
			}
			switch sel.Kind() {
			case types.MethodVal, types.MethodExpr:
				fn := sel.Obj().(*types.Func).Origin()
				if !abstract(fn) {
					return true
				}
				skip[n.Sel] = true
				if t := g.forbiddenMethod(fn); t != "" {
					visit(edge{pos: n.Sel.Pos(), target: t})
					return true
				}
				for _, im := range g.implementers(fn) {
					switch {
					case im.target != "" || im.exempt != "":
						visit(edge{pos: n.Sel.Pos(), target: im.target, exempt: im.exempt})
					default:
						g.ref(im.fn, n.Sel.Pos(), visit)
					}
				}
			case types.FieldVal:
				var r Reaches
				switch f := sel.Obj(); {
				case g.nodes[f] != nil:
					visit(edge{pos: n.Sel.Pos(), obj: f})
				case f.Pkg() != g.pass.Pkg && g.pass.ImportObjectFact(f, &r):
					visit(edge{pos: n.Sel.Pos(), target: f.Pkg().Path() + "." + f.Name() + " (a field), which reaches " + r.Target + " (via " + r.Path + ")"})
				}
			}
		case *ast.Ident:
			if !skip[n] {
				g.ref(info.Uses[n], n.Pos(), visit)
			}
		}
		return true
	})
}

// ref visits obj, used at pos, as a target or an object to follow.
func (g *graph) ref(obj types.Object, pos token.Pos, visit func(edge)) {
	switch o := obj.(type) {
	case *types.Func:
		fn := o.Origin()
		if abstract(fn) {
			return
		}
		if t := g.forbiddenFunc(fn); t != "" {
			visit(edge{pos: pos, target: t})
			return
		}
		if fn.Pkg() == g.pass.Pkg {
			visit(edge{pos: pos, obj: fn})
			return
		}
		if _, ok := g.c.Exempt[fn.FullName()]; ok && g.rules {
			visit(edge{pos: pos, obj: fn})
			return
		}
		var r Reaches
		if fn.Pkg() != nil && g.pass.ImportObjectFact(fn, &r) {
			visit(edge{pos: pos, target: fn.FullName() + ", which reaches " + r.Target + " (via " + r.Path + ")"})
		}
	case *types.Var:
		if o.Pkg() == nil || o.Parent() != o.Pkg().Scope() {
			return
		}
		var r Reaches
		switch {
		case slices.Contains(g.c.ForbiddenPackages, o.Pkg().Path()) && !types.Identical(o.Type(), types.Universe.Lookup("error").Type()):
			visit(edge{pos: pos, target: o.Pkg().Path() + "." + o.Name()})
		case o.Pkg() == g.pass.Pkg:
			visit(edge{pos: pos, obj: o})
		case g.pass.ImportObjectFact(o, &r):
			visit(edge{pos: pos, target: o.Pkg().Path() + "." + o.Name() + " (a variable), which reaches " + r.Target + " (via " + r.Path + ")"})
		}
	}
}

// forbiddenFunc is fn's forbidden name, or "": a function or method of a
// forbidden package, or a forbidden package-level function.
func (g *graph) forbiddenFunc(fn *types.Func) string {
	if fn.Pkg() == nil {
		return ""
	}
	full := fn.Pkg().Path() + "." + fn.Name()
	if slices.Contains(g.c.ForbiddenPackages, fn.Pkg().Path()) {
		return full
	}
	if fn.Type().(*types.Signature).Recv() == nil && slices.Contains(g.c.ForbiddenFuncs, full) {
		return full // not a method: time.Time.After is not time.After
	}
	return ""
}

// forbiddenMethod is "I.M" when fn is a method of forbidden interface I,
// declared in it or in an interface it embeds, or "".
func (g *graph) forbiddenMethod(fn *types.Func) string {
	if g.banned == nil {
		g.banned = map[*types.Func]string{}
		for _, tn := range g.types() {
			full := tn.Pkg().Path() + "." + tn.Name()
			iface, ok := tn.Type().Underlying().(*types.Interface)
			if !ok || !slices.Contains(g.c.ForbiddenInterfaces, full) {
				continue
			}
			for i := range iface.NumMethods() {
				g.banned[iface.Method(i)] = full + "." + iface.Method(i).Name()
			}
		}
	}
	return g.banned[fn]
}

// types are the named types of the package and everything it imports, as
// the driver shows them (an export-data view hides unexported ones): only
// for the forbidden interfaces, which are exported.
func (g *graph) types() []*types.TypeName {
	if g.scope != nil {
		return g.scope
	}
	seen := map[*types.Package]bool{}
	var walk func(p *types.Package)
	walk = func(p *types.Package) {
		if seen[p] {
			return
		}
		seen[p] = true
		for _, name := range p.Scope().Names() {
			if tn, ok := p.Scope().Lookup(name).(*types.TypeName); ok && !tn.IsAlias() {
				g.scope = append(g.scope, tn)
			}
		}
		for _, imp := range p.Imports() {
			walk(imp)
		}
	}
	walk(g.pass.Pkg)
	return g.scope
}

// export computes, for every function and package variable of the package
// and every field it stores functions in, the first target it reaches, and
// exports it as a Reaches fact; then the package's Methods fact.
func (g *graph) export() {
	objs := g.sorted()
	reach := map[types.Object]*Reaches{}
	next := map[types.Object][]types.Object{}
	for _, obj := range objs {
		for _, body := range g.nodes[obj].bodies {
			g.edges(body, func(e edge) {
				switch {
				case e.target != "" && reach[obj] == nil:
					reach[obj] = &Reaches{Target: e.target, Path: g.nodes[obj].name}
				case e.obj != nil && g.nodes[e.obj] != nil:
					next[obj] = append(next[obj], e.obj)
				}
			})
		}
		for _, l := range g.nodes[obj].links {
			if g.nodes[l] != nil {
				next[obj] = append(next[obj], l)
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, obj := range objs {
			if reach[obj] != nil {
				continue
			}
			for _, to := range next[obj] {
				if r := reach[to]; r != nil {
					reach[obj] = &Reaches{Target: r.Target, Path: g.nodes[obj].name + " -> " + r.Path}
					changed = true
					break
				}
			}
		}
	}
	for _, obj := range objs {
		if v, ok := obj.(*types.Var); reach[obj] != nil && obj.Pkg() == g.pass.Pkg && (!ok || v.IsField() || v.Parent() == g.pass.Pkg.Scope()) {
			g.pass.ExportObjectFact(obj, reach[obj])
		}
	}
	g.summarize(reach)
}

// sorted are the nodes in source order.
func (g *graph) sorted() []types.Object {
	objs := make([]types.Object, 0, len(g.nodes))
	for obj := range g.nodes {
		objs = append(objs, obj)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Pos() < objs[j].Pos() })
	return objs
}

// finding is one forbidden reference reached from a root: where, what,
// and the path to the node holding it.
type finding struct {
	pos    token.Pos
	target string
	path   []string
}

// purity reports each forbidden reference a root reaches, with the path,
// then checks the roots and the baseline.
func (g *graph) purity() {
	reported := map[token.Pos]bool{}
	reached := map[types.Object]bool{}
	for _, root := range g.roots {
		for _, f := range g.reach(root, reached, nil) {
			if !reported[f.pos] {
				reported[f.pos] = true
				g.pass.Reportf(f.pos, "%s reaches %s, but %s is //gc:pure (via %s)", f.path[len(f.path)-1], f.target, g.nodes[root].name, strings.Join(f.path, " -> "))
			}
		}
	}
	g.checkRoots()
	g.checkExempt(reached)
}

// reach walks from root breadth first, recording the exempt functions and
// the nodes it reaches, and returns every forbidden reference, each with
// the first path to it. It does not enter a blocked node.
func (g *graph) reach(root types.Object, reached map[types.Object]bool, blocked map[types.Object]bool) []finding {
	type step struct {
		obj  types.Object
		path []string
	}
	var out []finding
	seen := map[types.Object]bool{root: true}
	reached[root] = true
	queue := []step{{root, []string{g.nodes[root].name}}}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		visit := func(e edge) {
			switch {
			case e.target != "":
				out = append(out, finding{e.pos, e.target, s.path})
			case e.exempt != "":
				g.names[e.exempt] = true
			case seen[e.obj] || blocked[e.obj]:
			case g.nodes[e.obj] == nil:
				if _, ok := g.c.Exempt[funcFullName(e.obj)]; ok {
					reached[e.obj] = true
				}
			default:
				seen[e.obj], reached[e.obj] = true, true
				queue = append(queue, step{e.obj, append(slices.Clip(s.path), g.nodes[e.obj].name)})
			}
		}
		for _, body := range g.nodes[s.obj].bodies {
			g.edges(body, visit)
		}
		for _, l := range g.nodes[s.obj].links {
			visit(edge{obj: l})
		}
	}
	return out
}

// checkRoots reports a root added or removed against the pinned Roots.
func (g *graph) checkRoots() {
	if g.c.Roots == nil {
		return
	}
	var got []string
	for _, r := range g.roots {
		got = append(got, rootName(r, g.nodes[r].name))
	}
	for _, name := range got {
		if !slices.Contains(g.c.Roots, name) {
			g.pass.Reportf(g.pass.Files[0].Package, "v2purity root %s is not pinned in Roots: add it in review", name)
		}
	}
	for _, name := range g.c.Roots {
		if !slices.Contains(got, name) {
			g.pass.Reportf(g.pass.Files[0].Package, "pinned v2purity root %s is not marked //gc:pure", name)
		}
	}
}

// checkExempt reports a baseline entry that is malformed, names nothing a
// root reaches, reaches no target itself, or, dead, is reached around its
// Via: the baseline only ever shrinks to what is still true.
func (g *graph) checkExempt(reached map[types.Object]bool) {
	byName := map[string]types.Object{}
	for obj := range reached {
		byName[funcFullName(obj)] = obj
	}
	for obj := range g.exempt {
		byName[funcFullName(obj)] = obj
	}
	names := make([]string, 0, len(g.c.Exempt))
	for name := range g.c.Exempt {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e, obj := g.c.Exempt[name], byName[name]
		pos := g.pass.Files[0].Package
		if obj != nil && obj.Pkg() == g.pass.Pkg {
			pos = obj.Pos()
		}
		switch {
		case e.Class != "live" && e.Class != "dead", e.Bead == "", e.Reason == "",
			e.Class == "dead" && (!strings.Contains(e.Reason, ".go:") || len(e.Via) == 0):
			g.pass.Reportf(pos, "v2purity exemption %s is malformed: a class (live or dead), a bead and a reason; a dead one names its Via and cites the line that makes it dead", name)
		case obj == nil && g.names[name] && e.Class == "dead":
			g.checkViaByName(name, e.Via, pos)
		case obj == nil && g.names[name]: // another package's method an interface call reaches
		case obj == nil:
			g.pass.Reportf(pos, "v2purity exemption %s names no function a //gc:pure root reaches: drop it", name)
		case !reached[obj]:
			g.pass.Reportf(pos, "v2purity exemption %s is reached from no //gc:pure root: drop it", name)
		case !g.holdsForbidden(obj):
			g.pass.Reportf(pos, "v2purity exemption %s holds no forbidden reference: drop it", name)
		case e.Class == "dead":
			g.checkVia(name, obj, e.Via, pos)
		}
	}
}

// checkVia reports a dead entry's Via that names no reached function, or
// that a root reaches the entry around.
func (g *graph) checkVia(name string, obj types.Object, via []string, pos token.Pos) {
	blocked := map[types.Object]bool{}
	for _, v := range via {
		found := false
		for o := range g.nodes {
			if funcFullName(o) == v {
				blocked[o], found = true, true
			}
		}
		if !found {
			g.pass.Reportf(pos, "v2purity exemption %s: Via %s names no function of the package", name, v)
			return
		}
	}
	around := map[types.Object]bool{}
	for _, root := range g.roots {
		if !blocked[root] {
			g.reach(root, around, blocked)
		}
	}
	if around[obj] {
		g.pass.Reportf(pos, "v2purity exemption %s is dead through %s, but a //gc:pure root reaches it around them", name, strings.Join(via, ", "))
	}
}

// checkViaByName is checkVia for another package's method an interface call
// reaches, known by name.
func (g *graph) checkViaByName(name string, via []string, pos token.Pos) {
	saved := g.names
	defer func() { g.names = saved }()
	g.names = map[string]bool{}
	g.checkVia(name, nil, via, pos)
	if g.names[name] {
		g.pass.Reportf(pos, "v2purity exemption %s is dead through %s, but a //gc:pure root reaches it around them", name, strings.Join(via, ", "))
	}
}

// holdsForbidden reports obj, an exempt function, reaching a target: its
// body, through functions no other entry exempts, or an imported
// function's fact.
func (g *graph) holdsForbidden(obj types.Object) bool {
	body, ok := g.exempt[obj]
	if !ok {
		var r Reaches
		fn, isFunc := obj.(*types.Func)
		return isFunc && g.pass.ImportObjectFact(fn, &r)
	}
	found, seen, queue := false, map[types.Object]bool{}, []ast.Node{body}
	for len(queue) > 0 && !found {
		b := queue[0]
		queue = queue[1:]
		g.edges(b, func(e edge) {
			switch {
			case e.target != "":
				found = true
			case !seen[e.obj] && g.nodes[e.obj] != nil:
				seen[e.obj] = true
				queue = append(queue, g.nodes[e.obj].bodies...)
			}
		})
	}
	return found
}

// abstract reports an interface method.
func abstract(fn *types.Func) bool {
	recv := fn.Type().(*types.Signature).Recv()
	return recv != nil && types.IsInterface(recv.Type())
}

func funcFullName(obj types.Object) string {
	if fn, ok := obj.(*types.Func); ok {
		return fn.FullName()
	}
	return ""
}

// rootName is how Roots names root r with node name n.
func rootName(r types.Object, n string) string {
	if fn, ok := r.(*types.Func); ok {
		return fn.FullName()
	}
	return n
}

// typeutilCallee is the function or method a call statically calls, or nil.
func typeutilCallee(info *types.Info, call *ast.CallExpr) types.Object {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		return info.Uses[fun]
	case *ast.SelectorExpr:
		return info.Uses[fun.Sel]
	case *ast.IndexExpr:
		return info.Uses[ident(fun.X)]
	}
	return nil
}

// ident is e's identifier: e itself, or a selector's.
func ident(e ast.Expr) *ast.Ident {
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		return e
	case *ast.SelectorExpr:
		return e.Sel
	}
	return nil
}

// directive reports a doc comment holding the directive d.
func directive(doc *ast.CommentGroup, d string) bool {
	if doc == nil {
		return false
	}
	for _, cm := range doc.List {
		if strings.TrimSpace(cm.Text) == d {
			return true
		}
	}
	return false
}

// directives are the arguments of each prefix directive in doc.
func directives(doc *ast.CommentGroup, prefix string) []string {
	if doc == nil {
		return nil
	}
	var out []string
	for _, cm := range doc.List {
		if arg, ok := strings.CutPrefix(strings.TrimSpace(cm.Text), prefix); ok {
			out = append(out, strings.TrimSpace(arg))
		}
	}
	return out
}

// funcName is d's name, "Recv.Name" for a method.
func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	t := d.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	switch ix := t.(type) {
	case *ast.IndexExpr:
		t = ix.X
	case *ast.IndexListExpr:
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + d.Name.Name
	}
	return d.Name.Name
}
