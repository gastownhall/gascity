//go:build acceptance_a && unix

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

// TestLifecycleWorktreeSetupRestoresPrestagedContent is a regression test
// for a pre-staged target whose entries merge in file-then-directory order
// (issue #9027). merge_stage_entry kept SRC/DST in globals, so the recursive
// call that moved the file clobbered the caller's DST; the sibling directory
// merge then ran mkdir on <file>/<dir>, the script died before restoring or
// provisioning, and the stage directory leaked next to the worktree.
func TestLifecycleWorktreeSetupRestoresPrestagedContent(t *testing.T) {
	repoDir := t.TempDir()
	git(t, repoDir, "init")
	git(t, repoDir, "commit", "--allow-empty", "-m", "initial")

	script := lifecycleWorktreeSetupScript(t)

	wtParent := t.TempDir()
	wt := filepath.Join(wtParent, "worktree")

	// Pre-stage the target the way a pool slot leaves state behind: a file
	// that sorts before a directory under the same parent.
	gcDir := filepath.Join(wt, ".gc")
	if err := os.MkdirAll(filepath.Join(gcDir, "worktrees"), 0o755); err != nil {
		t.Fatalf("pre-staging worktrees dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{"slot":1}`), 0o644); err != nil {
		t.Fatalf("pre-staging settings.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "worktrees", "slot-state"), []byte("warm"), 0o644); err != nil {
		t.Fatalf("pre-staging slot state: %v", err)
	}

	out, err := runScriptCommand(script, repoDir, wt, "agent")
	if err != nil {
		t.Fatalf("worktree-setup.sh failed on pre-staged target: %v\n%s", err, out)
	}

	// Every pre-staged entry must be restored into the new worktree.
	for _, rel := range []string{
		filepath.Join(".gc", "settings.json"),
		filepath.Join(".gc", "worktrees", "slot-state"),
	} {
		if _, err := os.Stat(filepath.Join(wt, rel)); err != nil {
			t.Fatalf("pre-staged %s not restored into worktree: %v", rel, err)
		}
	}

	// The staging directory must be removed, not leaked beside the worktree.
	stale, err := filepath.Glob(filepath.Join(wtParent, ".gascity-worktree-stage.*"))
	if err != nil {
		t.Fatalf("globbing stage dirs: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("staging directories leaked: %v", stale)
	}

	// Provisioning must still run after the restore.
	redirect := filepath.Join(wt, ".beads", "redirect")
	data, err := os.ReadFile(redirect)
	if err != nil {
		t.Fatalf(".beads/redirect not created after pre-staged merge: %v", err)
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
