package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestAgentMayHaveSessionSpecificMCPTargets pins the exact discriminating
// shapes for resolveDeterministicAgentMCPProjection's alt-identity probe
// gate. Before ga-f6vcnj this gate was agent.SupportsMultipleSessions(),
// which is true for ANY agent with an unset max_active_sessions — the
// ordinary shape of a minimally-configured, dir-less agent (e.g. the
// shipping examples/t3bridge-gastown/packs/t3demo/agents/mayor/agent.toml:
// scope = "city", no dir, no work_dir, no max_active_sessions). That made
// the alt-identity probe run for every such agent, and
// internal/workdir.ResolveWorkDirPathStrict's dir-less-agent fallback
// (added by ga-61igzb) makes the probe's synthetic "<agent>-alt" identity
// resolve to a different path than the real identity for exactly that
// shape — spuriously tripping "has session-specific MCP targets; use
// --session" for an agent that never actually has session-varying MCP.
//
// agentMayHaveSessionSpecificMCPTargets must be narrow enough to say false
// for that shape while still saying true for agents that carry an explicit
// pool signal (namepool, min_active_sessions/scale_check, or an explicit
// max_active_sessions), matching
// internal/workdir.RequiresPoolWorkDirIsolationCheck exactly.
func TestAgentMayHaveSessionSpecificMCPTargets(t *testing.T) {
	cases := []struct {
		name  string
		agent config.Agent
		want  bool
	}{
		{
			name:  "shipping t3bridge-gastown mayor shape: dir-less, unset max_active_sessions",
			agent: config.Agent{Name: "mayor"},
			want:  false,
		},
		{
			name:  "explicit singleton (max_active_sessions=1) collapses to canonical identity",
			agent: config.Agent{Name: "mayor", Dir: "demo", MaxActiveSessions: intPtr(1)},
			want:  false,
		},
		{
			name:  "explicit multi-session pool",
			agent: config.Agent{Name: "builder", Dir: "demo", MaxActiveSessions: intPtr(2)},
			want:  true,
		},
		{
			name:  "namepool agent",
			agent: config.Agent{Name: "ant", Dir: "demo", Namepool: "names.txt"},
			want:  true,
		},
		{
			name:  "explicit min_active_sessions pool marker with unset max",
			agent: config.Agent{Name: "drifter", Dir: "demo", MinActiveSessions: intPtr(0)},
			want:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentMayHaveSessionSpecificMCPTargets(tc.agent); got != tc.want {
				t.Fatalf("agentMayHaveSessionSpecificMCPTargets(%+v) = %v, want %v", tc.agent, got, tc.want)
			}
		})
	}
}
