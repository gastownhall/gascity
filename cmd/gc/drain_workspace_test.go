package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worktree"
)

// initSharedDrainTestRepo creates a git repository at dir with one commit on
// branch main and returns dir. The shared workspace provisioner builds its
// worktrees from the rig's local HEAD, so the repo needs a resolvable HEAD.
func initSharedDrainTestRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	gitCmd(t, dir, "init", "-b", "main")
	gitCmd(t, dir, "config", "user.email", "test@example.invalid")
	gitCmd(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("repo\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitCmd(t, dir, "add", "README.md")
	gitCmd(t, dir, "commit", "-m", "init")
	return dir
}

func gitOutputForDrainWorkspace(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func sharedDrainWorkspaceControl(id, rootID string) beads.Bead {
	control := beads.Bead{ID: id, Metadata: map[string]string{}}
	if rootID != "" {
		control.Metadata[beadmeta.RootBeadIDMetadataKey] = rootID
	}
	return control
}

// sharedDrainWorkspaceVerifySpec rebuilds the managed spec the provisioner is
// expected to have ensured, for independent verification through worktree.Verify.
func sharedDrainWorkspaceVerifySpec(repo, worktreesRoot, rigName, storeRef string, control beads.Bead, rootID string) worktree.Spec {
	root := filepath.Join(worktreesRoot, rigName)
	return worktree.Spec{
		RepoDir:    repo,
		Root:       root,
		Path:       filepath.Join(root, rootID+"-"+control.ID),
		Branch:     "gc/drain/" + control.ID,
		Base:       "HEAD",
		BeadID:     control.ID,
		StoreRef:   storeRef,
		Creator:    sharedDrainWorkspaceOwner,
		Owner:      sharedDrainWorkspaceOwner,
		Generation: control.ID,
		Lifecycle:  worktree.LifecycleActive,
	}
}

func TestSharedDrainWorkspaceProvisionerEnsuresManagedWorktree(t *testing.T) {
	cityPath := t.TempDir()
	worktreesRoot := filepath.Join(t.TempDir(), "worktrees")
	t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	repo := initSharedDrainTestRepo(t, filepath.Join(t.TempDir(), "alpha"))
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: repo}}}
	provision := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)
	control := sharedDrainWorkspaceControl("gcg-drain-1", "gcg-root-1")

	path, err := provision(control)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	wantPath := filepath.Join(worktreesRoot, "alpha", "gcg-root-1-gcg-drain-1")
	if path != wantPath {
		t.Fatalf("path = %q, want %q", path, wantPath)
	}
	if path != filepath.Clean(path) {
		t.Fatalf("path = %q, want the canonical clean path", path)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("stat %q: %v (isDir %v), want an existing directory", path, err, info != nil && info.IsDir())
	}

	spec := sharedDrainWorkspaceVerifySpec(repo, worktreesRoot, "alpha", "rig:alpha", control, "gcg-root-1")
	rep, err := worktree.Verify(spec)
	if err != nil {
		t.Fatalf("worktree.Verify: %v", err)
	}
	if rep.Provenance == nil {
		t.Fatal("Verify returned no provenance for a managed worktree")
	}
	if rep.Provenance.BeadID != control.ID {
		t.Fatalf("provenance bead = %q, want the drain control %q", rep.Provenance.BeadID, control.ID)
	}
	if rep.Provenance.StoreRef != "rig:alpha" {
		t.Fatalf("provenance store ref = %q, want rig:alpha", rep.Provenance.StoreRef)
	}
	if got, want := rep.Head, gitOutputForDrainWorkspace(t, repo, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree head = %q, want the rig's local HEAD %q", got, want)
	}
	if got := gitOutputForDrainWorkspace(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/"+control.ID {
		t.Fatalf("checked-out branch = %q, want gc/drain/%s", got, control.ID)
	}
	firstAttempt := rep.Provenance.AttemptID
	if firstAttempt == "" {
		t.Fatal("provenance attempt id = empty, want one recorded at creation")
	}

	second, err := provision(control)
	if err != nil {
		t.Fatalf("provision (re-run): %v", err)
	}
	if second != path {
		t.Fatalf("second provision = %q, want the same path %q", second, path)
	}
	rep, err = worktree.Verify(spec)
	if err != nil {
		t.Fatalf("worktree.Verify (re-run): %v", err)
	}
	if rep.Provenance.AttemptID != firstAttempt {
		t.Fatalf("attempt id changed on re-run: %q, want unchanged %q", rep.Provenance.AttemptID, firstAttempt)
	}

	gitCmd(t, repo, "worktree", "remove", path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat %q after remove: %v, want gone", path, err)
	}
	recreated, err := provision(control)
	if err != nil {
		t.Fatalf("provision (after worktree remove): %v", err)
	}
	if recreated != path {
		t.Fatalf("recreated provision = %q, want the same path %q", recreated, path)
	}
	if _, err := worktree.Verify(spec); err != nil {
		t.Fatalf("worktree.Verify (recreated): %v", err)
	}
	if got := gitOutputForDrainWorkspace(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/"+control.ID {
		t.Fatalf("recreated checked-out branch = %q, want gc/drain/%s", got, control.ID)
	}
}

func TestSharedDrainWorkspaceProvisionerSkipsCityScope(t *testing.T) {
	cityPath := t.TempDir()
	worktreesRoot := filepath.Join(t.TempDir(), "worktrees")
	t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	repo := initSharedDrainTestRepo(t, filepath.Join(t.TempDir(), "alpha"))
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Rigs: []config.Rig{{Name: "alpha", Path: repo}}}
	provision := sharedDrainWorkspaceProvisioner(cityPath, cityPath, cfg)

	path, err := provision(sharedDrainWorkspaceControl("gcg-drain-1", "gcg-root-1"))
	if err != nil {
		t.Fatalf("provision (city scope): %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want no workspace for a city-scoped control", path)
	}
	if _, err := os.Stat(worktreesRoot); !os.IsNotExist(err) {
		t.Fatalf("worktrees root %q was created for a city-scoped control, want nothing", worktreesRoot)
	}
}

func TestSharedDrainWorkspaceProvisionerSkipsNonGitRig(t *testing.T) {
	cityPath := t.TempDir()
	worktreesRoot := filepath.Join(t.TempDir(), "worktrees")
	t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	rigPath := t.TempDir() // exists, but is not a git repository
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: rigPath}}}
	provision := sharedDrainWorkspaceProvisioner(cityPath, rigPath, cfg)

	path, err := provision(sharedDrainWorkspaceControl("gcg-drain-2", "gcg-root-1"))
	if err != nil {
		t.Fatalf("provision (non-git rig): %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want no workspace for a non-git rig", path)
	}
	if _, err := os.Stat(worktreesRoot); !os.IsNotExist(err) {
		t.Fatalf("worktrees root %q was created for a non-git rig, want nothing", worktreesRoot)
	}
}

func TestSharedDrainWorkspaceProvisionerRejectsRootInsideRig(t *testing.T) {
	cityPath := t.TempDir()
	repo := initSharedDrainTestRepo(t, filepath.Join(t.TempDir(), "alpha"))
	t.Setenv("GC_WORKTREES_DIR", filepath.Join(repo, "inside-wt"))
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: repo}}}
	provision := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)

	_, err := provision(sharedDrainWorkspaceControl("gcg-drain-1", "gcg-root-1"))
	if err == nil {
		t.Fatal("provision with a worktrees root inside the rig checkout = nil error, want refusal")
	}
	if !strings.Contains(err.Error(), "GC_WORKTREES_DIR") {
		t.Fatalf("error = %v, want it to name GC_WORKTREES_DIR so an operator can move the root", err)
	}
	if !strings.Contains(err.Error(), "inside-wt") {
		t.Fatalf("error = %v, want it to name the offending worktrees root", err)
	}
}

func TestSharedDrainWorkspaceProvisionerRequiresRootBeadID(t *testing.T) {
	cityPath := t.TempDir()
	worktreesRoot := filepath.Join(t.TempDir(), "worktrees")
	t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	repo := initSharedDrainTestRepo(t, filepath.Join(t.TempDir(), "alpha"))
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: repo}}}
	provision := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)

	_, err := provision(sharedDrainWorkspaceControl("gcg-drain-3", ""))
	if err == nil {
		t.Fatal("provision without gc.root_bead_id = nil error, want refusal")
	}
	if !strings.Contains(err.Error(), "gcg-drain-3") {
		t.Fatalf("error = %v, want it to name the control", err)
	}
}
