package packman

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCheckInstalledNoRemoteImportsMissingLockOK(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	report, err := CheckInstalled(city, map[string]config.Import{
		"local": {Source: "./packs/local"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	if report.HasIssues() {
		t.Fatalf("issues = %#v, want none", report.Issues)
	}
	if report.CheckedSources != 0 {
		t.Fatalf("CheckedSources = %d, want 0", report.CheckedSources)
	}
}

func TestCheckInstalledReportsMissingLockfile(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: "https://example.com/tools.git", Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-lockfile")
}

func TestCheckInstalledReportsMissingCache(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/tools.git": {Version: "1.0.0", Commit: "aaaa"},
	})

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: "https://example.com/tools.git", Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-cache")
}

func TestCheckInstalledMissingCacheDoesNotCreateCacheEntry(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	source := "https://example.com/tools.git"
	commit := "aaaa"
	writeTestLockfile(t, city, map[string]LockedPack{
		source: {Version: "1.0.0", Commit: commit},
	})

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: source, Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-cache")
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("repo cache entry stat err = %v, want not exist", err)
	}
}

func TestCheckInstalledDeduplicatesRepeatedSourceIssues(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/tools.git": {Version: "1.0.0", Commit: "aaaa"},
	})

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools":         {Source: "https://example.com/tools.git", Version: "^1.0"},
		"rig:frontend:tools": {Source: "https://example.com/tools.git", Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-cache")
	if report.CheckedSources != 1 {
		t.Fatalf("CheckedSources = %d, want 1", report.CheckedSources)
	}
}

func TestCheckInstalledSkipsStaleLockEntriesWhenClosureIncomplete(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/a.git": {Version: "1.0.0", Commit: "aaaa"},
		"https://example.com/b.git": {Version: "1.0.0", Commit: "bbbb"},
	})

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:a": {Source: "https://example.com/a.git", Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-cache")
}

func TestCheckInstalledReportsConstraintMismatch(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/tools.git": {Version: "1.0.0", Commit: "aaaa"},
	})

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: "https://example.com/tools.git", Version: "^2.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "lock-constraint-mismatch")
}

func TestCheckInstalledWalksTransitiveClosureAndReportsStaleLockEntry(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/a.git":     {Version: "1.0.0", Commit: "aaaa"},
		"https://example.com/b.git":     {Version: "1.0.0", Commit: "bbbb"},
		"https://example.com/stale.git": {Version: "1.0.0", Commit: "cccc"},
	})
	stageCachedPack(t, "https://example.com/a.git", "aaaa", `
[pack]
name = "a"
schema = 1

[imports.b]
source = "https://example.com/b.git"
version = "^1.0"
`)
	stageCachedPack(t, "https://example.com/b.git", "bbbb", `
[pack]
name = "b"
schema = 1
`)

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:a": {Source: "https://example.com/a.git", Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "stale-lock-entry")
	if report.CheckedSources != 2 {
		t.Fatalf("CheckedSources = %d, want 2", report.CheckedSources)
	}
}

func TestCheckInstalledReportsMissingTransitiveLockEntry(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/a.git": {Version: "1.0.0", Commit: "aaaa"},
	})
	stageCachedPack(t, "https://example.com/a.git", "aaaa", `
[pack]
name = "a"
schema = 1

[imports.b]
source = "https://example.com/b.git"
version = "^1.0"
`)

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:a": {Source: "https://example.com/a.git", Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-lock-entry")
}

func TestCheckInstalledExpandsRepeatedSourceWhenAnyImportIsTransitive(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/shared.git": {Version: "1.0.0", Commit: "aaaa"},
		"https://example.com/inner.git":  {Version: "1.0.0", Commit: "bbbb"},
	})
	stageCachedPack(t, "https://example.com/shared.git", "aaaa", `
[pack]
name = "shared"
schema = 1

[imports.inner]
source = "https://example.com/inner.git"
version = "^1.0"
`)
	stageCachedPack(t, "https://example.com/inner.git", "bbbb", `
[pack]
name = "inner"
schema = 1
`)

	transitiveFalse := false
	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:a": {Source: "https://example.com/shared.git", Version: "^1.0", Transitive: &transitiveFalse},
		"pack:z": {Source: "https://example.com/shared.git", Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	if report.HasIssues() {
		t.Fatalf("issues = %#v, want none", report.Issues)
	}
	if report.CheckedSources != 2 {
		t.Fatalf("CheckedSources = %d, want 2", report.CheckedSources)
	}
}

func TestCheckInstalledParsesNonTransitiveCachedPack(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/tools.git": {Version: "1.0.0", Commit: "aaaa"},
	})
	stageCachedPack(t, "https://example.com/tools.git", "aaaa", `
[pack
name = "tools"
schema = 1
`)

	transitiveFalse := false
	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: "https://example.com/tools.git", Version: "^1.0", Transitive: &transitiveFalse},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "invalid-cached-pack")
}

func TestCheckInstalledReportsCacheCheckoutMismatch(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/tools.git": {Version: "1.0.0", Commit: "aaaa"},
	})
	stageCachedPackAtCommit(t, "https://example.com/tools.git", "aaaa", "bbbb", `
[pack]
name = "tools"
schema = 1
`)

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: "https://example.com/tools.git", Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "cache-checkout-mismatch")
}

func TestCheckInstalledReportsDirtyCacheWorktree(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)
	writeTestLockfile(t, city, map[string]LockedPack{
		"https://example.com/tools.git": {Version: "1.0.0", Commit: "aaaa"},
	})
	stageCachedPackAtCommit(t, "https://example.com/tools.git", "aaaa", "aaaa", `
[pack]
name = "tools"
schema = 1
`)
	markCachedPackDirty(t, "https://example.com/tools.git", "aaaa")

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: "https://example.com/tools.git", Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "cache-worktree-dirty")
}

func TestCheckInstalledUsesRemoteSubpath(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)
	source := "file:///tmp/repo.git//packs/base"
	writeTestLockfile(t, city, map[string]LockedPack{
		source: {Version: "1.0.0", Commit: "aaaa"},
	})
	path, err := RepoCachePath(source, "aaaa")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git): %v", err)
	}
	writeCachedPackCommit(t, path, "aaaa")

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:base": {Source: source, Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-cached-pack")
}

func assertSingleIssue(t *testing.T, report *CheckReport, code string) {
	t.Helper()
	if report == nil {
		t.Fatal("report is nil")
	}
	if len(report.Issues) != 1 {
		t.Fatalf("len(Issues) = %d, want 1: %#v", len(report.Issues), report.Issues)
	}
	if report.Issues[0].Code != code {
		t.Fatalf("issue code = %q, want %q; issue=%#v", report.Issues[0].Code, code, report.Issues[0])
	}
	if report.Issues[0].Severity != CheckSeverityError {
		t.Fatalf("issue severity = %q, want error", report.Issues[0].Severity)
	}
	if report.ErrorCount() != 1 {
		t.Fatalf("ErrorCount = %d, want 1", report.ErrorCount())
	}
}

func writeTestLockfile(t *testing.T, city string, packs map[string]LockedPack) {
	t.Helper()
	for source, pack := range packs {
		if pack.Fetched.IsZero() {
			pack.Fetched = time.Unix(10, 0).UTC()
			packs[source] = pack
		}
	}
	if err := WriteLockfile(fsys.OSFS{}, city, &Lockfile{
		Schema: LockfileSchema,
		Packs:  packs,
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}
}

func stubCachedPackGit(t *testing.T) {
	t.Helper()
	prev := runGit
	runGit = func(dir string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "rev-parse" && args[1] == "HEAD" {
			data, err := os.ReadFile(filepath.Join(dir, ".packman-test-commit"))
			if err != nil {
				return "", err
			}
			return string(data), nil
		}
		if len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain" {
			if _, err := os.Stat(filepath.Join(dir, ".packman-test-dirty")); err == nil {
				return " M pack.toml", nil
			} else if err != nil && !os.IsNotExist(err) {
				return "", err
			}
			return "", nil
		}
		if len(args) >= 1 && args[0] == "checkout" {
			if dir == "" {
				return "", fmt.Errorf("checkout requires dir")
			}
			writeCachedPackCommit(t, dir, args[len(args)-1])
			return "", nil
		}
		return prev(dir, args...)
	}
	t.Cleanup(func() { runGit = prev })
}

func stageCachedPackAtCommit(t *testing.T, source, cacheCommit, headCommit, packToml string) {
	t.Helper()
	path, err := RepoCachePath(source, cacheCommit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeCachedPackCommit(t, path, headCommit)
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
}

func writeCachedPackCommit(t *testing.T, cachePath, commit string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cachePath, ".packman-test-commit"), []byte(commit), 0o644); err != nil {
		t.Fatalf("WriteFile(.packman-test-commit): %v", err)
	}
}

func markCachedPackDirty(t *testing.T, source, commit string) {
	t.Helper()
	path, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, ".packman-test-dirty"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("WriteFile(.packman-test-dirty): %v", err)
	}
}
