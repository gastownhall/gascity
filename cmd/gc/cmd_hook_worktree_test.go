package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worktree"
)

func writeHookWorktreeOverlay(t *testing.T, cityPath, rigName string) string {
	t.Helper()
	overlayDir := filepath.Join(cityPath, "packs", rigName, "overlay")
	target := filepath.Join(overlayDir, "per-provider", "copilot", ".github", "copilot-instructions.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("MkdirAll overlay: %v", err)
	}
	if err := os.WriteFile(target, []byte("gas city instructions\n"), 0o644); err != nil {
		t.Fatalf("WriteFile overlay: %v", err)
	}
	return overlayDir
}

func addTrackedInstructions(t *testing.T, repo string) {
	t.Helper()
	testutil.RunGit(t, repo, "config", "core.autocrlf", "false")
	target := filepath.Join(repo, ".github", "copilot-instructions.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("MkdirAll instructions: %v", err)
	}
	if err := os.WriteFile(target, []byte("repository instructions\n"), 0o644); err != nil {
		t.Fatalf("WriteFile instructions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644); err != nil {
		t.Fatalf("WriteFile gitattributes: %v", err)
	}
	testutil.RunGit(t, repo, "add", ".github/copilot-instructions.md", ".gitattributes")
	testutil.RunGit(t, repo, "commit", "-m", "track instructions")
}

func TestHookClaimWorktreeProvisionerCreatesOwnedWorktreeAndSkipsTrackedOverlay(t *testing.T) {
	repo, base := testutil.InitGitRepo(t)
	addTrackedInstructions(t, repo)
	cityPath := t.TempDir()
	overlayDir := writeHookWorktreeOverlay(t, cityPath, "rig")
	agentCfg := config.Agent{Name: "worker", Scope: "rig", Dir: "rig", Provider: "copilot"}
	cfg := &config.City{
		Workspace:      config.Workspace{Name: "test-city"},
		Agents:         []config.Agent{agentCfg},
		Rigs:           []config.Rig{{Name: "rig", Path: repo, DefaultBranch: base}},
		RigOverlayDirs: map[string][]string{"rig": {overlayDir}},
	}

	provision, err := newHookClaimWorktreeProvisioner(cityPath, "test-city", cfg, &agentCfg, "rig/worker", &config.ResolvedProvider{Name: "copilot", BuiltinAncestor: "copilot"})
	if err != nil {
		t.Fatalf("newHookClaimWorktreeProvisioner: %v", err)
	}
	var patch map[string]string
	ops := hookClaimOps{
		StampWorkMeta: func(_ context.Context, _ string, _ []string, _ string, _ string, got map[string]string) error {
			patch = maps.Clone(got)
			return nil
		},
	}
	provisioned, updated, err := provision(beads.Bead{ID: "ga-work", Metadata: map[string]string{}}, hookClaimOptions{Assignee: "rig/worker"}, ops, repo, io.Discard)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	wantRoot := filepath.Join(cityPath, ".gc", "worktrees", "rig")
	wantPath := filepath.Join(wantRoot, "ga-work")
	if provisioned.WorkDir != wantPath {
		t.Fatalf("WorkDir = %q, want %q", provisioned.WorkDir, wantPath)
	}
	if provisioned.WorkBranch != "gc/rig/ga-work" {
		t.Fatalf("WorkBranch = %q, want gc/rig/ga-work", provisioned.WorkBranch)
	}
	if got := testutil.RunGit(t, provisioned.WorkDir, "rev-parse", "--abbrev-ref", "HEAD"); got != provisioned.WorkBranch {
		t.Fatalf("worktree branch = %q, want %q", got, provisioned.WorkBranch)
	}
	instructions, err := os.ReadFile(filepath.Join(provisioned.WorkDir, ".github", "copilot-instructions.md"))
	if err != nil {
		t.Fatalf("ReadFile instructions: %v", err)
	}
	if string(instructions) != "repository instructions\n" {
		t.Fatalf("tracked instructions were overwritten: %q", instructions)
	}
	for _, key := range []string{
		beadmeta.WorkDirMetadataKey,
		beadmeta.LegacyWorkDirMetadataKey,
		beadmeta.WorkBranchMetadataKey,
		beadmeta.RootStoreRefMetadataKey,
		beadmeta.WorktreeRepoMetadataKey,
		beadmeta.WorktreeRootMetadataKey,
		beadmeta.WorktreeBaseRefMetadataKey,
		beadmeta.WorktreeBaseSHAMetadataKey,
		beadmeta.WorktreeCreatorMetadataKey,
		beadmeta.WorktreeOwnerMetadataKey,
		beadmeta.WorktreeGenerationMetadataKey,
		beadmeta.WorktreeLifecycleMetadataKey,
	} {
		if strings.TrimSpace(patch[key]) == "" {
			t.Fatalf("metadata patch missing %s: %#v", key, patch)
		}
		if updated.Metadata[key] != patch[key] {
			t.Fatalf("updated metadata %s = %q, want patch value %q", key, updated.Metadata[key], patch[key])
		}
	}
	if patch[beadmeta.WorkDirMetadataKey] != wantPath || patch[beadmeta.LegacyWorkDirMetadataKey] != wantPath {
		t.Fatalf("work_dir patch = %q/%q, want %q", patch[beadmeta.WorkDirMetadataKey], patch[beadmeta.LegacyWorkDirMetadataKey], wantPath)
	}
	if patch[beadmeta.RootStoreRefMetadataKey] != "rig:rig" {
		t.Fatalf("root store ref = %q, want rig:rig", patch[beadmeta.RootStoreRefMetadataKey])
	}
}

func TestHookClaimWorktreeProvisionerRejectsIncompleteEvidence(t *testing.T) {
	repo, base := testutil.InitGitRepo(t)
	cityPath := t.TempDir()
	agentCfg := config.Agent{Name: "worker", Scope: "rig", Dir: "rig", Provider: "copilot"}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{agentCfg},
		Rigs:      []config.Rig{{Name: "rig", Path: repo, DefaultBranch: base}},
	}
	provision, err := newHookClaimWorktreeProvisioner(cityPath, "test-city", cfg, &agentCfg, "rig/worker", &config.ResolvedProvider{Name: "copilot"})
	if err != nil {
		t.Fatalf("newHookClaimWorktreeProvisioner: %v", err)
	}
	_, _, err = provision(beads.Bead{
		ID: "ga-work",
		Metadata: map[string]string{
			beadmeta.WorktreeRepoMetadataKey: repo,
		},
	}, hookClaimOptions{Assignee: "rig/worker"}, hookClaimOps{}, repo, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "incomplete worktree ownership metadata") {
		t.Fatalf("provision with incomplete evidence = %v, want incomplete-evidence error", err)
	}
}

func TestHookClaimWorktreeProvisionerRejectsOutsideCityRootOverride(t *testing.T) {
	repo, base := testutil.InitGitRepo(t)
	cityPath := t.TempDir()
	t.Setenv("GC_WORKTREES_DIR", t.TempDir())
	t.Setenv("T3CODE_WORKTREES_DIR", "")
	t.Setenv("T3CODE_HOME", "")
	agentCfg := config.Agent{Name: "worker", Scope: "rig", Dir: "rig", Provider: "copilot"}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{agentCfg},
		Rigs:      []config.Rig{{Name: "rig", Path: repo, DefaultBranch: base}},
	}
	_, err := newHookClaimWorktreeProvisioner(cityPath, "test-city", cfg, &agentCfg, "rig/worker", &config.ResolvedProvider{Name: "copilot"})
	if err == nil || !strings.Contains(err.Error(), "requires worktrees under") {
		t.Fatalf("newHookClaimWorktreeProvisioner with outside GC_WORKTREES_DIR = %v, want outside-city-root refusal", err)
	}
}

func TestHookClaimWorktreeProvisionerReusesPublishedEvidence(t *testing.T) {
	repo, base := testutil.InitGitRepo(t)
	addTrackedInstructions(t, repo)
	cityPath := t.TempDir()
	agentCfg := config.Agent{Name: "worker", Scope: "rig", Dir: "rig", Provider: "copilot"}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{agentCfg},
		Rigs:      []config.Rig{{Name: "rig", Path: repo, DefaultBranch: base}},
	}
	root := filepath.Join(cityPath, ".gc", "worktrees", "rig")
	publishedPath := filepath.Join(root, "published-non-derived")
	spec := worktree.Spec{
		RepoDir:       repo,
		Root:          root,
		Path:          publishedPath,
		Branch:        "gc/rig/published-non-derived",
		Base:          base,
		BeadID:        "ga-work",
		StoreRef:      "rig:rig",
		Creator:       hookWorktreeCreator,
		Owner:         "rig-worker",
		Generation:    hookWorktreeGeneration,
		Lifecycle:     worktree.LifecycleActive,
		SkipLFSSmudge: true,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure published worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(publishedPath, "local.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile local: %v", err)
	}
	testutil.RunGit(t, publishedPath, "add", "local.txt")
	testutil.RunGit(t, publishedPath, "commit", "-m", "local work")
	patch, err := hookWorktreeMetadataPatch(spec, report)
	if err != nil {
		t.Fatalf("hookWorktreeMetadataPatch: %v", err)
	}
	bead := beads.Bead{ID: "ga-work", Metadata: patch}
	provision, err := newHookClaimWorktreeProvisioner(cityPath, "test-city", cfg, &agentCfg, "rig/worker", &config.ResolvedProvider{Name: "copilot"})
	if err != nil {
		t.Fatalf("newHookClaimWorktreeProvisioner: %v", err)
	}
	ops := hookClaimOps{
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			t.Fatal("StampWorkMeta was called while reusing complete published evidence")
			return nil
		},
	}
	provisioned, updated, err := provision(bead, hookClaimOptions{Assignee: "rig/worker"}, ops, repo, io.Discard)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if provisioned.WorkDir != publishedPath {
		t.Fatalf("WorkDir = %q, want published path %q", provisioned.WorkDir, publishedPath)
	}
	if provisioned.WorkBranch != spec.Branch {
		t.Fatalf("WorkBranch = %q, want %q", provisioned.WorkBranch, spec.Branch)
	}
	if updated.Metadata[beadmeta.WorkDirMetadataKey] != publishedPath {
		t.Fatalf("updated work dir = %q, want unchanged published path %q", updated.Metadata[beadmeta.WorkDirMetadataKey], publishedPath)
	}
	derived := filepath.Join(root, "ga-work")
	if _, err := os.Stat(derived); !os.IsNotExist(err) {
		t.Fatalf("derived worktree %q exists after evidence reuse (err=%v)", derived, err)
	}
	if got := testutil.RunGit(t, publishedPath, "log", "-1", "--format=%s"); got != "local work" {
		t.Fatalf("published worktree HEAD = %q, want local work commit", got)
	}
}

func TestHookClaimWorktreeProvisionerKeepsCityStoreOwnership(t *testing.T) {
	repo, base := testutil.InitGitRepo(t)
	cityPath := t.TempDir()
	agentCfg := config.Agent{Name: "worker", Scope: "rig", Dir: "rig", Provider: "copilot"}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{agentCfg},
		Rigs:      []config.Rig{{Name: "rig", Path: repo, DefaultBranch: base}},
	}
	provision, err := newHookClaimWorktreeProvisioner(cityPath, "test-city", cfg, &agentCfg, "rig/worker", &config.ResolvedProvider{Name: "copilot"})
	if err != nil {
		t.Fatalf("newHookClaimWorktreeProvisioner: %v", err)
	}
	_, updated, err := provision(beads.Bead{ID: "ga-work", Metadata: map[string]string{}}, hookClaimOptions{Assignee: "rig/worker"}, hookClaimOps{StampWorkMeta: noopStampWorkMeta}, cityPath, io.Discard)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := updated.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "city:test-city" {
		t.Fatalf("root store ref = %q, want city:test-city", got)
	}
	spec, err := worktreeSpecForBead(updated, "city")
	if err != nil {
		t.Fatalf("worktreeSpecForBead: %v", err)
	}
	if spec == nil {
		t.Fatal("worktreeSpecForBead returned nil spec")
	}
	if _, err := verifiedPoolTriggerWorkDir(nil, nil, "", SessionRequest{WorkBeadID: updated.ID, WorkStoreRef: "city", WorktreeSpec: spec}); err != nil {
		t.Fatalf("city claim cannot resume from published evidence: %v", err)
	}
}

func TestHookClaimWorktreeReceiptIncludesWorktreeFields(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"ga-work","status":"open","assignee":"","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			return beads.Bead{ID: "ga-work", Status: "in_progress", Assignee: "worker", Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		StampWorkMeta: noopStampWorkMeta,
		ProvisionWorktree: func(bead beads.Bead, _ hookClaimOptions, _ hookClaimOps, _ string, _ io.Writer) (hookProvisionedWorktree, beads.Bead, error) {
			bead.Metadata[beadmeta.WorkDirMetadataKey] = `C:\city\.gc\worktrees\rig\ga-work`
			bead.Metadata[beadmeta.WorkBranchMetadataKey] = "gc/rig/ga-work"
			return hookProvisionedWorktree{WorkDir: bead.Metadata[beadmeta.WorkDirMetadataKey], WorkBranch: bead.Metadata[beadmeta.WorkBranchMetadataKey]}, bead, nil
		},
		StampSessionClaim: func(string, string) error { return nil },
		PublishRunMap:     func(string, string, ...string) error { return nil },
	}
	opts := hookClaimOptions{
		Assignee:           "worker",
		IdentityCandidates: []string{"worker"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}
	if code := doHookClaim("query", t.TempDir(), opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal result: %v\n%s", err, stdout.String())
	}
	if result.WorkDir != `C:\city\.gc\worktrees\rig\ga-work` || result.WorkBranch != "gc/rig/ga-work" {
		t.Fatalf("worktree result fields = %q/%q", result.WorkDir, result.WorkBranch)
	}
}
