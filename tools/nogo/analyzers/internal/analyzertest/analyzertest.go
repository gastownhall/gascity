// Package analyzertest runs an analyzer over a small self-contained package
// written to a temporary directory, or over a small graph of them. Unlike
// x/tools' analysistest it needs no `go` command or GOPATH layout, so it
// works the same under `go test` and in a Bazel sandbox. A package may
// import only the graph's packages before it, not the standard library.
package analyzertest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"
)

// Diagnostic is a reported finding, positioned by file base name and line.
type Diagnostic struct {
	File    string
	Line    int
	Message string
}

// Run writes files (base name to source) into dir/<pkgPath>, type-checks them
// as one package, runs a, and returns its diagnostics sorted by position.
func Run(t *testing.T, a *analysis.Analyzer, pkgPath string, files map[string]string) []Diagnostic {
	t.Helper()
	return RunGraph(t, a, []Package{{Path: pkgPath, Files: files}})
}

// Package is one package of a graph: its path and its files, base name to
// source.
type Package struct {
	Path  string
	Files map[string]string
}

// RunGraph type-checks pkgs in order, each importing only packages before
// it, runs a on the last and on every package it needs facts from, and
// returns every package's diagnostics sorted by position.
func RunGraph(t *testing.T, a *analysis.Analyzer, pkgs []Package) []Diagnostic {
	t.Helper()
	return runGraph(t, a, pkgs, false)
}

// RunGraphExportView is RunGraph as an export-data driver (go vet, nogo)
// shows a package its imports: their exported objects only, and none of
// their imports.
func RunGraphExportView(t *testing.T, a *analysis.Analyzer, pkgs []Package) []Diagnostic {
	t.Helper()
	return runGraph(t, a, pkgs, true)
}

func runGraph(t *testing.T, a *analysis.Analyzer, pkgs []Package, exportView bool) []Diagnostic {
	t.Helper()
	if exportView {
		a = directFacts(a)
	}
	root := t.TempDir()
	fset := token.NewFileSet()
	sizes := types.SizesFor("gc", "amd64")
	checked := map[string]*packages.Package{}
	var last *packages.Package
	for _, p := range pkgs {
		last = load(t, root, fset, sizes, checked, p, exportView)
		checked[p.Path] = last
	}
	graph, err := checker.Analyze([]*analysis.Analyzer{a}, []*packages.Package{last}, nil)
	if err != nil {
		t.Fatalf("analyzing: %v", err)
	}
	var out []Diagnostic
	for act := range graph.All() {
		if act.Err != nil {
			t.Fatalf("%s: %v", act.Analyzer.Name, act.Err)
		}
		for _, d := range act.Diagnostics {
			p := fset.Position(d.Pos)
			out = append(out, Diagnostic{File: filepath.Base(p.Filename), Line: p.Line, Message: d.Message})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// load writes, parses and type-checks p against the packages checked before
// it.
func load(t *testing.T, root string, fset *token.FileSet, sizes types.Sizes, checked map[string]*packages.Package, p Package, exportView bool) *packages.Package {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(p.Path))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var syntax []*ast.File
	var names, paths []string
	for name := range p.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(p.Files[name]), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, p.Files[name], parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		syntax = append(syntax, f)
		paths = append(paths, path)
	}
	info := &types.Info{
		Types:        map[ast.Expr]types.TypeAndValue{},
		Instances:    map[*ast.Ident]types.Instance{},
		Defs:         map[*ast.Ident]types.Object{},
		Uses:         map[*ast.Ident]types.Object{},
		Implicits:    map[ast.Node]types.Object{},
		Selections:   map[*ast.SelectorExpr]*types.Selection{},
		Scopes:       map[ast.Node]*types.Scope{},
		FileVersions: map[*ast.File]string{},
	}
	imports := map[string]*packages.Package{}
	importer := importerFunc(func(path string) (*types.Package, error) {
		dep, ok := checked[path]
		if !ok {
			return nil, fmt.Errorf("%s imports %s, which is not before it", p.Path, path)
		}
		imports[path] = dep
		if exportView {
			return exported(dep.Types), nil
		}
		return dep.Types, nil
	})
	conf := types.Config{GoVersion: "go1.26", Sizes: sizes, Importer: importer}
	tpkg, err := conf.Check(p.Path, fset, syntax, info)
	if err != nil {
		t.Fatalf("type-checking %s: %v", p.Path, err)
	}
	return &packages.Package{
		ID:              p.Path,
		Name:            tpkg.Name(),
		PkgPath:         p.Path,
		GoFiles:         paths,
		CompiledGoFiles: paths,
		Fset:            fset,
		Syntax:          syntax,
		Types:           tpkg,
		TypesInfo:       info,
		TypesSizes:      sizes,
		Imports:         imports,
	}
}

// importerFunc is a function as a types.Importer.
type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// exported is p as export data shows it to an importer: its exported
// objects, and no imports.
func exported(p *types.Package) *types.Package {
	v := types.NewPackage(p.Path(), p.Name())
	for _, name := range p.Scope().Names() {
		if obj := p.Scope().Lookup(name); obj.Exported() {
			v.Scope().Insert(obj)
		}
	}
	v.MarkComplete()
	return v
}

// directFacts is a as an export-data driver runs it: a pass sees the package
// facts of its direct imports only, which is all their facts files hold.
func directFacts(a *analysis.Analyzer) *analysis.Analyzer {
	b := *a
	b.Run = func(pass *analysis.Pass) (any, error) {
		direct := map[string]bool{pass.Pkg.Path(): true}
		for _, imp := range pass.Pkg.Imports() {
			direct[imp.Path()] = true
		}
		p := *pass
		p.AllPackageFacts = func() []analysis.PackageFact {
			var out []analysis.PackageFact
			for _, f := range pass.AllPackageFacts() {
				if direct[f.Package.Path()] {
					out = append(out, f)
				}
			}
			return out
		}
		p.ImportPackageFact = func(pkg *types.Package, f analysis.Fact) bool {
			return direct[pkg.Path()] && pass.ImportPackageFact(pkg, f)
		}
		return a.Run(&p)
	}
	return &b
}
