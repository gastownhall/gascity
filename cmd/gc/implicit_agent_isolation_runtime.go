package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
)

// implicitAgentWorktreeRuntimes are the session providers that run an
// agent's pre_start on the host before the session starts. "" is the
// default provider, which the runtime registry constructs as tmux.
var implicitAgentWorktreeRuntimes = map[string]bool{"": true, "tmux": true, "herdr": true}

// validateImplicitAgentIsolationRuntime rejects
// workspace.implicit_agent_isolation = "worktree" when a rig-scoped implicit
// agent's effective session provider does not run pre_start on the host.
// Worktree mode creates each instance's git worktree from pre_start before
// the session starts in it; a runtime that skips or relocates pre_start
// would start the session in a directory that does not exist.
func validateImplicitAgentIsolationRuntime(cfg *config.City) error {
	if cfg == nil || !cfg.Workspace.ImplicitAgentWorktreeIsolation() {
		return nil
	}
	cityProvider := effectiveProviderName(cfg.Session.Provider)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if !a.Implicit || a.Dir == "" {
			continue
		}
		provider := effectiveSessionProvider(a.Session, cityProvider)
		if implicitAgentWorktreeRuntimes[provider] {
			continue
		}
		return fmt.Errorf("workspace.implicit_agent_isolation = %q requires a runtime that runs pre_start on the host (tmux, herdr); agent %q uses %q",
			config.ImplicitAgentIsolationWorktree, a.QualifiedName(), provider)
	}
	return nil
}
