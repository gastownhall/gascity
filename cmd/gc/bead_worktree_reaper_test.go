package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func gaConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
	}
}

func TestExtractBeadIDFromWorktreeNameBareID(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "ga-n0oafq")
	if got != "ga-n0oafq" {
		t.Errorf("got %q, want %q", got, "ga-n0oafq")
	}
}

func TestExtractBeadIDFromWorktreeNameCompound(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder-ga-34q3ss")
	if got != "ga-34q3ss" {
		t.Errorf("got %q, want %q", got, "ga-34q3ss")
	}
}

func TestExtractBeadIDFromWorktreeNameNoMatch(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder-feature-branch")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractBeadIDFromWorktreeNameSingleSegment(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractBeadIDFromWorktreeNameNilConfig(t *testing.T) {
	got := extractBeadIDFromWorktreeName(nil, "ga-n0oafq")
	if got != "" {
		t.Errorf("got %q, want empty for nil config", got)
	}
}

func TestExtractBeadIDFromWorktreeNameEmptyName(t *testing.T) {
	got := extractBeadIDFromWorktreeName(gaConfig(), "")
	if got != "" {
		t.Errorf("got %q, want empty for empty name", got)
	}
}

func TestIsStrictlyUnderDirSubpath(t *testing.T) {
	dir := filepath.Join("a", "b")
	path := filepath.Join("a", "b", "c")
	if !isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = false, want true", dir, path)
	}
}

func TestIsStrictlyUnderDirSameDir(t *testing.T) {
	dir := filepath.Join("a", "b")
	if isStrictlyUnderDir(dir, dir) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false (same dir)", dir, dir)
	}
}

func TestIsStrictlyUnderDirPathTraversal(t *testing.T) {
	dir := filepath.Join("a", "b")
	path := filepath.Join("a", "c") // sibling — relative path starts with ".."
	if isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false (path traversal)", dir, path)
	}
}

func TestIsStrictlyUnderDirDeepSubpath(t *testing.T) {
	dir := filepath.Join("root", "worktrees")
	path := filepath.Join("root", "worktrees", "gascity", "builder")
	if !isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = false, want true", dir, path)
	}
}

// TestReapClosedBeadWorktrees_UsesConfiguredWorktreesRoot verifies the reaper
// discovers closed-bead worktrees under the configured worktrees root and
// ignores the default <city>/.gc/worktrees tree while the override is set.
func TestReapClosedBeadWorktrees_UsesConfiguredWorktreesRoot(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	configuredRoot := filepath.Join(t.TempDir(), "configured-worktrees")
	t.Setenv("GC_WORKTREES_DIR", configuredRoot)

	addWorktree := func(root, beadID string) string {
		t.Helper()
		wtPath := filepath.Join(root, reapTestRigName, "builder", beadID)
		if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
			t.Fatalf("mkdir worktree parent: %v", err)
		}
		mustGit(t, rigRoot, "worktree", "add", "-b", "wt-"+beadID, wtPath)
		backdateWorktreeGitFile(t, wtPath, 24*time.Hour)
		return wtPath
	}
	configuredWT := addWorktree(configuredRoot, "ga-conf01")
	defaultWT := addWorktree(filepath.Join(cityPath, ".gc", "worktrees"), "ga-dflt01")

	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-conf01", Status: "closed"},
		{ID: "ga-dflt01", Status: "closed"},
	}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, true, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-conf01" {
		t.Fatalf("Reaped (would-reap) = %+v, want exactly ga-conf01\nstderr:\n%s", report.Reaped, stderr.String())
	}
	for _, d := range report.Protected {
		if d.BeadID == "ga-dflt01" {
			t.Fatalf("default-root worktree %s was a candidate while GC_WORKTREES_DIR is set: %+v", defaultWT, d)
		}
	}
	for _, wt := range []string{configuredWT, defaultWT} {
		if _, err := os.Stat(wt); err != nil {
			t.Fatalf("dry-run removed or broke worktree %s: %v", wt, err)
		}
	}
}
