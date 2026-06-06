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
)

// fakeGitProbe is a hand-rolled gitProbe stub. Each field controls one
// probe; WorktreeRemoveErr controls the destructive call, and removed
// records the (path, force) of every WorktreeRemove invocation so tests
// can assert which directory the removal targeted.
type fakeGitProbe struct {
	isRepo         bool
	hasUncommitted bool
	hasUnpushed    bool
	unpushedErr    error
	hasStashes     bool
	stashesErr     error
	worktreeRemove func(path string, force bool) error
	removedPath    string
	removedForce   bool
	removeInvoked  bool
}

func (f *fakeGitProbe) IsRepo() bool             { return f.isRepo }
func (f *fakeGitProbe) HasUncommittedWork() bool { return f.hasUncommitted }
func (f *fakeGitProbe) HasUnpushedCommitsResult() (bool, error) {
	return f.hasUnpushed, f.unpushedErr
}
func (f *fakeGitProbe) HasStashesResult() (bool, error) { return f.hasStashes, f.stashesErr }
func (f *fakeGitProbe) WorktreeRemove(path string, force bool) error {
	f.removeInvoked = true
	f.removedPath = path
	f.removedForce = force
	if f.worktreeRemove != nil {
		return f.worktreeRemove(path, force)
	}
	return nil
}

// pruneTestFixture wires a temp city directory, a writable worker_dir
// under .gc/worktrees with a .git marker, and a config with a rig that
// points at a separate "rig root" directory the prune call uses as the
// git -C target.
type pruneTestFixture struct {
	t          *testing.T
	cityPath   string
	rigRoot    string
	workerDir  string
	cfg        *config.City
	probesByWD map[string]*fakeGitProbe
}

func newPruneFixture(t *testing.T) *pruneTestFixture {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	workerDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "polecats", "polecat-3")

	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rigRoot: %v", err)
	}
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatalf("mkdir workerDir: %v", err)
	}
	// .git marker so the existence check passes; contents don't matter
	// because gitProbe is faked.
	if err := os.WriteFile(filepath.Join(workerDir, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git marker: %v", err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "demo", Path: rigRoot},
		},
	}

	fx := &pruneTestFixture{
		t:          t,
		cityPath:   cityPath,
		rigRoot:    rigRoot,
		workerDir:  workerDir,
		cfg:        cfg,
		probesByWD: make(map[string]*fakeGitProbe),
	}

	orig := newGitProbe
	t.Cleanup(func() { newGitProbe = orig })
	newGitProbe = func(workDir string) gitProbe {
		probe, ok := fx.probesByWD[workDir]
		if !ok {
			// Default: a healthy probe. The WorktreeRemove call goes
			// through this default for the rig-root invocation.
			probe = &fakeGitProbe{isRepo: true}
			fx.probesByWD[workDir] = probe
		}
		return probe
	}

	return fx
}

func (fx *pruneTestFixture) setProbe(workDir string, probe *fakeGitProbe) {
	fx.probesByWD[workDir] = probe
}

func (fx *pruneTestFixture) sessionBead() beads.Bead {
	return beads.Bead{
		ID: "session-1",
		Metadata: map[string]string{
			"worker_dir":   fx.workerDir,
			"template":     "demo/polecat",
			"session_name": "demo/polecat-3",
		},
	}
}

func TestPruneAgentHomeWorktreeIfSafe_DisabledByConfig(t *testing.T) {
	fx := newPruneFixture(t)
	off := false
	fx.cfg.Daemon.AutoPruneWorkerDir = &off

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true while disabled")
	}
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called while config disabled prune")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_NoWorkerDir(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	delete(session.Metadata, "worker_dir")

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with no worker_dir")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_LegacyWorkDirKey(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	delete(session.Metadata, "worker_dir")
	session.Metadata["work_dir"] = fx.workerDir

	probe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.workerDir, probe)
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatalf("prune returned false on legacy work_dir; stderr=%s", stderr.String())
	}
	if !rigProbe.removeInvoked || rigProbe.removedPath != fx.workerDir || !rigProbe.removedForce {
		t.Fatalf("expected WorktreeRemove(%q, true) on rig root; got invoked=%v path=%q force=%v",
			fx.workerDir, rigProbe.removeInvoked, rigProbe.removedPath, rigProbe.removedForce)
	}
}

func TestPruneAgentHomeWorktreeIfSafe_OutsideWorktreesTree(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	outside := filepath.Join(fx.cityPath, "elsewhere", "polecat-3")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	session.Metadata["worker_dir"] = outside

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true for path outside .gc/worktrees")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RejectsWorktreesRoot(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	wtRoot := filepath.Join(fx.cityPath, ".gc", "worktrees")
	if err := os.WriteFile(filepath.Join(wtRoot, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git on wtRoot: %v", err)
	}
	session.Metadata["worker_dir"] = wtRoot

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true for .gc/worktrees root itself")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RelativeWorkerDir(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	session.Metadata["worker_dir"] = "relative/path"

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true for relative worker_dir")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_MissingDotGit(t *testing.T) {
	fx := newPruneFixture(t)
	if err := os.Remove(filepath.Join(fx.workerDir, ".git")); err != nil {
		t.Fatalf("remove .git: %v", err)
	}

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with missing .git pointer")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_NotARepo(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: false})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true when IsRepo=false")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_HasUncommitted(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUncommitted: true})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with uncommitted work")
	}
	if !strings.Contains(stderr.String(), "uncommitted changes") {
		t.Errorf("expected uncommitted-reason log; got %q", stderr.String())
	}
}

func TestPruneAgentHomeWorktreeIfSafe_HasUnpushed(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUnpushed: true})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with unpushed commits")
	}
	if !strings.Contains(stderr.String(), "unpushed commits") {
		t.Errorf("expected unpushed-reason log; got %q", stderr.String())
	}
}

func TestPruneAgentHomeWorktreeIfSafe_UnpushedProbeError(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, unpushedErr: errors.New("boom")})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true after unpushed probe error")
	}
	if !strings.Contains(stderr.String(), "unpushed probe failed") {
		t.Errorf("expected unpushed-error log; got %q", stderr.String())
	}
}

func TestPruneAgentHomeWorktreeIfSafe_HasStashes(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasStashes: true})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with stashes")
	}
	if !strings.Contains(stderr.String(), "stashed work") {
		t.Errorf("expected stashes-reason log; got %q", stderr.String())
	}
}

func TestPruneAgentHomeWorktreeIfSafe_StashProbeError(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, stashesErr: errors.New("boom")})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true after stash probe error")
	}
	if !strings.Contains(stderr.String(), "stash probe failed") {
		t.Errorf("expected stash-error log; got %q", stderr.String())
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RigPathUnresolved(t *testing.T) {
	fx := newPruneFixture(t)
	fx.cfg.Rigs = nil
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true without rig path")
	}
	if !strings.Contains(stderr.String(), "rig path unresolved") {
		t.Errorf("expected rig-unresolved log; got %q", stderr.String())
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RigPathEmpty(t *testing.T) {
	fx := newPruneFixture(t)
	fx.cfg.Rigs = []config.Rig{{Name: "demo", Path: ""}}
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with empty rig path")
	}
	if !strings.Contains(stderr.String(), "rig path unresolved") {
		t.Errorf("expected rig-unresolved log; got %q", stderr.String())
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RemoveFails(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})
	fx.setProbe(fx.rigRoot, &fakeGitProbe{
		isRepo:         true,
		worktreeRemove: func(_ string, _ bool) error { return errors.New("locked") },
	})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true when WorktreeRemove failed")
	}
	if !strings.Contains(stderr.String(), "pruning worker_dir") || !strings.Contains(stderr.String(), "locked") {
		t.Errorf("expected removal-error log; got %q", stderr.String())
	}
}

func TestPruneAgentHomeWorktreeIfSafe_HappyPath(t *testing.T) {
	fx := newPruneFixture(t)
	wdProbe := &fakeGitProbe{isRepo: true}
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.workerDir, wdProbe)
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatalf("prune returned false on happy path; stderr=%s", stderr.String())
	}
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
}

func TestPruneAgentHomeWorktreeIfSafe_NilConfig(t *testing.T) {
	fx := newPruneFixture(t)
	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, nil, &stderr) {
		t.Fatal("prune returned true with nil cfg")
	}
}

func TestLookupRigRootForSession(t *testing.T) {
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
			bead := beads.Bead{Metadata: map[string]string{"template": c.template}}
			got := lookupRigRootForSession(bead, cfg)
			if got != c.want {
				t.Errorf("lookupRigRootForSession(%q) = %q, want %q", c.template, got, c.want)
			}
		})
	}
}
