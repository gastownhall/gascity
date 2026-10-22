package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// validateImplicitAgentIsolationRuntime gates worktree-mode implicit pools
// at the pre-expansion points (gc start, controller config reload, supervisor
// prepare). Worktree mode's injected pre_start provisions the per-slot
// worktree on the host before the session starts, so a provider that runs
// pre_start elsewhere — or not at all — would leave the session cwd pointing
// at a directory nobody created. The allowed set is the providers that
// execute pre_start on the host: unset (the default), tmux and herdr.
// Shared mode returns nil: nothing is injected, so nothing is gated.
func validateImplicitAgentIsolationRuntime(cfg *config.City) error {
	if cfg == nil || strings.TrimSpace(cfg.Workspace.ImplicitAgentIsolation) != "worktree" {
		return nil
	}
	// Same city provider rule as the runtime paths: GC_SESSION, when set,
	// overrides [session] provider.
	cityProvider := strings.TrimSpace(os.Getenv("GC_SESSION"))
	if cityProvider == "" {
		cityProvider = strings.TrimSpace(cfg.Session.Provider)
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		// Only rig-scoped implicit agents carry the injected work_dir and
		// pre_start; explicit agents and city-scoped implicit agents are
		// their authors' call.
		if !a.Implicit || !agentDirNamesARig(a.Dir, cfg.Rigs) {
			continue
		}
		switch provider := effectiveSessionProvider(a.Session, cityProvider); provider {
		case "", "tmux", "herdr":
			continue
		default:
			return fmt.Errorf(`workspace.implicit_agent_isolation = "worktree" requires a runtime that runs pre_start on the host (tmux, herdr); agent %q uses %q`, a.QualifiedName(), provider)
		}
	}
	return nil
}

// agentDirNamesARig reports whether the agent's Dir is a configured rig name
// (rig-scoped) rather than empty (city-scoped) or something else.
func agentDirNamesARig(dir string, rigs []config.Rig) bool {
	if dir == "" {
		return false
	}
	for _, r := range rigs {
		if r.Name == dir {
			return true
		}
	}
	return false
}
