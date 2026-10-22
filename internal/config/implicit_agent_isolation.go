package config

import (
	"fmt"
	"os"
)

// validateImplicitAgentIsolation rejects an out-of-enum
// workspace.implicit_agent_isolation value. Parse runs it on its single layer
// and LoadWithIncludesOptions runs it again on the composed root, because a
// fragment may supply the field. The empty string (unset) means "shared".
func validateImplicitAgentIsolation(raw string) error {
	switch raw {
	case "", ImplicitAgentIsolationShared, ImplicitAgentIsolationWorktree:
		return nil
	}
	return fmt.Errorf("workspace.implicit_agent_isolation: invalid value %q: want one of %s, %s",
		raw, ImplicitAgentIsolationShared, ImplicitAgentIsolationWorktree)
}

// validateImplicitAgentWorktreeSetup rejects worktree isolation on a composed
// city whose core pack, and so the worktree-setup.sh pre_start that
// InjectImplicitAgents wires in, is unavailable. Without it the implicit
// agents would start in an empty, non-repository work_dir.
func validateImplicitAgentWorktreeSetup(cfg *City) error {
	if !cfg.Workspace.ImplicitAgentWorktreeIsolation() {
		return nil
	}
	coreDir := cfg.PackDirByName("core")
	if coreDir == "" {
		return fmt.Errorf("workspace.implicit_agent_isolation = %q requires the core pack, which provides assets/scripts/worktree-setup.sh, but core is not composed",
			cfg.Workspace.ImplicitAgentIsolation)
	}
	script := ImplicitAgentWorktreeSetupScript(coreDir)
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("workspace.implicit_agent_isolation = %q requires %s: %w",
			cfg.Workspace.ImplicitAgentIsolation, script, err)
	}
	return nil
}
