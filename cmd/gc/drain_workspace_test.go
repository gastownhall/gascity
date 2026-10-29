package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worktree"
)

// sharedDrainWorkspaceCity returns a city directory and a config with one rig
// named alpha at rigPath.
func sharedDrainWorkspaceCity(t *testing.T, rigPath string) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "alpha", Path: rigPath}},
	}
	return cityPath, cfg
}

func sharedDrainControl(rootID, drainID string) beads.Bead {
	return beads.Bead{ID: drainID, Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}}
}

func TestSharedDrainWorkspaceProvisionerEnsuresManagedRigWorktree(t *testing.T) {
	repo, _ := testutil.InitGitRepo(t)
	worktreesRoot := t.TempDir()
	t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	cityPath, cfg := sharedDrainWorkspaceCity(t, repo)
	control := sharedDrainControl("gc-root1", "gc-drain1")
	provision := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)

	path, err := provision(control)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	root := filepath.Join(worktreesRoot, "alpha")
	wantPath := filepath.Join(canonicalTestPath(root), "gc-root1-gc-drain1")
	if path != wantPath {
		t.Fatalf("path = %q, want %q", path, wantPath)
	}
	if path != canonicalTestPath(path) {
		t.Fatalf("path = %q, want canonical", path)
	}
	if got := testutil.RunGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/gc-drain1" {
		t.Fatalf("worktree branch = %q, want gc/drain/gc-drain1", got)
	}
	if got, want := testutil.RunGit(t, path, "rev-parse", "HEAD"), testutil.RunGit(t, repo, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree HEAD = %s, want rig HEAD %s", got, want)
	}
	spec := worktree.Spec{
		RepoDir:    repo,
		Path:       filepath.Join(root, "gc-root1-gc-drain1"),
		Root:       root,
		Branch:     "gc/drain/gc-drain1",
		Base:       "HEAD",
		BeadID:     "gc-drain1",
		StoreRef:   "rig:alpha",
		Creator:    sharedDrainWorkspaceOwner,
		Owner:      sharedDrainWorkspaceOwner,
		Generation: "gc-drain1",
		Lifecycle:  worktree.LifecycleActive,
	}
	verified, err := worktree.Verify(spec)
	if err != nil {
		t.Fatalf("worktree.Verify: %v", err)
	}
	if verified.Provenance == nil || verified.Provenance.BeadID != "gc-drain1" || verified.Provenance.StoreRef != "rig:alpha" {
		t.Fatalf("provenance = %+v, want bead gc-drain1 store ref rig:alpha", verified.Provenance)
	}
	attempt := verified.Provenance.AttemptID

	// A commit on the rig's HEAD between calls does not break re-verification.
	testutil.RunGit(t, repo, "commit", "--allow-empty", "-m", "rig moved")
	again, err := provision(control)
	if err != nil {
		t.Fatalf("provision again: %v", err)
	}
	if again != path {
		t.Fatalf("second path = %q, want %q", again, path)
	}
	reverified, err := worktree.Verify(spec)
	if err != nil {
		t.Fatalf("worktree.Verify after second call: %v", err)
	}
	if reverified.Provenance.AttemptID != attempt {
		t.Fatalf("attempt id = %q after re-call, want unchanged %q", reverified.Provenance.AttemptID, attempt)
	}

	testutil.RunGit(t, repo, "worktree", "remove", path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat removed worktree: %v, want not exist", err)
	}
	recreated, err := provision(control)
	if err != nil {
		t.Fatalf("provision after worktree remove: %v", err)
	}
	if recreated != path {
		t.Fatalf("recreated path = %q, want %q", recreated, path)
	}
	if got := testutil.RunGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/gc-drain1" {
		t.Fatalf("recreated worktree branch = %q, want gc/drain/gc-drain1", got)
	}

	// An agent switching branches inside the shared worktree makes the next
	// call fail closed rather than adopt the foreign checkout.
	testutil.RunGit(t, path, "switch", "-c", "elsewhere")
	if _, err := provision(control); err == nil {
		t.Fatal("provision after branch switch succeeded, want a refusal")
	}
}
