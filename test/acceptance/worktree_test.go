//go:build acceptance_a

// Worktree acceptance tests.
//
// Regression test for Bug 6 (2026-03-18): worktree branch collisions
// when multiple cities share the same underlying git repo.
package acceptance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorktreeBranchNamespacing verifies that worktree-setup.sh creates
// namespaced branches (gc-<agent>-<hash>) instead of bare gc-<agent>,
// preventing collisions when multiple cities share the same repo.
func TestWorktreeBranchNamespacing(t *testing.T) {
	// Create a git repo to serve as the "rig".
	repoDir := t.TempDir()
	git(t, repoDir, "init")
	git(t, repoDir, "commit", "--allow-empty", "-m", "initial")

	// Find the worktree-setup script from examples.
	scriptSrc := filepath.Join(findModuleRoot(t), "examples", "gastown",
		"packs", "gastown", "assets", "scripts", "worktree-setup.sh")
	if _, err := os.Stat(scriptSrc); err != nil {
		t.Skipf("worktree-setup.sh not found: %v", err)
	}

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

	scriptSrc := filepath.Join(findModuleRoot(t), "examples", "gastown",
		"packs", "gastown", "assets", "scripts", "worktree-setup.sh")
	if _, err := os.Stat(scriptSrc); err != nil {
		t.Skipf("worktree-setup.sh not found: %v", err)
	}

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

	scriptSrc := filepath.Join(findModuleRoot(t), "examples", "gastown",
		"packs", "gastown", "assets", "scripts", "worktree-setup.sh")
	if _, err := os.Stat(scriptSrc); err != nil {
		t.Skipf("worktree-setup.sh not found: %v", err)
	}

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

// TestWorktreeSetupRefusesNonGitRigRoot verifies that worktree-setup.sh fails
// fast with an actionable error when the rig root has no .git, rather than
// silently producing a worktree dir that lacks .git. Regression test for
// hq:gc-aeh / gco-1ly.
func TestWorktreeSetupRefusesNonGitRigRoot(t *testing.T) {
	scriptSrc := filepath.Join(findModuleRoot(t), "examples", "gastown",
		"packs", "gastown", "assets", "scripts", "worktree-setup.sh")
	if _, err := os.Stat(scriptSrc); err != nil {
		t.Skipf("worktree-setup.sh not found: %v", err)
	}

	repoDir := t.TempDir() // no `git init`
	wt := filepath.Join(t.TempDir(), "worktree")

	cmd := exec.Command("sh", scriptSrc, repoDir, wt, "polecat", "--sync")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("worktree-setup.sh should fail when rig root has no .git, exited 0; output:\n%s", out)
	}
	if !strings.Contains(string(out), "no .git") {
		t.Fatalf("expected error message to mention 'no .git', got:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(wt, ".git")); statErr == nil {
		t.Fatalf("worktree dir should not have .git after a failed setup; output:\n%s", out)
	}
}

// TestWorktreeSetupSelfHealsBoilerplateOnly verifies that when a previous
// failed spawn left the worktree dir populated with agent boilerplate but no
// .git, a subsequent invocation (after the rig root has been git-init'd)
// rebuilds the worktree and preserves the boilerplate. This is the
// acceptance-1 scenario from gco-1ly: add rig before git init, then init,
// then respawn — worktree must end up with a correct .git.
func TestWorktreeSetupSelfHealsBoilerplateOnly(t *testing.T) {
	scriptSrc := filepath.Join(findModuleRoot(t), "examples", "gastown",
		"packs", "gastown", "assets", "scripts", "worktree-setup.sh")
	if _, err := os.Stat(scriptSrc); err != nil {
		t.Skipf("worktree-setup.sh not found: %v", err)
	}

	// Simulate the first spawn aftermath: the rig path is not yet a git repo
	// AND the agent provider has already laid boilerplate down in the
	// worktree directory.
	repoDir := t.TempDir()
	wt := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".claude", "marker"), []byte("provider boilerplate"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Now the operator git-inits the rig path and respawns.
	git(t, repoDir, "init")
	git(t, repoDir, "config", "user.email", "test@test")
	git(t, repoDir, "config", "user.name", "test")
	git(t, repoDir, "commit", "--allow-empty", "-m", "initial")

	runScript(t, scriptSrc, repoDir, wt, "polecat")

	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("worktree should have .git after self-heal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude", "marker")); err != nil {
		t.Fatalf("self-heal should preserve agent boilerplate: %v", err)
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

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
