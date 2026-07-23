package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

type fakeBeadWorktreeGitProbe struct {
	isRepo         bool
	branch         string
	branchErr      error
	hasUncommitted bool
	hasUnpushed    bool
	unpushedErr    error
	hasStashes     bool
	stashesErr     error
	removeErr      error
	removeInvoked  bool
	removedPath    string
	removedForce   bool
}

func (f *fakeBeadWorktreeGitProbe) IsRepo() bool { return f.isRepo }
func (f *fakeBeadWorktreeGitProbe) CurrentBranch() (string, error) {
	return f.branch, f.branchErr
}

func (f *fakeBeadWorktreeGitProbe) HasUncommittedWork() bool {
	return f.hasUncommitted
}

func (f *fakeBeadWorktreeGitProbe) HasUnpushedCommitsResult() (bool, error) {
	return f.hasUnpushed, f.unpushedErr
}

func (f *fakeBeadWorktreeGitProbe) HasStashesResult() (bool, error) {
	return f.hasStashes, f.stashesErr
}

func (f *fakeBeadWorktreeGitProbe) WorktreeRemove(path string, force bool) error {
	f.removeInvoked = true
	f.removedPath = path
	f.removedForce = force
	return f.removeErr
}

type beadWorktreeEventRecorder struct {
	events []events.Event
}

func (r *beadWorktreeEventRecorder) Record(event events.Event) {
	r.events = append(r.events, event)
}

func gaConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
	}
}

func TestReapClosedBeadWorktreesFindsCanonicalArtifactLayout(t *testing.T) {
	cityPath := t.TempDir()
	rigRepo := filepath.Join(cityPath, "repos", "mrig")
	worktreePath := filepath.Join(cityPath, ".gc", "worktrees", "mrig", "artifacts", "worktrees", "ga-abc123")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("creating canonical worktree path: %v", err)
	}
	if err := os.MkdirAll(rigRepo, 0o755); err != nil {
		t.Fatalf("creating rig repo path: %v", err)
	}

	cfg := gaConfig()
	cfg.Rigs = []config.Rig{{Name: "mrig", Path: filepath.Join("repos", "mrig")}}
	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-abc123",
		Status: "closed",
	}}, nil)
	rigStore := beads.NewMemStore()
	worktreeProbe := &fakeBeadWorktreeGitProbe{
		isRepo: true,
		branch: "polecat/ga-abc123",
	}
	rigProbe := &fakeBeadWorktreeGitProbe{isRepo: true}

	originalFactory := newBeadWorktreeGitProbe
	t.Cleanup(func() { newBeadWorktreeGitProbe = originalFactory })
	newBeadWorktreeGitProbe = func(workDir string) beadWorktreeGitProbe {
		switch filepath.Clean(workDir) {
		case filepath.Clean(worktreePath):
			return worktreeProbe
		case filepath.Clean(rigRepo):
			return rigProbe
		default:
			return &fakeBeadWorktreeGitProbe{}
		}
	}

	var stderr bytes.Buffer
	rec := &beadWorktreeEventRecorder{}
	got := reapClosedBeadWorktrees(
		cityPath,
		cfg,
		cityStore,
		map[string]beads.Store{"mrig": rigStore},
		rec,
		&stderr,
	)
	if got != 1 {
		t.Fatalf("reapClosedBeadWorktrees = %d, want 1; stderr=%s", got, stderr.String())
	}
	if !rigProbe.removeInvoked {
		t.Fatal("configured rig repository did not remove canonical artifact worktree")
	}
	if rigProbe.removedPath != worktreePath {
		t.Errorf("WorktreeRemove path = %q, want %q", rigProbe.removedPath, worktreePath)
	}
	if rigProbe.removedForce {
		t.Error("WorktreeRemove force = true, want false")
	}
	if worktreeProbe.removeInvoked {
		t.Fatal("WorktreeRemove was invoked from the candidate worktree")
	}
	if len(rec.events) != 1 || rec.events[0].Type != events.BeadWorktreeReaped {
		t.Fatalf("events = %+v, want one %s event", rec.events, events.BeadWorktreeReaped)
	}
}

func TestReapClosedBeadWorktreesDuplicateStoreRowsFailClosed(t *testing.T) {
	cityPath := t.TempDir()
	rigRepo := filepath.Join(cityPath, "repos", "mrig")
	worktreePath := filepath.Join(cityPath, ".gc", "worktrees", "mrig", "artifacts", "worktrees", "ga-abc123")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("creating canonical worktree path: %v", err)
	}

	cfg := gaConfig()
	cfg.Rigs = []config.Rig{{Name: "mrig", Path: rigRepo}}
	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-abc123",
		Status: "closed",
	}}, nil)
	rigStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-abc123",
		Status: "open",
	}}, nil)
	worktreeProbe := &fakeBeadWorktreeGitProbe{
		isRepo: true,
		branch: "polecat/ga-abc123",
	}
	rigProbe := &fakeBeadWorktreeGitProbe{isRepo: true}

	originalFactory := newBeadWorktreeGitProbe
	t.Cleanup(func() { newBeadWorktreeGitProbe = originalFactory })
	newBeadWorktreeGitProbe = func(workDir string) beadWorktreeGitProbe {
		if filepath.Clean(workDir) == filepath.Clean(worktreePath) {
			return worktreeProbe
		}
		if filepath.Clean(workDir) == filepath.Clean(rigRepo) {
			return rigProbe
		}
		return &fakeBeadWorktreeGitProbe{}
	}

	var stderr bytes.Buffer
	rec := &beadWorktreeEventRecorder{}
	if got := reapClosedBeadWorktrees(
		cityPath,
		cfg,
		cityStore,
		map[string]beads.Store{"mrig": rigStore},
		rec,
		&stderr,
	); got != 0 {
		t.Fatalf("reapClosedBeadWorktrees = %d with duplicate rows, want 0", got)
	}
	if worktreeProbe.removeInvoked || rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove invoked despite duplicate HQ/rig bead rows")
	}
	if !strings.Contains(stderr.String(), "duplicate bead rows") {
		t.Fatalf("stderr = %q, want duplicate-row reason", stderr.String())
	}
	if len(rec.events) != 1 || rec.events[0].Type != events.BeadWorktreeReapSkipped {
		t.Fatalf("events = %+v, want one %s event", rec.events, events.BeadWorktreeReapSkipped)
	}
}

func TestReapClosedBeadWorktreesProbeErrorsFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		probe      *fakeBeadWorktreeGitProbe
		wantReason string
	}{
		{
			name: "unpushed",
			probe: &fakeBeadWorktreeGitProbe{
				isRepo:      true,
				unpushedErr: errors.New("upstream unavailable"),
			},
			wantReason: "unpushed probe failed",
		},
		{
			name: "stashes",
			probe: &fakeBeadWorktreeGitProbe{
				isRepo:     true,
				stashesErr: errors.New("stash metadata unreadable"),
			},
			wantReason: "stash probe failed",
		},
		{
			name: "branch",
			probe: &fakeBeadWorktreeGitProbe{
				isRepo:    true,
				branchErr: errors.New("head unreadable"),
			},
			wantReason: "branch probe failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			rigRepo := filepath.Join(cityPath, "repos", "mrig")
			worktreePath := filepath.Join(cityPath, ".gc", "worktrees", "mrig", "artifacts", "worktrees", "ga-abc123")
			if err := os.MkdirAll(worktreePath, 0o755); err != nil {
				t.Fatalf("creating canonical worktree path: %v", err)
			}

			cfg := gaConfig()
			cfg.Rigs = []config.Rig{{Name: "mrig", Path: rigRepo}}
			cityStore := beads.NewMemStoreFrom(1, []beads.Bead{{
				ID:     "ga-abc123",
				Status: "closed",
			}}, nil)
			rigStore := beads.NewMemStore()
			rigProbe := &fakeBeadWorktreeGitProbe{isRepo: true}

			originalFactory := newBeadWorktreeGitProbe
			t.Cleanup(func() { newBeadWorktreeGitProbe = originalFactory })
			newBeadWorktreeGitProbe = func(workDir string) beadWorktreeGitProbe {
				if filepath.Clean(workDir) == filepath.Clean(worktreePath) {
					return tc.probe
				}
				if filepath.Clean(workDir) == filepath.Clean(rigRepo) {
					return rigProbe
				}
				return &fakeBeadWorktreeGitProbe{}
			}

			var stderr bytes.Buffer
			rec := &beadWorktreeEventRecorder{}
			if got := reapClosedBeadWorktrees(
				cityPath,
				cfg,
				cityStore,
				map[string]beads.Store{"mrig": rigStore},
				rec,
				&stderr,
			); got != 0 {
				t.Fatalf("reapClosedBeadWorktrees = %d after probe error, want 0", got)
			}
			if rigProbe.removeInvoked {
				t.Fatal("WorktreeRemove invoked after inconclusive safety probe")
			}
			if !strings.Contains(stderr.String(), tc.wantReason) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.wantReason)
			}
			if len(rec.events) != 1 || rec.events[0].Type != events.BeadWorktreeReapSkipped {
				t.Fatalf("events = %+v, want one %s event", rec.events, events.BeadWorktreeReapSkipped)
			}
		})
	}
}

func TestReapClosedBeadWorktreesRejectsCanonicalScanSymlinkEscape(t *testing.T) {
	cityPath := t.TempDir()
	rigWorktreeDir := filepath.Join(cityPath, ".gc", "worktrees", "mrig")
	if err := os.MkdirAll(rigWorktreeDir, 0o755); err != nil {
		t.Fatalf("creating rig worktree path: %v", err)
	}
	outsideArtifacts := filepath.Join(t.TempDir(), "artifacts")
	outsideWorktree := filepath.Join(outsideArtifacts, "worktrees", "ga-abc123")
	if err := os.MkdirAll(outsideWorktree, 0o755); err != nil {
		t.Fatalf("creating outside worktree path: %v", err)
	}
	if err := os.Symlink(outsideArtifacts, filepath.Join(rigWorktreeDir, "artifacts")); err != nil {
		t.Fatalf("creating canonical artifacts symlink: %v", err)
	}

	cfg := gaConfig()
	cfg.Rigs = []config.Rig{{Name: "mrig", Path: filepath.Join(cityPath, "repos", "mrig")}}
	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-abc123",
		Status: "closed",
	}}, nil)
	rigStore := beads.NewMemStore()

	probeInvoked := false
	originalFactory := newBeadWorktreeGitProbe
	t.Cleanup(func() { newBeadWorktreeGitProbe = originalFactory })
	newBeadWorktreeGitProbe = func(string) beadWorktreeGitProbe {
		probeInvoked = true
		return &fakeBeadWorktreeGitProbe{isRepo: true}
	}

	var stderr bytes.Buffer
	if got := reapClosedBeadWorktrees(
		cityPath,
		cfg,
		cityStore,
		map[string]beads.Store{"mrig": rigStore},
		events.Discard,
		&stderr,
	); got != 0 {
		t.Fatalf("reapClosedBeadWorktrees = %d for escaping canonical scan path, want 0", got)
	}
	if probeInvoked {
		t.Fatal("git probe invoked for worktree reached through escaping artifacts symlink")
	}
	if !strings.Contains(stderr.String(), "escapes worktree root") {
		t.Fatalf("stderr = %q, want realpath-containment rejection", stderr.String())
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

func TestExtractBeadIDFromWorktreeNameHyphenatedConfiguredPrefix(t *testing.T) {
	cfg := gaConfig()
	cfg.Rigs = []config.Rig{{
		Name:   "diagnostics",
		Prefix: "agent-diagnostics",
	}}
	got := extractBeadIDFromWorktreeName(cfg, "builder-agent-diagnostics-hnn-pr2738")
	if got != "agent-diagnostics-hnn" {
		t.Errorf("got %q, want %q", got, "agent-diagnostics-hnn")
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
