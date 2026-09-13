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

func TestPruneAgentHomeWorktreeIfSafeInfo_HasStashes(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasStashes: true, currentBranch: "builder/ga-ghi789"})

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if !strings.Contains(stderr.String(), "stashed work") {
		t.Errorf("expected stashes-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-ghi789", "stashed-work")
}

func TestPruneAgentHomeWorktreeIfSafeInfo_StashProbeError(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, stashesErr: errors.New("boom")})

	var stderr bytes.Buffer
	pruneAgentHomeWorktreeIfSafeInfo(fx.sessionInfo(), fx.cityPath, fx.cfg, &stderr)
	if !strings.Contains(stderr.String(), "stash probe failed") {
		t.Errorf("expected stash-error log; got %q", stderr.String())
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
