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

	first, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-1", agent, cfg.Rigs)
	if err != nil {
		t.Fatalf("resolving app/claude-1: %v", err)
	}
	second, err := resolveConfiguredWorkDir(cityPath, cfg.Workspace.Name, "app/claude-2", agent, cfg.Rigs)
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
