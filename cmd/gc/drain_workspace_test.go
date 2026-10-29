package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formulatest"
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

// TestControlDispatchProvisionsSharedDrainWorkspace pins the wiring: a
// shared exclusive drain dispatched from a rig's store through
// runControlDispatcherWithStoreAndConfig provisions the managed worktree and
// stamps it on the member, and the session reconciler's
// resolveDrainSourceWorkDir — the launch-time reader the item prompts rely
// on — resolves that same path for the materialized item work step.
func TestControlDispatchProvisionsSharedDrainWorkspace(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	cityPath := t.TempDir()
	worktreesRoot := filepath.Join(t.TempDir(), "worktrees")
	t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	repo := initSharedDrainTestRepo(t, filepath.Join(t.TempDir(), "alpha"))
	formulaDir := t.TempDir()
	formulaSrc := `formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"
`
	if err := os.WriteFile(filepath.Join(formulaDir, "drain-item.formula.toml"), []byte(strings.TrimSpace(formulaSrc)+"\n"), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	cfg := &config.City{
		Daemon:        config.DaemonConfig{FormulaV2: boolPtr(true)},
		Workspace:     config.Workspace{Name: "test-city"},
		Rigs:          []config.Rig{{Name: "alpha", Path: repo}},
		FormulaLayers: config.FormulaLayers{City: []string{formulaDir}},
	}
	// The drain decorates its item recipe through the rig-scoped control
	// dispatcher (storeRef is "rig:alpha"), which config matches on Dir, so
	// the dispatcher agent must carry the rig's name as its scope.
	addTestControlDispatcherAgents(cfg, "alpha")
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	store := beads.NewMemStore()
	parent, err := store.Create(beads.Bead{Title: "parent", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	first, err := store.Create(beads.Bead{Title: "first", Type: "task"})
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	second, err := store.Create(beads.Bead{Title: "second", Type: "task"})
	if err != nil {
		t.Fatalf("Create(second): %v", err)
	}
	for _, member := range []string{first.ID, second.ID} {
		if err := convoycore.TrackItem(store, parent.ID, member); err != nil {
			t.Fatalf("TrackItem(%s): %v", member, err)
		}
	}
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.InputConvoyIDMetadataKey:   parent.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	drain, err := store.Create(beads.Bead{
		Title: "drain",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:              "drain",
			beadmeta.RootBeadIDMetadataKey:        root.ID,
			beadmeta.DrainContextMetadataKey:      beadmeta.DrainContextShared,
			beadmeta.DrainMemberAccessMetadataKey: beadmeta.DrainMemberAccessExclusive,
			beadmeta.DrainFormulaMetadataKey:      "drain-item",
		},
	})
	if err != nil {
		t.Fatalf("Create(drain): %v", err)
	}

	var stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, repo, store, drain.ID, cfg, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v (stderr: %s)", err, stderr.String())
	}

	firstMember, err := store.Get(first.ID)
	if err != nil {
		t.Fatalf("Get(first member): %v", err)
	}
	workDir := strings.TrimSpace(firstMember.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	if workDir == "" {
		t.Fatalf("first member work_dir is empty; stderr: %s", stderr.String())
	}
	if got := gitOutputForDrainWorkspace(t, workDir, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/"+drain.ID {
		t.Fatalf("provisioned worktree branch = %q, want gc/drain/%s", got, drain.ID)
	}
	if !strings.Contains(workDir, filepath.Join(worktreesRoot, "alpha")) {
		t.Fatalf("work_dir = %q, want it under the configured worktrees root for rig alpha", workDir)
	}

	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// The drain stamps its identity on the item root (and on the unit
	// convoy, which carries the same keys); the step beads hang off the
	// item root through gc.root_bead_id, so locate the root — the bead
	// with the drain item-root key — first.
	var itemRoot *beads.Bead
	for i := range all {
		if all[i].Metadata[beadmeta.DrainControlIDMetadataKey] != drain.ID {
			continue
		}
		if all[i].Metadata[beadmeta.DrainMemberIDMetadataKey] != first.ID {
			continue
		}
		if all[i].Metadata[beadmeta.ItemRootKeyMetadataKey] == "" {
			continue
		}
		itemRoot = &all[i]
		break
	}
	if itemRoot == nil {
		t.Fatalf("no item root found for drain %s over member %s; beads = %+v", drain.ID, first.ID, all)
	}
	var workStep *beads.Bead
	for i := range all {
		bead := all[i]
		if bead.ID == itemRoot.ID {
			continue
		}
		if bead.Metadata[beadmeta.RootBeadIDMetadataKey] != itemRoot.ID {
			continue
		}
		if strings.HasPrefix(bead.Title, "Work ") {
			workStep = &bead
			break
		}
	}
	if workStep == nil {
		t.Fatalf("no item work step found for drain %s; beads = %+v", drain.ID, all)
	}
	if got := resolveDrainSourceWorkDir(cityPath, store, *workStep); got != workDir {
		t.Fatalf("resolveDrainSourceWorkDir = %q, want the provisioned workspace %q", got, workDir)
	}
}
