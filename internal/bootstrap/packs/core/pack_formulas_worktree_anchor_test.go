package core

import (
	"strings"
	"testing"
)

// TestWorkspaceSetupAnchorsWorktreeToAgentHome pins issue #6014: the
// workspace-setup step must place the per-bead worktree under the agent home
// ($GC_DIR), not under whatever directory the session last cd'd into. An
// earlier step may leave the session standing in the tracked rig checkout,
// and $(pwd)/worktrees then plants an untracked directory inside it.
func TestWorkspaceSetupAnchorsWorktreeToAgentHome(t *testing.T) {
	for _, file := range []string{"mol-polecat-commit.toml", "mol-scoped-work.toml"} {
		t.Run(file, func(t *testing.T) {
			step := formulaStep(t, readFormula(t, file), "workspace-setup")
			if strings.Contains(step, "$(pwd)/worktrees") {
				t.Errorf("%s workspace-setup derives the worktree path from $(pwd)/worktrees; anchor it to the agent home ($GC_DIR) instead", file)
			}
			if !strings.Contains(step, "GC_DIR") {
				t.Errorf("%s workspace-setup does not anchor the worktree to $GC_DIR", file)
			}
		})
	}
}
