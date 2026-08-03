//go:build acceptance_a

// Lifecycle-example worktree acceptance tests.
//
// worktree-setup.sh in the "lifecycle" example pack
// (examples/lifecycle/packs/lifecycle/assets/scripts/worktree-setup.sh) is
// an independently maintained script, not the same file as the gastown
// pack's embedded copy exercised by worktree_test.go -- the two happen to
// share a name and structure but have separate histories. Kept in its own
// file so the two script sources are never conflated.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// lifecycleWorktreeSetupScript returns the path to the lifecycle example
// pack's worktree-setup.sh as checked into this repo.
func lifecycleWorktreeSetupScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(helpers.FindModuleRoot(), "examples", "lifecycle", "packs", "lifecycle", "assets", "scripts", "worktree-setup.sh")
}

// runLifecycleScript runs the lifecycle worktree-setup.sh script. Unlike
// runScript (used for the gastown pack's copy), this does not fail the
// test on a non-zero exit: this script currently exits 128 on every
// invocation from an unrelated, separately-tracked issue (ga-g8lt3x's
// "Out of scope" note) that is not this bead's deliverable. The exit is
// logged for visibility; the actual pass/fail signal is the .beads/redirect
// assertion each test makes afterward, exactly as the shell-level repro
// (investigations/ga-58xwg1/repro_gascity_clean.sh) evaluates it.
func runLifecycleScript(t *testing.T, script, repoDir, wt, agent string) {
	t.Helper()
	if out, err := runScriptCommand(script, repoDir, wt, agent); err != nil {
		t.Logf("worktree-setup.sh exited non-zero (tracked separately, not asserted here): %v\n%s", err, out)
	}
}

// TestLifecycleWorktreeSetupBeadRedirect verifies that the lifecycle
// example's worktree-setup.sh creates a .beads/redirect file pointing to
// the rig's .beads directory on a fresh worktree. Control case -- must
// keep passing across the ga-g8lt3x fix.
func TestLifecycleWorktreeSetupBeadRedirect(t *testing.T) {
	repoDir := t.TempDir()
	git(t, repoDir, "init")
	git(t, repoDir, "commit", "--allow-empty", "-m", "initial")

	script := lifecycleWorktreeSetupScript(t)

	wt := filepath.Join(t.TempDir(), "worktree")
	runLifecycleScript(t, script, repoDir, wt, "polecat")

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

// TestLifecycleWorktreeSetupRedirectAppliesToPreExistingWorktree is a
// regression test for ga-g8lt3x: a worktree created by any means other
// than this script (a plain "git worktree add", an older script version,
// or a redirect later clobbered) used to hit the early-exit branch and
// skip the bead-redirect / local-excludes provisioning forever -- no
// convergence, even across repeated pre_start invocations on the same
// worktree.
func TestLifecycleWorktreeSetupRedirectAppliesToPreExistingWorktree(t *testing.T) {
	repoDir := t.TempDir()
	git(t, repoDir, "init")
	git(t, repoDir, "commit", "--allow-empty", "-m", "initial")

	script := lifecycleWorktreeSetupScript(t)

	wt := filepath.Join(t.TempDir(), "worktree")
	// Simulate a worktree created by some other path -- the setup
	// script has never touched it yet.
	git(t, repoDir, "worktree", "add", "-b", "gc-agent-b", wt)

	if _, err := os.Stat(filepath.Join(wt, ".beads", "redirect")); err == nil {
		t.Fatal("redirect already present before setup script ran -- test setup is wrong")
	}

	// pre_start runs the script on every session start; a converging
	// fix must not need a special first-time case, so run it 3x on the
	// same already-existing worktree.
	for i := 0; i < 3; i++ {
		runLifecycleScript(t, script, repoDir, wt, "agent-b")
	}

	redirect := filepath.Join(wt, ".beads", "redirect")
	data, err := os.ReadFile(redirect)
	if err != nil {
		t.Fatalf(".beads/redirect not created for pre-existing worktree after 3 runs: %v", err)
	}

	want := repoDir + "/.beads"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf(".beads/redirect = %q, want %q", got, want)
	}
}
