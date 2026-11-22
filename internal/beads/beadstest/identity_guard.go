package beadstest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The store-identity guard (typed-stores T6 plan, OD-9): a class handle boxed
// into a beads.Store is never == the raw store it wraps, and the compiler
// cannot see the comparison go wrong. beads.SameStore and
// beads.PhysicalStoreKey see through the class layers; ==, a switch, the
// slices helpers and a map keyed by the store value do not. CheckStoreIdentity
// fails a package that, at a site its reviewed table does not list, compares a
// store, a class handle or a beads.MoleculeTarget with == or != (other than
// against nil or a zero handle), switches on one, finds one with
// slices.Contains or slices.Index, compares store slices with slices.Equal, or
// declares a map keyed by one.
//
// It reads the package's non-test sources with go/parser alone, as the
// repository's other source guards do, so it runs wherever the tests run. An
// operand is a store when the package's own declarations say so: a
// parameter, result, variable or struct field of a beads store type, an
// element of a map or slice of them, the result of the package's own function
// or interface method returning one, or a handle reached from any package: a
// class accessor (Work(), Graph(), ...) or a field of that name (RigWork.Work),
// the first result of Rig or GraphFor, or a .Store field. It can miss a store
// another package's function returns under another name; the review covers
// that gap, and a same-named non-store is listed with its reason.

// IdentitySite is one reviewed use the guard tolerates: how many sites of a
// rule ("eq", "switch", "slices", "map-key") one function holds, and why.
type IdentitySite struct {
	Count  int
	Reason string
}

// storeTypeNames are the beads types whose values are stores: the interface,
// the class handles and the molecule target.
var storeTypeNames = map[string]bool{
	"Store": true, "WorkStore": true, "GraphStore": true, "SessionStore": true,
	"MailStore": true, "OrdersStore": true, "NudgesStore": true, "MoleculeTarget": true,
}

// classAccessors are the names that hand out a class handle in any package:
// storeref.Stores' methods (Stores().Graph()), and the fields of
// storeref.RigWork (Rigs()[i].Work) and beadstest.ClassStores.
var classAccessors = map[string]bool{
	"Work": true, "Graph": true, "Sessions": true, "Mail": true, "Orders": true, "Nudges": true,
}

// handleLookups are storeref.Stores' methods whose first result is a handle:
// Rig(name) and GraphFor(ref).
var handleLookups = map[string]bool{"Rig": true, "GraphFor": true}

// CheckStoreIdentity scans the Go package in dir and fails t for every
// "file func rule" key whose count differs from reviewed, which is keyed the
// same way. A count above the table is a new identity site: use
// beads.SameStore or beads.PhysicalStoreKey. A count below it, or a listed key
// that is gone, is the table going stale: shrink it in the same change.
func CheckStoreIdentity(t *testing.T, dir string, reviewed map[string]IdentitySite) {
	t.Helper()
	sites, err := ScanStoreIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(sites)+len(reviewed))
	for k := range sites {
		keys = append(keys, k)
	}
	for k := range reviewed {
		if _, ok := sites[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		n, want := len(sites[k]), reviewed[k].Count
		switch {
		case n > want:
			t.Errorf("%s: %d store-identity site(s) (lines %v), %d reviewed: compare stores with beads.SameStore and key maps by beads.PhysicalStoreKey; a class handle is never == the store it wraps", k, n, sites[k], want)
		case n < want:
			t.Errorf("%s: %d store-identity site(s) (lines %v), the table lists %d: shrink the reviewed table", k, n, sites[k], want)
		}
	}
}

// ScanStoreIdentity returns the line of each store-identity site of the
// package in dir, keyed "file func rule" (the file's base name; func is
// Recv.Name for a method, or "" at package level).
func ScanStoreIdentity(dir string) (map[string][]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	s := &identityScanner{funcs: map[string]storeKind{}, fields: map[string]storeKind{}}
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files[name] = f
		s.index(f)
	}
	out := map[string][]int{}
	for name, f := range files {
		s.imports = importNames(f)
		for _, d := range f.Decls {
			s.scanDecl(fset, name, d, out)
		}
	}
	return out, nil
}

// storeKind is what an expression is known to hold.
type storeKind int

const (
	notStore  storeKind = iota
	isStore             // a store, a handle or a molecule target
	storeColl           // a map or slice whose elements are stores
)

// identityScanner holds the package-wide declarations names are typed
// against, and the current file's import names.
type identityScanner struct {
	funcs   map[string]storeKind // package functions, methods and interface methods, by their first result
	fields  map[string]storeKind // struct fields
	imports map[string]string    // the current file's import name -> path
}

// importNames maps each of f's import names to its path.
func importNames(f *ast.File) map[string]string {
	names := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = path
	}
	return names
}

func (s *identityScanner) index(f *ast.File) {
	s.imports = importNames(f)
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			s.funcs[d.Name.Name] = s.firstResult(d.Type)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				switch t := ts.Type.(type) {
				case *ast.StructType:
					for _, field := range t.Fields.List {
						for _, n := range field.Names {
							s.fields[n.Name] = s.kindOfType(field.Type)
						}
					}
				case *ast.InterfaceType:
					for _, m := range t.Methods.List {
						if ft, ok := m.Type.(*ast.FuncType); ok {
							for _, n := range m.Names {
								s.funcs[n.Name] = s.firstResult(ft)
							}
						}
					}
				}
			}
		}
	}
}

func (s *identityScanner) firstResult(ft *ast.FuncType) storeKind {
	if ft.Results == nil || len(ft.Results.List) == 0 {
		return notStore
	}
	return s.kindOfType(ft.Results.List[0].Type)
}

// isStoreType reports whether t names a beads store type.
func (s *identityScanner) isStoreType(t ast.Expr) bool {
	sel, ok := t.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && s.imports[x.Name] == "github.com/gastownhall/gascity/internal/beads" && storeTypeNames[sel.Sel.Name]
}

// kindOfType is what a value of type t holds.
func (s *identityScanner) kindOfType(t ast.Expr) storeKind {
	switch t := t.(type) {
	case *ast.MapType:
		if s.isStoreType(t.Value) {
			return storeColl
		}
	case *ast.ArrayType:
		if s.isStoreType(t.Elt) {
			return storeColl
		}
	default:
		if s.isStoreType(t) {
			return isStore
		}
	}
	return notStore
}

// scope is one function's typed names.
type scope map[string]storeKind

func (s *identityScanner) scanDecl(fset *token.FileSet, file string, d ast.Decl, out map[string][]int) {
	fn := ""
	sc := scope{}
	if fd, ok := d.(*ast.FuncDecl); ok {
		fn = fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			fn = recvTypeName(fd.Recv.List[0].Type) + "." + fn
		}
		s.declare(sc, fd.Recv)
		s.declare(sc, fd.Type.Params)
		s.declare(sc, fd.Type.Results)
		if fd.Body != nil {
			s.inferLocals(fd.Body, sc)
		}
	}
	hit := func(rule string, n ast.Node) {
		k := file + " " + fn + " " + rule
		out[k] = append(out[k], fset.Position(n.Pos()).Line)
	}
	ast.Inspect(d, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.MapType:
			if s.isStoreType(n.Key) {
				hit("map-key", n)
			}
		case *ast.BinaryExpr:
			if (n.Op == token.EQL || n.Op == token.NEQ) && !s.isNoStore(n.X) && !s.isNoStore(n.Y) &&
				(s.kindOf(n.X, sc) == isStore || s.kindOf(n.Y, sc) == isStore) {
				hit("eq", n)
			}
		case *ast.SwitchStmt:
			if n.Tag != nil && s.kindOf(n.Tag, sc) == isStore {
				hit("switch", n)
			}
		case *ast.CallExpr:
			if s.slicesIdentity(n, sc) {
				hit("slices", n)
			}
		}
		return true
	})
}

// slicesIdentity reports whether call is slices.Contains or slices.Index
// looking for a store, or slices.Equal over stores: each compares with ==.
func (s *identityScanner) slicesIdentity(call *ast.CallExpr, sc scope) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	if x, ok := sel.X.(*ast.Ident); !ok || s.imports[x.Name] != "slices" {
		return false
	}
	switch sel.Sel.Name {
	case "Contains", "Index":
		return s.kindOf(call.Args[0], sc) == storeColl || s.kindOf(call.Args[1], sc) == isStore
	case "Equal":
		return s.kindOf(call.Args[0], sc) == storeColl || s.kindOf(call.Args[1], sc) == storeColl
	}
	return false
}

func (s *identityScanner) declare(sc scope, fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, field := range fl.List {
		if k := s.kindOfType(field.Type); k != notStore {
			for _, n := range field.Names {
				sc[n.Name] = k
			}
		}
	}
}

// inferLocals types the function's local names from their declarations and
// assignments, to a fixpoint. A multi-value assignment from one expression
// types its first name only: a store-returning call's store, a map index's
// value.
func (s *identityScanner) inferLocals(body *ast.BlockStmt, sc scope) {
	assign := func(name ast.Expr, k storeKind) {
		if id, ok := name.(*ast.Ident); ok && id.Name != "_" && k != notStore {
			sc[id.Name] = k
		}
	}
	for range 4 {
		before := len(sc)
		ast.Inspect(body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				s.declare(sc, n.Type.Params)
			case *ast.ValueSpec:
				for i, name := range n.Names {
					switch {
					case n.Type != nil:
						assign(name, s.kindOfType(n.Type))
					case i < len(n.Values):
						assign(name, s.kindOf(n.Values[i], sc))
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					switch {
					case len(n.Rhs) == len(n.Lhs):
						assign(lhs, s.kindOf(n.Rhs[i], sc))
					case i == 0:
						assign(lhs, s.kindOf(n.Rhs[0], sc))
					}
				}
			case *ast.RangeStmt:
				if s.kindOf(n.X, sc) == storeColl && n.Value != nil {
					assign(n.Value, isStore)
				}
			}
			return true
		})
		if len(sc) == before {
			return
		}
	}
}

// kindOf is what expression e holds, as far as the declarations say.
func (s *identityScanner) kindOf(e ast.Expr, sc scope) storeKind {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return s.kindOf(e.X, sc)
	case *ast.Ident:
		return sc[e.Name]
	case *ast.SelectorExpr:
		if e.Sel.Name == "Store" {
			return isStore // a handle's embedded store, or a store field
		}
		k, declared := s.fields[e.Sel.Name]
		if declared && k != notStore {
			return k // the package's own store field
		}
		if classAccessors[e.Sel.Name] {
			// Another package's handle field: Rigs()[i].Work. The selector's
			// receiver is untyped here, so a field of this package with the
			// same name and no store in it (api's patch types have Work and
			// Mail) is flagged too, and listed with its reason.
			return isStore
		}
	case *ast.CallExpr:
		switch fun := e.Fun.(type) {
		case *ast.Ident:
			return s.funcs[fun.Name]
		case *ast.SelectorExpr:
			if x, ok := fun.X.(*ast.Ident); ok && s.imports[x.Name] != "" {
				return notStore // another package's function: SameStore and friends return no store
			}
			if k, declared := s.funcs[fun.Sel.Name]; declared {
				return k // the package's own method of that name wins (State.Orders lists orders)
			}
			if (classAccessors[fun.Sel.Name] && len(e.Args) == 0) || handleLookups[fun.Sel.Name] {
				return isStore
			}
		}
	case *ast.IndexExpr:
		if s.kindOf(e.X, sc) == storeColl {
			return isStore
		}
	}
	return notStore
}

// isNoStore reports whether e is nil or an empty store-typed composite
// literal (a zero handle): comparing with one asks whether a value is absent,
// not which store it is.
func (s *identityScanner) isNoStore(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return s.isNoStore(e.X)
	case *ast.Ident:
		return e.Name == "nil"
	case *ast.CompositeLit:
		return len(e.Elts) == 0 && s.isStoreType(e.Type)
	}
	return false
}

func recvTypeName(t ast.Expr) string {
	switch t := t.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}
