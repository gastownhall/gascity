package configedit

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestRemoveAgentPatch_RigKeyedIdentity verifies removeAgentPatch — the
// DeleteAgent cleanup path — resolves rig= and rig="*" patches by their
// canonical qualified identity, not by Dir alone. Before the identity fix this
// path matched on Dir, so a patch authored with the new rig key was
// unreachable and would dangle after its target agent was deleted.
func TestRemoveAgentPatch_RigKeyedIdentity(t *testing.T) {
	cfg := &config.City{
		Patches: config.Patches{
			Agents: []config.AgentPatch{
				{Rig: "rigA", Name: "worker"},
				{Rig: "*", Name: "worker"},
			},
		},
	}
	if !removeAgentPatch(cfg, "rigA/worker") {
		t.Fatal("removeAgentPatch(rigA/worker) should report a change")
	}
	if len(cfg.Patches.Agents) != 1 || cfg.Patches.Agents[0].Rig != "*" {
		t.Fatalf("after removing rigA/worker, want only the wildcard patch, got %#v", cfg.Patches.Agents)
	}
	if !removeAgentPatch(cfg, "*/worker") {
		t.Fatal("removeAgentPatch(*/worker) should report a change")
	}
	if len(cfg.Patches.Agents) != 0 {
		t.Fatalf("after removing */worker, want none, got %#v", cfg.Patches.Agents)
	}
	// A non-matching identity is a no-op.
	if removeAgentPatch(cfg, "rigA/worker") {
		t.Fatal("removeAgentPatch on empty patch set should be a no-op")
	}
}

// TestStripAgentPatchUpdate_RigKeyedIdentity verifies stripAgentPatchUpdate —
// the UpdateAgent cleanup path — resolves a rig= patch by its qualified
// identity when clearing an override the durable write now owns. The patch here
// carries only Provider, so once that override is stripped the whole
// identity-only entry is dropped rather than left behind in city.toml.
func TestStripAgentPatchUpdate_RigKeyedIdentity(t *testing.T) {
	provider := "claude"
	cfg := &config.City{
		Patches: config.Patches{
			Agents: []config.AgentPatch{
				{Rig: "rigA", Name: "worker", Provider: &provider},
			},
		},
	}
	if !stripAgentPatchUpdate(cfg, "rigA/worker", AgentUpdate{Provider: "gemini"}) {
		t.Fatal("stripAgentPatchUpdate(rigA/worker) should report a change")
	}
	if len(cfg.Patches.Agents) != 0 {
		t.Fatalf("after stripping rigA/worker provider override, want none, got %#v", cfg.Patches.Agents)
	}
}
