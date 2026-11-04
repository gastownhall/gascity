package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// sessionInfo returns a session.Info mirroring fx.sessionBead(), for
// exercising the session.Info form of the worker_dir auto-prune path
// (pruneAgentHomeWorktreeIfSafeInfo) against the same fixture.
func (fx *pruneTestFixture) sessionInfo() sessionpkg.Info {
	return sessionpkg.Info{
		ID:                  "session-1",
		WorkerDir:           fx.workerDir,
		Template:            "demo/polecat",
		SessionNameMetadata: "demo/polecat-3",
	}
}

func TestPruneAgentHomeWorktreeIfSafeInfo_DisabledByConfig(t *testing.T) {
	fx := newPruneFixture(t)
	off := false
	fx.cfg.Daemon.AutoPruneWorkerDir = &off

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called while config disabled prune")
	}
}

func TestPruneAgentHomeWorktreeIfSafeInfo_NoWorkerDir(t *testing.T) {
	fx := newPruneFixture(t)
	info := fx.sessionInfo()
	info.WorkerDir = ""

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(info, fx.cityPath, fx.cfg, &stderr)
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called with no worker_dir")
	}
}

func TestPruneAgentHomeWorktreeIfSafeInfo_LegacyWorkDirKey(t *testing.T) {
	fx := newPruneFixture(t)
	info := fx.sessionInfo()
	info.WorkerDir = ""
	info.WorkDir = fx.workerDir

	probe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.workerDir, probe)
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(info, fx.cityPath, fx.cfg, &stderr)
	if !rigProbe.removeInvoked || rigProbe.removedPath != fx.workerDir || !rigProbe.removedForce {
		t.Fatalf("expected WorktreeRemove(%q, true) on rig root; got invoked=%v path=%q force=%v",
			fx.workerDir, rigProbe.removeInvoked, rigProbe.removedPath, rigProbe.removedForce)
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafeInfo_OutsideWorktreesTree(t *testing.T) {
	fx := newPruneFixture(t)
	info := fx.sessionInfo()
	outside := filepath.Join(fx.cityPath, "elsewhere", "polecat-3")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	info.WorkerDir = outside

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(info, fx.cityPath, fx.cfg, &stderr)
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called for path outside .gc/worktrees")
	}
}

func TestPruneAgentHomeWorktreeIfSafeInfo_RejectsWorktreesRoot(t *testing.T) {
	fx := newPruneFixture(t)
	info := fx.sessionInfo()
	wtRoot := filepath.Join(fx.cityPath, ".gc", "worktrees")
	if err := os.WriteFile(filepath.Join(wtRoot, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git on wtRoot: %v", err)
	}
	info.WorkerDir = wtRoot

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(info, fx.cityPath, fx.cfg, &stderr)
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called for .gc/worktrees root itself")
	}
}

func TestPruneAgentHomeWorktreeIfSafeInfo_RelativeWorkerDir(t *testing.T) {
	fx := newPruneFixture(t)
	info := fx.sessionInfo()
	info.WorkerDir = "relative/path"

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(info, fx.cityPath, fx.cfg, &stderr)
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called for relative worker_dir")
	}
}

func TestPruneAgentHomeWorktreeIfSafeInfo_MissingDotGit(t *testing.T) {
	fx := newPruneFixture(t)
	if err := os.Remove(filepath.Join(fx.workerDir, ".git")); err != nil {
		t.Fatalf("remove .git: %v", err)
	}

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called with missing .git pointer")
	}
}

func TestPruneAgentHomeWorktreeIfSafeInfo_NotARepo(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: false})

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafeInfo_HasUncommitted(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUncommitted: true, currentBranch: "builder/ga-abc123"})

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if !strings.Contains(stderr.String(), "uncommitted changes") {
		t.Errorf("expected uncommitted-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-abc123", "uncommitted-work")
}

func TestPruneAgentHomeWorktreeIfSafeInfo_HasUnpushed(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUnpushed: true, currentBranch: "builder/ga-def456"})

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if !strings.Contains(stderr.String(), "unpushed commits") {
		t.Errorf("expected unpushed-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-def456", "unpushed-commits")
}

func TestPruneAgentHomeWorktreeIfSafeInfo_UnpushedProbeError(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, unpushedErr: errors.New("boom")})

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if !strings.Contains(stderr.String(), "unpushed probe failed") {
		t.Errorf("expected unpushed-error log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafeInfo_RigPathUnresolved(t *testing.T) {
	fx := newPruneFixture(t)
	fx.cfg.Rigs = nil
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if !strings.Contains(stderr.String(), "rig path unresolved") {
		t.Errorf("expected rig-unresolved log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafeInfo_RemoveFails(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})
	fx.setProbe(fx.rigRoot, &fakeGitProbe{
		isRepo:         true,
		worktreeRemove: func(_ string, _ bool) error { return errors.New("locked") },
	})

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if !strings.Contains(stderr.String(), "pruning worker_dir") || !strings.Contains(stderr.String(), "locked") {
		t.Errorf("expected removal-error log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafeInfo_HappyPath(t *testing.T) {
	fx := newPruneFixture(t)
	wdProbe := &fakeGitProbe{isRepo: true}
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.workerDir, wdProbe)
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if wdProbe.removeInvoked {
		t.Error("WorktreeRemove invoked on worker_dir; should be invoked on rig root only")
	}
	if !rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove not invoked on rig root")
	}
	if rigProbe.removedPath != fx.workerDir {
		t.Errorf("WorktreeRemove path = %q, want %q", rigProbe.removedPath, fx.workerDir)
	}
	if !rigProbe.removedForce {
		t.Error("WorktreeRemove force flag = false, want true")
	}
	if !strings.Contains(stderr.String(), "pruned worker_dir") {
		t.Errorf("expected success log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafeInfo_UsesConfiguredWorktreesRoot(t *testing.T) {
	fx := newPruneFixture(t)
	configuredRoot := filepath.Join(fx.cityPath, "configured-worktrees")
	configuredWorker := filepath.Join(configuredRoot, "demo", "polecats", "polecat-3")
	t.Setenv("GC_WORKTREES_DIR", configuredRoot)
	if err := os.MkdirAll(configuredWorker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configuredWorker, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := fx.sessionInfo()
	info.WorkerDir = configuredWorker
	fx.setProbe(configuredWorker, &fakeGitProbe{isRepo: true})
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(info, fx.cityPath, fx.cfg, &stderr)
	if !rigProbe.removeInvoked || rigProbe.removedPath != configuredWorker {
		t.Fatalf("WorktreeRemove path = %q, invoked=%v; stderr=%q", rigProbe.removedPath, rigProbe.removeInvoked, stderr.String())
	}
}

// managedSeatInfo is the closing pool seat of ga-tlus3e.22: the controller
// started it inside a work bead's verified managed worktree, so both
// work-dir keys of its row record that worktree's path.
func managedSeatInfo(path string) sessionpkg.Info {
	return sessionpkg.Info{
		ID:                  "session-1",
		WorkDir:             path,
		WorkDirCanonical:    path,
		Template:            "demo/worker",
		SessionNameMetadata: "demo/worker-1",
	}
}

// TestPruneAgentHomeWorktreeIfSafeInfo_LeavesManagedWorktreeOfAnotherOwner
// reproduces ga-tlus3e.22: a pool seat's work dir is a work bead's managed
// worktree whose provenance names another owner, and the seat closes. The
// worktree is clean and pushed, so before the ownership gate the prune
// force-removed it, leaving the owner only its branch.
func TestPruneAgentHomeWorktreeIfSafeInfo_LeavesManagedWorktreeOfAnotherOwner(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot, spec := managedPruneWorktree(t, cityPath)
	cfg := &config.City{Rigs: []config.Rig{{Name: "demo", Path: rigRoot}}}

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(managedSeatInfo(spec.Path), cityPath, cfg, &stderr)

	assertManagedWorktreeIntact(t, spec, stderr.String())
	if !strings.Contains(stderr.String(), "managed worktree") || !strings.Contains(stderr.String(), spec.Owner) {
		t.Errorf("expected a managed-worktree refusal naming owner %q; got %q", spec.Owner, stderr.String())
	}
}

// TestPruneAgentHomeWorktreeIfSafeInfo_LeavesDirtyManagedWorktreeUnmarked
// pins that the ownership gate runs before the git safety probes: a closing
// seat writes no .worktree-stale marker into another owner's checkout,
// which would add an untracked file to it.
func TestPruneAgentHomeWorktreeIfSafeInfo_LeavesDirtyManagedWorktreeUnmarked(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot, spec := managedPruneWorktree(t, cityPath)
	cfg := &config.City{Rigs: []config.Rig{{Name: "demo", Path: rigRoot}}}
	wip := filepath.Join(spec.Path, "wip.txt")
	if err := os.WriteFile(wip, []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("write wip: %v", err)
	}

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(managedSeatInfo(spec.Path), cityPath, cfg, &stderr)

	assertManagedWorktreeIntact(t, spec, stderr.String())
	assertNoWorktreeStaleMarker(t, spec.Path)
	if _, err := os.Stat(wip); err != nil {
		t.Fatalf("owner's uncommitted file is gone: %v", err)
	}
}

// TestPruneAgentHomeWorktreeIfSafeInfo_UnresolvedGitDirFailsClosed pins
// that a worker_dir whose git dir cannot be resolved is left alone: without
// it the prune cannot tell whether a provisioner owns the worktree.
func TestPruneAgentHomeWorktreeIfSafeInfo_UnresolvedGitDirFailsClosed(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, gitDirErr: errors.New("rev-parse boom")})
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called although the worker_dir's ownership could not be read")
	}
	if !strings.Contains(stderr.String(), "rev-parse boom") {
		t.Errorf("expected git-dir error in log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

// TestPruneAgentHomeWorktreeIfSafeInfo_UnreadableProvenanceFailsClosed pins
// that only a provenance record that does not exist marks a worker_dir as
// unmanaged; one that cannot be read keeps the worktree.
func TestPruneAgentHomeWorktreeIfSafeInfo_UnreadableProvenanceFailsClosed(t *testing.T) {
	fx := newPruneFixture(t)
	// A git dir that is a regular file: reading any record inside it fails
	// with something other than "does not exist".
	notADir := filepath.Join(fx.cityPath, "git-dir-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write git dir file: %v", err)
	}
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, gitDir: notADir})
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called although the worker_dir's provenance could not be read")
	}
	if !strings.Contains(stderr.String(), "not pruning worker_dir") {
		t.Errorf("expected refusal log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafeInfo_NilConfig(t *testing.T) {
	fx := newPruneFixture(t)
	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, nil, &stderr)
}

func TestLookupRigRootForSessionInfo(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "demo", Path: "/x/demo"},
			{Name: "other", Path: "/x/other"},
		},
	}
	cases := []struct {
		name     string
		template string
		want     string
	}{
		{"qualified match", "demo/polecat", "/x/demo"},
		{"other rig", "other/refinery", "/x/other"},
		{"unqualified", "polecat", ""},
		{"unknown rig", "missing/polecat", ""},
		{"empty", "", ""},
		{"leading slash", "/polecat", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := sessionpkg.Info{Template: c.template}
			got := lookupRigRootForSessionInfo(info, cfg)
			if got != c.want {
				t.Errorf("lookupRigRootForSessionInfo(%q) = %q, want %q", c.template, got, c.want)
			}
		})
	}
}
