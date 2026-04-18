package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLoadWithIncludes_SplicesImplicitImports(t *testing.T) {
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
	if !found["import.runner"] {
		t.Fatalf("missing implicit import agent: %v", found)
	}
	if got := prov.Imports["import"]; got != "(implicit)" {
		t.Fatalf("prov.Imports[import] = %q, want %q", got, "(implicit)")
	}
}

// TestLoadWithIncludes_ExplicitImportCollidingWithImplicitIsHardError
// documents the v0.15.1 behavior change: an explicit [imports.<name>]
// that collides with a bootstrap implicit-import pack hard-stops the
// load instead of silently shadowing. Prior behavior (the explicit
// import winning without a diagnostic) is gone — on upgrade that would
// silently replace the user's expected bootstrap content.
func TestLoadWithIncludes_ExplicitImportCollidingWithImplicitIsHardError(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	gcHome := os.Getenv("GC_HOME")
	implicitCacheDir := GlobalRepoCachePath(gcHome, "github.com/gastownhall/gc-import", "abc123")
	if err := os.MkdirAll(implicitCacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(implicitCacheDir, "pack.toml"), []byte(`
[pack]
name = "gc-import"
schema = 1

[[agent]]
name = "implicit-agent"
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
	explicitDir := filepath.Join(cityDir, "packs", "explicit-import")
	if err := os.MkdirAll(explicitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(explicitDir, "pack.toml"), []byte(`
[pack]
name = "explicit-import"
schema = 1

[[agent]]
name = "explicit-agent"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test-city"

[imports.import]
source = "./packs/explicit-import"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("LoadWithIncludes should hard-stop on implicit-import collision")
	}
	msg := err.Error()
	if !strings.Contains(msg, "shadows the bootstrap implicit import") {
		t.Fatalf("error missing diagnostic: %v", err)
	}
	if !strings.Contains(msg, "import") {
		t.Fatalf("error should name the colliding import: %v", err)
	}
}

// TestLoadWithIncludes_NonBootstrapImplicitImportShadowingStillAllowed
// asserts that the v0.15.1 hard-stop is scoped to bootstrap-managed
// implicit imports only. A user-added implicit import (e.g. "custom")
// retains the pre-v0.15.1 contract: an explicit [imports.custom] in the
// city shadows the implicit entry silently. This preserves legit
// workflows for cities that use the implicit mechanism for their own
// non-bootstrap packs.
func TestLoadWithIncludes_NonBootstrapImplicitImportShadowingStillAllowed(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	gcHome := os.Getenv("GC_HOME")
	// Implicit import named "custom" — NOT in bootstrapManagedImportNames.
	implicitCacheDir := GlobalRepoCachePath(gcHome, "github.com/someone/custom-pack", "zzzz999")
	if err := os.MkdirAll(implicitCacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(implicitCacheDir, "pack.toml"), []byte(`
[pack]
name = "custom-pack"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcHome, "implicit-import.toml"), []byte(`
schema = 1

[imports.custom]
source = "github.com/someone/custom-pack"
version = "1.0.0"
commit = "zzzz999"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cityDir := t.TempDir()
	explicitDir := filepath.Join(cityDir, "packs", "explicit-custom")
	if err := os.MkdirAll(explicitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(explicitDir, "pack.toml"), []byte(`
[pack]
name = "explicit-custom"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "test-city"

[imports.custom]
source = "./packs/explicit-custom"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should load cleanly — non-bootstrap implicit imports are still
	// shadowable by explicit imports without a hard stop.
	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes should allow non-bootstrap implicit shadow: %v", err)
	}
}

func TestGlobalRepoCacheDirNameUsesCanonicalRepoCacheKey(t *testing.T) {
	source := "file:///tmp/repo.git//packs/base"
	commit := "abc123"

	if got, want := GlobalRepoCacheDirName(source, commit), RepoCacheKey(source, commit); got != want {
		t.Fatalf("GlobalRepoCacheDirName(%q, %q) = %q, want %q", source, commit, got, want)
	}
}
