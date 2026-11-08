package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/worktree"
)

// fakeGitProbe is a hand-rolled gitProbe stub. Each field controls one
// probe; WorktreeRemoveErr controls the destructive call, and removed
// records the (path, force) of every WorktreeRemove invocation so tests
// can assert which directory the removal targeted. gitDir is the private
// git directory the prune reads worktree provenance from.
type fakeGitProbe struct {
	isRepo             bool
	gitDir             string
	gitDirErr          error
	currentBranch      string
	currentBranchErr   error
	hasUncommitted     bool
	hasUnpushed        bool
	unpushedErr        error
	statusPorcelain    string
	statusPorcelainErr error
	worktreeRemove     func(path string, force bool) error
	removedPath        string
	removedForce       bool
	removeInvoked      bool
}

func (f *fakeGitProbe) IsRepo() bool { return f.isRepo }
func (f *fakeGitProbe) GitDir() (string, error) {
	return f.gitDir, f.gitDirErr
}

func (f *fakeGitProbe) CurrentBranch() (string, error) {
	return f.currentBranch, f.currentBranchErr
}
func (f *fakeGitProbe) HasUncommittedWork() bool { return f.hasUncommitted }
func (f *fakeGitProbe) HasUnpushedCommitsResult() (bool, error) {
	return f.hasUnpushed, f.unpushedErr
}

func (f *fakeGitProbe) StatusPorcelain() (string, error) {
	return f.statusPorcelain, f.statusPorcelainErr
}

func (f *fakeGitProbe) WorktreeRemove(path string, force bool) error {
	f.removeInvoked = true
	f.removedPath = path
	f.removedForce = force
	if f.worktreeRemove != nil {
		return f.worktreeRemove(path, force)
	}
	return nil
}

// assertWorktreeStaleMarker fails the test unless workerDir contains a
// .worktree-stale marker satisfying the full contract established by
// packs/gastown/scripts/worktree-setup.sh's write_stale_marker: the
// machine-readable header (branch=, worktree=, reason=, blocking=, at=)
// followed by a blank line and self-contained WHAT HAPPENED / WHAT TO DO
// prose, with the header preceding the prose. Shared by both the raw and
// session.Info test forms.
func assertWorktreeStaleMarker(t *testing.T, workerDir, wantBranch, wantReason string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workerDir, worktreeStaleFileName))
	if err != nil {
		t.Fatalf("reading %s marker: %v", worktreeStaleFileName, err)
	}
	content := string(data)
	if !strings.Contains(content, "branch="+wantBranch) {
		t.Errorf("marker content = %q, want to contain %q", content, "branch="+wantBranch)
	}
	if !strings.Contains(content, "worktree="+workerDir) {
		t.Errorf("marker content = %q, want to contain %q", content, "worktree="+workerDir)
	}
	if !strings.Contains(content, "reason="+wantReason) {
		t.Errorf("marker content = %q, want to contain %q", content, "reason="+wantReason)
	}
	if !strings.Contains(content, "blocking=no") {
		t.Errorf("marker content = %q, want to contain %q", content, "blocking=no")
	}
	if !strings.Contains(content, "written-by=session-reconciler") {
		t.Errorf("marker content = %q, want to contain %q", content, "written-by=session-reconciler")
	}

	var atLine string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "at=") {
			atLine = strings.TrimPrefix(line, "at=")
			break
		}
	}
	if atLine == "" {
		t.Errorf("marker content = %q, want an at= header line", content)
	} else if _, err := time.Parse(time.RFC3339, atLine); err != nil {
		t.Errorf("at= value %q is not RFC3339: %v", atLine, err)
	}

	if !strings.Contains(content, "WHAT HAPPENED") {
		t.Errorf("marker content = %q, want a WHAT HAPPENED section", content)
	}
	if !strings.Contains(content, "WHAT TO DO") {
		t.Errorf("marker content = %q, want a WHAT TO DO section", content)
	}
	reasonIdx := strings.Index(content, "reason=")
	whatIdx := strings.Index(content, "WHAT HAPPENED")
	if reasonIdx == -1 || whatIdx == -1 || reasonIdx > whatIdx {
		t.Errorf("marker content = %q, want header (reason=) to precede WHAT HAPPENED prose", content)
	}
}

// assertNoWorktreeStaleMarker fails the test if workerDir contains a
// .worktree-stale marker.
func assertNoWorktreeStaleMarker(t *testing.T, workerDir string) {
	t.Helper()
	if _, err := os.ReadFile(filepath.Join(workerDir, worktreeStaleFileName)); err == nil {
		t.Errorf("%s marker unexpectedly present in %s", worktreeStaleFileName, workerDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking %s marker: %v", worktreeStaleFileName, err)
	}
}

// pruneTestFixture wires a temp city directory, a writable worker_dir
// under .gc/worktrees with a .git marker, and a config with a rig that
// points at a separate "rig root" directory the prune call uses as the
// git -C target. gitDir is the worker_dir's private git directory: it
// holds no worktree provenance, so the worker_dir is an unmanaged
// worktree unless a test says otherwise.
type pruneTestFixture struct {
	t          *testing.T
	cityPath   string
	rigRoot    string
	workerDir  string
	gitDir     string
	cfg        *config.City
	probesByWD map[string]*fakeGitProbe
}

func newPruneFixture(t *testing.T) *pruneTestFixture {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	workerDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "polecats", "polecat-3")
	gitDir := filepath.Join(rigRoot, ".git", "worktrees", "polecat-3")

	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rigRoot: %v", err)
	}
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatalf("mkdir workerDir: %v", err)
	}
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir gitDir: %v", err)
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
		gitDir:     gitDir,
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
			probe = &fakeGitProbe{isRepo: true, gitDir: fx.gitDir}
			fx.probesByWD[workDir] = probe
		}
		return probe
	}

	return fx
}

// setProbe installs probe for workDir. A probe that names no git dir and
// no git dir error reads the fixture's provenance-free gitDir.
func (fx *pruneTestFixture) setProbe(workDir string, probe *fakeGitProbe) {
	if probe.gitDir == "" && probe.gitDirErr == nil {
		probe.gitDir = fx.gitDir
	}
	fx.probesByWD[workDir] = probe
}

// managedPruneWorktree publishes a real managed worktree under cityPath's
// worktrees root for rig "demo", as `gc worktree ensure` does for a
// provisioner that is not the session: its provenance names owner
// "provisioner" and bead gc-work. Its checkout is clean and its HEAD is
// pushed, so every git safety gate of the prune passes and only its
// ownership can keep it. It returns the rig root and the spec the owner
// holds, BaseSHA included, which worktree.Verify accepts while the
// worktree is intact.
func managedPruneWorktree(t *testing.T, cityPath string) (string, worktree.Spec) {
	t.Helper()
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	rigRoot := t.TempDir()
	runGit(t, rigRoot, "clone", bare, ".")
	runGit(t, rigRoot, "config", "user.email", "test@test.com")
	runGit(t, rigRoot, "config", "user.name", "Test")
	runGit(t, rigRoot, "commit", "--allow-empty", "-m", "init")
	runGit(t, rigRoot, "push", "origin", "HEAD")
	base := strings.TrimSpace(runGit(t, rigRoot, "rev-parse", "--abbrev-ref", "HEAD"))

	root := filepath.Join(cityPath, ".gc", "worktrees", "demo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir worktree root: %v", err)
	}
	spec := worktree.Spec{
		RepoDir: rigRoot, Root: root, Path: filepath.Join(root, "gc-work-g1"),
		Branch: "work/gc-work-g1", Base: base,
		BeadID: "gc-work", StoreRef: "rig:demo", Creator: "provisioner", Owner: "provisioner",
		Generation: "g1", Lifecycle: worktree.LifecycleActive,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure managed worktree: %v", err)
	}
	spec.BaseSHA = report.Provenance.BaseSHA
	if hasUnpushed, err := git.New(spec.Path).HasUnpushedCommitsResult(); err != nil || hasUnpushed {
		t.Fatalf("test setup invalid: managed worktree must probe pushed so only ownership keeps it; unpushed=%v err=%v", hasUnpushed, err)
	}
	return rigRoot, spec
}

// assertManagedWorktreeIntact fails the test unless the owner's worktree
// still verifies against the spec the owner holds: present, registered,
// on its branch, and carrying its provenance.
func assertManagedWorktreeIntact(t *testing.T, spec worktree.Spec, stderr string) {
	t.Helper()
	if _, err := worktree.Verify(spec); err != nil {
		t.Fatalf("closing session's prune removed or damaged a worktree another owner holds: %v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "pruned worker_dir") {
		t.Fatalf("prune reported removing a worktree another owner holds; stderr=%q", stderr)
	}
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
	assertNoWorktreeStaleMarker(t, fx.workerDir)
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

func TestPruneAgentHomeWorktreeIfSafe_UsesConfiguredWorktreesRoot(t *testing.T) {
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
	session := fx.sessionBead()
	session.Metadata["worker_dir"] = configuredWorker
	fx.setProbe(configuredWorker, &fakeGitProbe{isRepo: true})
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.rigRoot, rigProbe)

	if !pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, io.Discard) {
		t.Fatal("configured-root worktree was not pruned")
	}
	if !rigProbe.removeInvoked || rigProbe.removedPath != configuredWorker {
		t.Fatalf("WorktreeRemove path = %q, invoked=%v", rigProbe.removedPath, rigProbe.removeInvoked)
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
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_HasUncommitted(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUncommitted: true, currentBranch: "builder/ga-abc123"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with uncommitted work")
	}
	if !strings.Contains(stderr.String(), "uncommitted changes") {
		t.Errorf("expected uncommitted-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-abc123", "uncommitted-work")
}

func TestPruneAgentHomeWorktreeIfSafe_HasUnpushed(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUnpushed: true, currentBranch: "builder/ga-def456"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with unpushed commits")
	}
	if !strings.Contains(stderr.String(), "unpushed commits") {
		t.Errorf("expected unpushed-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-def456", "unpushed-commits")
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
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

// TestPruneAgentHomeWorktreeIfSafe_UnrelatedStashDoesNotBlock is a real-git
// regression test for the repo-global stash aliasing bug (ga-pyp2oh):
// `git stash list` reads refs/stash from the shared git admin dir, so its
// result used to be identical for every worktree of the same repo
// regardless of which worktree's own state was probed. pruneAgentHomeWorktreeIfSafe
// no longer probes stashes at all; this test exercises a real repo to prove
// an unrelated stash elsewhere in the shared repo never blocks reclaim.
func TestPruneAgentHomeWorktreeIfSafe_UnrelatedStashDoesNotBlock(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	rigRoot := t.TempDir()
	runGit(t, rigRoot, "clone", bare, ".")
	runGit(t, rigRoot, "config", "user.email", "test@test.com")
	runGit(t, rigRoot, "config", "user.name", "Test")
	runGit(t, rigRoot, "commit", "--allow-empty", "-m", "init")
	runGit(t, rigRoot, "push", "origin", "HEAD")

	// A stash rooted at the rig root — simulating some unrelated agent
	// session having stashed work anywhere in the shared repo.
	if err := os.WriteFile(filepath.Join(rigRoot, "wip.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatalf("write wip file: %v", err)
	}
	runGit(t, rigRoot, "add", "wip.txt")
	runGit(t, rigRoot, "stash")

	cityPath := t.TempDir()
	workerDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "polecat-1")
	if err := os.MkdirAll(filepath.Dir(workerDir), 0o755); err != nil {
		t.Fatalf("mkdir worktree parent: %v", err)
	}
	runGit(t, rigRoot, "worktree", "add", "-b", "polecat-1-branch", workerDir)

	// Sanity: prove the aliasing precondition. The stash created at
	// rigRoot must be visible from workerDir too, or this test proves
	// nothing about the bug.
	if out := runGit(t, workerDir, "stash", "list"); strings.TrimSpace(out) == "" {
		t.Fatalf("test setup invalid: rig-root stash not visible from workerDir — aliasing precondition not met")
	}

	cfg := &config.City{Rigs: []config.Rig{{Name: "demo", Path: rigRoot}}}
	session := beads.Bead{
		ID: "session-1",
		Metadata: map[string]string{
			"worker_dir":   workerDir,
			"template":     "demo/polecat",
			"session_name": "demo/polecat-1",
		},
	}

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(session, cityPath, cfg, &stderr) {
		t.Fatalf("prune returned false for a clean, fully-pushed worktree merely because an unrelated stash exists elsewhere in the shared repo; stderr=%s", stderr.String())
	}
	if _, err := os.Stat(workerDir); !os.IsNotExist(err) {
		t.Errorf("workerDir %s should have been removed; stat err=%v", workerDir, err)
	}
}

// TestPruneAgentHomeWorktreeIfSafe_StashInsideWorktreeSurvivesPrune locks in
// the premise the stash-gate removal rests on (ga-pyp2oh): a stash created
// from inside the worktree being pruned is not lost when the worktree is
// removed. Stash commits live in the shared object store and are held by
// refs/stash in the shared git admin dir, neither of which
// `git worktree remove` touches — so the gate could only ever have produced
// false positives, never prevented a real loss. This test proves the stash
// entry and its full diff are both still readable from the rig root after
// the worktree directory is gone.
func TestPruneAgentHomeWorktreeIfSafe_StashInsideWorktreeSurvivesPrune(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	rigRoot := t.TempDir()
	runGit(t, rigRoot, "clone", bare, ".")
	runGit(t, rigRoot, "config", "user.email", "test@test.com")
	runGit(t, rigRoot, "config", "user.name", "Test")
	runGit(t, rigRoot, "commit", "--allow-empty", "-m", "init")
	runGit(t, rigRoot, "push", "origin", "HEAD")

	cityPath := t.TempDir()
	workerDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "polecat-1")
	if err := os.MkdirAll(filepath.Dir(workerDir), 0o755); err != nil {
		t.Fatalf("mkdir worktree parent: %v", err)
	}
	runGit(t, rigRoot, "worktree", "add", "-b", "polecat-1-branch", workerDir)

	// Stash work from *inside* the worktree about to be pruned — the case
	// the removed gate nominally existed to protect. Stashing leaves the
	// worktree clean, so no other safety gate keeps it alive.
	const stashedLine = "work stashed inside the worktree"
	if err := os.WriteFile(filepath.Join(workerDir, "inside.txt"), []byte(stashedLine+"\n"), 0o644); err != nil {
		t.Fatalf("write in-worktree file: %v", err)
	}
	runGit(t, workerDir, "add", "inside.txt")
	runGit(t, workerDir, "stash")
	if out := runGit(t, workerDir, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Fatalf("test setup invalid: worktree not clean after stash; status=%q", out)
	}

	cfg := &config.City{Rigs: []config.Rig{{Name: "demo", Path: rigRoot}}}
	session := beads.Bead{
		ID: "session-1",
		Metadata: map[string]string{
			"worker_dir":   workerDir,
			"template":     "demo/polecat",
			"session_name": "demo/polecat-1",
		},
	}

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(session, cityPath, cfg, &stderr) {
		t.Fatalf("prune returned false for a clean, fully-pushed worktree; stderr=%s", stderr.String())
	}
	if _, err := os.Stat(workerDir); !os.IsNotExist(err) {
		t.Fatalf("workerDir %s should have been removed; stat err=%v", workerDir, err)
	}

	// The load-bearing assertion: the stash outlived the worktree it was
	// made in, and its contents are still recoverable from the rig root.
	if out := runGit(t, rigRoot, "stash", "list"); strings.TrimSpace(out) == "" {
		t.Fatalf("stash made inside the worktree was lost when the worktree was pruned")
	}
	if out := runGit(t, rigRoot, "stash", "show", "-p"); !strings.Contains(out, stashedLine) {
		t.Fatalf("stashed content unrecoverable after prune; `git stash show -p` =\n%s", out)
	}
}

// TestPruneAgentHomeWorktreeIfSafe_LeavesManagedWorktreeOfAnotherOwner is
// the raw form of the managed-worktree ownership gate (ga-tlus3e.22): a
// pool seat the controller started inside a work bead's verified managed
// worktree records that worktree as its work dir, and closing the seat must
// not remove a worktree whose provenance names another owner.
func TestPruneAgentHomeWorktreeIfSafe_LeavesManagedWorktreeOfAnotherOwner(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot, spec := managedPruneWorktree(t, cityPath)
	cfg := &config.City{Rigs: []config.Rig{{Name: "demo", Path: rigRoot}}}
	session := beads.Bead{
		ID: "session-1",
		Metadata: map[string]string{
			"work_dir":     spec.Path,
			"gc.work_dir":  spec.Path,
			"template":     "demo/worker",
			"session_name": "demo/worker-1",
		},
	}

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, cityPath, cfg, &stderr) {
		t.Fatalf("prune returned true for a managed worktree owned by %q; stderr=%s", spec.Owner, stderr.String())
	}
	assertManagedWorktreeIntact(t, spec, stderr.String())
	if !strings.Contains(stderr.String(), "managed worktree") {
		t.Errorf("expected managed-worktree refusal log; got %q", stderr.String())
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
	assertNoWorktreeStaleMarker(t, fx.workerDir)
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
	assertNoWorktreeStaleMarker(t, fx.workerDir)
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
	assertNoWorktreeStaleMarker(t, fx.workerDir)
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
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_NilConfig(t *testing.T) {
	fx := newPruneFixture(t)
	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, nil, &stderr) {
		t.Fatal("prune returned true with nil cfg")
	}
}

func TestWriteWorktreeStaleMarker_UncommittedWorkListsDirtyPaths(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{
		isRepo:          true,
		hasUncommitted:  true,
		currentBranch:   "builder/ga-abc123",
		statusPorcelain: " M cmd/gc/foo.go\n?? cmd/gc/bar_test.go\n",
	})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with uncommitted work")
	}
	data, err := os.ReadFile(filepath.Join(fx.workerDir, worktreeStaleFileName))
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	content := string(data)
	for _, want := range []string{"Dirty paths", "cmd/gc/foo.go", "cmd/gc/bar_test.go"} {
		if !strings.Contains(content, want) {
			t.Errorf("marker content = %q, want to contain %q", content, want)
		}
	}
}

func TestWriteWorktreeStaleMarker_UnpushedCommitsGuidanceIsReasonSpecific(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUnpushed: true, currentBranch: "builder/ga-def456"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with unpushed commits")
	}
	data, err := os.ReadFile(filepath.Join(fx.workerDir, worktreeStaleFileName))
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "git push origin HEAD") {
		t.Errorf("marker content = %q, want unpushed-specific guidance (git push origin HEAD)", content)
	}
	// gm-shwey8: a role/session branch must never gain an upstream, and
	// push -u sets one.
	if strings.Contains(content, "push -u") || strings.Contains(content, "--set-upstream") {
		t.Errorf("marker content = %q, want no upstream-setting push (role branches must never have an upstream)", content)
	}
	if strings.Contains(content, "git stash") {
		t.Errorf("marker content = %q, want no stash guidance for unpushed-commits reason", content)
	}
}

// TestWriteWorktreeStaleMarker_UnpushedGuidanceMentionsRecoveryCommand pins
// the exact recovery command against internal/git.Git.HasUnpushedCommitsResult
// (git log HEAD --oneline --not --remotes), not the @{u}-relative form that
// ga-owvytd's own description proposes. These fleet worktrees have no
// upstream tracking configured (verified directly: `git rev-parse
// --abbrev-ref @{u}` fails with "fatal: no upstream configured" on a fresh
// builder branch), so a reader who copy-pastes "git log --oneline
// @{u}..HEAD" hits that same error instead of recovering. --not --remotes
// needs no tracking branch and matches the command the codebase's own
// unpushed-commits probe already uses.
func TestWriteWorktreeStaleMarker_UnpushedGuidanceMentionsRecoveryCommand(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUnpushed: true, currentBranch: "builder/ga-def456"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, &stderr) {
		t.Fatal("prune returned true with unpushed commits")
	}
	data, err := os.ReadFile(filepath.Join(fx.workerDir, worktreeStaleFileName))
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "git log HEAD --oneline --not --remotes") {
		t.Errorf("marker content = %q, want the --not --remotes recovery command", content)
	}
	if strings.Contains(content, "@{u}") {
		t.Errorf("marker content = %q, want no @{u}-relative command (no upstream tracking in these worktrees)", content)
	}
}

// TestWorktreeStaleGuidance exercises worktreeStaleGuidance directly: every
// known reason code must be self-contained (WHAT HAPPENED / WHAT TO DO /
// NEVER) and reason-specific, and an unrecognized reason code must still
// fall back to guidance rather than leaving the reader with nothing — the
// same guarantee packs/gastown/scripts/worktree-setup.sh's
// stale_marker_guidance makes for its own fallback case.
func TestWorktreeStaleGuidance(t *testing.T) {
	cases := []struct {
		name    string
		reason  string
		dirty   string
		want    []string
		mustNot []string
	}{
		{
			name:   "uncommitted work lists dirty paths",
			reason: worktreeStaleReasonUncommittedWork,
			dirty:  " M cmd/gc/foo.go\n?? cmd/gc/bar_test.go\n",
			want:   []string{"WHAT HAPPENED", "WHAT TO DO", "NEVER", "cmd/gc/foo.go", "cmd/gc/bar_test.go", "git status --porcelain"},
		},
		{
			name:   "uncommitted work with no listing still self-describes",
			reason: worktreeStaleReasonUncommittedWork,
			dirty:  "",
			want:   []string{"WHAT HAPPENED", "WHAT TO DO", "NEVER"},
		},
		{
			name:    "unpushed commits",
			reason:  worktreeStaleReasonUnpushedCommits,
			want:    []string{"WHAT HAPPENED", "WHAT TO DO", "NEVER", "git log HEAD --oneline --not --remotes", "git push origin HEAD", "cherry-pick"},
			mustNot: []string{"git stash", "@{u}", "push -u", "--set-upstream"},
		},
		{
			name:   "unknown reason falls back rather than leaving no guidance",
			reason: "some-future-reason",
			want:   []string{"WHAT HAPPENED", "WHAT TO DO", "NEVER", "some-future-reason"},
		},
	}
	// The prune runs exactly once, when the session bead closes
	// (session_reconciler.go); nothing re-enters it afterwards. No reason
	// code's guidance may therefore promise that the reconciler will retry
	// and remove the worktree once the reader has cleaned up, and every one
	// must tell the reader the marker is advisory (blocking=no in the header).
	retryPromises := []string{"retries automatically", "next reconciler pass"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := worktreeStaleGuidance(c.reason, c.dirty)
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("worktreeStaleGuidance(%q) = %q, want to contain %q", c.reason, got, want)
				}
			}
			for _, mustNot := range c.mustNot {
				if strings.Contains(got, mustNot) {
					t.Errorf("worktreeStaleGuidance(%q) = %q, want NOT to contain %q", c.reason, got, mustNot)
				}
			}
			for _, promise := range retryPromises {
				if strings.Contains(got, promise) {
					t.Errorf("worktreeStaleGuidance(%q) = %q, want no retry promise %q (the prune is attempted once, at session close)", c.reason, got, promise)
				}
			}
			if !strings.Contains(got, "advisory") {
				t.Errorf("worktreeStaleGuidance(%q) = %q, want to tell the reader the marker is advisory", c.reason, got)
			}
		})
	}
}

// staleGuidanceSections splits guidance into the text before its NEVER
// section and the NEVER section itself, failing the test when there is none.
func staleGuidanceSections(t *testing.T, guidance string) (beforeNever, never string) {
	t.Helper()
	idx := strings.Index(guidance, "\nNEVER\n")
	if idx < 0 {
		t.Fatalf("guidance has no NEVER section: %q", guidance)
	}
	return guidance[:idx], guidance[idx:]
}

// TestWorktreeStaleGuidance_EveryBranchEndsWithMarkerRemoval pins the
// marker's lifecycle (ga-c44vk1 F2): the Go writer writes once, at session
// close, and never re-checks, so nothing but the reader clears it. Every
// branch, the unrecognized-reason fallback included, must say to remove
// the marker once resolved and that the reconciler will not do it.
func TestWorktreeStaleGuidance_EveryBranchEndsWithMarkerRemoval(t *testing.T) {
	for _, reason := range []string{
		worktreeStaleReasonUncommittedWork,
		worktreeStaleReasonUnpushedCommits,
		"some-future-reason",
	} {
		t.Run(reason, func(t *testing.T) {
			got := worktreeStaleGuidance(reason, " M a.go\n")
			beforeNever, _ := staleGuidanceSections(t, got)
			_, whatToDo, found := strings.Cut(beforeNever, "WHAT TO DO")
			if !found {
				t.Fatalf("guidance for %q has no WHAT TO DO section: %q", reason, got)
			}
			if !strings.Contains(whatToDo, "rm .worktree-stale") {
				t.Errorf("WHAT TO DO for %q = %q, want a final rm .worktree-stale step", reason, whatToDo)
			}
			if !strings.Contains(got, "does not re-check") {
				t.Errorf("guidance for %q = %q, want it to say the reconciler does not re-check the marker", reason, got)
			}
		})
	}
}

// TestWorktreeStaleGuidance_UncommittedAdviceCannotDestroyWork pins
// ga-c44vk1 F3: the reconciler promises "nothing was discarded", and the
// dirty paths may be an earlier session's work, so the advice must look
// before it acts, name paths, and forbid the wholesale commands.
func TestWorktreeStaleGuidance_UncommittedAdviceCannotDestroyWork(t *testing.T) {
	got := worktreeStaleGuidance(worktreeStaleReasonUncommittedWork, " M cmd/gc/foo.go\n?? cmd/gc/bar_test.go\n")
	beforeNever, never := staleGuidanceSections(t, got)

	for _, forbidden := range []string{"git restore .", "git add -A", "git add .", "git reset --hard", "git clean"} {
		if strings.Contains(beforeNever, forbidden) {
			t.Errorf("text before NEVER = %q, want no %q as advice", beforeNever, forbidden)
		}
		if forbidden == "git add -A" || forbidden == "git add ." {
			continue
		}
		if !strings.Contains(never, forbidden) {
			t.Errorf("NEVER section = %q, want it to forbid %q", never, forbidden)
		}
	}
	for _, want := range []string{"git diff", "git add <path>", "git restore <path>", "untracked"} {
		if !strings.Contains(beforeNever, want) {
			t.Errorf("text before NEVER = %q, want %q", beforeNever, want)
		}
	}
	if !strings.Contains(never, "earlier session") {
		t.Errorf("NEVER section = %q, want the reason: the changes may belong to an earlier session", never)
	}
	if !strings.Contains(never, "rm -rf") {
		t.Errorf("NEVER section = %q, want the existing rm -rf warning kept", never)
	}
}

// TestWorktreeStaleGuidance_UnpushedHasNoOriginFallback pins ga-c44vk1 F1:
// a rig with no origin, or a branch that must not be pushed, needs a way
// forward that is not a push.
func TestWorktreeStaleGuidance_UnpushedHasNoOriginFallback(t *testing.T) {
	got := worktreeStaleGuidance(worktreeStaleReasonUnpushedCommits, "")
	beforeNever, _ := staleGuidanceSections(t, got)
	for _, want := range []string{"no origin", "cherry-pick"} {
		if !strings.Contains(beforeNever, want) {
			t.Errorf("unpushed WHAT TO DO = %q, want %q", beforeNever, want)
		}
	}
}

// TestWorktreeStaleGuidance_KeepsStatusColumnWithLegend pins ga-c44vk1 F6:
// TrimSpace dropped the leading space of " M path", so unstaged and staged
// entries looked alike and nothing explained the letters.
func TestWorktreeStaleGuidance_KeepsStatusColumnWithLegend(t *testing.T) {
	got := worktreeStaleGuidance(worktreeStaleReasonUncommittedWork, " M cmd/gc/unstaged.go\nM  cmd/gc/staged.go\n?? cmd/gc/new.go\n")
	for _, want := range []string{
		"\n   M cmd/gc/unstaged.go\n",
		"\n  M  cmd/gc/staged.go\n",
		"\n  ?? cmd/gc/new.go\n",
		"?? untracked",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("guidance = %q, want to contain %q", got, want)
		}
	}
}

// TestWorktreeStaleGuidance_CapsDirtyPathList pins ga-c44vk1 F5: agents read
// the marker in full at session start, so thousands of dirty files must not
// become thousands of lines.
func TestWorktreeStaleGuidance_CapsDirtyPathList(t *testing.T) {
	var status strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&status, " M dir/file%03d.go\n", i)
	}
	got := worktreeStaleGuidance(worktreeStaleReasonUncommittedWork, status.String())
	if n := strings.Count(got, "dir/file"); n > 50 {
		t.Errorf("guidance lists %d dirty paths, want at most 50", n)
	}
	for _, want := range []string{"dir/file000.go", "... and 150 more; run git status --porcelain"} {
		if !strings.Contains(got, want) {
			t.Errorf("guidance = %q, want to contain %q", got, want)
		}
	}
	if strings.Contains(got, "dir/file199.go") {
		t.Errorf("guidance lists the 200th path, want it cut by the cap")
	}

	short := worktreeStaleGuidance(worktreeStaleReasonUncommittedWork, " M a.go\n M b.go\n")
	if strings.Contains(short, "more; run") {
		t.Errorf("guidance for a short list = %q, want no truncation line", short)
	}
}

// TestWriteWorktreeStaleMarker_StatusErrorIsLoggedAndMarkerStillWritten
// pins ga-c44vk1 F4: a failed StatusPorcelain is reported on stderr instead
// of being dropped, and the marker is still written without a listing.
func TestWriteWorktreeStaleMarker_StatusErrorIsLoggedAndMarkerStillWritten(t *testing.T) {
	dir := t.TempDir()
	gp := &fakeGitProbe{
		isRepo:             true,
		currentBranch:      "builder/ga-abc123",
		statusPorcelainErr: errors.New("status exploded"),
	}
	var stderr bytes.Buffer
	writeWorktreeStaleMarker(gp, dir, worktreeStaleReasonUncommittedWork, &stderr)

	for _, want := range []string{dir, "status exploded"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want to contain %q", stderr.String(), want)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, worktreeStaleFileName))
	if err != nil {
		t.Fatalf("marker not written after a status error: %v", err)
	}
	if !strings.Contains(string(data), "reason=uncommitted-work") || strings.Contains(string(data), "Dirty paths") {
		t.Errorf("marker = %q, want the uncommitted-work marker without a dirty-path listing", data)
	}
}

// TestWriteWorktreeStaleMarker_ReplacesPlantedSymlink pins ga-c44vk1 H1: the
// marker is written through a temp file and renamed, so a symlink planted
// at .worktree-stale is replaced rather than followed out of the worktree.
func TestWriteWorktreeStaleMarker_ReplacesPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("precious\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dir, worktreeStaleFileName)
	if err := os.Symlink(outside, markerPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var stderr bytes.Buffer
	writeWorktreeStaleMarker(&fakeGitProbe{isRepo: true, currentBranch: "b"}, dir, worktreeStaleReasonUnpushedCommits, &stderr)

	if got, _ := os.ReadFile(outside); string(got) != "precious\n" {
		t.Errorf("symlink target = %q, want it untouched", got)
	}
	info, err := os.Lstat(markerPath)
	if err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("marker mode = %v, want a regular file replacing the symlink", info.Mode())
	}
	if data, _ := os.ReadFile(markerPath); !strings.Contains(string(data), "reason=unpushed-commits") {
		t.Errorf("marker = %q, want the unpushed-commits marker", data)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("worktree entries = %v, want only the marker (no temp file left behind)", entries)
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
