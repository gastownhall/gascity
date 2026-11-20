package packman

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// stubStatusGitOnPath puts a git stub first and alone on PATH, modeled on
// config's fakeStatusGitOnPath. It answers rev-parse with headCommit and
// status with the given stdout and stderr, exiting zero, so a caller cannot
// tell the diagnostic apart from real git behavior by version. The script
// uses only shell builtins.
func stubStatusGitOnPath(t *testing.T, headCommit, statusStdout, statusStderr string) {
	t.Helper()
	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
	case "$arg" in
	rev-parse) printf '%%s\n' '%s'; exit 0 ;;
	status) printf '%%s' '%s'; printf '%%s\n' '%s' >&2; exit 0 ;;
	esac
done
exit 2
`, headCommit, statusStdout, statusStderr)
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
}

// TestCachedRepoChecksFailClosedOnGitStderr pins the fail-closed half of the
// shared repo-cache runner contract: a `git status --porcelain` that exits 0
// but writes a diagnostic must surface as an unreadable-cache-git failure
// naming the command and the diagnostic. A merged-output runner cannot tell
// the diagnostic apart from porcelain records and reports the cache as
// dirty ("local worktree changes" / cache-worktree-dirty) instead.
func TestCachedRepoChecksFailClosedOnGitStderr(t *testing.T) {
	const (
		source     = "https://example.com/tools.git"
		headCommit = "0123456789abcdef0123456789abcdef01234567"
		diagnostic = "warning: could not open directory sub/: Permission denied"
	)

	requireStatusDiagnostic := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("cache validation accepted a status that wrote diagnostics")
		}
		msg := err.Error()
		for _, want := range []string{"git status --porcelain", diagnostic} {
			if !strings.Contains(msg, want) {
				t.Errorf("error = %q, want it to contain %q", msg, want)
			}
		}
		if strings.Contains(msg, "local worktree changes") {
			t.Errorf("error = %q, want stderr not reported as local worktree changes", msg)
		}
	}

	requireStatusIssue := func(t *testing.T, report *CheckReport) {
		t.Helper()
		if report == nil {
			t.Fatal("report is nil")
		}
		if len(report.Issues) != 1 {
			t.Fatalf("len(Issues) = %d, want 1: %#v", len(report.Issues), report.Issues)
		}
		issue := report.Issues[0]
		if issue.Code != "unreadable-cache-git" {
			t.Fatalf("issue code = %q, want %q; issue=%#v", issue.Code, "unreadable-cache-git", issue)
		}
		for _, want := range []string{"git status --porcelain", diagnostic} {
			if !strings.Contains(issue.Message, want) {
				t.Errorf("issue message = %q, want it to contain %q", issue.Message, want)
			}
		}
		if strings.Contains(issue.Message, "local worktree changes") {
			t.Errorf("issue message = %q, want stderr not reported as local worktree changes", issue.Message)
		}
	}

	// stageStubbedCache places a cache directory whose .git marker and
	// pack.toml satisfy the stat-level checks, with a lockfile pinning it at
	// headCommit, so the checks reach the stubbed git status call.
	stageStubbedCache := func(t *testing.T) string {
		t.Helper()
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		city := t.TempDir()
		cachePath, err := RepoCachePath(source, headCommit)
		if err != nil {
			t.Fatalf("RepoCachePath: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(cachePath, ".git"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cachePath, "pack.toml"), []byte("[pack]\nname = \"tools\"\nschema = 1\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(pack.toml): %v", err)
		}
		writeTestLockfile(t, city, map[string]LockedPack{
			source: {Version: "1.0.0", Commit: headCommit},
		})
		return city
	}

	checkInstalled := func(t *testing.T) *CheckReport {
		t.Helper()
		report, err := CheckInstalled(stageStubbedCache(t), map[string]config.Import{
			"pack:tools": {Source: source, Version: "^1.0"},
		})
		if err != nil {
			t.Fatalf("CheckInstalled: %v", err)
		}
		return report
	}

	t.Run("stderr with clean stdout", func(t *testing.T) {
		stubStatusGitOnPath(t, headCommit, "", diagnostic)
		requireStatusDiagnostic(t, validateCachedRepoCheckout(t.TempDir(), headCommit))
		requireStatusIssue(t, checkInstalled(t))
	})

	t.Run("stderr with porcelain record", func(t *testing.T) {
		stubStatusGitOnPath(t, headCommit, "?? stray\n", diagnostic)
		requireStatusDiagnostic(t, validateCachedRepoCheckout(t.TempDir(), headCommit))
		requireStatusIssue(t, checkInstalled(t))
	})
}
