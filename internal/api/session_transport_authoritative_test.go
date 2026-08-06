package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// TestResolvedSessionTransportHonorsPersistedTmuxTransport is the API-side copy
// of the reproduction. storedSessionProvesACPTransport treats a non-empty
// mcp_identity as proof of ACP, and nothing ever cleared that key when a
// session moved to a tmux-only provider — so the resume path kept resolving the
// ACP runtime for a binary that has no ACP mode. The persisted transport is
// read first and must win; a legacy bead with no transport key must still fall
// back to the artifact and resolve ACP.
func TestResolvedSessionTransportHonorsPersistedTmuxTransport(t *testing.T) {
	tmuxOnly := &config.ResolvedProvider{
		Name:    "cli-provider",
		Command: "agent-cli",
	}
	metadata := map[string]string{
		session.MCPIdentityMetadataKey:        "example/agents.worker",
		session.MCPServersSnapshotMetadataKey: `[{"name":"fixture","transport":"stdio","command":"/bin/fixture-mcp"}]`,
	}

	stale := session.Info{
		Transport: config.SessionTransportTmux,
		Provider:  "cli-provider",
		Command:   "agent-cli --profile test",
	}
	for _, allowFallback := range []bool{false, true} {
		if got := resolvedSessionTransport(stale, tmuxOnly, config.SessionTransportACP, metadata, allowFallback); got != config.SessionTransportTmux {
			t.Errorf("resolvedSessionTransport(allowFallback=%v) = %q, want tmux; a stale mcp_identity must not beat the persisted transport", allowFallback, got)
		}
	}

	legacy := stale
	legacy.Transport = ""
	if got := resolvedSessionTransport(legacy, tmuxOnly, config.SessionTransportACP, metadata, false); got != "acp" {
		t.Errorf("resolvedSessionTransport(legacy bead) = %q, want acp; the legacy mcp_identity fallback must keep working", got)
	}
}
