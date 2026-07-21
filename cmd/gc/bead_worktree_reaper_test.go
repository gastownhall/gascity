package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func gaConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
	}
}

func gaConfigWithRig(rigName, rigPrefix string) *config.City {
	cfg := gaConfig()
	cfg.Rigs = []config.Rig{{Name: rigName, Prefix: rigPrefix}}
	return cfg
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

// TestExtractBeadIDFromWorktreeNameHyphenatedRigPrefixBare covers a rig
// whose configured prefix is itself hyphenated (e.g. "agent-diagnostics"),
// per #4492: a fixed two-segment window never forms the 3-segment
// candidate, so such worktrees were silently never recognized.
func TestExtractBeadIDFromWorktreeNameHyphenatedRigPrefixBare(t *testing.T) {
	cfg := gaConfigWithRig("diag", "agent-diagnostics")
	got := extractBeadIDFromWorktreeName(cfg, "agent-diagnostics-h1")
	if got != "agent-diagnostics-h1" {
		t.Errorf("got %q, want %q", got, "agent-diagnostics-h1")
	}
}

func TestExtractBeadIDFromWorktreeNameHyphenatedRigPrefixCompound(t *testing.T) {
	cfg := gaConfigWithRig("diag", "agent-diagnostics")
	got := extractBeadIDFromWorktreeName(cfg, "worker-agent-diagnostics-h1")
	if got != "agent-diagnostics-h1" {
		t.Errorf("got %q, want %q", got, "agent-diagnostics-h1")
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

// reapTestFixture wires a temp city directory with one closed-bead worktree
// under .gc/worktrees/<rig>/, plus a config pointing that rig at a separate
// "rig root" directory — mirroring pruneTestFixture's shape so both use the
// same fakeGitProbe/newGitProbe injection.
type reapTestFixture struct {
	t            *testing.T
	cityPath     string
	rigRoot      string
	worktreePath string
	cfg          *config.City
	stores       map[string]beads.Store
	probesByWD   map[string]*fakeGitProbe
}

func newReapFixture(t *testing.T) *reapTestFixture {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "ga-rig")
	worktreePath := filepath.Join(cityPath, ".gc", "worktrees", "ga-rig", "ga-abc123")

	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rigRoot: %v", err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("mkdir worktreePath: %v", err)
	}

	cfg := gaConfig()
	cfg.Rigs = []config.Rig{{Name: "ga-rig", Path: rigRoot}}

	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

	fx := &reapTestFixture{
		t:            t,
		cityPath:     cityPath,
		rigRoot:      rigRoot,
		worktreePath: worktreePath,
		cfg:          cfg,
		stores:       map[string]beads.Store{"ga-rig": store},
		probesByWD:   make(map[string]*fakeGitProbe),
	}

	orig := newGitProbe
	t.Cleanup(func() { newGitProbe = orig })
	newGitProbe = func(workDir string) gitProbe {
		probe, ok := fx.probesByWD[workDir]
		if !ok {
			probe = &fakeGitProbe{isRepo: true}
			fx.probesByWD[workDir] = probe
		}
		return probe
	}

	return fx
}

func (fx *reapTestFixture) setProbe(workDir string, probe *fakeGitProbe) {
	fx.probesByWD[workDir] = probe
}

type recordingRecorder struct {
	types []string
}

func (r *recordingRecorder) Record(e events.Event) { r.types = append(r.types, e.Type) }

func TestReapClosedBeadWorktrees_RemovesFromRigRootNotCityPath(t *testing.T) {
	fx := newReapFixture(t)

	var stderr bytes.Buffer
	reaped := reapClosedBeadWorktrees(fx.cityPath, fx.cfg, fx.stores, nil, &stderr)

	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1; stderr=%s", reaped, stderr.String())
	}
	cityProbe := fx.probesByWD[fx.cityPath]
	if cityProbe != nil && cityProbe.removeInvoked {
		t.Fatal("WorktreeRemove invoked against cityPath — should target the rig root")
	}
	rigProbe := fx.probesByWD[fx.rigRoot]
	if rigProbe == nil || !rigProbe.removeInvoked {
		t.Fatal("expected WorktreeRemove invoked against the rig root")
	}
	if rigProbe.removedPath != fx.worktreePath {
		t.Fatalf("removedPath = %q, want %q", rigProbe.removedPath, fx.worktreePath)
	}
}

func TestReapClosedBeadWorktrees_RigRootUnresolvedSkipsRemoval(t *testing.T) {
	fx := newReapFixture(t)
	fx.cfg.Rigs = nil // no rig configured — root can't be resolved

	var stderr bytes.Buffer
	reaped := reapClosedBeadWorktrees(fx.cityPath, fx.cfg, fx.stores, nil, &stderr)

	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0 (rig root unresolved)", reaped)
	}
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove invoked despite unresolved rig root")
	}
}

func TestReapClosedBeadWorktrees_UnpushedProbeErrorSkipsReap(t *testing.T) {
	fx := newReapFixture(t)
	fx.setProbe(fx.worktreePath, &fakeGitProbe{isRepo: true, unpushedErr: errors.New("probe failed")})

	rec := &recordingRecorder{}
	var stderr bytes.Buffer
	reaped := reapClosedBeadWorktrees(fx.cityPath, fx.cfg, fx.stores, rec, &stderr)

	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0 (unpushed probe errored)", reaped)
	}
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove invoked despite unpushed probe error")
	}
	if len(rec.types) != 1 || rec.types[0] != events.BeadWorktreeReapSkipped {
		t.Fatalf("recorded events = %v, want exactly one BeadWorktreeReapSkipped", rec.types)
	}
}

func TestReapClosedBeadWorktrees_StashProbeErrorSkipsReap(t *testing.T) {
	fx := newReapFixture(t)
	fx.setProbe(fx.worktreePath, &fakeGitProbe{isRepo: true, stashesErr: errors.New("probe failed")})

	var stderr bytes.Buffer
	reaped := reapClosedBeadWorktrees(fx.cityPath, fx.cfg, fx.stores, nil, &stderr)

	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0 (stash probe errored)", reaped)
	}
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove invoked despite stash probe error")
	}
}
