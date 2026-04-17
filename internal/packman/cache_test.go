package packman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoCacheKeyDeterministic(t *testing.T) {
	a := RepoCacheKey("https://github.com/example/repo", "abc123")
	b := RepoCacheKey("https://github.com/example/repo", "abc123")
	c := RepoCacheKey("https://github.com/example/repo", "def456")
	if a != b {
		t.Fatalf("equal inputs produced different keys: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("different commits produced same key: %q", a)
	}
}

func TestRepoCachePathUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if !strings.HasPrefix(got, filepath.Join(home, ".gc", "cache", "repos")) {
		t.Fatalf("RepoCachePath = %q", got)
	}
}

func TestRepoCacheKeyNormalizesSubpathSources(t *testing.T) {
	plain := RepoCacheKey("file:///tmp/repo.git", "abc123")
	subpath := RepoCacheKey("file:///tmp/repo.git//packs/base", "abc123")
	if plain != subpath {
		t.Fatalf("RepoCacheKey should ignore subpath for cache identity: %q != %q", plain, subpath)
	}
}

func TestRepoCacheKeyNormalizesGitHubShortcut(t *testing.T) {
	shortcut := RepoCacheKey("github.com/example/repo", "abc123")
	https := RepoCacheKey("https://github.com/example/repo", "abc123")
	if shortcut != https {
		t.Fatalf("RepoCacheKey should normalize bare github shortcut: %q != %q", shortcut, https)
	}
}

func TestEnsureRepoInCacheSkipsExistingClone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	called := false
	prev := runGit
	runGit = func(_ string, _ ...string) (string, error) {
		called = true
		return "", nil
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	if called {
		t.Fatal("runGit called for existing cache")
	}
}
