package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestResolvedSessionConfigForProviderBuildsNormalizedConfig(t *testing.T) {
	metadata := map[string]string{
		"session_origin": "named",
		"agent_name":     "myrig/worker-adhoc-123",
	}
	env := map[string]string{"API_TOKEN": "present"}
	mcpServers := []runtime.MCPServerConfig{{
		Name:    "filesystem",
		Command: "/bin/mcp",
		Args:    []string{"--stdio"},
	}}
	resolved := &config.ResolvedProvider{
		Name:                   "stub",
		Command:                "/bin/echo",
		ReadyPromptPrefix:      "stub-ready>",
		ReadyDelayMs:           250,
		ProcessNames:           []string{"echo"},
		EmitsPermissionWarning: true,
		Env:                    env,
		ResumeFlag:             "--resume",
		ResumeStyle:            "flag",
		ResumeCommand:          "resume-cmd",
		SessionIDFlag:          "--session-id",
	}

	cfg, err := resolvedSessionConfigForProvider(
		"worker",
		"worker-named",
		"myrig/worker",
		"Worker Named",
		"acp",
		metadata,
		resolved,
		"",
		"/tmp/workdir",
		mcpServers,
	)
	if err != nil {
		t.Fatalf("resolvedSessionConfigForProvider: %v", err)
	}

	if got, want := cfg.Runtime.Command, "/bin/echo"; got != want {
		t.Fatalf("Runtime.Command = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Provider, "stub"; got != want {
		t.Fatalf("Runtime.Provider = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.WorkDir, "/tmp/workdir"; got != want {
		t.Fatalf("Runtime.WorkDir = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Hints.WorkDir, "/tmp/workdir"; got != want {
		t.Fatalf("Runtime.Hints.WorkDir = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Hints.ReadyPromptPrefix, "stub-ready>"; got != want {
		t.Fatalf("Runtime.Hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if len(cfg.Runtime.Hints.MCPServers) != 1 {
		t.Fatalf("Runtime.Hints.MCPServers len = %d, want 1", len(cfg.Runtime.Hints.MCPServers))
	}
	if got, want := cfg.Runtime.Hints.MCPServers[0].Name, "filesystem"; got != want {
		t.Fatalf("Runtime.Hints.MCPServers[0].Name = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Resume.SessionIDFlag, "--session-id"; got != want {
		t.Fatalf("Runtime.Resume.SessionIDFlag = %q, want %q", got, want)
	}
	if got, want := cfg.Metadata[session.MCPIdentityMetadataKey], "myrig/worker-adhoc-123"; got != want {
		t.Fatalf("Metadata[mcp_identity] = %q, want %q", got, want)
	}
	if got := cfg.Metadata[session.MCPServersSnapshotMetadataKey]; got == "" {
		t.Fatal("Metadata[mcp_servers_snapshot] = empty, want persisted snapshot")
	}

	metadata["session_origin"] = "mutated"
	env["API_TOKEN"] = "mutated"
	if got, want := cfg.Metadata["session_origin"], "named"; got != want {
		t.Fatalf("Metadata[session_origin] = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.SessionEnv["API_TOKEN"], "present"; got != want {
		t.Fatalf("Runtime.SessionEnv[API_TOKEN] = %q, want %q", got, want)
	}
}

func TestResolvedSessionConfigForProviderRejectsNilProvider(t *testing.T) {
	if _, err := resolvedSessionConfigForProvider(
		"worker",
		"",
		"myrig/worker",
		"Worker",
		"",
		nil,
		nil,
		"",
		"/tmp/workdir",
		nil,
	); err == nil {
		t.Fatal("resolvedSessionConfigForProvider() error = nil, want error")
	}
}

func TestResolvedSessionConfigForProviderSkipsStoredMCPMetadataForTmuxTransport(t *testing.T) {
	cfg, err := resolvedSessionConfigForProvider(
		"worker",
		"",
		"myrig/worker",
		"Worker",
		"",
		map[string]string{
			"session_origin": "manual",
			"agent_name":     "myrig/worker-adhoc-123",
		},
		&config.ResolvedProvider{
			Name:    "stub",
			Command: "/bin/echo",
		},
		"",
		"/tmp/workdir",
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedSessionConfigForProvider: %v", err)
	}
	if got := cfg.Metadata[session.MCPIdentityMetadataKey]; got != "" {
		t.Fatalf("Metadata[mcp_identity] = %q, want empty for tmux transport", got)
	}
	if got := cfg.Metadata[session.MCPServersSnapshotMetadataKey]; got != "" {
		t.Fatalf("Metadata[mcp_servers_snapshot] = %q, want empty for tmux transport", got)
	}
}
