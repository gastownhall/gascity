package testenv_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testenv"
)

const (
	importPath = "github.com/gastownhall/gascity/internal/testenv"
	importFile = "testenv_import_test.go"
)

// TestRequiresDedicatedTestenvImportFile walks every test directory in the repo
// and fails unless it contains an untagged testenv_import_test.go file with the
// canonical blank import of internal/testenv. Parking the blank import in an
// arbitrary existing test file is brittle: build-tagged files can satisfy a
// directory-level lint while still being excluded from the default test binary.
func TestRequiresDedicatedTestenvImportFile(t *testing.T) {
	root := repoRoot(t)
	type dirInfo struct {
		packages      map[string]bool
		hasRealTests  bool
		canonicalFile string
	}
	dirInfos := map[string]*dirInfo{}
	var strayImports []string

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipRepoLintDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip the testenv package itself — it cannot import itself.
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		if rel == "internal/testenv" {
			return nil
		}
		info := dirInfos[rel]
		if info == nil {
			info = &dirInfo{packages: map[string]bool{}}
			dirInfos[rel] = info
		}
		if filepath.Base(path) == importFile {
			info.canonicalFile = path
			return nil
		}
		info.hasRealTests = true
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		info.packages[file.Name.Name] = true
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) == importPath {
				strayImports = append(strayImports, rel+"/"+filepath.Base(path))
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	var missing []string
	var malformed []string
	var orphaned []string
	for dir, info := range dirInfos {
		if !info.hasRealTests {
			if info.canonicalFile != "" {
				orphaned = append(orphaned, dir)
			}
			continue
		}
		path := filepath.Join(root, dir, importFile)
		err := validateImportFile(path, preferredPackage(info.packages))
		switch {
		case err == nil:
			continue
		case os.IsNotExist(err):
			missing = append(missing, dir)
		default:
			malformed = append(malformed, dir+": "+err.Error())
		}
	}
	sort.Strings(missing)
	sort.Strings(malformed)
	sort.Strings(orphaned)
	sort.Strings(strayImports)

	if len(missing) > 0 || len(malformed) > 0 || len(orphaned) > 0 || len(strayImports) > 0 {
		var b strings.Builder
		if len(missing) > 0 {
			b.WriteString("test directories missing ")
			b.WriteString(importFile)
			b.WriteString(" (")
			b.WriteString(strconv.Itoa(len(missing)))
			b.WriteString("):\n  ")
			b.WriteString(strings.Join(missing, "\n  "))
			b.WriteString("\n\n")
		}
		if len(malformed) > 0 {
			b.WriteString("malformed ")
			b.WriteString(importFile)
			b.WriteString(" files (")
			b.WriteString(strconv.Itoa(len(malformed)))
			b.WriteString("):\n  ")
			b.WriteString(strings.Join(malformed, "\n  "))
			b.WriteString("\n\n")
		}
		if len(orphaned) > 0 {
			b.WriteString("orphaned ")
			b.WriteString(importFile)
			b.WriteString(" files without other tests (")
			b.WriteString(strconv.Itoa(len(orphaned)))
			b.WriteString("):\n  ")
			b.WriteString(strings.Join(orphaned, "\n  "))
			b.WriteString("\n\n")
		}
		if len(strayImports) > 0 {
			b.WriteString("non-canonical test files importing internal/testenv (")
			b.WriteString(strconv.Itoa(len(strayImports)))
			b.WriteString("):\n  ")
			b.WriteString(strings.Join(strayImports, "\n  "))
			b.WriteString("\n\n")
		}
		b.WriteString("Every real test directory must contain an untagged ")
		b.WriteString(importFile)
		b.WriteString(" file with:\n\n")
		b.WriteString("    import _ ")
		b.WriteString("\"")
		b.WriteString(importPath)
		b.WriteString("\"\n\n")
		b.WriteString("This guarantees GC_* env vars are scrubbed before tests run,\n")
		b.WriteString("so a leak from an agent session cannot corrupt a live city.\n")
		b.WriteString("Run `go run scripts/add-testenv-import.go` to generate the canonical files,\n")
		b.WriteString("scrub legacy imports, and remove stale stubs.")
		t.Fatal(b.String())
	}
}

// TestNoLeakVectorReadsAtPackageInit blocks direct `go test` regressions where
// production code reads a leak-vector GC_* env var during package init or top-
// level var initialization before internal/testenv has a chance to scrub it.
// Runtime reads are fine; init-time reads are not.
func TestNoLeakVectorReadsAtPackageInit(t *testing.T) {
	root := repoRoot(t)
	leakVars := map[string]bool{}
	for _, name := range testenv.LeakVectorVars {
		leakVars[name] = true
	}
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipRepoLintDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					valueSpec, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, value := range valueSpec.Values {
						findLeakVectorGetenv(fset, value, leakVars, rel, &offenders)
					}
				}
			case *ast.FuncDecl:
				if d.Name == nil || d.Name.Name != "init" || d.Body == nil {
					continue
				}
				findLeakVectorGetenv(fset, d.Body, leakVars, rel, &offenders)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan init-time GC_* reads: %v", err)
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("production code must not read leak-vector GC_* vars during init or top-level var init:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func skipRepoLintDir(name string) bool {
	if name == "vendor" || name == "node_modules" {
		return true
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	return name == "worktrees" || strings.HasPrefix(name, "worktree-")
}

// repoRoot returns the repository root by asking git. Falls back to walking up
// from this file looking for go.mod if git is unavailable.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	// Fallback: walk up looking for go.mod.
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root")
		}
		dir = parent
	}
}

func validateImportFile(path, wantPackage string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if hasBuildTag(data) {
		return errMalformed("must be untagged")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		return errMalformed("parse: " + err.Error())
	}
	if file.Name.Name != wantPackage {
		return errMalformed("must use package " + wantPackage)
	}
	if len(file.Imports) != 1 {
		return errMalformed("must contain exactly one import")
	}
	imp := file.Imports[0]
	if imp.Name == nil || imp.Name.Name != "_" {
		return errMalformed("must blank-import internal/testenv")
	}
	if strings.Trim(imp.Path.Value, `"`) != importPath {
		return errMalformed("must import " + importPath)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			return errMalformed("must contain only the blank import")
		}
	}
	return nil
}

func preferredPackage(packages map[string]bool) string {
	names := make([]string, 0, len(packages))
	for pkg := range packages {
		names = append(names, pkg)
	}
	sort.Strings(names)
	for _, pkg := range names {
		if !strings.HasSuffix(pkg, "_test") {
			return pkg
		}
	}
	return names[0]
}

func hasBuildTag(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//go:build") {
			return true
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		return false
	}
	return false
}

func findLeakVectorGetenv(fset *token.FileSet, node ast.Node, leakVars map[string]bool, rel string, offenders *[]string) {
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" || sel.Sel == nil || sel.Sel.Name != "Getenv" {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		name := strings.Trim(lit.Value, `"`)
		if !leakVars[name] {
			return true
		}
		pos := fset.Position(call.Pos())
		*offenders = append(*offenders, rel+":"+strconv.Itoa(pos.Line)+" reads "+name)
		return true
	})
}

type errMalformed string

func (e errMalformed) Error() string {
	return string(e)
}
