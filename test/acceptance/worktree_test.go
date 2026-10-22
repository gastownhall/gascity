//go:build acceptance_a

// Worktree acceptance tests.
//
// Regression test for Bug 6 (2026-03-18): worktree branch collisions
// when multiple cities share the same underlying git repo.
package acceptance_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gascitypacks "github.com/gastownhall/gascity-packs"

	corepack "github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/builtinpacks"
)

// TestWorktreeBranchNamespacing verifies that worktree-setup.sh creates
// namespaced branches (gc-<agent>-<hash>) instead of bare gc-<agent>,
// preventing collisions when multiple cities share the same repo.
func TestWorktreeBranchNamespacing(t *testing.T) {
	// Create a git repo to serve as the "rig".
	repoDir := t.TempDir()
	git(t, repoDir, "init")
	git(t, repoDir, "commit", "--allow-empty", "-m", "initial")

	// Use the worktree-setup script from the embedded gastown pack.
	scriptSrc := worktreeSetupScript(t)

	// Create two different target paths (simulating two cities).
	city1WT := filepath.Join(t.TempDir(), "city1", "worktrees", "refinery")
	city2WT := filepath.Join(t.TempDir(), "city2", "worktrees", "refinery")

	// Run worktree-setup for "city 1".
	runScript(t, scriptSrc, repoDir, city1WT, "refinery")

	// Run worktree-setup for "city 2" — same repo, same agent name,
	// different target path. Must NOT collide.
	runScript(t, scriptSrc, repoDir, city2WT, "refinery")

	// Both worktrees must exist.
	if _, err := os.Stat(filepath.Join(city1WT, ".git")); err != nil {
		t.Fatal("city1 worktree not created")
	}
	if _, err := os.Stat(filepath.Join(city2WT, ".git")); err != nil {
		t.Fatal("city2 worktree not created")
	}

	// Branches must be different (namespaced by target path hash).
	branch1 := currentBranch(t, city1WT)
	branch2 := currentBranch(t, city2WT)
	if branch1 == branch2 {
		t.Fatalf("branch collision: both worktrees use %q — Bug 6 regression", branch1)
	}

	// Both must start with gc-refinery- (namespaced pattern).
	if !strings.HasPrefix(branch1, "gc-refinery-") {
		t.Fatalf("branch1 = %q, want gc-refinery-<hash> pattern", branch1)
	}
	if !strings.HasPrefix(branch2, "gc-refinery-") {
		t.Fatalf("branch2 = %q, want gc-refinery-<hash> pattern", branch2)
	}
}

// TestWorktreeIdempotent verifies that running worktree-setup.sh twice
// on the same target is a no-op (idempotent).
func TestWorktreeIdempotent(t *testing.T) {
	repoDir := t.TempDir()
	git(t, repoDir, "init")
	git(t, repoDir, "commit", "--allow-empty", "-m", "initial")

	scriptSrc := worktreeSetupScript(t)

	wt := filepath.Join(t.TempDir(), "worktree")

	// First run: creates worktree.
	runScript(t, scriptSrc, repoDir, wt, "witness")

	branch := currentBranch(t, wt)

	// Second run: should be a no-op.
	runScript(t, scriptSrc, repoDir, wt, "witness")

	// Branch should be unchanged.
	if got := currentBranch(t, wt); got != branch {
		t.Fatalf("branch changed on second run: %q -> %q", branch, got)
	}
}

// TestWorktreeBeadRedirect verifies that worktree-setup.sh creates
// a .beads/redirect file pointing to the rig's .beads directory.
func TestWorktreeBeadRedirect(t *testing.T) {
	repoDir := t.TempDir()
	git(t, repoDir, "init")
	git(t, repoDir, "commit", "--allow-empty", "-m", "initial")

	scriptSrc := worktreeSetupScript(t)

	wt := filepath.Join(t.TempDir(), "worktree")
	runScript(t, scriptSrc, repoDir, wt, "polecat")

	redirect := filepath.Join(wt, ".beads", "redirect")
	data, err := os.ReadFile(redirect)
	if err != nil {
		t.Fatalf(".beads/redirect not created: %v", err)
	}

	want := repoDir + "/.beads"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf(".beads/redirect = %q, want %q", got, want)
	}
}

// TestCoreWorktreeSetupCreatesDistinctSlotWorktrees runs the CORE pack's
// worktree-setup.sh — the script [workspace] implicit_agent_isolation =
// "worktree" injects as pre_start into rig-scoped implicit pool agents
// (#1181) — for two slot paths of one rig and proves the isolation
// contract end to end: distinct worktrees with per-slot namespaced
// branches, bead redirection to the rig, and a commit inside one slot
// leaving the rig root's HEAD, index and working tree untouched.
func TestCoreWorktreeSetupCreatesDistinctSlotWorktrees(t *testing.T) {
	repoDir := t.TempDir()
	git(t, repoDir, "init")
	git(t, repoDir, "commit", "--allow-empty", "-m", "initial")
	rigHead := git(t, repoDir, "rev-parse", "HEAD")

	scriptSrc := coreWorktreeSetupScript(t)

	// Two slot work_dirs of one rig-scoped implicit pool, shaped like the
	// injected template .gc/worktrees/{{.Rig}}/{{.AgentBase}}.
	slots := filepath.Join(t.TempDir(), ".gc", "worktrees", "demo")
	first := filepath.Join(slots, "claude-1")
	second := filepath.Join(slots, "claude-2")

	runScript(t, scriptSrc, repoDir, first, "claude-1")
	runScript(t, scriptSrc, repoDir, second, "claude-2")

	for _, slot := range []struct{ dir, name string }{
		{first, "claude-1"},
		{second, "claude-2"},
	} {
		if _, err := os.Stat(filepath.Join(slot.dir, ".git")); err != nil {
			t.Fatalf("slot %s: worktree not created: %v", slot.name, err)
		}
		if b := currentBranch(t, slot.dir); !strings.HasPrefix(b, "gc-"+slot.name+"-") {
			t.Fatalf("slot %s: branch = %q, want gc-%s-<hash> pattern", slot.name, b, slot.name)
		}
		redirect, err := os.ReadFile(filepath.Join(slot.dir, ".beads", "redirect"))
		if err != nil {
			t.Fatalf("slot %s: .beads/redirect not created: %v", slot.name, err)
		}
		if got := strings.TrimSpace(string(redirect)); got != filepath.Join(repoDir, ".beads") {
			t.Fatalf("slot %s: .beads/redirect = %q, want %q", slot.name, got, filepath.Join(repoDir, ".beads"))
		}
	}
	if currentBranch(t, first) == currentBranch(t, second) {
		t.Fatalf("parallel slots share branch %q — the #1181 clobber is back", currentBranch(t, first))
	}

	// A commit inside one slot stays inside that slot: the rig root keeps
	// its HEAD, a clean status, and no leaked file; the other slot is
	// untouched too.
	if err := os.WriteFile(filepath.Join(first, "slot-work.txt"), []byte("slot work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, first, "add", "slot-work.txt")
	git(t, first, "commit", "-m", "slot work")

	if got := git(t, repoDir, "rev-parse", "HEAD"); got != rigHead {
		t.Fatalf("rig HEAD moved %q -> %q after a slot commit", rigHead, got)
	}
	if out := git(t, repoDir, "status", "--porcelain"); out != "" {
		t.Fatalf("rig working tree dirty after a slot commit:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "slot-work.txt")); !os.IsNotExist(err) {
		t.Fatal("slot commit leaked into the rig root")
	}
	if _, err := os.Stat(filepath.Join(second, "slot-work.txt")); !os.IsNotExist(err) {
		t.Fatal("slot commit leaked into the other slot")
	}
}

// --- helpers ---

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func runScript(t *testing.T, script, repoDir, wt, agent string) {
	t.Helper()
	out, err := runScriptCommand(script, repoDir, wt, agent)
	if err != nil {
		t.Fatalf("worktree-setup.sh failed: %v\n%s", err, out)
	}
}

// runScriptCommand runs a worktree-setup.sh script and returns its
// combined output without asserting on the result. Shared by runScript
// (strict) and runLifecycleScript in worktree_lifecycle_test.go
// (tolerant of a known non-zero exit), so the package has a single
// os/exec call site for this pattern instead of one per caller.
func runScriptCommand(script, repoDir, wt, agent string) ([]byte, error) {
	cmd := exec.Command("sh", script, repoDir, wt, agent, "--sync")
	cmd.Env = os.Environ()
	return cmd.CombinedOutput()
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// coreWorktreeSetupScript materializes worktree-setup.sh from the core
// pack embedded in the gc binary — the copy that worktree isolation mode
// injects into rig-scoped implicit pool agents (#1181), which the composed
// config loader requires for implicit_agent_isolation = "worktree".
func coreWorktreeSetupScript(t *testing.T) string {
	t.Helper()
	const rel = "assets/scripts/worktree-setup.sh"
	data, err := fs.ReadFile(corepack.PackFS, rel)
	if err != nil {
		t.Fatalf("reading embedded core %s: %v", rel, err)
	}
	dst := filepath.Join(t.TempDir(), "core-worktree-setup.sh")
	if err := os.WriteFile(dst, data, builtinpacks.MaterializedFileMode(rel)); err != nil {
		t.Fatalf("materializing %s: %v", rel, err)
	}
	return dst
}

// worktreeSetupScript materializes worktree-setup.sh from the gastown pack
// embedded in the gc binary (via the gascity-packs module) into a temp dir
// and returns its path. The checked-in example no longer carries a pack
// copy, so the embedded bytes are the source of truth.
func worktreeSetupScript(t *testing.T) string {
	t.Helper()
	const rel = "assets/scripts/worktree-setup.sh"
	data, err := fs.ReadFile(gascitypacks.Gastown(), rel)
	if err != nil {
		t.Fatalf("reading embedded gastown %s: %v", rel, err)
	}
	dst := filepath.Join(t.TempDir(), "worktree-setup.sh")
	if err := os.WriteFile(dst, data, builtinpacks.MaterializedFileMode(rel)); err != nil {
		t.Fatalf("materializing %s: %v", rel, err)
	}
	return dst
}
