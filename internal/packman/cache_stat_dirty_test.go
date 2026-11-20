package packman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedStatCacheRepo commits a pack with an ignorable artifact pattern into a
// fresh git repository under dir, for cachedRepoDirty semantics tests.
func seedStatCacheRepo(t *testing.T, dir string) {
	t.Helper()
	writeFixtureFile(t, dir, "pack.toml", "[pack]\nname = \"tools\"\nschema = 1\n", 0o644)
	writeFixtureFile(t, dir, ".gitignore", "__pycache__/\n", 0o644)
	fixtureGit(t, dir, "init", "-q", "-b", "main")
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-q", "-m", "seed")
}

// rewriteStatFile replaces path with a byte-identical copy through a
// temp-file rename (a fresh inode) and a +2h mtime, as re-rendering tooling
// does when it reinstalls unchanged content.
func rewriteStatFile(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

// recordRunGit wraps the runGit seam, recording each invocation's arguments
// joined as a command line, so a test can pin exactly which git commands a
// check issues.
func recordRunGit(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	prev := runGit
	runGit = func(dir string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return prev(dir, args...)
	}
	t.Cleanup(func() { runGit = prev })
	return &calls
}

// TestCachedRepoDirtyIgnoresStatOnlyRewrite pins cachedRepoDirty's real-git
// semantics: a byte-identical rename-over with a fresh mtime reads clean —
// including under a held index lock and with opportunistic index writes
// disabled — while content changes and untracked non-ignored files still
// read dirty, and `status --porcelain` remains the only git command issued:
// no refresh, no extra index write.
func TestCachedRepoDirtyIgnoresStatOnlyRewrite(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, cacheDir string)
		dirty bool
	}{
		{name: "stat-only rewrite reads clean", setup: func(t *testing.T, cacheDir string) {
			rewriteStatFile(t, filepath.Join(cacheDir, "pack.toml"))
		}},
		{name: "stat-only rewrite with held index lock", setup: func(t *testing.T, cacheDir string) {
			rewriteStatFile(t, filepath.Join(cacheDir, "pack.toml"))
			writeFixtureFile(t, cacheDir, filepath.Join(".git", "index.lock"), "", 0o644)
		}},
		{name: "stat-only rewrite with optional locks disabled", setup: func(t *testing.T, cacheDir string) {
			rewriteStatFile(t, filepath.Join(cacheDir, "pack.toml"))
			t.Setenv("GIT_OPTIONAL_LOCKS", "0")
		}},
		{name: "modified tracked file reads dirty", dirty: true, setup: func(t *testing.T, cacheDir string) {
			writeFixtureFile(t, cacheDir, "pack.toml", "[pack]\nname = \"tools\"\nschema = 2\n", 0o644)
		}},
		{name: "untracked file reads dirty", dirty: true, setup: func(t *testing.T, cacheDir string) {
			writeFixtureFile(t, cacheDir, "stray.txt", "untracked\n", 0o644)
		}},
		{name: "ignored artifact reads clean", setup: func(t *testing.T, cacheDir string) {
			rewriteStatFile(t, filepath.Join(cacheDir, "pack.toml"))
			writeFixtureFile(t, cacheDir, filepath.Join("__pycache__", "x.pyc"), "bytes\n", 0o644)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useNeutralGitEnv(t)
			cacheDir := t.TempDir()
			seedStatCacheRepo(t, cacheDir)
			if tc.setup != nil {
				tc.setup(t, cacheDir)
			}

			calls := recordRunGit(t)
			dirty, err := cachedRepoDirty(cacheDir)
			if err != nil {
				t.Fatalf("cachedRepoDirty: %v", err)
			}
			if dirty != tc.dirty {
				t.Fatalf("cachedRepoDirty = %v, want %v", dirty, tc.dirty)
			}
			if got := strings.Join(*calls, "; "); got != "status --porcelain" {
				t.Fatalf("git calls = %q, want only \"status --porcelain\"", got)
			}
		})
	}
}
