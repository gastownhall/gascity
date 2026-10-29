package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/git"
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

// initSharedDrainRig returns a rig checkout cloned from a bare origin whose
// default branch is main, and the commit origin/main names. Like a deploy
// checkout, the rig has a local branch one commit ahead of origin/main checked
// out, so a worktree cut from the rig's HEAD differs from one cut from
// origin/main.
func initSharedDrainRig(t *testing.T) (string, string) {
	t.Helper()
	seed, _ := testutil.InitGitRepo(t)
	testutil.RunGit(t, seed, "branch", "-M", "main")
	origin := filepath.Join(t.TempDir(), "origin.git")
	testutil.RunGit(t, seed, "clone", "--bare", seed, origin)
	repo := filepath.Join(t.TempDir(), "rig")
	testutil.RunGit(t, seed, "clone", origin, repo)
	testutil.RunGit(t, repo, "config", "user.email", "test@test.com")
	testutil.RunGit(t, repo, "config", "user.name", "Test")
	testutil.RunGit(t, repo, "switch", "-c", "deploy")
	testutil.RunGit(t, repo, "commit", "--allow-empty", "-m", "deploy only")
	return repo, testutil.RunGit(t, repo, "rev-parse", "refs/remotes/origin/main")
}

func TestSharedDrainWorkspaceProvisionerEnsuresManagedRigWorktree(t *testing.T) {
	repo, originMain := initSharedDrainRig(t)
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
	// The branch has no upstream: cut from a remote-tracking branch, git
	// would otherwise make it track the default branch.
	if out, err := testutil.RunGitWithEnv(repo, git.SanitizedEnv(), "config", "--get", "branch.gc/drain/gc-drain1.merge"); err == nil {
		t.Fatalf("drain branch upstream = %q, want none", strings.TrimSpace(string(out)))
	}
	// The branch is cut from origin's default branch, never from the deploy
	// branch the rig checkout has checked out.
	if got := testutil.RunGit(t, path, "rev-parse", "HEAD"); got != originMain {
		t.Fatalf("worktree HEAD = %s, want origin/main %s (rig HEAD is %s)", got, originMain, testutil.RunGit(t, repo, "rev-parse", "HEAD"))
	}
	spec := worktree.Spec{
		RepoDir:    repo,
		Path:       filepath.Join(root, "gc-root1-gc-drain1"),
		Root:       root,
		Branch:     "gc/drain/gc-drain1",
		Base:       "refs/remotes/origin/main",
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
	if verified.Provenance.BaseSHA != originMain {
		t.Fatalf("provenance base SHA = %s, want origin/main %s", verified.Provenance.BaseSHA, originMain)
	}
	attempt := verified.Provenance.AttemptID

	// origin/main moving between calls does not break re-verification.
	testutil.RunGit(t, repo, "update-ref", "refs/remotes/origin/main", testutil.RunGit(t, repo, "rev-parse", "HEAD"))
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
}

// TestSharedDrainWorkspaceBase pins where a drain's branch is cut from: the
// rig's default_branch on origin, else origin's own default, and never the
// rig checkout's HEAD when origin has no such branch.
func TestSharedDrainWorkspaceBase(t *testing.T) {
	t.Run("origin's default branch", func(t *testing.T) {
		repo, _ := initSharedDrainRig(t)
		base, err := sharedDrainWorkspaceBase(context.Background(), repo, config.Rig{Name: "alpha", Path: repo})
		if err != nil || base != "refs/remotes/origin/main" {
			t.Fatalf("base = (%q, %v), want refs/remotes/origin/main", base, err)
		}
	})

	t.Run("the rig's default_branch wins", func(t *testing.T) {
		repo, _ := initSharedDrainRig(t)
		testutil.RunGit(t, repo, "update-ref", "refs/remotes/origin/trunk", "HEAD")
		base, err := sharedDrainWorkspaceBase(context.Background(), repo, config.Rig{Name: "alpha", Path: repo, DefaultBranch: "trunk"})
		if err != nil || base != "refs/remotes/origin/trunk" {
			t.Fatalf("base = (%q, %v), want refs/remotes/origin/trunk", base, err)
		}
	})

	t.Run("the lookup runs under the provisioning ctx", func(t *testing.T) {
		repo, _ := initSharedDrainRig(t)
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		base, err := sharedDrainWorkspaceBase(canceled, repo, config.Rig{Name: "alpha", Path: repo, DefaultBranch: "main"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("base = (%q, %v), want the lookup cut short by the ctx", base, err)
		}
	})

	for _, tc := range []struct {
		name          string
		defaultBranch string
		noOrigin      bool
		want          string
	}{
		{name: "rig with no origin", noOrigin: true, want: "refs/remotes/origin/main"},
		{name: "default_branch origin lacks", defaultBranch: "release", want: "refs/remotes/origin/release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := initSharedDrainRig(t)
			if tc.noOrigin {
				repo, _ = testutil.InitGitRepo(t)
			}
			base, err := sharedDrainWorkspaceBase(context.Background(), repo, config.Rig{Name: "alpha", Path: repo, DefaultBranch: tc.defaultBranch})
			if err == nil {
				t.Fatalf("base = %q, want a refusal rather than the rig's HEAD", base)
			}
			for _, want := range []string{`rig "alpha"`, tc.want, "default_branch"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}

// TestSharedDrainWorkspaceProvisionerRunsNoRigHooks pins that the controller
// checks the drain worktree out with the rig's hooks disabled: a post-checkout
// hook of the rig checkout that fails (or never returns) neither runs in the
// worktree nor fails or stalls the drain.
func TestSharedDrainWorkspaceProvisionerRunsNoRigHooks(t *testing.T) {
	repo, originMain := initSharedDrainRig(t)
	t.Setenv("GC_WORKTREES_DIR", t.TempDir())
	hooks := t.TempDir()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	for _, hook := range []string{"post-checkout", "reference-transaction"} {
		script := "#!/bin/sh\necho " + hook + " >> '" + marker + "'\nexit 3\n"
		if err := os.WriteFile(filepath.Join(hooks, hook), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s hook: %v", hook, err)
		}
	}
	testutil.RunGit(t, repo, "config", "core.hooksPath", hooks)
	cityPath, cfg := sharedDrainWorkspaceCity(t, repo)

	path, err := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)(sharedDrainControl("gc-root1", "gc-drain1"))
	if err != nil {
		t.Fatalf("provision with failing rig hooks: %v", err)
	}
	if got := testutil.RunGit(t, path, "rev-parse", "HEAD"); got != originMain {
		t.Fatalf("worktree HEAD = %s, want origin/main %s", got, originMain)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stat hook marker: %v, want no rig hook to have run", err)
	}

	// The same hooks fail an ordinary worktree add of the rig, so the
	// provisioner ran with them disabled rather than with none installed.
	if out, err := testutil.RunGitWithEnv(repo, git.SanitizedEnv(), "worktree", "add", "-b", "probe", filepath.Join(t.TempDir(), "probe"), "refs/remotes/origin/main"); err == nil {
		t.Fatalf("plain worktree add with failing hooks succeeded: %s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stat hook marker: %v, want the rig hooks to run on a plain add", err)
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
		parent := t.TempDir()
		t.Setenv("GIT_CEILING_DIRECTORIES", parent)
		rig := filepath.Join(parent, "rig")
		if err := os.Mkdir(rig, 0o755); err != nil {
			t.Fatalf("mkdir rig: %v", err)
		}
		worktreesRoot := t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, rig)

		path, err := sharedDrainWorkspaceProvisioner(cityPath, rig, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err != nil || path != "" {
			t.Fatalf("provision = (%q, %v), want (\"\", nil)", path, err)
		}
		assertDirEmpty(t, worktreesRoot)
	})

	// A rig that is a plain directory inside another repository's checkout
	// (a rig inside a versioned city, say) is not a git rig either: the drain
	// runs without a worktree rather than check out the enclosing repository.
	t.Run("rig that is a directory inside another repository", func(t *testing.T) {
		repo, _ := initSharedDrainRig(t)
		rig := filepath.Join(repo, "rigs", "notes")
		if err := os.MkdirAll(rig, 0o755); err != nil {
			t.Fatalf("mkdir rig: %v", err)
		}
		worktreesRoot := t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, rig)

		path, err := sharedDrainWorkspaceProvisioner(cityPath, rig, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err != nil || path != "" {
			t.Fatalf("provision = (%q, %v), want (\"\", nil)", path, err)
		}
		assertDirEmpty(t, worktreesRoot)
	})

	// A rig git cannot answer for is not "not a git rig": the drain must
	// record why it has no workspace instead of running without one.
	t.Run("rig whose path does not exist", func(t *testing.T) {
		rig := filepath.Join(t.TempDir(), "gone")
		worktreesRoot := t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, rig)

		_, err := sharedDrainWorkspaceProvisioner(cityPath, rig, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err == nil || !strings.Contains(err.Error(), rig) {
			t.Fatalf("provision err = %v, want an error naming the missing rig path %s", err, rig)
		}
		assertDirEmpty(t, worktreesRoot)
	})

	t.Run("rig with no origin default branch", func(t *testing.T) {
		repo, _ := testutil.InitGitRepo(t)
		worktreesRoot := t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, repo)

		_, err := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err == nil || !strings.Contains(err.Error(), "gc-drain2") || !strings.Contains(err.Error(), "refs/remotes/origin/main") {
			t.Fatalf("provision err = %v, want a refusal naming the drain and the missing origin branch", err)
		}
		assertDirEmpty(t, worktreesRoot)
	})

	t.Run("worktrees root inside the rig checkout", func(t *testing.T) {
		repo, _ := initSharedDrainRig(t)
		worktreesRoot := filepath.Join(repo, ".worktrees")
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
		cityPath, cfg := sharedDrainWorkspaceCity(t, repo)

		_, err := sharedDrainWorkspaceProvisioner(cityPath, repo, cfg)(sharedDrainControl("gc-root2", "gc-drain2"))
		if err == nil {
			t.Fatal("provision succeeded, want a refusal for a root inside the rig checkout")
		}
		for _, want := range []string{worktreesRoot, "GC_WORKTREES_DIR", "T3CODE_WORKTREES_DIR", "T3CODE_HOME"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("control without a workflow root", func(t *testing.T) {
		repo, _ := initSharedDrainRig(t)
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

// drainItemWorkFormula is a drain item formula with one worker step.
const drainItemWorkFormula = `formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"
metadata = { "gc.run_target" = "worker" }
`

// sharedDrainDispatchFixture is a rig-scoped shared exclusive drain over two
// task members, ready for the real control dispatcher. The rig alpha is a git
// repository.
type sharedDrainDispatchFixture struct {
	cityPath      string
	repo          string
	worktreesRoot string
	cfg           *config.City
	store         *beads.MemStore
	rootID        string
	drainID       string
	memberIDs     []string
}

// newSharedDrainDispatchFixture seeds the fixture with itemFormula as the
// drain's item formula. With externalRoot the worktrees root
// (GC_WORKTREES_DIR) lies outside both the city and the rig; without it the
// root is the default <city>/.gc/worktrees.
func newSharedDrainDispatchFixture(t *testing.T, itemFormula string, externalRoot bool) sharedDrainDispatchFixture {
	t.Helper()
	repo, _ := initSharedDrainRig(t)
	cityPath, cfg := sharedDrainWorkspaceCity(t, repo)
	for _, key := range []string{"GC_WORKTREES_DIR", "T3CODE_WORKTREES_DIR", "T3CODE_HOME"} {
		t.Setenv(key, "")
	}
	worktreesRoot := filepath.Join(cityPath, ".gc", "worktrees")
	if externalRoot {
		worktreesRoot = t.TempDir()
		t.Setenv("GC_WORKTREES_DIR", worktreesRoot)
	}
	cfg.Agents = []config.Agent{
		{Name: config.ControlDispatcherAgentName, Dir: "alpha"},
		{Name: "worker", Dir: "alpha"},
	}
	formulaDir := t.TempDir()
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
	return sharedDrainDispatchFixture{
		cityPath:      cityPath,
		repo:          repo,
		worktreesRoot: worktreesRoot,
		cfg:           cfg,
		store:         store,
		rootID:        root.ID,
		drainID:       drain.ID,
		memberIDs:     memberIDs,
	}
}

// dispatch runs the real control dispatcher over one control bead.
func (f sharedDrainDispatchFixture) dispatch(t *testing.T, beadID string) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(f.cityPath, f.repo, f.store, beadID, f.cfg, &stdout, &stderr); err != nil {
		t.Fatalf("control dispatch %s: %v; stderr=%q", beadID, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// workspace is the drain's managed rig worktree.
func (f sharedDrainDispatchFixture) workspace() string {
	return filepath.Join(canonicalTestPath(filepath.Join(f.worktreesRoot, "alpha")), f.rootID+"-"+f.drainID)
}

// firstItemRoot returns the item root the drain materialized for its first
// member.
func (f sharedDrainDispatchFixture) firstItemRoot(t *testing.T, stdout, stderr string) beads.Bead {
	t.Helper()
	raw := beadByID(t, f.store, f.drainID).Metadata[beadmeta.DrainManifestMetadataKey]
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
		t.Fatalf("manifest = %s, want the first member's item materialized; drain metadata=%v stdout=%q stderr=%q", raw, beadByID(t, f.store, f.drainID).Metadata, stdout, stderr)
	}
	return beadByID(t, f.store, manifest.Rows[0].ItemRootID)
}

// TestControlDispatchSharedDrainProvisionsRigWorktree drives the real
// dispatcher entry point over a rig-scoped shared exclusive drain and pins that
// the reserved member and its expanded item work in the drain's managed rig
// worktree.
func TestControlDispatchSharedDrainProvisionsRigWorktree(t *testing.T) {
	f := newSharedDrainDispatchFixture(t, drainItemWorkFormula, true)
	stdout, stderr := f.dispatch(t, f.drainID)

	want := f.workspace()
	member := beadByID(t, f.store, f.memberIDs[0])
	if got := member.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != want {
		t.Fatalf("member %s work_dir = %q, want the drain worktree %q; stderr=%q", member.ID, got, want, stderr)
	}
	if got := member.Metadata[beadmeta.DrainWorkspaceControlMetadataKey]; got != f.drainID {
		t.Fatalf("member %s %s = %q, want the drain %s", member.ID, beadmeta.DrainWorkspaceControlMetadataKey, got, f.drainID)
	}
	if got := testutil.RunGit(t, want, "rev-parse", "--abbrev-ref", "HEAD"); got != "gc/drain/"+f.drainID {
		t.Fatalf("drain worktree branch = %q, want gc/drain/%s", got, f.drainID)
	}
	itemRoot := f.firstItemRoot(t, stdout, stderr)
	for _, key := range []string{beadmeta.LegacyWorkDirMetadataKey, beadmeta.WorkDirMetadataKey} {
		if got := itemRoot.Metadata[key]; got != want {
			t.Fatalf("item root %s %s = %q, want %q", itemRoot.ID, key, got, want)
		}
	}
	all, err := f.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list beads: %v", err)
	}
	steps := 0
	for _, step := range all {
		if step.ID == itemRoot.ID || step.Metadata[beadmeta.RootBeadIDMetadataKey] != itemRoot.ID {
			continue
		}
		steps++
		if got := resolveDrainSourceWorkDir(f.cityPath, f.store, step); got != want {
			t.Fatalf("resolveDrainSourceWorkDir(step %s) = %q, want %q", step.ID, got, want)
		}
	}
	if steps == 0 {
		t.Fatalf("item root %s has no materialized steps", itemRoot.ID)
	}
}

// TestControlDispatchSharedDrainLendsWorkspaceAfterItemMovesBranch drives two
// items of a shared drain through the real dispatcher. The first item renames
// the workspace's branch and commits on it, as an implementation role that
// names its own branch does. The drain still lends the second member the same
// workspace, as the first item left it, rather than re-verifying it against
// gc/drain/<drain id> and failing the drain.
func TestControlDispatchSharedDrainLendsWorkspaceAfterItemMovesBranch(t *testing.T) {
	f := newSharedDrainDispatchFixture(t, drainItemWorkFormula, true)
	stdout, stderr := f.dispatch(t, f.drainID)
	first := f.firstItemRoot(t, stdout, stderr)
	workspace := f.workspace()
	testutil.RunGit(t, workspace, "branch", "-m", "feature/named-by-the-item")
	testutil.RunGit(t, workspace, "commit", "--allow-empty", "-m", "first item's work")
	if err := f.store.SetMetadata(first.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
		t.Fatalf("record first item outcome: %v", err)
	}
	if err := f.store.Close(first.ID); err != nil {
		t.Fatalf("close first item root: %v", err)
	}

	_, stderr = f.dispatch(t, f.drainID)
	drain := beadByID(t, f.store, f.drainID)
	if drain.Status == "closed" {
		t.Fatalf("drain closed with outcome %q reason %q (%s), want it to lend the second member the workspace; stderr=%q",
			drain.Metadata[beadmeta.OutcomeMetadataKey], drain.Metadata[beadmeta.FailureReasonMetadataKey], drain.Metadata[beadmeta.ControllerErrorMetadataKey], stderr)
	}
	second := beadByID(t, f.store, f.memberIDs[1])
	for key, want := range map[string]string{
		beadmeta.LegacyWorkDirMetadataKey:         workspace,
		beadmeta.WorkDirMetadataKey:               workspace,
		beadmeta.DrainWorkspaceControlMetadataKey: f.drainID,
	} {
		if got := second.Metadata[key]; got != want {
			t.Fatalf("second member %s %s = %q, want %q", second.ID, key, got, want)
		}
	}
	if got := testutil.RunGit(t, workspace, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/named-by-the-item" {
		t.Fatalf("workspace branch = %q, want the first item's branch kept", got)
	}
}

// TestControlDispatchSharedDrainItemCheckRunsInRigWorktree drives a shared
// drain item whose step has a relative exec check through the real
// dispatcher. The check script lives only in the rig checkout (like the
// gitignored .gc/scripts), and the item's worker plants a different one at
// the same path in the drain worktree. The check must run in the worktree with
// the rig's script, never the planted one, both when the worktrees root lies
// outside the city and the rig and when it is the default under the city.
func TestControlDispatchSharedDrainItemCheckRunsInRigWorktree(t *testing.T) {
	for _, tc := range []struct {
		name         string
		externalRoot bool
	}{
		{name: "worktrees root outside the city and the rig", externalRoot: true},
		{name: "default worktrees root under the city"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSharedDrainDispatchFixture(t, drainItemWorkFormula+`
[steps.check]
max_attempts = 1

[steps.check.check]
mode = "exec"
path = ".gc/scripts/checks/verify.sh"
timeout = "30s"
`, tc.externalRoot)
			checkRel := filepath.Join(".gc", "scripts", "checks", "verify.sh")
			marker := filepath.Join(t.TempDir(), "check-cwd")
			script := filepath.Join(f.repo, checkRel)
			if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
				t.Fatalf("mkdir check scripts: %v", err)
			}
			if err := os.WriteFile(script, []byte("#!/bin/sh\npwd -P > '"+marker+"'\n"), 0o755); err != nil {
				t.Fatalf("write check script: %v", err)
			}
			stdout, stderr := f.dispatch(t, f.drainID)
			itemRoot := f.firstItemRoot(t, stdout, stderr)

			planted := filepath.Join(t.TempDir(), "planted-ran")
			plantedScript := filepath.Join(f.workspace(), checkRel)
			if err := os.MkdirAll(filepath.Dir(plantedScript), 0o755); err != nil {
				t.Fatalf("mkdir planted check scripts: %v", err)
			}
			if err := os.WriteFile(plantedScript, []byte("#!/bin/sh\ntouch '"+planted+"'\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("write planted check script: %v", err)
			}
			all, err := f.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatalf("list beads: %v", err)
			}
			var attempt, check beads.Bead
			for _, bead := range all {
				if bead.Metadata[beadmeta.RootBeadIDMetadataKey] != itemRoot.ID {
					continue
				}
				switch {
				case bead.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindRalph:
					check = bead
				case bead.Metadata[beadmeta.StepRefMetadataKey] == "work.iteration.1":
					attempt = bead
				}
			}
			if attempt.ID == "" || check.ID == "" {
				t.Fatalf("item %s attempt=%q check=%q, want both materialized", itemRoot.ID, attempt.ID, check.ID)
			}
			if err := f.store.SetMetadata(attempt.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
				t.Fatalf("record attempt outcome: %v", err)
			}
			if err := f.store.Close(attempt.ID); err != nil {
				t.Fatalf("close attempt: %v", err)
			}

			_, stderr = f.dispatch(t, check.ID)
			check = beadByID(t, f.store, check.ID)
			if check.Status != "closed" || check.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
				t.Fatalf("check status=%q outcome=%q, want closed pass; metadata=%v stderr=%q", check.Status, check.Metadata[beadmeta.OutcomeMetadataKey], check.Metadata, stderr)
			}
			if _, err := os.Stat(planted); !os.IsNotExist(err) {
				t.Fatalf("stat planted marker: %v, want the script planted in the drain worktree never to run", err)
			}
			ran, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("read check marker: %v; stderr=%q", err, stderr)
			}
			if got, want := strings.TrimSpace(string(ran)), f.workspace(); got != want {
				t.Fatalf("check ran in %q, want the drain worktree %q", got, want)
			}
		})
	}
}

// TestControlDispatchScopeAbortReleasesSkippedSharedDrainOnSplitCity drives
// the real dispatcher over a control whose failed close aborts the scope of a
// shared exclusive drain, on a split city: the drain and the scope are graph
// class in the binding, and the member the drain lent its worktree to is a
// work bead in the scope store. The abort closes the drain as skipped, so it
// must take back the loan and the reservation in the drain's place, and it
// reaches the member only through the work-class member tail the dispatcher
// gives the control. Both abort triggers are covered: a scope-check over a
// failed member, and a control the dispatcher quarantines, whose quarantine
// close is a failed close of a scope member too.
func TestControlDispatchScopeAbortReleasesSkippedSharedDrainOnSplitCity(t *testing.T) {
	for _, tc := range []struct {
		name string
		// failedSubject: the scope-check blocks on a member that failed; else
		// it has no subject, so the dispatcher quarantines it.
		failedSubject bool
	}{
		{name: "scope-check over a failed member", failedSubject: true},
		{name: "quarantined scope member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			scopeStore := beads.NewMemStore()
			graphStore := beads.NewMemStoreFrom(1000, nil, nil)
			seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graphStore))

			convoy, err := scopeStore.Create(beads.Bead{
				Title:    "input convoy",
				Type:     "convoy",
				Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
			})
			if err != nil {
				t.Fatalf("create input convoy: %v", err)
			}
			var memberIDs []string
			for _, title := range []string{"first", "second"} {
				member, err := scopeStore.Create(beads.Bead{Title: title, Type: "task"})
				if err != nil {
					t.Fatalf("create member: %v", err)
				}
				if err := convoycore.TrackItem(scopeStore, convoy.ID, member.ID); err != nil {
					t.Fatalf("track member: %v", err)
				}
				memberIDs = append(memberIDs, member.ID)
			}
			mustCreateGraph := func(bead beads.Bead) beads.Bead {
				t.Helper()
				created, err := graphStore.Create(bead)
				if err != nil {
					t.Fatalf("create %s: %v", bead.Title, err)
				}
				return created
			}
			root := mustCreateGraph(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
				"gc.formula_contract":    "graph.v2",
				"gc.input_convoy_id":     convoy.ID,
			}})
			inScope := func(role string, metadata map[string]string) map[string]string {
				metadata[beadmeta.RootBeadIDMetadataKey] = root.ID
				metadata[beadmeta.ScopeRefMetadataKey] = "body"
				metadata[beadmeta.ScopeRoleMetadataKey] = role
				return metadata
			}
			drain := mustCreateGraph(beads.Bead{Title: "drain", Type: "task", Metadata: inScope("member", map[string]string{
				beadmeta.KindMetadataKey:    beadmeta.KindDrain,
				"gc.drain_context":          "shared",
				"gc.drain_formula":          "drain-item",
				"gc.drain_member_access":    "exclusive",
				"gc.drain_item_single_lane": "true",
			})})
			mustCreateGraph(beads.Bead{Title: "body", Type: "task", Metadata: inScope("body", map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindScope,
			})})

			// The drain's first pass lends its worktree to the first member and
			// reserves it, as the dispatcher's drain arm does on this city.
			workspace := filepath.Join(t.TempDir(), "drain-worktree")
			lend := dispatch.ProcessOptions{
				FormulaSearchPaths:   []string{splitClassDrainFormulaDir(t)},
				SharedDrainWorkspace: func(beads.Bead) (string, error) { return workspace, nil },
				MemberStores:         []beads.Store{scopeStore},
			}
			if _, err := dispatch.ProcessControl(graphStore, beadByID(t, graphStore, drain.ID), lend); err != nil {
				t.Fatalf("ProcessControl(drain first pass): %v", err)
			}
			lent := beadByID(t, scopeStore, memberIDs[0]).Metadata
			if lent[beadmeta.LegacyWorkDirMetadataKey] != workspace || lent[beadmeta.ExclusiveDrainReservationMetadataKey] != drain.ID {
				t.Fatalf("first member metadata = %v, want the drain's worktree lent and the member reserved before the abort", lent)
			}

			scopeCheck := mustCreateGraph(beads.Bead{Title: "scope check", Type: "task", Metadata: inScope("control", map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindScopeCheck,
			})})
			if tc.failedSubject {
				failed := mustCreateGraph(beads.Bead{Title: "failed member", Type: "task", Metadata: inScope("member", map[string]string{
					beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail,
				})})
				if err := graphStore.Close(failed.ID); err != nil {
					t.Fatalf("close failed member: %v", err)
				}
				if err := graphStore.DepAdd(scopeCheck.ID, failed.ID, "blocks"); err != nil {
					t.Fatalf("block scope check on failed member: %v", err)
				}
			}

			cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
			var stdout, stderr bytes.Buffer
			if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, scopeStore, scopeCheck.ID, cfg, &stdout, &stderr); err != nil {
				t.Fatalf("control dispatch: %v; stderr=%q", err, stderr.String())
			}
			if got := beadByID(t, graphStore, scopeCheck.ID); got.Status != "closed" {
				t.Fatalf("scope check status = %q, want closed; stderr=%q", got.Status, stderr.String())
			}
			if quarantined := beadByID(t, graphStore, scopeCheck.ID).Metadata[beadmeta.ControlQuarantinedMetadataKey] == "true"; quarantined == tc.failedSubject {
				t.Fatalf("scope check quarantined = %t, want %t; stderr=%q", quarantined, !tc.failedSubject, stderr.String())
			}
			skipped := beadByID(t, graphStore, drain.ID)
			if skipped.Status != "closed" || skipped.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
				t.Fatalf("drain = status %q outcome %q, want the abort to close it skipped", skipped.Status, skipped.Metadata[beadmeta.OutcomeMetadataKey])
			}
			for _, memberID := range memberIDs {
				member := beadByID(t, scopeStore, memberID)
				for _, key := range []string{
					beadmeta.LegacyWorkDirMetadataKey,
					beadmeta.WorkDirMetadataKey,
					beadmeta.DrainWorkspaceControlMetadataKey,
					beadmeta.ExclusiveDrainReservationMetadataKey,
				} {
					if got := member.Metadata[key]; got != "" {
						t.Fatalf("member %s %s = %q after the abort skipped drain %s, want it taken back", memberID, key, got, drain.ID)
					}
				}
			}
		})
	}
}
