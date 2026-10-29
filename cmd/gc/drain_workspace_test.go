package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formulatest"
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

func TestSharedDrainWorkspaceProvisionerSkipsAndRefuses(t *testing.T) {
	t.Run("city scope", func(t *testing.T) {
		repo, _ := testutil.InitGitRepo(t)
		worktreesRoot := t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, repo)

		path, err := sharedDrainWorkspaceProvisioner(cityPath, cityPath, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err != nil || path != "" {
			t.Fatalf("provision = (%q, %v), want (\"\", nil)", path, err)
		}
		assertDirEmpty(t, worktreesRoot)
	})

	t.Run("rig that is not a git repository", func(t *testing.T) {
		rig := t.TempDir()
		worktreesRoot := t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, rig)

		path, err := sharedDrainWorkspaceProvisioner(cityPath, rig, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err != nil || path != "" {
			t.Fatalf("provision = (%q, %v), want (\"\", nil)", path, err)
		}
		assertDirEmpty(t, worktreesRoot)
	})

	t.Run("worktrees root inside the rig checkout", func(t *testing.T) {
		repo, _ := testutil.InitGitRepo(t)
		worktreesRoot := filepath.Join(repo, ".worktrees")
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, repo)

		_, err := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err == nil {
			t.Fatal("provision succeeded, want a refusal for a root inside the rig checkout")
		}
		for _, want := range []string{worktreesRoot, "GC_WORKTREES_DIR"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("control without a workflow root", func(t *testing.T) {
		repo, _ := testutil.InitGitRepo(t)
		t.Setenv("GC_WORKTREES_DIR", t.TempDir())
		cityPath, cfg := sharedDrainWorkspaceCity(t, repo)

		_, err := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)(beads.Bead{ID: "gc-drain1"})
		if err == nil || !strings.Contains(err.Error(), "gc-drain1") {
			t.Fatalf("provision err = %v, want an error naming the control", err)
		}
	})
}

func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s has %d entries, want none", dir, len(entries))
	}
}

// TestControlDispatchSharedDrainProvisionsRigWorktree drives the real
// dispatcher entry point over a rig-scoped shared exclusive drain and pins that
// the reserved member and its expanded item work in the drain's managed rig
// worktree.
func TestControlDispatchSharedDrainProvisionsRigWorktree(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	repo, _ := testutil.InitGitRepo(t)
	worktreesRoot := t.TempDir()
	t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	cityPath, cfg := sharedDrainWorkspaceCity(t, repo)
	cfg.Agents = []config.Agent{
		{Name: config.ControlDispatcherAgentName, Dir: "alpha"},
		{Name: "worker", Dir: "alpha"},
	}
	formulaDir := t.TempDir()
	itemFormula := `formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"
metadata = { "gc.run_target" = "worker" }
`
	if err := os.WriteFile(filepath.Join(formulaDir, "drain-item.formula.toml"), []byte(itemFormula), 0o644); err != nil {
		t.Fatalf("write drain item formula: %v", err)
	}
	cfg.FormulaLayers.City = []string{formulaDir}
	store := beads.NewMemStore()

	convoy, err := store.Create(beads.Bead{
		Title:    "input convoy",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	})
	if err != nil {
		t.Fatalf("create input convoy: %v", err)
	}
	var memberIDs []string
	for _, title := range []string{"first", "second"} {
		member, err := store.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatalf("create member: %v", err)
		}
		if err := convoycore.TrackItem(store, convoy.ID, member.ID); err != nil {
			t.Fatalf("track member: %v", err)
		}
		memberIDs = append(memberIDs, member.ID)
	}
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
			"gc.formula_contract":    "graph.v2",
			"gc.input_convoy_id":     convoy.ID,
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	drain, err := store.Create(beads.Bead{
		Title: "drain",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "drain",
			beadmeta.RootBeadIDMetadataKey: root.ID,
			"gc.drain_context":             "shared",
			"gc.drain_formula":             "drain-item",
			"gc.drain_member_access":       "exclusive",
			"gc.drain_item_single_lane":    "true",
		},
	})
	if err != nil {
		t.Fatalf("create drain control: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, repo, store, drain.ID, cfg, &stdout, &stderr); err != nil {
		t.Fatalf("control dispatch: %v; stderr=%q", err, stderr.String())
	}

	want := filepath.Join(canonicalTestPath(filepath.Join(worktreesRoot, "alpha")), root.ID+"-"+drain.ID)
	member := beadByID(t, store, memberIDs[0])
	if got := member.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != want {
		t.Fatalf("member %s work_dir = %q, want the drain worktree %q; stderr=%q", member.ID, got, want, stderr.String())
	}
	if got := testutil.RunGit(t, want, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/"+drain.ID {
		t.Fatalf("drain worktree branch = %q, want gc/drain/%s", got, drain.ID)
	}
	raw := beadByID(t, store, drain.ID).Metadata[beadmeta.DrainManifestMetadataKey]
	var manifest struct {
		Rows []struct {
			MemberID   string `json:"member_id"`
			ItemRootID string `json:"item_root_id"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("parse drain manifest %q: %v", raw, err)
	}
	if len(manifest.Rows) == 0 || manifest.Rows[0].ItemRootID == "" {
		t.Fatalf("manifest = %s, want the first member's item materialized; drain metadata=%v stdout=%q stderr=%q", raw, beadByID(t, store, drain.ID).Metadata, stdout.String(), stderr.String())
	}
	itemRoot := beadByID(t, store, manifest.Rows[0].ItemRootID)
	for _, key := range []string{beadmeta.LegacyWorkDirMetadataKey, beadmeta.WorkDirMetadataKey} {
		if got := itemRoot.Metadata[key]; got != want {
			t.Fatalf("item root %s %s = %q, want %q", itemRoot.ID, key, got, want)
		}
	}
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list beads: %v", err)
	}
	steps := 0
	for _, step := range all {
		if step.ID == itemRoot.ID || step.Metadata[beadmeta.RootBeadIDMetadataKey] != itemRoot.ID {
			continue
		}
		steps++
		if got := resolveDrainSourceWorkDir(cityPath, store, step); got != want {
			t.Fatalf("resolveDrainSourceWorkDir(step %s) = %q, want %q", step.ID, got, want)
		}
	}
	if steps == 0 {
		t.Fatalf("item root %s has no materialized steps", itemRoot.ID)
	}
}
