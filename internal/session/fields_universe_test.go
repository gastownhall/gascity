package session

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The census reads production Go as syntax, without type checking. A key is a
// string literal or a string constant, resolved through its declaring
// package. A function is a top-level func or method, "Func" or "Type.Method";
// its closures count as part of it. A call resolves to a function of the
// named package, of its own package, or to the only scanned method of that
// name; anything else stays unresolved.

const modulePath = "github.com/gastownhall/gascity/"

// censusDirs hold every production writer of a session row (their own
// files; subpackages are other packages). The others write their own bead
// classes (mail, extmsg, nudge queue).
var censusDirs = []string{"internal/session", "cmd/gc", "internal/api", "internal/worker"}

// constDirs also declare the key constants the census dirs spell keys with.
var constDirs = []string{"internal/beadmeta"}

const (
	kWrite uint8 = 1 << iota // written with a non-empty or unknown value
	kClear                   // written as ""
)

type keyKinds map[string]uint8

func (k keyKinds) add(key string, kind uint8) { k[key] |= kind }

// funcFacts is one function's summary. kinds are the keys it puts in a
// metadata map, its own or one a callee returned (loose: a site's role is
// checked against them). sinks are the keys that reach a store write or an
// effect's patch (strict: the universe). ret, sinkParams and keyParams are
// what its callers inherit.
type funcFacts struct {
	id, file, name string
	decl           *ast.FuncDecl
	r              keyResolver
	params         map[string]int
	kinds, ret     keyKinds
	reads, sinks   map[string]bool
	direct         map[string]bool // keys it spells itself
	sinkParams     map[int]bool
	paramKinds     map[int]keyKinds // what it puts in each map parameter
	keyParams      map[[3]int]bool  // (map, key, value) parameter indexes; value -1 when not a parameter
	calls          map[string]bool  // resolved callees, by callKey
}

type census struct {
	funcs    map[string]*funcFacts // by "file:Func"
	byCall   map[string]*funcFacts // by callKey
	universe map[string]string     // key -> one function that writes it
	lists    map[string][]string   // "dir:var" -> a package-level key list
}

// batchWriters write a row's metadata from a map argument.
var batchWriters = map[string]bool{
	"SetMetadataBatch": true, "CloseWithMetadataIfMatch": true, "CloseWithTerminalPatch": true, "CloseAll": true,
	"ApplyPatch": true, "applyStore": true,
}

// keyWriters write one key: the index of the key argument; the value is
// last. setMarker is legacy's local (key, value) wrapper.
var keyWriters = map[string]int{"SetMetadata": 1, "CompareAndSetMetadataKey": 1, "SetMarker": 1, "setMetadataValue": 1, "setMarker": 0}

// sinkFields carry a row's metadata (a created bead's, a session CreateSpec's
// or CreateOptions' extra metadata, an UpdateIfMatch's) or an effect's patch
// to its write, by the literal type that holds them.
var sinkFields = map[string]map[string]bool{
	"Metadata":  {"Bead": true, "UpdateOpts": true, "CreateSpec": true},
	"ExtraMeta": {"CreateOptions": true},
	"Patch":     {"intent": true},
}

// metadataKeyShape is a row metadata key: lower snake case, dotted in the gc.*
// namespace. Env (GC_*) and template ({{.Name}}) keys do not match.
var metadataKeyShape = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)

type parsedFile struct {
	rel string
	f   *ast.File
}

func scanCensus(root string) (*census, error) {
	var files []parsedFile
	for _, dir := range append(append([]string{}, censusDirs...), constDirs...) {
		ents, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, dir, e.Name()), nil, 0)
			if err != nil {
				return nil, err
			}
			files = append(files, parsedFile{dir + "/" + e.Name(), f})
		}
	}
	return scanFiles(files), nil
}

func scanFiles(files []parsedFile) *census {
	consts, lists := collectConsts(files)
	c := &census{funcs: map[string]*funcFacts{}, byCall: map[string]*funcFacts{}, universe: map[string]string{}, lists: lists}
	var fieldKeys map[string][]string
	methods := map[string][]*funcFacts{}
	for _, p := range files {
		r := resolverFor(p, consts)
		if p.rel == "internal/session/info_codec.go" {
			fieldKeys = codecFieldKeys(p.f, r)
		}
		if !inCensusDirs(p.rel) {
			continue
		}
		for _, decl := range p.f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				ff := &funcFacts{id: p.rel + ":" + funcName(fd), file: p.rel, name: fd.Name.Name, decl: fd, r: r, params: map[string]int{}}
				i := 0
				for _, fld := range fd.Type.Params.List {
					for _, n := range fld.Names {
						ff.params[n.Name] = i
						i++
					}
					if len(fld.Names) == 0 {
						i++
					}
				}
				c.funcs[ff.id] = ff
				if fd.Recv != nil {
					methods[ff.name] = append(methods[ff.name], ff)
				} else {
					c.byCall[filepath.Dir(p.rel)+":"+ff.name] = ff
				}
			}
		}
	}
	for name, ms := range methods {
		if len(ms) == 1 {
			c.byCall["method:"+name] = ms[0]
		}
	}
	for round, changed := 0, true; changed && round < 10; round++ {
		changed = false
		for _, ff := range c.funcs {
			changed = c.analyze(ff, fieldKeys) || changed
		}
	}
	for _, ff := range c.funcs {
		for k := range ff.sinks {
			if prev, seen := c.universe[k]; (!seen || ff.id < prev) && metadataKeyShape.MatchString(strings.TrimSuffix(k, "*")) {
				c.universe[k] = ff.id
			}
		}
	}
	return c
}

func inCensusDirs(rel string) bool {
	for _, d := range censusDirs {
		if filepath.Dir(rel) == d {
			return true
		}
	}
	return false
}

// callKey names a call target: "dir:Func" for a function, "method:Name" for
// a method.
func (r keyResolver) callKey(x *ast.CallExpr) string {
	switch f := x.Fun.(type) {
	case *ast.Ident:
		return r.dir + ":" + f.Name
	case *ast.SelectorExpr:
		if pkg, ok := f.X.(*ast.Ident); ok && r.imports[pkg.Name] != "" {
			return r.imports[pkg.Name] + ":" + f.Sel.Name
		}
		return "method:" + f.Sel.Name
	}
	return ""
}

// collectConsts reads every string constant and package-level string list by
// package dir. A constant declared as another one (const k = session.Key)
// resolves after all files are read; an alias of an alias, on a later round.
func collectConsts(files []parsedFile) (map[string]map[string]string, map[string][]string) {
	consts, lists := map[string]map[string]string{}, map[string][]string{}
	type pending struct {
		p    parsedFile
		name string
		expr ast.Expr
	}
	var aliases, listDecls []pending
	for _, p := range files {
		dir := filepath.Dir(p.rel)
		if consts[dir] == nil {
			consts[dir] = map[string]string{}
		}
		for _, decl := range p.f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				for i := 0; ok && i < len(vs.Names) && i < len(vs.Values); i++ {
					switch v := vs.Values[i].(type) {
					case *ast.BasicLit:
						if v.Kind == token.STRING && gd.Tok == token.CONST {
							consts[dir][vs.Names[i].Name], _ = strconv.Unquote(v.Value)
						}
					case *ast.Ident, *ast.SelectorExpr:
						if gd.Tok == token.CONST || gd.Tok == token.VAR { // var k = session.Key aliases a constant too
							aliases = append(aliases, pending{p, vs.Names[i].Name, v})
						}
					case *ast.CompositeLit:
						listDecls = append(listDecls, pending{p, vs.Names[i].Name, v})
					}
				}
			}
		}
	}
	for range 3 {
		for _, a := range aliases {
			if v, ok := resolverFor(a.p, consts).key(a.expr); ok {
				consts[filepath.Dir(a.p.rel)][a.name] = v
			}
		}
	}
	for _, l := range listDecls {
		r := resolverFor(l.p, consts)
		for _, el := range l.expr.(*ast.CompositeLit).Elts {
			if k, ok := r.key(el); ok {
				lists[r.dir+":"+l.name] = append(lists[r.dir+":"+l.name], k)
			}
		}
	}
	return consts, lists
}

type keyResolver struct {
	consts  map[string]map[string]string
	dir     string
	imports map[string]string // import name -> repo dir
}

func resolverFor(p parsedFile, consts map[string]map[string]string) keyResolver {
	r := keyResolver{consts: consts, dir: filepath.Dir(p.rel), imports: map[string]string{}}
	for _, imp := range p.f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if !strings.HasPrefix(path, modulePath) {
			continue
		}
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		r.imports[name] = strings.TrimPrefix(path, modulePath)
	}
	return r
}

// key resolves e to a metadata key: a string literal, or a string constant
// of this package or an imported one.
func (r keyResolver) key(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			v, err := strconv.Unquote(x.Value)
			return v, err == nil
		}
	case *ast.Ident:
		v, ok := r.consts[r.dir][x.Name]
		return v, ok
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok {
			v, ok := r.consts[r.imports[pkg.Name]][x.Sel.Name]
			return v, ok
		}
	case *ast.BinaryExpr: // an open-world family: prefix + name
		if prefix, ok := r.key(x.X); ok && x.Op == token.ADD {
			return prefix + "*", true
		}
	}
	return "", false
}

func kindOf(value ast.Expr) uint8 {
	if lit, ok := value.(*ast.BasicLit); ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``") {
		return kClear
	}
	return kWrite
}

// isPatchType reports MetadataPatch or map[string]string.
func isPatchType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "MetadataPatch"
	case *ast.SelectorExpr:
		return t.Sel.Name == "MetadataPatch"
	case *ast.MapType:
		k, kok := t.Key.(*ast.Ident)
		v, vok := t.Value.(*ast.Ident)
		return kok && vok && k.Name == "string" && v.Name == "string"
	}
	return false
}

func isMetadataPatch(e ast.Expr) bool { _, isMap := e.(*ast.MapType); return isPatchType(e) && !isMap }

func resultIs(ft *ast.FuncType, pred func(ast.Expr) bool) bool {
	if ft.Results != nil {
		for _, r := range ft.Results.List {
			if pred(r.Type) {
				return true
			}
		}
	}
	return false
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func isMetadataField(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Metadata"
}

// analyze recomputes ff's summary from its callees'; it reports a change to
// what callers inherit.
func (c *census) analyze(ff *funcFacts, fieldKeys map[string][]string) bool {
	r, body := ff.r, ff.decl.Body
	before := fmt.Sprint(ff.ret, ff.sinkParams, ff.paramKinds, ff.keyParams)
	ff.kinds, ff.ret, ff.reads, ff.sinks, ff.direct = keyKinds{}, keyKinds{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	ff.sinkParams, ff.paramKinds, ff.keyParams, ff.calls = map[int]bool{}, map[int]keyKinds{}, map[[3]int]bool{}, map[string]bool{}
	local := map[string]keyKinds{} // what each local map holds
	hold := func(name string, kinds keyKinds) {
		if name == "" {
			return
		}
		if local[name] == nil {
			local[name] = keyKinds{}
		}
		for k, v := range kinds {
			local[name].add(k, v)
			ff.kinds.add(k, v)
		}
	}
	// exprKinds is what a map-valued expression holds.
	exprKinds := func(e ast.Expr) keyKinds {
		out := keyKinds{}
		switch x := e.(type) {
		case *ast.CompositeLit:
			if isPatchType(x.Type) {
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := r.key(kv.Key); ok {
							out.add(k, kindOf(kv.Value))
						}
					}
				}
			}
		case *ast.CallExpr:
			if callee := c.byCall[r.callKey(x)]; callee != nil {
				for k, v := range callee.ret {
					out.add(k, v)
				}
			}
		case *ast.Ident:
			for k, v := range local[x.Name] {
				out.add(k, v)
			}
		}
		return out
	}
	// Loops over a key list, a package-level one or a callee's patch, write
	// each listed key at the indexes their variables make in the loop body.
	loopKeys := map[ast.Node]keyKinds{}
	loopLocal := map[ast.Node]loopFrom{} // an index a loop over a local map writes
	rangeLits := map[ast.Node]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		keys := keyKinds{}
		localRange := ""
		switch x := rs.X.(type) {
		case *ast.CompositeLit:
			rangeLits[x] = true
			for _, el := range x.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					el = kv.Key
				}
				if k, ok := r.key(el); ok {
					keys.add(k, kWrite)
				}
			}
		case *ast.Ident:
			for _, k := range c.lists[r.dir+":"+x.Name] {
				keys.add(k, kWrite)
			}
			if c.lists[r.dir+":"+x.Name] == nil { // a local map: pass 1 knows what it holds
				localRange = x.Name
			}
		case *ast.CallExpr:
			keys = exprKinds(x)
		}
		kv := identName(rs.Key) // a map's key; a list's element is its value
		if lit, ok := rs.X.(*ast.CompositeLit); ok {
			if _, isArr := lit.Type.(*ast.ArrayType); isArr {
				kv = identName(rs.Value)
			}
		} else if _, isList := rs.X.(*ast.Ident); isList && localRange == "" {
			kv = identName(rs.Value)
		}
		ast.Inspect(rs.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
				for i, l := range as.Lhs {
					if ix, ok := l.(*ast.IndexExpr); ok && kv != "" && identName(ix.Index) == kv {
						set := keyKinds{}
						for k, v := range keys {
							if rs.Value == nil || identName(as.Rhs[i]) != identName(rs.Value) || kv == identName(rs.Value) {
								v = kindOf(as.Rhs[i])
							}
							set[k] = v
						}
						loopKeys[ix] = set
						if localRange != "" {
							keep := rs.Value != nil && identName(as.Rhs[i]) == identName(rs.Value)
							loopLocal[ix] = loopFrom{localRange, keep, kindOf(as.Rhs[i])}
						}
					}
				}
			}
			return true
		})
		return true
	})
	// Pass 1: what each local map holds.
	lhs := map[ast.Node]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, l := range x.Lhs {
				lhs[l] = true
				var rhs ast.Expr
				if len(x.Rhs) == len(x.Lhs) {
					rhs = x.Rhs[i]
				}
				switch lt := l.(type) {
				case *ast.Ident:
					if rhs != nil {
						hold(lt.Name, exprKinds(rhs))
					}
				case *ast.IndexExpr:
					set := loopKeys[lt]
					if from, ok := loopLocal[lt]; ok {
						for k, v := range local[from.name] {
							if !from.keep {
								v = from.kind
							}
							set.add(k, v)
						}
					}
					if k, ok := r.key(lt.Index); ok && rhs != nil {
						set = keyKinds{k: kindOf(rhs)}
					}
					if isMetadataField(lt.X) {
						for k, v := range set {
							ff.sinks[k] = true
							ff.kinds.add(k, v)
						}
					}
					hold(identName(lt.X), set)
					if mi, ok := ff.params[identName(lt.X)]; ok {
						if ki, ok := ff.params[identName(lt.Index)]; ok {
							vi, isParam := ff.params[identName(rhs)]
							if !isParam {
								vi = -1
							}
							ff.keyParams[[3]int{mi, ki, vi}] = true
						}
					}
				}
			}
		case *ast.ValueSpec:
			for i, n := range x.Names {
				if i < len(x.Values) {
					hold(n.Name, exprKinds(x.Values[i]))
				}
			}
		case *ast.CompositeLit:
			if isPatchType(x.Type) && !rangeLits[x] {
				for k, v := range exprKinds(x) {
					ff.kinds.add(k, v)
					if isMetadataPatch(x.Type) { // a MetadataPatch is only ever a row's
						ff.sinks[k] = true
					}
				}
			}
		case *ast.CallExpr:
			ff.calls[r.callKey(x)] = true
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && identName(sel.X) == "maps" && sel.Sel.Name == "Copy" && len(x.Args) == 2 {
				copied := exprKinds(x.Args[1])
				hold(identName(x.Args[0]), copied)
				if isMetadataField(x.Args[0]) {
					for k, v := range copied {
						ff.sinks[k] = true
						ff.kinds.add(k, v)
					}
				}
			}
			name := ""
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				name = sel.Sel.Name
			} else {
				name = identName(x.Fun)
			}
			if i, ok := keyWriters[name]; ok && len(x.Args) >= i+2 {
				if k, ok := r.key(x.Args[i]); ok {
					ff.kinds.add(k, kindOf(x.Args[len(x.Args)-1]))
					ff.sinks[k] = true
				}
			}
			if callee := c.byCall[r.callKey(x)]; callee != nil {
				for i, kinds := range callee.paramKinds {
					if i < len(x.Args) {
						hold(identName(x.Args[i]), kinds)
						if isMetadataField(x.Args[i]) {
							for k, v := range kinds {
								ff.sinks[k] = true
								ff.kinds.add(k, v)
							}
						}
					}
				}
				for kp := range callee.keyParams {
					if kp[0] >= len(x.Args) || kp[1] >= len(x.Args) {
						continue
					}
					k, ok := r.key(x.Args[kp[1]])
					if !ok {
						continue
					}
					kind := kWrite
					if kp[2] >= 0 && kp[2] < len(x.Args) {
						kind = kindOf(x.Args[kp[2]])
					}
					hold(identName(x.Args[kp[0]]), keyKinds{k: kind})
					if isMetadataField(x.Args[kp[0]]) {
						ff.sinks[k] = true
						ff.kinds.add(k, kind)
					}
				}
			}
		case *ast.IndexExpr:
			if k, ok := r.key(x.Index); ok && !lhs[x] {
				ff.reads[k] = true
			}
		case *ast.Ident:
			if c.byCall[r.dir+":"+x.Name] != nil { // a function used as a value
				ff.calls[r.dir+":"+x.Name] = true
			}
		case *ast.SelectorExpr:
			for _, k := range fieldKeys[x.Sel.Name] {
				ff.reads[k] = true
			}
			if pkg, ok := x.X.(*ast.Ident); ok && r.imports[pkg.Name] != "" { // a function used as a value
				ff.calls[r.imports[pkg.Name]+":"+x.Sel.Name] = true
			}
		}
		if e, ok := n.(ast.Expr); ok {
			if k, ok := r.key(e); ok && metadataKeyShape.MatchString(k) {
				ff.direct[k] = true
			}
		}
		return true
	})
	for name, i := range ff.params {
		if len(local[name]) > 0 {
			ff.paramKinds[i] = local[name]
		}
	}
	// Pass 2: which maps reach a store write, an effect's patch, or a return.
	sink := func(e ast.Expr) {
		for k, v := range exprKinds(e) {
			ff.sinks[k] = true
			ff.kinds.add(k, v)
		}
		if i, ok := ff.params[identName(e)]; ok {
			ff.sinkParams[i] = true
		}
	}
	var walk func(n ast.Node, ft *ast.FuncType, top bool)
	walk = func(n ast.Node, ft *ast.FuncType, top bool) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				walk(x.Body, x.Type, false)
				return false
			case *ast.ReturnStmt:
				for _, e := range x.Results {
					switch {
					case !top && resultIs(ft, isMetadataPatch): // an UpdateMetadataFenced decide
						sink(e)
					case top && resultIs(ft, isPatchType):
						for k, v := range exprKinds(e) {
							ff.ret.add(k, v)
						}
					}
				}
			case *ast.CallExpr:
				name := ""
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					name = sel.Sel.Name
				}
				if batchWriters[name] {
					for _, a := range x.Args {
						sink(a)
					}
				}
				if callee := c.byCall[r.callKey(x)]; callee != nil {
					for i := range callee.sinkParams {
						if i < len(x.Args) {
							sink(x.Args[i])
						}
					}
				}
			case *ast.CompositeLit:
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok && sinkFields[identName(kv.Key)][typeName(x.Type)] {
						sink(kv.Value)
					}
				}
			case *ast.AssignStmt:
				for i, l := range x.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "Metadata" && len(x.Rhs) == len(x.Lhs) {
						sink(x.Rhs[i])
					}
				}
			}
			return true
		})
	}
	walk(body, ff.decl.Type, true)
	for k, v := range ff.ret {
		ff.kinds.add(k, v)
	}
	return fmt.Sprint(ff.ret, ff.sinkParams, ff.paramKinds, ff.keyParams) != before
}

// loopFrom is a loop over a local map: its name, whether the body writes the
// map's own values (keeping their kinds), else the kind of what it writes.
type loopFrom struct {
	name string
	keep bool
	kind uint8
}

// codecFieldKeys maps each Info field to the keys whose info_codec.go setter
// assigns it (i.F = ...), so a read through the projection reads its key.
func codecFieldKeys(f *ast.File, r keyResolver) map[string][]string {
	out := map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || len(cl.Elts) != 2 {
			return true
		}
		k, ok := r.key(cl.Elts[0])
		fl, isFn := cl.Elts[1].(*ast.FuncLit)
		if !ok || !isFn {
			return true
		}
		ast.Inspect(fl.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok {
				for _, l := range as.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok {
						out[sel.Sel.Name] = append(out[sel.Sel.Name], k)
					}
				}
			}
			return true
		})
		return false
	})
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	censusOnce *census
	censusErr  error
)

func scannedCensus(t *testing.T) *census {
	t.Helper()
	if censusOnce == nil && censusErr == nil {
		censusOnce, censusErr = scanCensus(repoRoot(t))
	}
	if censusErr != nil {
		t.Fatalf("scanning: %v", censusErr)
	}
	if len(censusOnce.funcs) < 1000 || len(censusOnce.universe) < 100 {
		t.Fatalf("scanned %d functions and %d written keys; was a census dir moved?", len(censusOnce.funcs), len(censusOnce.universe))
	}
	return censusOnce
}

// TestSessionFieldScanReadsTheTree pins keys each shape finds on the real tree:
// the ones a literal-only scan missed (written through a helper's map
// parameter, a (map, key) helper, a builder's return, a bead's Metadata, or
// in internal/worker).
func TestSessionFieldScanReadsTheTree(t *testing.T) {
	c := scannedCensus(t)
	for _, k := range strings.Fields(`state wake_request drain_intent_reason execution_claim_hold execution_claim_nudge_work
		session_circuit_last_observed session_circuit_restarts wait_lookup_capped_at worker_profile
		worker_profile_transport_class started_config_hash requested_sleep_after_idle config_wake_suppressed opt_*`) {
		if c.universe[k] == "" {
			t.Errorf("%s: written in production, but the census does not see it", k)
		}
	}
	// reopenNamedSessionBatch merges ClearWakeBlockersPatch through a ranged local.
	if ff := c.funcs["cmd/gc/session_beads.go:reopenNamedSessionBatch"]; ff == nil || ff.ret["wake_refused_event_at"] == 0 {
		t.Errorf("reopenNamedSessionBatch: the census misses the keys it merges from a ranged local map")
	}
}

// TestSessionFieldScannerWriteShapes pins the ways production code writes a
// row's key (the nine shapes, a session CreateSpec, CreateOptions.ExtraMeta,
// and a maps.Copy merge), and that a ranged map, a query filter and a trace payload are
// not writes.
func TestSessionFieldScannerWriteShapes(t *testing.T) {
	src := `package x
import "github.com/gastownhall/gascity/internal/session"
var stopKeys = []string{"s5"}
var aliasKey = session.ConstKey2 // 13: a package-level var alias of a constant
func shapes(s store, b beads.Bead, info session.Info) {
	_ = s.SetMetadataBatch("id", map[string]string{"s1": "v"}) // 1: a literal at a store write
	patch := map[string]string{}
	patch[session.ConstKey] = "v"                             // 2: a constant index into a written local
	for _, k := range []string{"s3", "s3b"} { patch[k] = "" } // 3: a key-list loop (clears)
	for k, v := range builder() { patch[k] = v }              // 4: a builder's patch merged in
	for _, k := range stopKeys { patch[k] = "" }              // 5: a package-level key list
	_ = s.SetMetadataBatch("id", patch)
	_ = s.SetMarker("id", "s6", "")                           // 6: a per-key writer
	_, _ = s.Create(beads.Bead{Metadata: map[string]string{"s7": "v"}}) // 7: a created bead
	viaParam(s, map[string]string{"s8": "v"})                 // 8: a helper whose map parameter is written
	setIfEmpty(b.Metadata, "s9", "v")                         // 9: a (map, key) helper on a row's metadata
	stamp(b.Metadata)                                         // 9b: a helper that fills its map parameter
	_, _ = s.CreateSession(session.CreateSpec{Metadata: map[string]string{"s12": "v"}})          // 10: a session CreateSpec
	_, _ = m.CreateSession(ctx, session.CreateOptions{ExtraMeta: map[string]string{"s13": "v"}}) // 11: CreateOptions' extra metadata
	merged := map[string]string{}
	maps.Copy(merged, map[string]string{"s14": "v"}) // 12: a maps.Copy into a written map
	_ = s.SetMetadataBatch("id", merged)
	ranged := map[string]string{"s15": "v"}
	out := map[string]string{}
	for k, v := range ranged { out[k] = v } // 14: a loop over a local map
	out[aliasKey] = "v"
	_ = s.SetMetadataBatch("id", out)
	_, _ = s.CloseAll([]string{"id"}, map[string]string{"s17": "v"}) // 15: CloseAll
	out["test_"+name] = "v"                                          // 16: a computed family key
	for k, v := range map[string]string{"r": info.WakeRequest} { _, _ = k, v }
	_, _ = s.List(beads.ListQuery{Metadata: map[string]string{"q": "v"}})
	trace(map[string]string{"t": "payload"})
}
func ambiguous(a, z twin) { a.put(map[string]string{"u": "v"}); _ = z }
type twin struct{}
type other struct{}
func (twin) put(m map[string]string) { _ = s.SetMetadataBatch("id", m) }
func (other) put(m map[string]string) {}
func builder() session.MetadataPatch { return session.MetadataPatch{"s4": ""} }
func viaParam(s store, m map[string]string) { _ = s.SetMetadataBatch("id", m) }
func setIfEmpty(m map[string]string, key, value string) { m[key] = value }
func stamp(m map[string]string) { m["s10"] = "v"; setIfEmpty(m, "s11", "v") }
`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := parser.ParseFile(token.NewFileSet(), "info_codec.go", `package session
const ConstKey = "s2"
const ConstKey2 = "s16"
var infoKeyCodec = []infoKeySpec{{"wake_request", func(i *Info, v string) { i.WakeRequest = v }}}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := scanFiles([]parsedFile{{"internal/session/info_codec.go", codec}, {"cmd/gc/x.go", f}})
	ff := c.funcs["cmd/gc/x.go:shapes"]
	var clears []string
	for k, v := range ff.kinds {
		if v&kClear != 0 {
			clears = append(clears, k)
		}
	}
	sort.Strings(clears)
	if got, want := strings.Join(sortedKeys(ff.sinks), " "), "s1 s10 s11 s12 s13 s14 s15 s16 s17 s2 s3 s3b s4 s5 s6 s7 s8 s9 test_*"; got != want {
		t.Errorf("sinks %q, want %q", got, want)
	}
	if got, want := strings.Join(clears, " "), "s3 s3b s4 s5 s6"; got != want {
		t.Errorf("clears %q, want %q", got, want)
	}
	if !ff.reads["wake_request"] {
		t.Error("the Info field read of wake_request is not a read")
	}
	for _, k := range []string{"r", "q", "t", "u"} {
		if c.universe[k] != "" {
			t.Errorf("%q is in the universe: a ranged map, a query filter, a payload or an unresolved method call", k)
		}
	}
}

// nonSessionKeys are keys the census dirs write to other bead classes, named
// so a session key cannot hide among them.
var nonSessionKeys = func() map[string]string {
	out := map[string]string{}
	for class, keys := range map[string]string{
		"wait bead":                   "canceled_at commit_boundary delivery_attempt expired_at failed_at last_error nudge_id ready_at created_at created_by_session dep_ids dep_mode kind registered_epoch retried_from_wait session_id",
		"startup-health episode bead": "startup_health_alert_disposition startup_health_consecutive startup_health_first_failure_at startup_health_kind startup_health_last_detail startup_health_last_failure_at startup_health_quarantined_until startup_health_session_name",
		"work bead":                   "gc.attempt gc.ralph_step_id gc.step_id gc.routed_to gc.session_name gc.detached gc.control_pending_stalled gc.route_recovery_quarantined gc.route_recovery_quarantine_reason workflow_id gc.run_target gc.root_bead_id gc.root_store_ref gc.scope_ref gc.scope_role gc.on_fail gc.outcome gc.final_disposition gc.failure_class gc.failure_reason gc.execution_routed_to gc.dynamic_fragment gc.graphv2_root_key gc.input_convoy_id gc.finalizer_bead_id gc.root_settle_failed gc.root_settle_failed_at gc.control_dispatcher_fallback gc.control_quarantined gc.control_quarantined_at gc.control_quarantine_reason gc.controller_error gc.controller_error_class gc.controller_retryable synth_context synth_dest synth_meta_path synth_role synth_writer",
		"GitHub PR bead and trace":    "source github.base github.failed_checks github.failure_kind github.head github.head_sha github.merge_state_status github.monitor github.owner github.pending_checks github.pr github.repo github.state github.url",
		"order wisp bead":             "order_tracking_sweep order_tracking_sweep_by order_wisp_sweep",
		"rig idempotency bead":        "gc.idem.created_dir gc.idem.dolt_db gc.idem.event_cursor gc.idem.kind gc.idem.result.branch gc.idem.result.prefix gc.idem.result.rig gc.idem.state",
	} {
		for _, k := range strings.Fields(keys) {
			out[k] = class
		}
	}
	return out
}()

// TestSessionFieldUniverse backstops the write guard for paths no test runs:
// every key production code in the census dirs sends to a store write is
// registered (or another bead class's, in nonSessionKeys), and none is the
// fixture-only test_* family.
func TestSessionFieldUniverse(t *testing.T) {
	for _, f := range universeFindings(scannedCensus(t), LookupField, nonSessionKeys) {
		t.Error(f)
	}
	// Mutants: a production test_* write, an unregistered key, a stale entry.
	fake := &census{universe: map[string]string{"test_x": "cmd/gc/x.go:f", "unheard_of": "cmd/gc/x.go:g", "state": "cmd/gc/x.go:h"}}
	got := strings.Join(universeFindings(fake, LookupField, map[string]string{"gone_key": "work bead"}), "\n")
	for _, want := range []string{"test_x: cmd/gc/x.go:f writes a fixture-only", "unheard_of: cmd/gc/x.go:g writes it", "gone_key: stale nonSessionKeys entry"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}

func universeFindings(c *census, lookup func(string) (Field, bool), other map[string]string) []string {
	var out []string
	for k, writer := range c.universe {
		f, ok := lookup(k)
		switch {
		case ok && f.Family && f.Key == "test_":
			out = append(out, fmt.Sprintf("%s: %s writes a fixture-only test_* key; production code must not", k, writer))
		case !ok && other[k] == "":
			out = append(out, fmt.Sprintf("%s: %s writes it, but neither the session field registry nor nonSessionKeys has it", k, writer))
		}
	}
	for k := range other {
		if _, ok := lookup(k); ok || c.universe[k] == "" {
			out = append(out, fmt.Sprintf("%s: stale nonSessionKeys entry (registered, or no longer written)", k))
		}
	}
	sort.Strings(out)
	return out
}
