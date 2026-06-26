package packman

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
)

// canonicalBundledCommit returns the only commit the running binary
// pre-seeds from embedded content for a bundled source.
func canonicalBundledCommit(source string) string {
	return strings.TrimPrefix(config.BundledSourcePinnedVersion(source), "sha:")
}

func TestEnsureRepoInCacheMaterializesBundledSourceWithoutGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("gastown")
	commit := canonicalBundledCommit(source)

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
	packToml := filepath.Join(got, "examples", "gastown", "packs", "gastown", "pack.toml")
	if _, err := os.Stat(packToml); err != nil {
		t.Fatalf("synthetic cache missing gastown pack.toml: %v", err)
	}
	if err := builtinpacks.ValidateSyntheticRepo(got, commit); err != nil {
		t.Fatalf("ValidateSyntheticRepo: %v", err)
	}
}

func TestBundledSyntheticCacheKeyDoesNotCollideWithSameRepoGitSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("core")
	gitSource := builtinpacks.Repository + "//contrib/k8s"
	commit := canonicalBundledCommit(source)

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

	// At a non-canonical commit a bundled source is an ordinary remote
	// import: its cache key is the plain same-repo derivation, so it is
	// cache-compatible with an ordinary clone of the same repository.
	foreign := "abc123def456"
	if config.IsBundledSourceAtCanonicalPin(source, foreign) {
		t.Fatalf("commit %q is unexpectedly canonical for %q", foreign, source)
	}
	foreignPath, err := RepoCachePath(source, foreign)
	if err != nil {
		t.Fatalf("RepoCachePath bundled at foreign pin: %v", err)
	}
	plainPath, err := RepoCachePath(gitSource, foreign)
	if err != nil {
		t.Fatalf("RepoCachePath git at foreign pin: %v", err)
	}
	if foreignPath != plainPath {
		t.Fatalf("bundled cache path at foreign pin = %q, want plain same-repo derivation %q", foreignPath, plainPath)
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("gastown")
	commit := canonicalBundledCommit(source)
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("gastown")
	commit := canonicalBundledCommit(source)
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("gastown")
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("gastown")
	commit := canonicalBundledCommit(source)
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
		case "status --porcelain":
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

func TestEnsureRepoInCacheClonesBundledSourceAtNonCanonicalPin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("gastown")
	commit := "abc123def456"
	if config.IsBundledSourceAtCanonicalPin(source, commit) {
		t.Fatalf("commit %q is unexpectedly canonical for %q", commit, source)
	}

	stubPackToml := "[pack]\nname = \"gastown\"\nschema = 2\n"
	var gitCalls [][]string
	prevGit := runGit
	runGit = func(_ string, args ...string) (string, error) {
		gitCalls = append(gitCalls, args)
		switch args[0] {
		case "clone":
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			packToml := filepath.Join(target, "examples", "gastown", "packs", "gastown", "pack.toml")
			if err := os.MkdirAll(filepath.Dir(packToml), 0o755); err != nil {
				return "", err
			}
			return "", os.WriteFile(packToml, []byte(stubPackToml), 0o644)
		case "checkout":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prevGit })

	prevMaterialize := materializeSyntheticRepo
	materializeSyntheticRepo = func(string, string) error {
		t.Fatal("materializeSyntheticRepo was called for a non-canonical pin")
		return nil
	}
	t.Cleanup(func() { materializeSyntheticRepo = prevMaterialize })

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
	cloned := false
	for _, args := range gitCalls {
		if args[0] == "clone" {
			cloned = true
		}
	}
	if !cloned {
		t.Fatalf("git calls = %v, want a clone for the non-canonical pin", gitCalls)
	}
	if _, err := os.Stat(filepath.Join(got, ".gc-bundled-pack-cache.toml")); !os.IsNotExist(err) {
		t.Fatalf("synthetic marker stat err = %v, want not exist", err)
	}
	if err := builtinpacks.ValidateSyntheticRepo(got, commit); err == nil {
		t.Fatal("clone result validates as a synthetic cache; embedded content must not be materialized")
	}
	data, err := os.ReadFile(filepath.Join(got, "examples", "gastown", "packs", "gastown", "pack.toml"))
	if err != nil || string(data) != stubPackToml {
		t.Fatalf("pack.toml = %q, %v; want clone-stub content preserved", data, err)
	}
}
