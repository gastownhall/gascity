package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
)

func TestResolveLockedRemoteImportAcceptsBundledSyntheticCache(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := "abc123def456abc123def456abc123def456abc123de"
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}

	got, ok, err := resolveLockedRemoteImport(source, cityDir)
	if err != nil {
		t.Fatalf("resolveLockedRemoteImport: %v", err)
	}
	if !ok {
		t.Fatal("resolveLockedRemoteImport ok = false, want true")
	}
	if got != cacheDir {
		t.Fatalf("cacheDir = %q, want %q", got, cacheDir)
	}
}

func TestResolveLockedRemoteImportRejectsBundledSyntheticContentDrift(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := "abc123def456abc123def456abc123def456abc123de"
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}
	writeTestFile(t, cacheDir, "internal/bootstrap/packs/core/pack.toml", `
[pack]
name = "tampered"
schema = 1
`)

	_, _, err := resolveLockedRemoteImport(source, cityDir)
	if err == nil {
		t.Fatal("expected synthetic cache content drift error")
	}
	if !strings.Contains(err.Error(), "synthetic cache is invalid") || !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("error = %v, want synthetic cache content drift", err)
	}
}

func TestResolveLockedRemoteImportRejectsBundledSyntheticExtraFile(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := "abc123def456abc123def456abc123def456abc123de"
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}
	writeTestFile(t, cacheDir, "internal/bootstrap/packs/core/agents/injected/prompt.md", "malicious")

	_, _, err := resolveLockedRemoteImport(source, cityDir)
	if err == nil {
		t.Fatal("expected synthetic cache extra-file error")
	}
	if !strings.Contains(err.Error(), "synthetic cache is invalid") || !strings.Contains(err.Error(), "unexpected file") {
		t.Fatalf("error = %v, want synthetic cache unexpected-file rejection", err)
	}
}

func TestResolveInstalledRemoteImportAcceptsBundledSyntheticCache(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := "abc123def456abc123def456abc123def456abc123de"
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}

	got, err := resolveInstalledRemoteImport(source, cityDir)
	if err != nil {
		t.Fatalf("resolveInstalledRemoteImport: %v", err)
	}
	if got != cacheDir {
		t.Fatalf("cacheDir = %q, want %q", got, cacheDir)
	}
}

func TestResolveLockedRemoteImportSurfacesInvalidBundledMarker(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := "abc123def456abc123def456abc123def456abc123de"
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	writeTestFile(t, cacheDir, ".gc-bundled-pack-cache.toml", `
schema = 99
repository = "https://github.com/gastownhall/gascity.git"
commit = "abc123def456abc123def456abc123def456abc123de"
content_hash = "sha256:deadbeef"
`)

	_, _, err := resolveLockedRemoteImport(source, cityDir)
	if err == nil {
		t.Fatal("expected invalid marker error")
	}
	if !strings.Contains(err.Error(), "synthetic cache is invalid") || !strings.Contains(err.Error(), "unsupported bundled pack cache marker schema 99") {
		t.Fatalf("error = %v, want invalid marker detail", err)
	}
}

func TestResolveLockedRemoteImportRejectsSyntheticMarkerForNonBundledSource(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := "https://github.com/example/other.git//pack"
	commit := "abc123def456"
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	writeTestFile(t, cacheDir, ".gc-bundled-pack-cache.toml", fmt.Sprintf(`
schema = 1
repository = %q
commit = %q
content_hash = "sha256:deadbeef"
`, source, commit))

	_, _, err := resolveLockedRemoteImport(source, cityDir)
	if err == nil {
		t.Fatal("expected non-bundled synthetic cache to be rejected")
	}
	if !strings.Contains(err.Error(), "locked but not cached") {
		t.Fatalf("error = %v, want ordinary missing-cache error", err)
	}
}

func TestResolveLockedRemoteImportPrefersGitCacheOverInvalidBundledMarker(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := "abc123def456abc123def456abc123def456abc123de"
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	mustMkdirAll(t, filepath.Join(cacheDir, ".git"), 0o755)
	writeTestFile(t, cacheDir, ".gc-bundled-pack-cache.toml", `
schema = 99
repository = "https://github.com/gastownhall/gascity.git"
commit = "different"
content_hash = "sha256:deadbeef"
`)
	oldRunRepoCacheGit := runRepoCacheGit
	t.Cleanup(func() { runRepoCacheGit = oldRunRepoCacheGit })
	runRepoCacheGit = func(dir string, args ...string) (string, error) {
		if dir != cacheDir {
			t.Fatalf("git dir = %q, want %q", dir, cacheDir)
		}
		switch strings.Join(args, " ") {
		case "rev-parse HEAD":
			return commit, nil
		case "status --porcelain --ignored":
			return "", nil
		default:
			t.Fatalf("unexpected git args %q", strings.Join(args, " "))
			return "", nil
		}
	}

	_, ok, err := resolveLockedRemoteImport(source, cityDir)
	if err != nil {
		t.Fatalf("resolveLockedRemoteImport: %v", err)
	}
	if !ok {
		t.Fatal("resolveLockedRemoteImport ok = false, want true")
	}
}

func TestValidateInstalledRemoteCacheTreatsBundledGitENOTDIRAsNonCheckout(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(cacheDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", cacheDir, err)
	}

	err := validateInstalledRemoteCache(bundledPackSource(), cacheDir, "abc123def456")
	if err == nil {
		t.Fatal("validateInstalledRemoteCache accepted file cache")
	}
	if !strings.Contains(err.Error(), "synthetic cache is invalid") || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error = %v, want synthetic validation context", err)
	}
	if strings.Contains(err.Error(), "checking cached import") {
		t.Fatalf("error = %v, want ENOTDIR classified as non-checkout", err)
	}
}

func setupBundledImportTest(t *testing.T) (home, cityDir string) {
	t.Helper()
	dir := t.TempDir()
	home = filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	cityDir = filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	return home, cityDir
}

func bundledPackSource() string {
	source, ok := builtinpacks.Source("core")
	if !ok {
		panic("missing core bundled pack source")
	}
	return source
}

func writeBundledImportLock(t *testing.T, cityDir, source, commit string) {
	t.Helper()
	writeTestFile(t, cityDir, "packs.lock", fmt.Sprintf(`
schema = 1

[packs.%q]
version = "1.2.3"
commit = %q
fetched = "2026-04-10T00:00:00Z"
`, source, commit))
}

func bundledRepoCacheDir(home, source, commit string) string {
	return filepath.Join(home, ".gc", "cache", "repos", RepoCacheKey(source, commit))
}
