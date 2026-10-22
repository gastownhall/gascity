package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/worktree"
)

// implicitSlotGitOut runs git in dir and returns trimmed stdout, failing the
// test on error.
func implicitSlotGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("git %v: %v\n%s", args, err, ee.Stderr)
		}
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// implicitSlotBranchFor returns the deterministic slot branch name for a slot
// path: gc-<instance>-<12 hex of sha256(path)>.
func implicitSlotBranchFor(slotPath string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(slotPath)))
	return "gc-" + filepath.Base(filepath.Clean(slotPath)) + "-" + hex.EncodeToString(sum[:])[:12]
}

// implicitSlotTestConfig builds a city config with one real git rig "app"
// (one commit on main) and the injected rig-scoped implicit claude agent.
func implicitSlotTestConfig(t *testing.T, cityPath string) (*config.City, *config.Agent, string) {
	t.Helper()
	rigRoot := filepath.Join(cityPath, "rigs", "app")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}
	implicitSlotGitOut(t, rigRoot, "init", "-b", "main")
	implicitSlotGitOut(t, rigRoot, "config", "user.email", "test@example.com")
	implicitSlotGitOut(t, rigRoot, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(rigRoot, "hello.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("writing rig file: %v", err)
	}
	implicitSlotGitOut(t, rigRoot, "add", "hello.txt")
	implicitSlotGitOut(t, rigRoot, "commit", "-m", "initial")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city", Provider: "claude"},
		Providers: map[string]config.ProviderSpec{"claude": config.BuiltinProviderAlias("claude")},
		Rigs:      []config.Rig{{Name: "app", Path: rigRoot}},
	}
	config.InjectImplicitAgents(cfg)
	for i := range cfg.Agents {
		if cfg.Agents[i].Dir == "app" && cfg.Agents[i].Name == "claude" {
			return cfg, &cfg.Agents[i], rigRoot
		}
	}
	t.Fatal("rig-scoped implicit claude agent not found after InjectImplicitAgents")
	return nil, nil, ""
}

// TestImplicitRigPoolSlotProvisioning is AC-1: starting two numbered instances
// of a rig-scoped implicit agent provisions each in its own git worktree on a
// distinct gc-<instance>-<hash> branch from rig HEAD, isolates their files
// from each other and from the rig root, and leaves the rig root's branch,
// index, and files unchanged.
func TestImplicitRigPoolSlotProvisioning(t *testing.T) {
	cityPath := t.TempDir()
	cfg, agent, rigRoot := implicitSlotTestConfig(t, cityPath)
	rigHead := implicitSlotGitOut(t, rigRoot, "rev-parse", "HEAD")

	first, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("resolving app/claude-1: %v", err)
	}
	second, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-2", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("resolving app/claude-2: %v", err)
	}

	wantFirst := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-1")
	wantSecond := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-2")
	if first != wantFirst || second != wantSecond {
		t.Fatalf("work dirs = (%q, %q), want (%q, %q)", first, second, wantFirst, wantSecond)
	}
	if first == second {
		t.Fatalf("pool instances share one working directory %q", first)
	}

	// Each slot is a registered worktree of the rig on its own deterministic
	// branch, created from rig HEAD.
	for _, tc := range []struct{ slot, branch string }{
		{first, implicitSlotBranchFor(wantFirst)},
		{second, implicitSlotBranchFor(wantSecond)},
	} {
		if ref := implicitSlotGitOut(t, tc.slot, "symbolic-ref", "--quiet", "HEAD"); ref != "refs/heads/"+tc.branch {
			t.Errorf("slot %q HEAD = %q, want refs/heads/%q", tc.slot, ref, tc.branch)
		}
		if head := implicitSlotGitOut(t, tc.slot, "rev-parse", "HEAD"); head != rigHead {
			t.Errorf("slot %q HEAD = %q, want rig HEAD %q", tc.slot, head, rigHead)
		}
	}

	// A file written in one slot appears in neither the other slot nor the
	// rig root.
	if err := os.WriteFile(filepath.Join(first, "from-1.txt"), []byte("only in slot 1\n"), 0o644); err != nil {
		t.Fatalf("writing slot-1 file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "from-1.txt")); !os.IsNotExist(err) {
		t.Errorf("slot-1 file leaked into slot 2: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigRoot, "from-1.txt")); !os.IsNotExist(err) {
		t.Errorf("slot-1 file leaked into the rig root: %v", err)
	}

	// The rig root is untouched: same branch, clean index, original files.
	if ref := implicitSlotGitOut(t, rigRoot, "symbolic-ref", "--quiet", "HEAD"); ref != "refs/heads/main" {
		t.Errorf("rig root HEAD = %q, want refs/heads/main", ref)
	}
	if status := implicitSlotGitOut(t, rigRoot, "status", "--porcelain"); status != "" {
		t.Errorf("rig root working tree dirty:\n%s", status)
	}
	if data, err := os.ReadFile(filepath.Join(rigRoot, "hello.txt")); err != nil || string(data) != "v1\n" {
		t.Errorf("rig root hello.txt = %q (%v), want %q", string(data), err, "v1\n")
	}
}

// TestHookClaimIdentityPatchStampsPoolSlotCheckout pins AC-1's provenance
// clause: a bead claimed from a session running in an implicit pool slot
// worktree records both the slot checkout and its deterministic slot branch,
// via the same gc-2n4c session fallback a per-bead worktree uses.
func TestHookClaimIdentityPatchStampsPoolSlotCheckout(t *testing.T) {
	slot := filepath.Join(t.TempDir(), ".gc", "worktrees", "app", "claude-1")
	branch := implicitSlotBranchFor(slot)

	bead := beads.Bead{ID: "hw-slot-1", Metadata: beads.StringMap{}}
	ops := hookClaimOps{
		ResolveWorkBranch:     hookClaimBranchByDir(map[string]string{slot: branch}),
		ResolveSessionWorkDir: func(string) string { return slot },
	}
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=mc-sess1"}}

	patch := hookClaimIdentityPatch(bead, opts, ops, hookClaimWorkBranchStoreDir)

	if got := patch[beadmeta.WorkDirMetadataKey]; got != slot {
		t.Errorf("gc.work_dir = %q, want the claiming session's slot checkout %q", got, slot)
	}
	if got := patch[beadmeta.WorkBranchMetadataKey]; got != branch {
		t.Errorf("gc.work_branch = %q, want the slot branch %q", got, branch)
	}
}

// implicitSlotNoneCity loads a city config with
// workspace.implicit_isolation = "none", attaches the builtin claude
// provider and a real git rig, and runs InjectImplicitAgents.
func implicitSlotNoneCity(t *testing.T, cityPath string) (*config.City, string) {
	t.Helper()
	rigRoot := filepath.Join(cityPath, "rigs", "app")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir rig .git: %v", err)
	}
	cfg, err := config.Parse([]byte("[workspace]\nname = \"city\"\nimplicit_isolation = \"none\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cfg.Providers = map[string]config.ProviderSpec{"claude": config.BuiltinProviderAlias("claude")}
	cfg.Rigs = []config.Rig{{Name: "app", Path: rigRoot}}
	config.InjectImplicitAgents(cfg)
	return cfg, rigRoot
}

// TestImplicitIsolationNoneKeepsSharedRigRoot pins the opt-out: with
// implicit_isolation = "none", a numbered pool instance resolves to the rig
// root exactly as before, and nothing is created under .gc/worktrees.
func TestImplicitIsolationNoneKeepsSharedRigRoot(t *testing.T) {
	cityPath := t.TempDir()
	cfg, rigRoot := implicitSlotNoneCity(t, cityPath)

	var agent *config.Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Dir == "app" && cfg.Agents[i].Name == "claude" {
			agent = &cfg.Agents[i]
		}
	}
	if agent == nil {
		t.Fatal("rig-scoped implicit claude agent not found after InjectImplicitAgents")
	}

	dir, err := resolveWorkDirForQualifiedName(cityPath, cfg, agent, "app/claude-1")
	if err != nil {
		t.Fatalf("resolving app/claude-1 under mode none: %v", err)
	}
	if dir != rigRoot {
		t.Errorf("app/claude-1 work dir = %q, want the rig root %q under mode none", dir, rigRoot)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "worktrees")); !os.IsNotExist(err) {
		t.Errorf("mode none created something under .gc/worktrees: %v", err)
	}
}

// TestImplicitSlotSkipsNonHostLocalProviders pins the runtime gate: a slot
// worktree can only be provisioned on the machine the controller runs on, so
// session providers that execute elsewhere — k8s, hybrid routing, ssh:, and
// exec: transports, plus the fake/fail test runtimes — keep today's rig-root
// resolution and create nothing.
func TestImplicitSlotSkipsNonHostLocalProviders(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	cityPath := t.TempDir()
	cfg, agent, rigRoot := implicitSlotTestConfig(t, cityPath)

	for _, provider := range []string{"k8s", "hybrid", "ssh:build.example.internal", "exec:/bin/true", "fake", "fail"} {
		t.Run(provider, func(t *testing.T) {
			cfg.Session.Provider = provider
			dir, err := resolveWorkDirForQualifiedName(cityPath, cfg, agent, "app/claude-1")
			if err != nil {
				t.Fatalf("resolving app/claude-1 under provider %q: %v", provider, err)
			}
			if dir != rigRoot {
				t.Errorf("provider %q: work dir = %q, want the rig root %q", provider, dir, rigRoot)
			}
			if _, err := os.Stat(filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-1")); !os.IsNotExist(err) {
				t.Errorf("provider %q created a slot worktree; remote runtimes must not get one", provider)
			}
		})
	}
}

// TestImplicitSlotReuseRemovesStaleMarkerAndKeepsWork is AC-3's reuse half:
// a slot left dirty, committed, or on a switched branch, with the pruner's
// .worktree-stale marker on it, is reused untouched by the next session —
// same directory, same work — and only the stale marker is removed.
func TestImplicitSlotReuseRemovesStaleMarkerAndKeepsWork(t *testing.T) {
	cityPath := t.TempDir()
	cfg, agent, _ := implicitSlotTestConfig(t, cityPath)

	slot, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("first resolve of app/claude-1: %v", err)
	}

	// Slot carries committed work, uncommitted edits, and a switched branch.
	committedBefore := implicitSlotGitOut(t, slot, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(slot, "work.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatalf("writing committed file: %v", err)
	}
	implicitSlotGitOut(t, slot, "add", "work.txt")
	implicitSlotGitOut(t, slot, "commit", "-m", "slot work")
	if err := os.WriteFile(filepath.Join(slot, "uncommitted.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("writing uncommitted file: %v", err)
	}
	implicitSlotGitOut(t, slot, "checkout", "-b", "gc-switched-away")

	if err := os.WriteFile(filepath.Join(slot, ".worktree-stale"), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("writing stale marker: %v", err)
	}

	again, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("second resolve of app/claude-1: %v", err)
	}
	if again != slot {
		t.Fatalf("second resolve = %q, want to reuse the existing slot %q", again, slot)
	}

	// Untouched: the committed work is still on the switched branch, the
	// uncommitted edit is still present.
	if ref := implicitSlotGitOut(t, slot, "symbolic-ref", "--quiet", "HEAD"); ref != "refs/heads/gc-switched-away" {
		t.Errorf("slot HEAD = %q, want the slot's own switched branch untouched", ref)
	}
	if data, err := os.ReadFile(filepath.Join(slot, "uncommitted.txt")); err != nil || string(data) != "dirty\n" {
		t.Errorf("uncommitted edit lost or altered: %q (%v)", string(data), err)
	}
	if head := implicitSlotGitOut(t, slot, "rev-parse", "HEAD"); head == committedBefore {
		// The slot had a commit; HEAD must have moved past the base commit
		// and still reach it.
		if !strings.Contains(implicitSlotGitOut(t, slot, "log", "--oneline"), "slot work") {
			t.Errorf("committed work lost from slot branch")
		}
	}

	// The stale marker is the only thing removed.
	if _, err := os.Stat(filepath.Join(slot, ".worktree-stale")); !os.IsNotExist(err) {
		t.Errorf(".worktree-stale marker survived slot reuse: %v", err)
	}
}

// TestImplicitSlotReuseRunsNoGitSubprocess pins the reuse path's purity: a
// valid existing slot is validated and reclaimed through the filesystem
// alone — neither the git seam nor worktree.Ensure may run.
func TestImplicitSlotReuseRunsNoGitSubprocess(t *testing.T) {
	cityPath := t.TempDir()
	cfg, agent, _ := implicitSlotTestConfig(t, cityPath)

	slot, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("first resolve of app/claude-1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slot, "uncommitted.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("writing uncommitted file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slot, ".worktree-stale"), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("writing stale marker: %v", err)
	}

	newImplicitSlotGit = func(string) implicitSlotGitOps {
		t.Error("reuse path ran a git subprocess")
		return nil
	}
	implicitSlotWorktreeEnsure = func(worktree.Spec) (worktree.Report, error) {
		t.Error("reuse path reached worktree.Ensure")
		return worktree.Report{}, nil
	}
	defer func() {
		newImplicitSlotGit = func(dir string) implicitSlotGitOps { return git.New(dir) }
		implicitSlotWorktreeEnsure = worktree.Ensure
	}()

	if _, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, ""); err != nil {
		t.Fatalf("second resolve of app/claude-1: %v", err)
	}
}

// TestImplicitSlotRecreateAfterDirectoryLoss is AC-3's recreate half: a
// slot whose directory vanished while its registration and unmerged branch
// remain is re-created at the same path on the same branch, with the work
// the branch held intact.
func TestImplicitSlotRecreateAfterDirectoryLoss(t *testing.T) {
	cityPath := t.TempDir()
	cfg, agent, rigRoot := implicitSlotTestConfig(t, cityPath)

	slot, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("first resolve of app/claude-1: %v", err)
	}
	branch := implicitSlotBranchFor(slot)
	if err := os.WriteFile(filepath.Join(slot, "kept.txt"), []byte("unmerged work\n"), 0o644); err != nil {
		t.Fatalf("writing slot file: %v", err)
	}
	implicitSlotGitOut(t, slot, "add", "kept.txt")
	implicitSlotGitOut(t, slot, "commit", "-m", "slot work")
	oldHead := implicitSlotGitOut(t, slot, "rev-parse", "HEAD")

	// The directory is lost (crash, cleanup); registration and branch stay.
	if err := os.RemoveAll(slot); err != nil {
		t.Fatalf("removing slot directory: %v", err)
	}

	again, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("recreating app/claude-1: %v", err)
	}
	if again != slot {
		t.Fatalf("recreated slot at %q, want the same path %q", again, slot)
	}
	if ref := implicitSlotGitOut(t, slot, "symbolic-ref", "--quiet", "HEAD"); ref != "refs/heads/"+branch {
		t.Errorf("recreated slot HEAD = %q, want the slot's unmerged branch re-attached", ref)
	}
	if head := implicitSlotGitOut(t, slot, "rev-parse", "HEAD"); head != oldHead {
		t.Errorf("recreated slot HEAD = %q, want the unmerged branch tip %q", head, oldHead)
	}
	if data, err := os.ReadFile(filepath.Join(slot, "kept.txt")); err != nil || string(data) != "unmerged work\n" {
		t.Errorf("unmerged work lost on recreate: %q (%v)", string(data), err)
	}
	// Exactly one registration remains for the slot.
	seen := 0
	for _, line := range strings.Split(implicitSlotGitOut(t, rigRoot, "worktree", "list", "--porcelain"), "\n") {
		if strings.HasPrefix(line, "worktree ") && strings.HasSuffix(line, filepath.Join("app", "claude-1")) {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("slot has %d registrations after recreate, want exactly 1", seen)
	}
}

// TestImplicitSlotRecreateMergedBranchRestartsFromHead is the recreate
// complement: a slot branch git can prove fully merged is deleted before
// re-creation, so the slot restarts from rig HEAD rather than re-attaching
// its stale tip.
func TestImplicitSlotRecreateMergedBranchRestartsFromHead(t *testing.T) {
	cityPath := t.TempDir()
	cfg, agent, rigRoot := implicitSlotTestConfig(t, cityPath)

	slot, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("first resolve of app/claude-1: %v", err)
	}
	branch := implicitSlotBranchFor(slot)
	if err := os.WriteFile(filepath.Join(slot, "merged.txt"), []byte("merged work\n"), 0o644); err != nil {
		t.Fatalf("writing slot file: %v", err)
	}
	implicitSlotGitOut(t, slot, "add", "merged.txt")
	implicitSlotGitOut(t, slot, "commit", "-m", "slot work")
	// Merge the slot branch into the rig's mainline, then advance mainline:
	// the slot branch is provably merged, and rig HEAD has moved past it.
	implicitSlotGitOut(t, rigRoot, "merge", branch)
	if err := os.WriteFile(filepath.Join(rigRoot, "later.txt"), []byte("mainline moved\n"), 0o644); err != nil {
		t.Fatalf("writing mainline file: %v", err)
	}
	implicitSlotGitOut(t, rigRoot, "add", "later.txt")
	implicitSlotGitOut(t, rigRoot, "commit", "-m", "mainline moves on")
	rigHead := implicitSlotGitOut(t, rigRoot, "rev-parse", "HEAD")

	if err := os.RemoveAll(slot); err != nil {
		t.Fatalf("removing slot directory: %v", err)
	}

	again, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("recreating app/claude-1: %v", err)
	}
	if again != slot {
		t.Fatalf("recreated slot at %q, want the same path %q", again, slot)
	}
	if head := implicitSlotGitOut(t, slot, "rev-parse", "HEAD"); head != rigHead {
		t.Errorf("recreated slot HEAD = %q, want a fresh start from rig HEAD %q", head, rigHead)
	}
	if ref := implicitSlotGitOut(t, slot, "symbolic-ref", "--quiet", "HEAD"); ref != "refs/heads/"+branch {
		t.Errorf("recreated slot HEAD = %q, want the same deterministic slot branch", ref)
	}
	if _, err := os.Stat(filepath.Join(slot, "later.txt")); err != nil {
		t.Errorf("recreated slot missing rig HEAD content: %v", err)
	}
}

// TestImplicitSlotRecreateFromEmptyDirectory covers the empty-directory
// variant of recreate: an empty leftover slot directory is cleared away and
// replaced by a real worktree rather than failing Ensure's not-a-repo check.
func TestImplicitSlotRecreateFromEmptyDirectory(t *testing.T) {
	cityPath := t.TempDir()
	cfg, agent, _ := implicitSlotTestConfig(t, cityPath)
	slotPath := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-1")
	if err := os.MkdirAll(slotPath, 0o755); err != nil {
		t.Fatalf("mkdir empty slot: %v", err)
	}

	got, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs, "")
	if err != nil {
		t.Fatalf("resolving over an empty slot directory: %v", err)
	}
	if got != slotPath {
		t.Fatalf("resolved %q, want the slot path %q", got, slotPath)
	}
	if ref := implicitSlotGitOut(t, slotPath, "symbolic-ref", "--quiet", "HEAD"); ref != "refs/heads/"+implicitSlotBranchFor(slotPath) {
		t.Errorf("slot HEAD = %q, want a provisioned worktree on the slot branch", ref)
	}
}
