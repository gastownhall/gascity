package providerledger

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// runtimeGuardPackage is the worker-site guard provider (DES-CN §4.4). It has
// no registry key, so the catalog discovery above cannot see it; this test
// is what forces its ledger row once cmd/gc constructs it.
const runtimeGuardPackage = "internal/runtime/guard"

// productionImporters returns the non-test .go files directly in dir that
// import importPath, sorted.
func productionImporters(dir, importPath string) ([]string, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var importers []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, err
			}
			if path == importPath {
				importers = append(importers, filepath.Base(name))
				break
			}
		}
	}
	slices.Sort(importers)
	return importers, nil
}

func TestProductionImportersFindsOnlyNonTestImporters(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("wired.go", "package main\n\nimport g \"example.com/guard\"\n\nvar _ = g.New\n")
	write("other.go", "package main\n\nimport \"example.com/guardian\"\n")
	write("wired_test.go", "package main\n\nimport \"example.com/guard\"\n")
	got, err := productionImporters(dir, "example.com/guard")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"wired.go"}; !slices.Equal(got, want) {
		t.Fatalf("productionImporters = %q, want %q", got, want)
	}
}

// TestRuntimeGuardIsLedgeredOnceWired fails the moment a production cmd/gc
// file imports the guard while the ledger still lacks a guard.New entry. The
// guard's disposition is recorded in internal/runtime/guard/guard_test.go
// until then.
func TestRuntimeGuardIsLedgeredOnceWired(t *testing.T) {
	cmdDir := filepath.Join(repoRoot(t), "cmd/gc")
	if _, err := os.Stat(filepath.Join(cmdDir, "providers.go")); err != nil {
		t.Fatalf("cmd/gc sources are not visible to this test: %v", err)
	}
	importers, err := productionImporters(cmdDir, moduleImportPath+"/"+runtimeGuardPackage)
	if err != nil {
		t.Fatalf("scanning cmd/gc imports: %v", err)
	}
	if len(importers) == 0 {
		return
	}
	guardNew := repoSymbol(runtimeGuardPackage, "New")
	for _, entry := range Catalog() {
		if slices.Contains(entry.Constructors, guardNew) {
			return
		}
	}
	t.Fatalf("cmd/gc %s import %s but the provider ledger has no entry for %s; "+
		"add a not_applicable runtime row with Source cmd/gc/providers.go#resolveWorkerSpec, "+
		"following the NewFailFake precedent (resolveWorkerSpec must directly return or bind "+
		"the guard.New result for validateSourceRef)",
		strings.Join(importers, ", "), runtimeGuardPackage, renderSymbolRef(guardNew))
}
