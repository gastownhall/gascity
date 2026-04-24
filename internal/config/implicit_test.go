package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestReadImplicitImports_MissingFile(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	imports, path, err := ReadImplicitImports()
	if err != nil {
		t.Fatalf("ReadImplicitImports: %v", err)
	}
	if path == "" {
		t.Fatal("ReadImplicitImports returned empty path")
	}
	if len(imports) != 0 {
		t.Fatalf("len(imports) = %d, want 0", len(imports))
	}
}

func TestLoadWithIncludes_IgnoresImplicitImports(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	gcHome := os.Getenv("GC_HOME")
	cacheDir := GlobalRepoCachePath(gcHome, "github.com/gastownhall/gc-import", "abc123")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "pack.toml"), []byte(`
[pack]
name = "gc-import"
schema = 1

[[agent]]
name = "runner"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcHome, "implicit-import.toml"), []byte(`
schema = 1

[imports.import]
source = "github.com/gastownhall/gc-import"
version = "0.2.0"
commit = "abc123"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	found := map[string]bool{}
	for _, a := range explicitAgents(cfg.Agents) {
		found[a.QualifiedName()] = true
	}
	if !found["mayor"] {
		t.Fatalf("missing mayor agent: %v", found)
	}
	if found["import.runner"] {
		t.Fatalf("implicit imports should not be spliced into cities: %v", found)
	}
	if _, ok := prov.Imports["import"]; ok {
		t.Fatalf("prov.Imports[import] should not be marked implicit when implicit imports are ignored")
	}
}

func TestGlobalRepoCacheDirNameUsesCanonicalRepoCacheKey(t *testing.T) {
	source := "file:///tmp/repo.git//packs/base"
	commit := "abc123"

	if got, want := GlobalRepoCacheDirName(source, commit), RepoCacheKey(source, commit); got != want {
		t.Fatalf("GlobalRepoCacheDirName(%q, %q) = %q, want %q", source, commit, got, want)
	}
}
