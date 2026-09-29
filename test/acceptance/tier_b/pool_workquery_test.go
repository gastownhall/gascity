//go:build acceptance_b

// Pool work_query acceptance test (Tier B — requires bd, ~50s).
//
// Regression test for Bug 3 (2026-03-18): BEADS_DIR not set for
// rig-scoped agents running in worktrees. When bd walks up from a
// worktree cwd, it finds the city root's .beads instead of the rig's.
// Label queries aren't federated, so pool work_query returns empty.
//
// This test creates a bead in a rig's store and verifies that bd can
// find it from a worktree directory when BEADS_DIR is set correctly.
package tierb_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// TestPoolWorkQueryFromWorktree verifies that bd ready --label=pool:X
// finds work from a worktree directory when BEADS_DIR is set to the
// rig's .beads directory. Without BEADS_DIR, bd walks up from cwd and
// finds the city root's .beads, which doesn't have the pool work.
func TestPoolWorkQueryFromWorktree(t *testing.T) {
	bdPath := helpers.RequireBD(t)

	// Set up a city with a rig.
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig")
	worktreeDir := filepath.Join(cityDir, ".gc", "worktrees", "myrig", "polecats", "polecat-1")

	for _, d := range []string{
		rigDir,
		worktreeDir,
		filepath.Join(cityDir, ".beads"),
		filepath.Join(rigDir, ".beads"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Initialize beads in the rig's .beads directory.
	bdRun(t, bdPath, rigDir, "init")

	// Create a bead with a pool label in the rig's store.
	bdRun(t, bdPath, rigDir, "create", "--title", "test work",
		"--label=pool:myrig/polecat", "--priority=P2")

	// Verify: from the rig dir, bd finds the work.
	out := bdRun(t, bdPath, rigDir, "ready", "--label=pool:myrig/polecat", "--limit=1")
	if strings.Contains(out, "No ready work found") || !strings.Contains(out, "test work") {
		t.Fatalf("bd ready from rig dir should find work, got:\n%s", out)
	}

	// Without BEADS_DIR: from worktree, bd should NOT find it (walks up to city .beads).
	out = bdRunWithEnv(t, bdPath, worktreeDir, nil, "ready", "--label=pool:myrig/polecat", "--limit=1")
	if strings.Contains(out, "test work") {
		t.Skip("bd found work without BEADS_DIR — federation may be active, test not applicable")
	}

	// With BEADS_DIR: from worktree, bd SHOULD find it.
	env := map[string]string{"BEADS_DIR": filepath.Join(rigDir, ".beads")}
	out = bdRunWithEnv(t, bdPath, worktreeDir, env, "ready", "--label=pool:myrig/polecat", "--limit=1")
	if strings.Contains(out, "No ready work found") || !strings.Contains(out, "test work") {
		t.Fatalf("bd ready from worktree with BEADS_DIR should find work, got:\n%s", out)
	}
}

func bdRun(t *testing.T, bdPath, dir string, args ...string) string {
	t.Helper()
	return bdRunWithEnv(t, bdPath, dir, nil, args...)
}

func bdRunWithEnv(t *testing.T, bdPath, dir string, extraEnv map[string]string, args ...string) string {
	t.Helper()
	cmd := helpers.ToolCommand(t, bdPath, args...)
	cmd.Dir = dir
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// bd returns non-zero for "no results" which is expected in some cases.
		// Only fail if it's not an exit-code-1 (no results) situation.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return string(out)
		}
		t.Fatalf("bd %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
