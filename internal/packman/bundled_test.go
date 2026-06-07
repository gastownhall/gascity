package packman

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
)

func TestEnsureRepoInCacheMaterializesBundledSourceWithoutGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := builtinpacks.MustSource("maintenance")
	commit := "abc123def456"

	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		return "", fmt.Errorf("unexpected git call for bundled pack: %v", args)
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache(source, commit)
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	want, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if got != want {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(got, ".git")); !os.IsNotExist(err) {
		t.Fatalf("synthetic cache should not contain .git, stat err = %v", err)
	}
	packToml := filepath.Join(got, "examples", "gastown", "packs", "maintenance", "pack.toml")
	if _, err := os.Stat(packToml); err != nil {
		t.Fatalf("synthetic cache missing maintenance pack.toml: %v", err)
	}
	if err := builtinpacks.ValidateSyntheticRepo(got, commit); err != nil {
		t.Fatalf("ValidateSyntheticRepo: %v", err)
	}
}

func TestBundledSyntheticCacheKeyDoesNotCollideWithSameRepoGitSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := builtinpacks.MustSource("core")
	gitSource := builtinpacks.Repository + "//contrib/k8s"
	commit := "abc123def456"

	syntheticPath, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath bundled: %v", err)
	}
	gitPath, err := RepoCachePath(gitSource, commit)
	if err != nil {
		t.Fatalf("RepoCachePath git: %v", err)
	}
	if syntheticPath == gitPath {
		t.Fatalf("bundled cache path collides with same-repo git source: %q", syntheticPath)
	}
	if err := os.MkdirAll(filepath.Join(gitPath, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git): %v", err)
	}
	sentinel := filepath.Join(gitPath, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile(sentinel): %v", err)
	}

	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		return "", fmt.Errorf("unexpected git call for bundled pack: %v", args)
	}
	t.Cleanup(func() { runGit = prev })

	if _, err := EnsureRepoInCache(source, commit); err != nil {
		t.Fatalf("EnsureRepoInCache bundled: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("same-repo git cache sentinel = %q, %v; want preserved", got, err)
	}
}

func TestReadCachedPackImportsAcceptsBundledSyntheticCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := builtinpacks.MustSource("maintenance")
	commit := "abc123def456"
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cachePath, commit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}

	if _, err := ReadCachedPackImports(source, commit); err != nil {
		t.Fatalf("ReadCachedPackImports: %v", err)
	}
}

func TestReadCachedPackImportsTreatsBundledGitENOTDIRAsNonCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := builtinpacks.MustSource("maintenance")
	commit := "abc123def456"
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("MkdirAll(cache parent): %v", err)
	}
	if err := os.WriteFile(cachePath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile(cache path): %v", err)
	}

	_, err = ReadCachedPackImports(source, commit)
	if err == nil {
		t.Fatal("ReadCachedPackImports accepted invalid bundled cache")
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error = %v, want synthetic validation context", err)
	}
	if strings.Contains(err.Error(), "checking bundled repo cache") {
		t.Fatalf("error = %v, want ENOTDIR treated as non-checkout", err)
	}
}

func TestMaterializeBundledRepoInCacheLockedRejectsNonCanonicalPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := builtinpacks.MustSource("maintenance")
	commit := "abc123def456"
	nonCanonical := filepath.Join(t.TempDir(), "cache")

	prevMaterialize := materializeSyntheticRepo
	materializeSyntheticRepo = func(string, string) error {
		t.Fatal("materializeSyntheticRepo was called for non-canonical path")
		return nil
	}
	t.Cleanup(func() { materializeSyntheticRepo = prevMaterialize })

	err := materializeBundledRepoInCacheLocked(source, commit, nonCanonical)
	if err == nil {
		t.Fatal("materializeBundledRepoInCacheLocked accepted non-canonical path")
	}
	if !strings.Contains(err.Error(), "non-canonical path") {
		t.Fatalf("error = %v, want non-canonical path rejection", err)
	}
}

func TestEnsureBundledCacheMaterializeFailureIncludesRecoveryCause(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := builtinpacks.MustSource("maintenance")
	commit := "abc123def456"
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cachePath, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git): %v", err)
	}

	prevGit := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "rev-parse HEAD":
			return commit, nil
		case "status --porcelain --ignored":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prevGit })

	prevMaterialize := materializeSyntheticRepo
	materializeSyntheticRepo = func(dst, gotCommit string) error {
		if dst != cachePath {
			t.Fatalf("materialize dst = %q, want %q", dst, cachePath)
		}
		if gotCommit != commit {
			t.Fatalf("materialize commit = %q, want %q", gotCommit, commit)
		}
		return fmt.Errorf("materialize boom")
	}
	t.Cleanup(func() { materializeSyntheticRepo = prevMaterialize })

	_, err = EnsureRepoInCache(source, commit)
	if err == nil {
		t.Fatal("EnsureRepoInCache succeeded, want materialize failure")
	}
	if !strings.Contains(err.Error(), "missing pack.toml") || !strings.Contains(err.Error(), "materialize boom") {
		t.Fatalf("error = %v, want recovery cause and materialize failure", err)
	}
}
