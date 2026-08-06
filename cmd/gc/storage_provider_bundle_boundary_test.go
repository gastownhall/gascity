package main

// The storage-provider boundary, enforced rather than described.
//
// Storage providers are compiled in, so the properties that make the seam safe
// to extend are properties of the tree — and a property of the tree that is
// only prose will be broken by a change that compiles and passes every other
// test. Each arm below closes one of those holes:
//
//   - the registry has exactly one construction site, and the compiled-factory
//     list is declared exactly once, so "which providers does this binary
//     have?" is answerable by reading one function;
//   - the registry that composition root yields is frozen and explicit: it
//     resolves nothing it was not given and accepts no late registration;
//   - every provider ID compiled into the storage surface is a built-in one,
//     so an out-of-tree provider cannot leak back here by accident;
//   - the whole storage surface compiles identically with CGO on and off, so
//     the pure-Go driver choice is a checked property rather than a comment;
//   - the module graph carries no replace directive, so a build of this repo
//     resolves the dependencies its manifest names and nothing else.
//
// The last two are what a downstream fork relies on. A fork appends its own
// factory in its own tree; these arms are what keep the seam it appends to
// honest here.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/storebinding"
)

const (
	bundleRootFile        = "cmd/gc/storage_provider_bundle.go"
	compiledFactoriesFunc = "compiledStorageProviderFactories"
	registryConstructor   = "NewProviderRegistry"
)

// sanctionedProviderIDs are the provider IDs compiled into this binary:
// "sqlite" for the SQLite graph-engine inspection surface and "sqlite-beads"
// for the bead-store provider. An out-of-tree provider registers its own ID
// from its own tree, never from this one.
var sanctionedProviderIDs = map[string]bool{"sqlite": true, "sqlite-beads": true}

// storageSurfaceDirs are the module-local directories that make up the storage
// class surface. They are the packages the CGO invariance assertion covers.
var storageSurfaceDirs = []string{
	"cmd/gc",
	"internal/storebinding",
}

// replaceDirective is one parsed `replace` line, in either the single-line or
// the parenthesized-block form.
type replaceDirective struct {
	line       int
	oldPath    string
	oldVersion string
	newPath    string
	newVersion string
}

// TestStorageProviderBundleHasOneConstructionSite proves the registry is
// constructed exactly once, in the file that owns the composition root, and
// that the compiled-factory function is declared exactly once. A duplicated or
// shadowed declaration must fail loudly, not win by build order.
func TestStorageProviderBundleHasOneConstructionSite(t *testing.T) {
	root := moduleRoot(t)
	sites := map[string]int{}
	declarations := map[string]int{}

	for _, rel := range moduleGoFiles(t, root) {
		file := parseModuleFile(t, root, rel)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == registryConstructor && filepath.Dir(rel) != "internal/storebinding" {
					sites[rel]++
				}
			case *ast.SelectorExpr:
				if fn.Sel.Name == registryConstructor {
					sites[rel]++
				}
			}
			return true
		})
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == compiledFactoriesFunc {
				declarations[rel]++
			}
		}
	}

	if got := len(sites); got != 1 || sites[bundleRootFile] != 1 {
		t.Fatalf("registry construction sites = %v, want exactly one in %s", sites, bundleRootFile)
	}
	if len(declarations) != 1 || declarations[bundleRootFile] != 1 {
		t.Fatalf("%s is declared in %v, want exactly one declaration in %s",
			compiledFactoriesFunc, declarations, bundleRootFile)
	}
}

// TestCompiledStorageProviderRegistryIsFrozenAndExplicit exercises the
// composition root itself: it yields a frozen registry that resolves nothing
// it was not given.
func TestCompiledStorageProviderRegistryIsFrozenAndExplicit(t *testing.T) {
	registry, err := newStorageProviderRegistry()
	if err != nil {
		t.Fatalf("newStorageProviderRegistry: %v", err)
	}
	if registry == nil {
		t.Fatal("newStorageProviderRegistry returned no registry")
	}
	if _, err := registry.Lookup(storebinding.ProviderID("not-compiled-in")); err == nil {
		t.Fatal("the frozen registry resolved a provider that was never registered")
	}
	// The late registration offers a WELL-FORMED factory. A nil one is rejected
	// by an unfrozen registry too, so refusing it proves nothing about the
	// freeze; only ErrProviderRegistryFrozen does.
	err = registry.Register(lateProviderFactory{id: "late-registered"})
	if !errors.Is(err, storebinding.ErrProviderRegistryFrozen) {
		t.Fatalf("late registration of a well-formed factory = %v, want %v; the registry is not frozen before config resolution",
			err, storebinding.ErrProviderRegistryFrozen)
	}
	if _, err := registry.Lookup(storebinding.ProviderID("late-registered")); err == nil {
		t.Fatal("the frozen registry resolves a factory it refused to register")
	}
}

// lateProviderFactory is a valid factory that no binary compiles in. It exists
// so the freeze arm can be refused for being late rather than for being
// malformed.
type lateProviderFactory struct{ id storebinding.ProviderID }

func (f lateProviderFactory) ID() storebinding.ProviderID { return f.id }

func (f lateProviderFactory) New(storebinding.BindingSpec) (storebinding.Provider, error) {
	return nil, errors.New("late-registered provider is never constructed")
}

// TestStorageSurfaceDeclaresOnlySanctionedProviderIDs proves no file compiles
// in a provider ID beyond the built-ins. An out-of-tree ID added here would
// compile and ship today.
func TestStorageSurfaceDeclaresOnlySanctionedProviderIDs(t *testing.T) {
	root := moduleRoot(t)
	found, findings := scanProviderIDs(t, root, moduleGoFiles(t, root))
	if len(found) == 0 {
		t.Fatal("the provider-ID scan found no literal provider ID; its subject set is empty")
	}
	for _, finding := range findings {
		t.Error(finding)
	}
}

// TestStorageSurfaceCompilesIdenticallyWithAndWithoutCGO proves the storage
// class surface is pure Go: the same files are selected with CGO enabled and
// disabled, and no package in the surface has a cgo file or reaches for a cgo
// SQLite driver. The pure-Go driver (modernc.org/sqlite) is what lets a
// CGO_ENABLED=0 binary open a SQLite binding at all, and a comment saying so
// is not a check.
func TestStorageSurfaceCompilesIdenticallyWithAndWithoutCGO(t *testing.T) {
	root := moduleRoot(t)
	dirs := storageSurfacePackageDirs(t, root)
	if len(dirs) < len(storageSurfaceDirs) {
		t.Fatalf("the storage surface walk found %d package directories, want at least %d", len(dirs), len(storageSurfaceDirs))
	}

	for _, dir := range dirs {
		enabled := importStorageDir(t, root, dir, true)
		disabled := importStorageDir(t, root, dir, false)
		if len(enabled.CgoFiles) != 0 {
			t.Errorf("%s has cgo files %v; a CGO_ENABLED=0 build could not compile the storage surface", dir, enabled.CgoFiles)
		}
		if !reflect.DeepEqual(sorted(enabled.GoFiles), sorted(disabled.GoFiles)) {
			t.Errorf("%s selects %v with CGO enabled and %v with it disabled; the surface is not CGO-invariant",
				dir, sorted(enabled.GoFiles), sorted(disabled.GoFiles))
		}
		for _, imported := range enabled.Imports {
			if imported == "github.com/mattn/go-sqlite3" {
				t.Errorf("%s imports the cgo SQLite driver; a SQLite binding opens through the pure-Go driver", dir)
			}
		}
	}
}

// TestModuleGraphCarriesNoReplaceDirective is the module-graph guarantee a
// downstream fork builds on: this repo's dependencies are exactly what its
// manifest names, at released versions, with nothing redirected. It is the
// tree-side companion to scripts/check-gomod-replace.sh's released-semver-only
// policy — that script gates what a change adds, this arm gates the result.
//
// A replace this parser cannot read is a violation, not a pass: silently
// ignoring a line we cannot parse is how a guard goes blind.
func TestModuleGraphCarriesNoReplaceDirective(t *testing.T) {
	root := moduleRoot(t)
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	directives, malformed := replaceDirectives(string(goMod))
	if len(malformed) > 0 {
		t.Fatalf("go.mod has replace directives this guard cannot parse (lines %v); a manifest we cannot read is a violation, not a pass", malformed)
	}
	for _, directive := range directives {
		t.Errorf("go.mod line %d replaces %q with %q; this module graph carries no replace directive, so a build resolves the dependencies the manifest names and nothing else",
			directive.line, directive.oldPath, directive.newPath)
	}
	if anyGoWorkFile(t, root) {
		t.Error("the tree commits a go.work; a workspace redirects the module graph for every go invocation started at or below it")
	}
}

// --- the arms, as functions over an arbitrary tree ---------------------------

// scanProviderIDs reports every literal provider ID compiled into the tree and
// a finding for each that is not built in.
func scanProviderIDs(t *testing.T, root string, files []string) (map[string]string, []string) {
	t.Helper()
	found := map[string]string{}
	for _, rel := range files {
		file := parseModuleFile(t, root, rel)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			if !namesProviderIDConversion(call.Fun) {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				// A conversion of a variable carries no compiled-in identity.
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			found[value] = rel
			return true
		})
	}
	var findings []string
	for _, id := range sortedKeys(toSet(found)) {
		if !sanctionedProviderIDs[id] {
			findings = append(findings, fmt.Sprintf("%s compiles in the provider ID %q; only the built-in IDs %v ship here, and an out-of-tree provider registers its own ID from its own tree",
				found[id], id, sortedKeys(sanctionedProviderIDs)))
		}
	}
	return found, findings
}

// --- manifest parsing --------------------------------------------------------

// replaceDirectives parses every replace directive in a go.mod, in both the
// single-line and parenthesized-block forms, and reports the line numbers of
// any it could not parse.
func replaceDirectives(goMod string) ([]replaceDirective, []int) {
	var directives []replaceDirective
	var malformed []int
	inBlock := false

	for index, raw := range strings.Split(goMod, "\n") {
		number := index + 1
		line := strings.TrimSpace(stripLineComment(raw))
		if line == "" {
			continue
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			directive, ok := parseReplaceBody(line, number)
			if !ok {
				malformed = append(malformed, number)
				continue
			}
			directives = append(directives, directive)
			continue
		}
		fields := strings.Fields(line)
		switch {
		case fields[0] == "replace", fields[0] == "replace(":
			// Parsed below.
		case strings.HasPrefix(fields[0], "replace("):
			// The replace verb glued to its body. Go itself rejects this, so
			// such a tree cannot build — but treating an unreadable replace as
			// "not a replace" is the default that lets a quoted form through,
			// so report it rather than skip it.
			malformed = append(malformed, number)
			continue
		default:
			// "replacement" or any other directive is not this one.
			continue
		}
		rest := strings.TrimSpace(line[len("replace"):])
		if rest == "(" {
			inBlock = true
			continue
		}
		directive, ok := parseReplaceBody(rest, number)
		if !ok {
			malformed = append(malformed, number)
			continue
		}
		directives = append(directives, directive)
	}
	if inBlock {
		// An unterminated block means the rest of the file was never read as
		// directives. Report it as unparseable rather than trusting the
		// prefix we did read.
		malformed = append(malformed, 0)
	}
	return directives, malformed
}

// parseReplaceBody parses "old [version] => new [version]".
func parseReplaceBody(body string, number int) (replaceDirective, bool) {
	if strings.ContainsAny(body, "()") {
		// No module path or version contains a parenthesis, so this is a
		// block delimiter we did not expect. Fail closed.
		return replaceDirective{}, false
	}
	sides := strings.Split(body, "=>")
	if len(sides) != 2 {
		return replaceDirective{}, false
	}
	left := strings.Fields(sides[0])
	right := strings.Fields(sides[1])
	if len(left) < 1 || len(left) > 2 || len(right) < 1 || len(right) > 2 {
		return replaceDirective{}, false
	}
	fields := slices.Concat(left, right)
	tokens := make([]string, len(fields))
	for index, field := range fields {
		token, ok := unquoteModToken(field)
		if !ok {
			return replaceDirective{}, false
		}
		tokens[index] = token
	}
	directive := replaceDirective{line: number, oldPath: tokens[0], newPath: tokens[len(left)]}
	if len(left) == 2 {
		directive.oldVersion = tokens[1]
	}
	if len(right) == 2 {
		directive.newVersion = tokens[len(tokens)-1]
	}
	return directive, true
}

// unquoteModToken resolves a go.mod token to the path or version Go itself
// sees. Go's module lexer accepts a quoted module path, so
// `replace "example.com/mod" => ./elsewhere` is a live replacement — but a
// parser that compares the raw token sees a replacement of some other,
// quote-named module and reports the manifest as replace-free. Comparing
// against Go's semantics rather than against the bytes is what closes that.
func unquoteModToken(token string) (string, bool) {
	if !strings.ContainsAny(token, "\"`") {
		return token, true
	}
	if !strings.HasPrefix(token, `"`) {
		// A backquote, or a quote anywhere but the front, is not a spelling Go
		// accepts. Fail closed rather than guess where the path ends.
		return "", false
	}
	unquoted, err := strconv.Unquote(token)
	if err != nil {
		return "", false
	}
	return unquoted, true
}

func stripLineComment(line string) string {
	if index := strings.Index(line, "//"); index >= 0 {
		return line[:index]
	}
	return line
}

// anyGoWorkFile reports whether the tree commits a go.work anywhere, not just
// at the module root. Go resolves a workspace by walking up from the working
// directory, so a go.work in any subdirectory redirects the module graph for
// every go invocation started at or below it — a committed tree fact, and so
// in scope for a guard that keys on committed tree facts. Ambient GOWORK and
// -modfile redirections are not tree facts and stay out of scope.
func anyGoWorkFile(t *testing.T, root string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "testdata", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "go.work" || entry.Name() == "go.work.sum" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module for go.work files: %v", err)
	}
	return found
}

// --- shared helpers ----------------------------------------------------------

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory; the boundary scan has no module root")
		}
		dir = parent
	}
}

// moduleGoFiles lists every non-test Go file in the module, module-relative.
func moduleGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "testdata", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	sort.Strings(files)
	return files
}

func parseModuleFile(t *testing.T, root, rel string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", rel, err)
	}
	return file
}

// storageSurfacePackageDirs lists every module-relative package directory in
// the storage surface.
func storageSurfacePackageDirs(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, prefix := range storageSurfaceDirs {
		err := filepath.WalkDir(filepath.Join(root, prefix), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			rel, relErr := filepath.Rel(root, filepath.Dir(path))
			if relErr != nil {
				return relErr
			}
			seen[filepath.ToSlash(rel)] = true
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", prefix, err)
		}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

func importStorageDir(t *testing.T, root, dir string, cgo bool) *build.Package {
	t.Helper()
	context := build.Default
	context.CgoEnabled = cgo
	pkg, err := context.ImportDir(filepath.Join(root, dir), 0)
	if err != nil {
		t.Fatalf("resolving %s with CgoEnabled=%t: %v", dir, cgo, err)
	}
	return pkg
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// namesProviderIDConversion reports whether expr is the ProviderID type in
// either the storage package or a consumer that imported it under any name.
func namesProviderIDConversion(expr ast.Expr) bool {
	switch fn := expr.(type) {
	case *ast.Ident:
		return fn.Name == "ProviderID"
	case *ast.SelectorExpr:
		return fn.Sel.Name == "ProviderID"
	}
	return false
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func toSet(values map[string]string) map[string]bool {
	set := make(map[string]bool, len(values))
	for key := range values {
		set[key] = true
	}
	return set
}
