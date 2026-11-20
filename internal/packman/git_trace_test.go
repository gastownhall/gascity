package packman

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// useNeutralGitEnv isolates real-git packman tests from the host's user
// config and locale so seeding and diagnostics are deterministic. It mirrors
// config's useNeutralGitTestEnv: both packages share the repo-cache git
// runner, so their real-git fixtures need the same neutral environment.
func useNeutralGitEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("LC_ALL", "C")
}

// seedPackmanCacheRepo commits a minimal pack into a fresh git repository
// under dir through the real fixture git and returns the HEAD commit.
func seedPackmanCacheRepo(t *testing.T, dir string) string {
	t.Helper()
	writeFixtureFile(t, dir, "pack.toml", "[pack]\nname = \"tools\"\nschema = 1\n", 0o644)
	fixtureGit(t, dir, "init", "-q", "-b", "main")
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-q", "-m", "seed")
	return fixtureGit(t, dir, "rev-parse", "HEAD")
}

// TestValidateCachedRepoCheckoutIgnoresGitTrace pins the trace half of the
// shared repo-cache runner contract: GIT_TRACE and GIT_TRACE2_PERF write to
// stderr, which a merged-output runner folds into the parsed HEAD and status
// answers. The shared runner drops the trace variables from git's
// environment, so a clean cache at its locked commit validates with trace
// enabled anywhere in the ambient environment.
func TestValidateCachedRepoCheckoutIgnoresGitTrace(t *testing.T) {
	useNeutralGitEnv(t)
	cacheDir := t.TempDir()
	head := seedPackmanCacheRepo(t, cacheDir)

	t.Setenv("GIT_TRACE", "1")
	t.Setenv("GIT_TRACE2_PERF", "1")

	if err := validateCachedRepoCheckout(cacheDir, head); err != nil {
		t.Fatalf("validateCachedRepoCheckout with GIT_TRACE set: %v", err)
	}
	dirty, err := cachedRepoDirty(cacheDir)
	if err != nil {
		t.Fatalf("cachedRepoDirty with GIT_TRACE set: %v", err)
	}
	if dirty {
		t.Fatal("cachedRepoDirty with GIT_TRACE set reported a clean cache as dirty")
	}
}

// TestCheckInstalledAcceptsCleanCacheUnderGitTrace pins the same contract at
// the CheckInstalled level: with the trace variables enabled after the
// fixture is seeded, a locked import of a clean real-git cache reports no
// issues. A merged-output runner reads the trace lines as part of HEAD and
// reports cache-checkout-mismatch instead.
func TestCheckInstalledAcceptsCleanCacheUnderGitTrace(t *testing.T) {
	useNeutralGitEnv(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	city := t.TempDir()
	const source = "https://example.com/tools.git"

	staging := filepath.Join(t.TempDir(), "cache")
	head := seedPackmanCacheRepo(t, staging)
	cachePath, err := RepoCachePath(source, head)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Rename(staging, cachePath); err != nil {
		t.Fatalf("moving seeded repo into the cache path: %v", err)
	}
	writeTestLockfile(t, city, map[string]LockedPack{
		source: {Version: "1.0.0", Commit: head},
	})

	t.Setenv("GIT_TRACE", "1")
	t.Setenv("GIT_TRACE2_PERF", "1")

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: source, Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	if report.HasIssues() {
		t.Fatalf("issues = %#v, want none for a clean cache under GIT_TRACE", report.Issues)
	}
}
