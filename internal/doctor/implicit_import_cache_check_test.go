package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap"
	"github.com/gastownhall/gascity/internal/config"
)

func TestImplicitImportCacheCheckDetectsLegacyOnlyCache(t *testing.T) {
	gcHome, imp, canonical, legacy := prepareImplicitImportCacheFixture(t)
	if err := os.Rename(canonical, legacy); err != nil {
		t.Fatalf("Rename(%s -> %s): %v", canonical, legacy, err)
	}

	check := &ImplicitImportCacheCheck{}
	result := check.Run(&CheckContext{})
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s", result.Status, result.Message)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), legacy) {
		t.Fatalf("details %q do not mention legacy cache %s", result.Details, legacy)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), config.GlobalRepoCachePath(gcHome, imp.Source, imp.Commit)) {
		t.Fatalf("details %q do not mention canonical cache", result.Details)
	}
}

func TestImplicitImportCacheCheckFixBackfillsAndPrunesLegacy(t *testing.T) {
	_, _, canonical, legacy := prepareImplicitImportCacheFixture(t)
	if err := os.Rename(canonical, legacy); err != nil {
		t.Fatalf("Rename(%s -> %s): %v", canonical, legacy, err)
	}

	check := &ImplicitImportCacheCheck{}
	if err := check.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix(): %v", err)
	}
	if !hasImplicitImportPack(canonical) {
		t.Fatalf("canonical cache %s was not recreated", canonical)
	}
	if hasImplicitImportPack(legacy) {
		t.Fatalf("legacy cache %s was not pruned", legacy)
	}

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func TestImplicitImportCacheCheckWarnsForStaleLegacySibling(t *testing.T) {
	_, _, canonical, legacy := prepareImplicitImportCacheFixture(t)
	if err := copyTree(canonical, legacy); err != nil {
		t.Fatalf("copyTree(%s, %s): %v", canonical, legacy, err)
	}

	check := &ImplicitImportCacheCheck{}
	result := check.Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; msg = %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "stale") {
		t.Fatalf("message = %q, want stale warning", result.Message)
	}
	if err := check.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix(): %v", err)
	}
	if hasImplicitImportPack(legacy) {
		t.Fatalf("legacy cache %s was not pruned", legacy)
	}
}

func TestImplicitImportCacheCheckIgnoresNonBootstrapImports(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	if err := os.WriteFile(implicitPath, []byte(`
schema = 1

[imports.custom]
source = "github.com/example/custom-pack"
version = "1.0.0"
commit = "deadbeef"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", implicitPath, err)
	}

	check := &ImplicitImportCacheCheck{}
	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

func prepareImplicitImportCacheFixture(t *testing.T) (string, config.ImplicitImport, string, string) {
	t.Helper()

	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	if err := bootstrap.EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("EnsureBootstrap(%s): %v", gcHome, err)
	}

	imports, _, err := config.ReadImplicitImports()
	if err != nil {
		t.Fatalf("ReadImplicitImports(): %v", err)
	}
	imp, ok := imports["import"]
	if !ok {
		t.Fatal("bootstrap import entry missing")
	}

	canonical := config.GlobalRepoCachePath(gcHome, imp.Source, imp.Commit)
	legacy := legacyImplicitImportCachePath(gcHome, bootstrapSource(t, "import"), imp.Commit)
	if !hasImplicitImportPack(canonical) {
		t.Fatalf("canonical cache %s missing", canonical)
	}
	if legacy == canonical {
		t.Fatalf("legacy cache path unexpectedly equals canonical path: %s", canonical)
	}
	if err := os.RemoveAll(legacy); err != nil {
		t.Fatalf("RemoveAll(%s): %v", legacy, err)
	}

	return gcHome, imp, canonical, legacy
}

func bootstrapSource(t *testing.T, name string) string {
	t.Helper()
	for _, entry := range bootstrap.BootstrapPacks {
		if entry.Name == name {
			return entry.Source
		}
	}
	t.Fatalf("bootstrap source for %q not found", name)
	return ""
}

func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
