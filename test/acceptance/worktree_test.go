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
	cmd := exec.Command("sh", script, repoDir, wt, agent, "--sync")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worktree-setup.sh failed: %v\n%s", err, out)
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
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
