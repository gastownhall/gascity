package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// clearWorktreesRootEnv pins WorktreesRoot to the default
// cityPath/.gc/worktrees layout regardless of the host environment.
func clearWorktreesRootEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GC_WORKTREES_DIR", "")
	t.Setenv("T3CODE_WORKTREES_DIR", "")
	t.Setenv("T3CODE_HOME", "")
}

// initProvisionTestRepo creates a git repository with one committed tracked
// file so a provisioned worktree has observable checkout content.
func initProvisionTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runProvisionGit(t, dir, "init", "-q")
	runProvisionGit(t, dir, "config", "user.email", "test@test.com")
	runProvisionGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runProvisionGit(t, dir, "add", "tracked.txt")
	runProvisionGit(t, dir, "commit", "-q", "-m", "init")
	// A bare origin with HEAD pushed keeps HasUnpushedCommitsResult false in
	// provisioned worktrees, matching the reaper's safety gates.
	origin := t.TempDir()
	runProvisionGit(t, origin, "init", "-q", "--bare")
	runProvisionGit(t, dir, "remote", "add", "origin", origin)
	runProvisionGit(t, dir, "push", "-q", "origin", "HEAD")
	return dir
}

func runProvisionGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "GIT_") {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestProvisionSessionWorktree_CreatesWorktreeUnderRoot(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	rigRoot := initProvisionTestRepo(t)
	workDir := filepath.Join(workdirutil.WorktreesRoot(cityPath), "gascity", "gc-abc12")

	if err := provisionSessionWorktree(cityPath, rigRoot, workDir); err != nil {
		t.Fatalf("provisionSessionWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err != nil {
		t.Fatalf("provisioned dir is not a git worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "tracked.txt")); err != nil {
		t.Fatalf("checked-out tracked file missing: %v", err)
	}
}

func TestProvisionSessionWorktree_PreservesSeededContents(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	rigRoot := initProvisionTestRepo(t)
	workDir := filepath.Join(workdirutil.WorktreesRoot(cityPath), "gascity", "gc-seed1")
	if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".claude", "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := provisionSessionWorktree(cityPath, rigRoot, workDir); err != nil {
		t.Fatalf("provisionSessionWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err != nil {
		t.Fatalf("provisioned dir is not a git worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "settings.json")); err != nil {
		t.Fatalf("pre-seeded contents lost: %v", err)
	}
}

func TestProvisionSessionWorktree_SkipsWorkDirOutsideRoot(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	rigRoot := initProvisionTestRepo(t)
	workDir := filepath.Join(t.TempDir(), "elsewhere")

	if err := provisionSessionWorktree(cityPath, rigRoot, workDir); err != nil {
		t.Fatalf("provisionSessionWorktree: %v", err)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("workDir outside the worktrees root must not be created, stat err = %v", err)
	}
}

func TestProvisionSessionWorktree_SkipsWithoutRigRoot(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	workDir := filepath.Join(workdirutil.WorktreesRoot(cityPath), "gascity", "gc-norig")

	if err := provisionSessionWorktree(cityPath, "", workDir); err != nil {
		t.Fatalf("provisionSessionWorktree: %v", err)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("workDir without a rig root must not be created, stat err = %v", err)
	}
}

func TestProvisionSessionWorktree_IdempotentOnExistingWorktree(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	rigRoot := initProvisionTestRepo(t)
	workDir := filepath.Join(workdirutil.WorktreesRoot(cityPath), "gascity", "gc-twice")

	if err := provisionSessionWorktree(cityPath, rigRoot, workDir); err != nil {
		t.Fatalf("first provisionSessionWorktree: %v", err)
	}
	if err := provisionSessionWorktree(cityPath, rigRoot, workDir); err != nil {
		t.Fatalf("second provisionSessionWorktree must be a no-op, got: %v", err)
	}
}

func TestProvisionSessionWorktree_FailsClosedOnStaleAncestor(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	rigRoot := initProvisionTestRepo(t)
	rigDir := filepath.Join(workdirutil.WorktreesRoot(cityPath), "gascity")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := "gitdir: " + filepath.Join(cityPath, "does-not-exist") + "\n"
	if err := os.WriteFile(filepath.Join(rigDir, ".git"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	err := provisionSessionWorktree(cityPath, rigRoot, filepath.Join(rigDir, "gc-stale"))
	if err == nil {
		t.Fatal("expected stale-ancestor error, got nil")
	}
}

func TestPoolTriggerWorkDir_RedirectsRigBeadToWorktreesRoot(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	rigRoot := t.TempDir()
	bp := &agentBuildParams{
		cityPath: cityPath,
		cityName: "test",
		rigs:     []config.Rig{{Name: "gascity", Path: rigRoot}},
	}
	cfgAgent := &config.Agent{
		Name:    "polecat",
		Dir:     "gascity",
		WorkDir: "{{.WorktreesRoot}}/polecat",
	}
	request := SessionRequest{WorkBeadID: "gc-abc12", WorkBeadTitle: "fix the thing"}

	got := poolTriggerWorkDir(bp, cfgAgent, "gascity/polecat", request)
	want := filepath.Join(workdirutil.WorktreesRoot(cityPath), "gascity", "gc-abc12")
	if got != want {
		t.Fatalf("poolTriggerWorkDir = %q, want %q", got, want)
	}
}

func TestPoolTriggerWorkDir_KeepsSlugLayoutOutsideWorktreesRoot(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	rigRoot := t.TempDir()
	bp := &agentBuildParams{
		cityPath: cityPath,
		cityName: "test",
		rigs:     []config.Rig{{Name: "gascity", Path: rigRoot}},
	}
	// No work_dir template: the configured base resolves to the rig root,
	// which does not opt into the worktrees root.
	cfgAgent := &config.Agent{Name: "polecat", Dir: "gascity"}
	request := SessionRequest{WorkBeadID: "gc-abc12", WorkBeadTitle: "fix the thing"}

	got := poolTriggerWorkDir(bp, cfgAgent, "gascity/polecat", request)
	if got == "" {
		t.Fatal("poolTriggerWorkDir returned empty")
	}
	if !strings.HasPrefix(got, rigRoot+string(filepath.Separator)) {
		t.Fatalf("poolTriggerWorkDir = %q, want slug under rig root %q", got, rigRoot)
	}
}

func TestPoolTriggerWorkDir_KeepsSlugLayoutForRiglessAgent(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	bp := &agentBuildParams{cityPath: cityPath, cityName: "test"}
	cfgAgent := &config.Agent{Name: "helper", WorkDir: "{{.WorktreesRoot}}/helper"}
	request := SessionRequest{WorkBeadID: "gc-abc12", WorkBeadTitle: "fix the thing"}

	got := poolTriggerWorkDir(bp, cfgAgent, "helper", request)
	base := filepath.Join(workdirutil.WorktreesRoot(cityPath), "helper")
	if !strings.HasPrefix(got, base+string(filepath.Separator)) {
		t.Fatalf("poolTriggerWorkDir = %q, want slug under configured base %q", got, base)
	}
}

func TestBuildPreparedStart_ProvisionsWorktreeForWorkDirUnderRoot(t *testing.T) {
	clearWorktreesRootEnv(t)
	cityPath := t.TempDir()
	rigRoot := initProvisionTestRepo(t)
	workDir := filepath.Join(workdirutil.WorktreesRoot(cityPath), "gascity", "gc-prep1")

	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:gascity/polecat-1"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-1",
			"pool_slot":    "1",
			"work_dir":     workDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := buildPreparedStartWithWorkDirResolver(startCandidate{
		session: &session,
		tp: TemplateParams{
			TemplateName: "gascity/polecat",
			SessionName:  "polecat-1",
			RigName:      "gascity",
			RigRoot:      rigRoot,
		},
	}, cityPath, &config.City{
		Agents: []config.Agent{
			{Name: "polecat", Dir: "gascity", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
		},
	}, store, nil)
	if err != nil {
		t.Fatalf("buildPreparedStartWithWorkDirResolver: %v", err)
	}
	if prepared.cfg.WorkDir != workDir {
		t.Fatalf("prepared.cfg.WorkDir = %q, want %q", prepared.cfg.WorkDir, workDir)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err != nil {
		t.Fatalf("work_dir was not provisioned as a git worktree: %v", err)
	}
}

func TestReapClosedBeadWorktreesHonorsWorktreesRootOverride(t *testing.T) {
	cityPath := t.TempDir()
	override := t.TempDir()
	t.Setenv("GC_WORKTREES_DIR", override)
	t.Setenv("T3CODE_WORKTREES_DIR", "")
	t.Setenv("T3CODE_HOME", "")

	rigRoot := initProvisionTestRepo(t)

	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "done"})
	if err != nil {
		t.Fatal(err)
	}
	closed := "closed"
	if err := store.Update(bead.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(override, "gascity", bead.ID)
	if err := provisionSessionWorktree(cityPath, rigRoot, workDir); err != nil {
		t.Fatalf("provisionSessionWorktree: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigRoot}},
	}
	var debug strings.Builder
	reaped := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"gascity": store}, nil, &debug)
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1 (reaper must scan the overridden worktrees root); stderr:\n%s", reaped, debug.String())
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("worktree for closed bead still present, stat err = %v", err)
	}
}
