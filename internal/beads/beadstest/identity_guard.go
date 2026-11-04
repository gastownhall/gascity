package beadstest

import (
	"fmt"
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
// beads.PhysicalStoreKey see through the class layers; == and a map keyed by
// the store value do not. CheckStoreIdentity fails a package that compares a
// store, a class handle or a beads.MoleculeTarget with == or != (other than
// against nil), or declares a map keyed by one, at a site its reviewed table
// does not list.
//
// It reads the package's non-test sources with go/parser alone, as the
// repository's other source guards do, so it runs wherever the tests run. The
// operand types are inferred from the declarations it can see: parameters,
// results, variables, struct fields, range variables and the results of the
// package's own functions and interface methods, each typed as a store, a map
// or slice of stores, or a function returning one. That is an approximation:
// it can miss a store reached through another package's function, and it can
// flag a same-named non-store. A miss is a gap the review covers; a false hit
// is listed with its reason.

// IdentitySite is one reviewed use the guard tolerates: how many identity
// comparisons (rule "eq") or store-keyed map types (rule "map-key") one
// function holds, and why.
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
	got := make(map[string]int, len(sites))
	for k, lines := range sites {
		got[k] = len(lines)
	}
	keys := make(map[string]bool, len(got)+len(reviewed))
	for k := range got {
		keys[k] = true
	}
	for k := range reviewed {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		n, want := got[k], reviewed[k].Count
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
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		names = append(names, name)
	}
	s := newIdentityScanner()
	for _, f := range files {
		s.indexPackage(f)
	}
	out := map[string][]int{}
	for i, f := range files {
		for _, d := range f.Decls {
			s.scanDecl(fset, names[i], f, d, out)
		}
	}
	return out, nil
}

// storeKind is what an expression is known to hold.
type storeKind int

const (
	notStore   storeKind = iota
	isStore              // a store, a handle or a molecule target
	storeColl            // a map or slice whose elements are stores
	storeFunc            // a function value returning a store
	storeCollF           // a function value returning a collection of stores
)

// identityScanner holds the package-wide declarations the per-function scan
// types names against.
type identityScanner struct {
	beadsNames map[*ast.File]map[string]bool // the names each file imports internal/beads under
	funcs      map[string][]storeKind        // package funcs, methods and interface methods by name: their results
	fields     map[string]storeKind          // struct field names by kind
	pkgVars    map[string]storeKind
	file       *ast.File
}

func newIdentityScanner() *identityScanner {
	return &identityScanner{
		beadsNames: map[*ast.File]map[string]bool{},
		funcs:      map[string][]storeKind{},
		fields:     map[string]storeKind{},
		pkgVars:    map[string]storeKind{},
	}
}

func (s *identityScanner) indexPackage(f *ast.File) {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != "github.com/gastownhall/gascity/internal/beads" {
			continue
		}
		if imp.Name != nil {
			names[imp.Name.Name] = true
		} else {
			names["beads"] = true
		}
	}
	if f.Name.Name == "beads" {
		names[""] = true // the package's own unqualified names
	}
	s.beadsNames[f] = names
	s.file = f
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			s.funcs[d.Name.Name] = s.resultKinds(d.Type)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					s.indexType(spec.Type)
				case *ast.ValueSpec:
					k := s.kindOfType(spec.Type)
					for _, n := range spec.Names {
						if k != notStore {
							s.pkgVars[n.Name] = k
						}
					}
				}
			}
		}
	}
}

func (s *identityScanner) indexType(t ast.Expr) {
	switch t := t.(type) {
	case *ast.StructType:
		for _, field := range t.Fields.List {
			if k := s.kindOfType(field.Type); k != notStore {
				for _, n := range field.Names {
					s.fields[n.Name] = k
				}
			}
		}
	case *ast.InterfaceType:
		for _, m := range t.Methods.List {
			if ft, ok := m.Type.(*ast.FuncType); ok {
				for _, n := range m.Names {
					s.funcs[n.Name] = s.resultKinds(ft)
				}
			}
		}
	}
}

// resultKinds is the kind of each result of a function type.
func (s *identityScanner) resultKinds(ft *ast.FuncType) []storeKind {
	if ft.Results == nil {
		return nil
	}
	var out []storeKind
	for _, r := range ft.Results.List {
		k := s.kindOfType(r.Type)
		n := max(len(r.Names), 1)
		for range n {
			out = append(out, k)
		}
	}
	return out
}

// isStoreType reports whether t names a beads store type.
func (s *identityScanner) isStoreType(t ast.Expr) bool {
	names := s.beadsNames[s.file]
	switch t := t.(type) {
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		return ok && names[x.Name] && storeTypeNames[t.Sel.Name]
	case *ast.Ident:
		return names[""] && storeTypeNames[t.Name]
	case *ast.ParenExpr:
		return s.isStoreType(t.X)
	}
	return false
}

// kindOfType is what a value of type t holds.
func (s *identityScanner) kindOfType(t ast.Expr) storeKind {
	switch t := t.(type) {
	case nil:
		return notStore
	case *ast.MapType:
		if s.isStoreType(t.Value) {
			return storeColl
		}
	case *ast.ArrayType:
		if s.isStoreType(t.Elt) {
			return storeColl
		}
	case *ast.FuncType:
		switch r := s.resultKinds(t); {
		case len(r) > 0 && r[0] == isStore:
			return storeFunc
		case len(r) > 0 && r[0] == storeColl:
			return storeCollF
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

func (s *identityScanner) scanDecl(fset *token.FileSet, file string, f *ast.File, d ast.Decl, out map[string][]int) {
	s.file = f
	fn := ""
	sc := scope{}
	switch d := d.(type) {
	case *ast.FuncDecl:
		fn = d.Name.Name
		if d.Recv != nil && len(d.Recv.List) > 0 {
			fn = recvTypeName(d.Recv.List[0].Type) + "." + fn
			s.declareFields(sc, d.Recv)
		}
		s.declareFields(sc, d.Type.Params)
		s.declareFields(sc, d.Type.Results)
		if d.Body != nil {
			s.inferLocals(d.Body, sc)
		}
	case *ast.GenDecl:
		// Package-level declarations: only map-key types can hit here.
	}
	ast.Inspect(d, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			s.declareFields(sc, n.Type.Params)
		case *ast.MapType:
			if s.isStoreType(n.Key) {
				k := file + " " + fn + " map-key"
				out[k] = append(out[k], fset.Position(n.Pos()).Line)
			}
		case *ast.BinaryExpr:
			if n.Op != token.EQL && n.Op != token.NEQ {
				return true
			}
			if s.isNoStore(n.X) || s.isNoStore(n.Y) {
				return true // a nil or zero-handle check, not identity
			}
			if s.kindOf(n.X, sc) == isStore || s.kindOf(n.Y, sc) == isStore {
				k := file + " " + fn + " eq"
				out[k] = append(out[k], fset.Position(n.Pos()).Line)
			}
		}
		return true
	})
}

func (s *identityScanner) declareFields(sc scope, fl *ast.FieldList) {
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
// assignments, to a fixpoint (an assignment can depend on a later-typed name
// only through a loop, so a few passes suffice).
func (s *identityScanner) inferLocals(body *ast.BlockStmt, sc scope) {
	for range 4 {
		before := len(sc)
		ast.Inspect(body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				s.declareFields(sc, n.Type.Params)
			case *ast.ValueSpec:
				k := s.kindOfType(n.Type)
				for i, name := range n.Names {
					switch {
					case k != notStore:
						sc[name.Name] = k
					case i < len(n.Values):
						s.assign(sc, name, n.Values[i])
					case len(n.Values) == 1:
						s.assignResult(sc, name, n.Values[0], i)
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					name, ok := lhs.(*ast.Ident)
					if !ok || name.Name == "_" {
						continue
					}
					if len(n.Rhs) == len(n.Lhs) {
						s.assign(sc, name, n.Rhs[i])
					} else if len(n.Rhs) == 1 {
						s.assignResult(sc, name, n.Rhs[0], i)
					}
				}
			case *ast.RangeStmt:
				if s.kindOf(n.X, sc) == storeColl {
					if v, ok := n.Value.(*ast.Ident); ok && v.Name != "_" {
						sc[v.Name] = isStore
					}
				}
			case *ast.TypeSwitchStmt:
				// x := y.(type) keeps y's kind only in a store case; leave it.
			}
			return true
		})
		if len(sc) == before {
			return
		}
	}
}

func (s *identityScanner) assign(sc scope, name *ast.Ident, rhs ast.Expr) {
	if k := s.kindOf(rhs, sc); k != notStore {
		sc[name.Name] = k
	}
}

// assignResult types name from result i of a multi-value rhs: a call, or the
// value of a comma-ok map index.
func (s *identityScanner) assignResult(sc scope, name *ast.Ident, rhs ast.Expr, i int) {
	switch rhs := rhs.(type) {
	case *ast.CallExpr:
		if r := s.callResults(rhs, sc); i < len(r) && r[i] != notStore {
			sc[name.Name] = r[i]
		}
	case *ast.IndexExpr:
		if i == 0 && s.kindOf(rhs.X, sc) == storeColl {
			sc[name.Name] = isStore
		}
	case *ast.TypeAssertExpr:
		if i == 0 && s.isStoreType(rhs.Type) {
			sc[name.Name] = isStore
		}
	}
}

// callResults is the result kinds of a call to a package function, method or
// interface method, or of a store-returning function value.
func (s *identityScanner) callResults(call *ast.CallExpr, sc scope) []storeKind {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if r, ok := s.funcs[fun.Name]; ok {
			return r
		}
		switch sc[fun.Name] {
		case storeFunc:
			return []storeKind{isStore}
		case storeCollF:
			return []storeKind{storeColl}
		}
	case *ast.SelectorExpr:
		if x, ok := fun.X.(*ast.Ident); ok && s.beadsNames[s.file][x.Name] {
			return nil // a beads function: SameStore and friends return no store
		}
		if r, ok := s.funcs[fun.Sel.Name]; ok {
			return r
		}
		switch s.fields[fun.Sel.Name] {
		case storeFunc:
			return []storeKind{isStore}
		case storeCollF:
			return []storeKind{storeColl}
		}
	}
	return nil
}

// kindOf is what expression e holds, as far as the declarations say.
func (s *identityScanner) kindOf(e ast.Expr, sc scope) storeKind {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return s.kindOf(e.X, sc)
	case *ast.Ident:
		if k, ok := sc[e.Name]; ok {
			return k
		}
		return s.pkgVars[e.Name]
	case *ast.SelectorExpr:
		if e.Sel.Name == "Store" && s.kindOf(e.X, sc) == isStore {
			return isStore // a handle's embedded store
		}
		if x, ok := e.X.(*ast.Ident); ok && s.beadsNames[s.file][x.Name] {
			return notStore
		}
		return s.fields[e.Sel.Name]
	case *ast.CallExpr:
		if r := s.callResults(e, sc); len(r) > 0 {
			return r[0]
		}
		if len(e.Args) == 1 && s.isStoreType(e.Fun) {
			return isStore // a conversion to a store type
		}
	case *ast.IndexExpr:
		if s.kindOf(e.X, sc) == storeColl {
			return isStore
		}
	case *ast.CompositeLit:
		if s.isStoreType(e.Type) {
			return isStore
		}
		return s.kindOfType(e.Type)
	case *ast.TypeAssertExpr:
		if s.isStoreType(e.Type) {
			return isStore
		}
	case *ast.StarExpr:
		return s.kindOf(e.X, sc)
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
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return fmt.Sprintf("%T", t)
}
