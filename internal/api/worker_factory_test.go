package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestResolveWorkerSessionRuntimePreservesStoredResolvedCommandAndBackfillsCurrentResumeSettings(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "api-resume-anthropic-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://process.example.test")
	t.Setenv("OLLAMA_API_KEY", "api-resume-ollama-token")
	t.Setenv("GC_RIG", "caller-rig")
	t.Setenv("GC_SESSION_NAME", "caller-session")

	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:       "Resolved Worker",
		Command:           "/bin/echo",
		ReadyPromptPrefix: "resolved-ready>",
		ReadyDelayMs:      321,
		ResumeFlag:        "--resume-resolved",
		ResumeStyle:       "flag",
		ResumeCommand:     "resolved resume {{.SessionKey}}",
		SessionIDFlag:     "--session-id-resolved",
		Env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://resolved.example.test",
		},
	}

	srv := New(fs)
	info := session.Info{
		ID:            "sess-1",
		Template:      "myrig/worker",
		Command:       "/bin/echo --composed",
		Provider:      "persisted-provider",
		WorkDir:       t.TempDir(),
		ResumeFlag:    "--resume-persisted",
		ResumeStyle:   "subcommand",
		ResumeCommand: "persisted resume {{.SessionKey}}",
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntime(info)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntime: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntime() = nil")
	}
	if got, want := runtimeCfg.Command, info.Command; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, info.Provider; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.WorkDir, info.WorkDir; got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeFlag, "--resume-resolved"; got != want {
		t.Fatalf("Resume.ResumeFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeStyle, "flag"; got != want {
		t.Fatalf("Resume.ResumeStyle = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeCommand, "resolved resume {{.SessionKey}}"; got != want {
		t.Fatalf("Resume.ResumeCommand = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.SessionIDFlag, "--session-id-resolved"; got != want {
		t.Fatalf("Resume.SessionIDFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("Hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyDelayMs, 321; got != want {
		t.Fatalf("Hints.ReadyDelayMs = %d, want %d", got, want)
	}
	// Regression for upstream gastownhall/gascity#101 (re-opened): the
	// API resume resolver must seed the three city-anchor identity vars
	// so the restarted shell can locate its city. Without this assertion
	// the new reseed could silently regress without test coverage.
	if got, want := runtimeCfg.SessionEnv["GC_CITY"], fs.cityPath; got != want {
		t.Errorf("SessionEnv[GC_CITY] = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.SessionEnv["GC_CITY_PATH"], fs.cityPath; got != want {
		t.Errorf("SessionEnv[GC_CITY_PATH] = %q, want %q", got, want)
	}
	if runtimeCfg.SessionEnv["GC_CITY_RUNTIME_DIR"] == "" {
		t.Error("SessionEnv[GC_CITY_RUNTIME_DIR] = empty, want set")
	}
	// Identity-only contract (per Copilot review): no dispatcher trace
	// default — that must stay per-dispatcher-qualified, not reseeded
	// to the city-uniform value here.
	if got, present := runtimeCfg.SessionEnv["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; present {
		t.Errorf("SessionEnv[GC_CONTROL_DISPATCHER_TRACE_DEFAULT] = %q present, want absent (identity-only)", got)
	}
	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "api-resume-anthropic-token",
		"ANTHROPIC_BASE_URL":   "https://resolved.example.test",
		"OLLAMA_API_KEY":       "api-resume-ollama-token",
	} {
		if got := runtimeCfg.SessionEnv[key]; got != want {
			t.Errorf("SessionEnv[%s] = %q, want %q", key, got, want)
		}
		if got := runtimeCfg.Hints.Env[key]; got != want {
			t.Errorf("Hints.Env[%s] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"GC_RIG", "GC_SESSION_NAME"} {
		if got, present := runtimeCfg.SessionEnv[key]; present {
			t.Errorf("SessionEnv[%s] = %q present, want absent caller context", key, got)
		}
		if got, present := runtimeCfg.Hints.Env[key]; present {
			t.Errorf("Hints.Env[%s] = %q present, want absent caller context", key, got)
		}
	}
	if got, want := runtimeCfg.Hints.Env["GC_CITY"], fs.cityPath; got != want {
		t.Errorf("Hints.Env[GC_CITY] = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.Env["GC_CITY_PATH"], fs.cityPath; got != want {
		t.Errorf("Hints.Env[GC_CITY_PATH] = %q, want %q", got, want)
	}
	if runtimeCfg.Hints.Env["GC_CITY_RUNTIME_DIR"] == "" {
		t.Error("Hints.Env[GC_CITY_RUNTIME_DIR] = empty, want set")
	}
	if got, present := runtimeCfg.Hints.Env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; present {
		t.Errorf("Hints.Env[GC_CONTROL_DISPATCHER_TRACE_DEFAULT] = %q present, want absent (identity-only)", got)
	}
}

func TestResolveWorkerSessionRuntimeUsesResolvedCommandWhenPersistedCommandIsStale(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:       "Resolved Worker",
		Command:           "/bin/echo",
		ReadyPromptPrefix: "resolved-ready>",
		ReadyDelayMs:      321,
		ResumeFlag:        "--resume-resolved",
		ResumeStyle:       "flag",
		ResumeCommand:     "resolved resume {{.SessionKey}}",
		SessionIDFlag:     "--session-id-resolved",
	}

	srv := New(fs)
	info := session.Info{
		ID:            "sess-1",
		Template:      "myrig/worker",
		Command:       "legacy-agent --dangerously-skip-permissions",
		Provider:      "persisted-provider",
		WorkDir:       t.TempDir(),
		ResumeFlag:    "--resume-persisted",
		ResumeStyle:   "subcommand",
		ResumeCommand: "persisted resume {{.SessionKey}}",
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntime(info)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntime: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntime() = nil")
	}
	if got, want := runtimeCfg.Command, "/bin/echo"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, info.Provider; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.WorkDir, info.WorkDir; got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeFlag, "--resume-resolved"; got != want {
		t.Fatalf("Resume.ResumeFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeStyle, "flag"; got != want {
		t.Fatalf("Resume.ResumeStyle = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeCommand, "resolved resume {{.SessionKey}}"; got != want {
		t.Fatalf("Resume.ResumeCommand = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.SessionIDFlag, "--session-id-resolved"; got != want {
		t.Fatalf("Resume.SessionIDFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("Hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyDelayMs, 321; got != want {
		t.Fatalf("Hints.ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestResolveWorkerSessionRuntimeIncludesEffectiveMCPServers(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Agents[0].Session = "acp"
	supportsACP := true
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName: "Resolved Worker",
		Command:     "/bin/echo",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "filesystem.toml"), []byte(`
name = "filesystem"
command = "/bin/mcp"
args = ["--stdio"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	srv := New(fs)
	info := session.Info{
		ID:        "sess-1",
		Template:  "myrig/worker",
		Transport: "acp",
		WorkDir:   t.TempDir(),
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntime(info)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntime: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntime() = nil")
	}
	if len(runtimeCfg.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(runtimeCfg.Hints.MCPServers))
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Name, "filesystem"; got != want {
		t.Fatalf("Hints.MCPServers[0].Name = %q, want %q", got, want)
	}
}

func TestResolveWorkerSessionRuntimeUsesStoredAgentNameForResumeMCPMaterialization(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "resolved-worker",
		Session:           "acp",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	supportsACP := true
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName: "Resolved Worker",
		Command:     "/bin/echo",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	workDir := filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "ants", "ant")
	srv := New(fs)
	info := session.Info{
		ID:        "sess-1",
		Template:  "myrig/ant",
		Alias:     "ant",
		AgentName: "myrig/ant-adhoc-123",
		Transport: "acp",
		WorkDir:   workDir,
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntime(info)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntime: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntime() = nil")
	}
	if len(runtimeCfg.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(runtimeCfg.Hints.MCPServers))
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Args[0], info.AgentName; got != want {
		t.Fatalf("Args[0] = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Args[1], workDir; got != want {
		t.Fatalf("Args[1] = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Args[2], "myrig/ant"; got != want {
		t.Fatalf("Args[2] = %q, want %q", got, want)
	}
}

func TestResolveWorkerSessionRuntimeFallsBackToStoredMCPServersWhenCatalogBreaks(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "resolved-worker",
		Session:           "acp",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	supportsACP := true
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName: "Resolved Worker",
		Command:     "/bin/echo",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = [broken
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	workDir := filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "ants", "ant")
	metadata, err := session.WithStoredMCPMetadata(nil, "myrig/ant-adhoc-123", []runtime.MCPServerConfig{{
		Name:      "identity",
		Transport: runtime.MCPTransportStdio,
		Command:   "/bin/mcp",
		Args:      []string{"myrig/ant-adhoc-123", workDir, "myrig/ant"},
	}})
	if err != nil {
		t.Fatalf("WithStoredMCPMetadata: %v", err)
	}

	srv := New(fs)
	info := session.Info{
		ID:        "sess-1",
		Template:  "myrig/ant",
		Alias:     "ant",
		AgentName: "myrig/ant-adhoc-123",
		Transport: "acp",
		WorkDir:   workDir,
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", metadata)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if len(runtimeCfg.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(runtimeCfg.Hints.MCPServers))
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Args[0], "myrig/ant-adhoc-123"; got != want {
		t.Fatalf("Args[0] = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Args[1], workDir; got != want {
		t.Fatalf("Args[1] = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Args[2], "myrig/ant"; got != want {
		t.Fatalf("Args[2] = %q, want %q", got, want)
	}
}

func TestResolveWorkerSessionRuntimeFallsBackToRuntimeMCPServersSnapshotWhenCatalogBreaks(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "resolved-worker",
		Session:           "acp",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	supportsACP := true
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName: "Resolved Worker",
		Command:     "/bin/echo",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = [broken
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	servers := []runtime.MCPServerConfig{{
		Name:      "identity",
		Transport: runtime.MCPTransportHTTP,
		Command:   "/bin/mcp",
		Args:      []string{"--api-key", "super-secret"},
		Env: map[string]string{
			"API_TOKEN": "super-secret",
		},
		URL: "https://user:pass@example.invalid/mcp?token=abc123",
		Headers: map[string]string{
			"Authorization": "Bearer secret",
		},
	}}
	metadata, err := session.WithStoredMCPMetadata(nil, "myrig/ant-adhoc-123", servers)
	if err != nil {
		t.Fatalf("WithStoredMCPMetadata: %v", err)
	}
	if err := session.PersistRuntimeMCPServersSnapshot(fs.cityPath, "sess-1", servers); err != nil {
		t.Fatalf("PersistRuntimeMCPServersSnapshot: %v", err)
	}

	srv := New(fs)
	info := session.Info{
		ID:        "sess-1",
		Template:  "myrig/ant",
		Alias:     "ant",
		AgentName: "myrig/ant-adhoc-123",
		Transport: "acp",
		WorkDir:   filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "ants", "ant"),
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", metadata)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if len(runtimeCfg.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(runtimeCfg.Hints.MCPServers))
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Args[1], "super-secret"; got != want {
		t.Fatalf("Args[1] = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Env["API_TOKEN"], "super-secret"; got != want {
		t.Fatalf("Env[API_TOKEN] = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Headers["Authorization"], "Bearer secret"; got != want {
		t.Fatalf("Headers[Authorization] = %q, want %q", got, want)
	}
}

func TestResolveWorkerSessionRuntimeFallsBackToSanitizedStoredMCPServersWhenRuntimeSnapshotMissing(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "resolved-worker",
		Session:           "acp",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	supportsACP := true
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName: "Resolved Worker",
		Command:     "/bin/echo",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = [broken
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	metadata, err := session.WithStoredMCPMetadata(nil, "myrig/ant-adhoc-123", []runtime.MCPServerConfig{{
		Name:      "identity",
		Transport: runtime.MCPTransportHTTP,
		Command:   "/bin/mcp",
		Args:      []string{"--serve", "--api-key", "super-secret"},
		Env: map[string]string{
			"API_TOKEN": "super-secret",
		},
		URL: "https://user:pass@example.invalid/mcp?token=abc123",
		Headers: map[string]string{
			"Authorization": "Bearer secret",
		},
	}})
	if err != nil {
		t.Fatalf("WithStoredMCPMetadata: %v", err)
	}

	srv := New(fs)
	info := session.Info{
		ID:        "sess-1",
		Template:  "myrig/ant",
		Alias:     "ant",
		AgentName: "myrig/ant-adhoc-123",
		Transport: "acp",
		WorkDir:   filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "ants", "ant"),
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", metadata)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if len(runtimeCfg.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(runtimeCfg.Hints.MCPServers))
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].Args, []string{"--serve"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("Args = %#v, want %#v", got, want)
	}
	if len(runtimeCfg.Hints.MCPServers[0].Env) != 0 {
		t.Fatalf("Env = %#v, want empty", runtimeCfg.Hints.MCPServers[0].Env)
	}
	if len(runtimeCfg.Hints.MCPServers[0].Headers) != 0 {
		t.Fatalf("Headers = %#v, want empty", runtimeCfg.Hints.MCPServers[0].Headers)
	}
	if got, want := runtimeCfg.Hints.MCPServers[0].URL, "https://example.invalid/mcp"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestResolveWorkerSessionRuntimeFallsBackToStoredCommandWhenTemplateOverridesInvalid(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Providers["test-agent"] = config.ProviderSpec{
		Command:   "/bin/echo",
		PathCheck: "true",
	}

	srv := New(fs)
	info := session.Info{
		ID:       "sess-1",
		Template: "myrig/worker",
		Command:  "/bin/echo --stored",
		WorkDir:  t.TempDir(),
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", map[string]string{
		"template_overrides": `{`,
	})
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if got, want := runtimeCfg.Command, "/bin/echo --stored"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

// TestResolveWorkerSessionRuntimeResolvesMouseOnlyForInteractiveResume locks the
// ga-c4w controller-poll-safety invariant on the API worker-factory resume seam
// (regression ga-g7go): a resumed pool/headless agent must resolve mouse-OFF so
// the tmux wheel never re-enables on a controller-polled session, while an
// interactive (session_origin=manual) resume keeps mouse-ON — symmetric with the
// create path's session_origin=="manual" gate in templateParamsToConfig. This
// exercises the real resolveWorkerSessionRuntimeWithMetadata -> sessionResumeHints
// chain wired as the worker factory's ResolveSessionRuntime, not a contrived stub
// that dodges the worker-factory path.
func TestResolveWorkerSessionRuntimeResolvesMouseOnlyForInteractiveResume(t *testing.T) {
	for _, tc := range []struct {
		name        string
		metadata    map[string]string
		wantMouseOn bool
	}{
		{
			name:        "pool agent resume stays mouse-off",
			metadata:    map[string]string{"session_origin": "worker"},
			wantMouseOn: false,
		},
		{
			name:        "interactive resume keeps mouse-on",
			metadata:    map[string]string{"agent_name": "myrig/worker", "session_origin": "manual"},
			wantMouseOn: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			fs.cfg.Providers["test-agent"] = config.ProviderSpec{
				Command:   "/bin/echo",
				PathCheck: "true",
			}

			srv := New(fs)
			runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(session.Info{
				Template: "myrig/worker",
				Provider: "test-agent",
				WorkDir:  t.TempDir(),
			}, "", tc.metadata)
			if err != nil {
				t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
			}
			if runtimeCfg == nil {
				t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
			}
			if got := runtimeCfg.Hints.MouseOn; got != tc.wantMouseOn {
				t.Errorf("Hints.MouseOn = %v, want %v (poll-safety: mouse-on only for interactive resume)", got, tc.wantMouseOn)
			}
		})
	}
}

func TestResolveWorkerSessionRuntimeUsesProviderACPDefaultWithoutTemplateSessionOverride(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["test-agent"] = config.ProviderSpec{
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}

	srv := New(fs)
	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(session.Info{
		Template: "myrig/worker",
		WorkDir:  t.TempDir(),
	}, "", nil)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if got, want := runtimeCfg.Command, "/bin/echo acp"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

func TestResolveWorkerSessionRuntimeFallsBackToPersistedRuntimeOnIncompleteResolvedConfig(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Providers["test-agent"] = config.ProviderSpec{
		ReadyPromptPrefix: "resolved-ready>",
		ReadyDelayMs:      321,
	}

	srv := New(fs)
	info := session.Info{
		Template:      "myrig/worker",
		Command:       "persisted-worker --dangerously-skip-permissions",
		Provider:      "persisted-provider",
		WorkDir:       "/tmp/persisted-workdir",
		ResumeFlag:    "--resume-persisted",
		ResumeStyle:   "subcommand",
		ResumeCommand: "persisted resume {{.SessionKey}}",
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", nil)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if got, want := runtimeCfg.Command, info.Command; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, info.Provider; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.WorkDir, info.WorkDir; got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeFlag, info.ResumeFlag; got != want {
		t.Fatalf("Resume.ResumeFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeStyle, info.ResumeStyle; got != want {
		t.Fatalf("Resume.ResumeStyle = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeCommand, info.ResumeCommand; got != want {
		t.Fatalf("Resume.ResumeCommand = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.WorkDir, info.WorkDir; got != want {
		t.Fatalf("Hints.WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("Hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyDelayMs, 321; got != want {
		t.Fatalf("Hints.ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestResolveWorkerSessionRuntimeFallsBackToPersistedProviderWhenCommandMissing(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Providers["test-agent"] = config.ProviderSpec{
		ReadyPromptPrefix: "resolved-ready>",
	}

	srv := New(fs)
	info := session.Info{
		Template: "myrig/worker",
		Provider: "persisted-provider",
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", nil)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if got, want := runtimeCfg.Command, info.Provider; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, info.Provider; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
}

func TestResolveWorkerSessionRuntimeProviderCollisionUsesPersistedProvider(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:     "test-agent",
		Provider: "agent-provider",
	}}
	fs.cfg.Providers["test-agent"] = config.ProviderSpec{
		Command:   "/bin/echo",
		Args:      []string{"provider-session"},
		PathCheck: "true",
	}
	fs.cfg.Providers["agent-provider"] = config.ProviderSpec{
		Command:   "/bin/echo",
		Args:      []string{"agent-template"},
		PathCheck: "true",
	}

	srv := New(fs)
	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(session.Info{
		Template: "test-agent",
		Provider: "test-agent",
		WorkDir:  t.TempDir(),
	}, "", map[string]string{
		"session_origin": "manual",
	})
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if got, wantPrefix := runtimeCfg.Command, "/bin/echo provider-session"; !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("Command = %q, want prefix %q", got, wantPrefix)
	}
}

func TestResolveWorkerSessionRuntimeProviderNameCollisionUsesPersistedProvider(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:     "codex",
		Provider: "codex",
		WorkDir:  ".gc/worktrees/agent-codex",
	}}
	fs.cfg.Providers["codex"] = config.ProviderSpec{
		Command:   "/bin/echo",
		Args:      []string{"provider-session"},
		PathCheck: "true",
	}

	srv := New(fs)
	providerWorkDir := t.TempDir()
	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(session.Info{
		Template: "codex",
		Provider: "codex",
		WorkDir:  providerWorkDir,
	}, "", map[string]string{
		"session_origin": "manual",
	})
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if got, wantPrefix := runtimeCfg.Command, "/bin/echo provider-session"; !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("Command = %q, want prefix %q", got, wantPrefix)
	}
	if got, want := runtimeCfg.WorkDir, providerWorkDir; got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
}

func TestResolveWorkerSessionRuntimeLegacyProviderKindSkipsNameCollisionTemplate(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:     "codex",
		Provider: "codex",
		Session:  "acp",
		WorkDir:  ".gc/worktrees/agent-codex",
	}}
	fs.cfg.Providers["codex"] = config.ProviderSpec{
		Command:    "/bin/echo",
		Args:       []string{"provider-session"},
		PathCheck:  "true",
		ACPCommand: "/bin/echo",
		ACPArgs:    []string{"agent-acp"},
	}

	srv := New(fs)
	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(session.Info{
		Template: "codex",
		Provider: "codex",
		WorkDir:  t.TempDir(),
	}, "", map[string]string{
		"real_world_app_session_kind": "provider",
	})
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if got, wantPrefix := runtimeCfg.Command, "/bin/echo provider-session"; !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("Command = %q, want prefix %q", got, wantPrefix)
	}
}

func TestResolveWorkerSessionRuntimeLegacyManualAgentUsesTemplateWhenProviderMatches(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:     "worker",
		Dir:      "myrig",
		Provider: "test-agent",
	}}
	fs.cfg.Providers["test-agent"] = config.ProviderSpec{
		Command:           "/bin/echo",
		Args:              []string{"agent-template"},
		PathCheck:         "true",
		ReadyPromptPrefix: "agent-ready>",
	}

	srv := New(fs)
	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(session.Info{
		Template: "myrig/worker",
		Provider: "test-agent",
		WorkDir:  t.TempDir(),
	}, "", map[string]string{
		"agent_name":     "myrig/worker",
		"session_origin": "manual",
	})
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if got, want := runtimeCfg.Command, "/bin/echo agent-template"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyPromptPrefix, "agent-ready>"; got != want {
		t.Fatalf("Hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
}

func TestWorkerFactorySessionByIDUsesResolvedTemplateRuntime(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:       "Resolved Worker",
		Command:           "/bin/echo",
		ReadyPromptPrefix: "resolved-ready>",
		ReadyDelayMs:      321,
		ResumeFlag:        "--resume-resolved",
		ResumeStyle:       "flag",
		SessionIDFlag:     "--session-id-resolved",
	}

	srv := New(fs)
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{BeadOnly: true, Template: "myrig/worker", Title: "Chat", Command: "", WorkDir: t.TempDir(), Provider: "", Transport: "", Resume: session.ProviderResume{SessionIDFlag: "--stale-session-id"}})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --session-id-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	if got, want := start.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := start.ReadyDelayMs, 321; got != want {
		t.Fatalf("ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestWorkerFactorySessionByIDPreservesStoredResolvedCommand(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:   "Resolved Worker",
		Command:       "/bin/echo",
		SessionIDFlag: "--session-id-resolved",
	}

	srv := New(fs)
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{BeadOnly: true, Template: "myrig/worker", Title: "Chat", Command: "/bin/echo --composed", WorkDir: t.TempDir(), Provider: "resolved-worker", Transport: "", Resume: session.ProviderResume{SessionIDFlag: "--stale-session-id"}})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --composed --session-id-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
}

func TestWorkerFactorySessionByIDUsesResolvedCommandAndResumeSettingsOnResume(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:   "Resolved Worker",
		Command:       "/bin/echo",
		ResumeFlag:    "--resume-resolved",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id-resolved",
	}

	srv := New(fs)
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(
		context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "legacy-agent", WorkDir: t.TempDir(), Provider: "resolved-worker", Env: nil, Resume: session.ProviderResume{
			ResumeFlag:    "--old-resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id-resolved",
		}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --resume-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
}

func TestWorkerFactorySessionByIDAppliesTemplateOverridesToExplicitResumeCommand(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	spec := fs.cfg.Providers["test-agent"]
	spec.Command = "/bin/echo"
	spec.ResumeCommand = "/bin/echo resume {{.SessionKey}} --skip-permissions"
	spec.SessionIDFlag = "--session-id"
	fs.cfg.Providers["resolved-worker"] = spec

	srv := New(fs)
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(
		context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "/bin/echo --skip-permissions", WorkDir: t.TempDir(), Provider: "resolved-worker", Env: nil, Resume: session.ProviderResume{
			ResumeCommand: "/bin/echo resume {{.SessionKey}} --skip-permissions",
			SessionIDFlag: "--session-id",
		}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := fs.cityBeadStore.SetMetadata(info.ID, "template_overrides", `{"permission_mode":"plan"}`); err != nil {
		t.Fatalf("SetMetadata(template_overrides): %v", err)
	}

	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	want := "/bin/echo resume " + info.SessionKey + " --permission-mode plan --effort max"
	if got := start.Command; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
}

func TestWorkerFactoryHandleForTargetUsesResolvedTemplateRuntimeForSessionMeta(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:       "Resolved Worker",
		Command:           "/bin/echo",
		ReadyPromptPrefix: "resolved-ready>",
		ReadyDelayMs:      321,
		ResumeFlag:        "--resume-resolved",
		ResumeStyle:       "flag",
		SessionIDFlag:     "--session-id-resolved",
	}

	srv := New(fs)
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{BeadOnly: true, Template: "myrig/worker", Title: "Chat", Command: "", WorkDir: t.TempDir(), Provider: "", Transport: "", Resume: session.ProviderResume{SessionIDFlag: "--stale-session-id"}})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if err := fs.sp.SetMeta("legacy-runtime-name", "GC_SESSION_ID", info.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.HandleForTarget("legacy-runtime-name", nil)
	if err != nil {
		t.Fatalf("HandleForTarget: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --session-id-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	if got, want := start.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := start.ReadyDelayMs, 321; got != want {
		t.Fatalf("ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestNewResolvedWorkerSessionHandleStartsResolvedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	handle, err := srv.newResolvedWorkerSessionHandle(fs.cityBeadStore, worker.ResolvedSessionConfig{
		Alias:        "worker",
		ExplicitName: "worker-named",
		Template:     "myrig/worker",
		Title:        "Worker Named",
		Transport:    "acp",
		Metadata:     map[string]string{"session_origin": "named"},
		Runtime: worker.ResolvedRuntime{
			Command:    "/bin/echo",
			WorkDir:    t.TempDir(),
			Provider:   "resolved-worker",
			SessionEnv: map[string]string{"API_RESOLVED_ENV": "present"},
			Resume: session.ProviderResume{
				SessionIDFlag: "--session-id-resolved",
			},
			Hints: runtime.Config{
				ReadyPromptPrefix: "resolved-ready>",
				ReadyDelayMs:      321,
			},
		},
	})
	if err != nil {
		t.Fatalf("newResolvedWorkerSessionHandle: %v", err)
	}

	info, err := handle.Create(context.Background(), worker.CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started): %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --session-id-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	if got, want := start.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := start.ReadyDelayMs, 321; got != want {
		t.Fatalf("ReadyDelayMs = %d, want %d", got, want)
	}
	if got, want := start.Env["API_RESOLVED_ENV"], "present"; got != want {
		t.Fatalf("Env[API_RESOLVED_ENV] = %q, want %q", got, want)
	}
}

func TestNewResolvedWorkerSessionHandleDerivesProviderFromCommand(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	handle, err := srv.newResolvedWorkerSessionHandle(fs.cityBeadStore, worker.ResolvedSessionConfig{
		Alias:        "worker",
		ExplicitName: "worker-command-only",
		Template:     "myrig/worker",
		Title:        "Worker Command Only",
		Runtime: worker.ResolvedRuntime{
			Command: "/bin/echo --print",
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		t.Fatalf("newResolvedWorkerSessionHandle: %v", err)
	}

	info, err := handle.Create(context.Background(), worker.CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started): %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --print"; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}

	bead, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%q): %v", info.ID, err)
	}
	if got, want := bead.Metadata["provider"], "/bin/echo"; got != want {
		t.Fatalf("Metadata[provider] = %q, want %q", got, want)
	}
}

func TestWorkerFactoryRoutesWorkerOperationEventsToStateProvider(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	handle, err := srv.newResolvedWorkerSessionHandle(fs.cityBeadStore, worker.ResolvedSessionConfig{
		Alias:        "worker",
		ExplicitName: "worker-events",
		Template:     "myrig/worker",
		Title:        "Worker Events",
		Runtime: worker.ResolvedRuntime{
			Command:  "/bin/echo",
			WorkDir:  t.TempDir(),
			Provider: "resolved-worker",
			Resume: session.ProviderResume{
				SessionIDFlag: "--session-id",
			},
		},
	})
	if err != nil {
		t.Fatalf("newResolvedWorkerSessionHandle: %v", err)
	}

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	recorded := fs.eventProv.(*events.Fake).Events
	if len(recorded) == 0 {
		t.Fatal("worker start recorded no events")
	}
	last := recorded[len(recorded)-1]
	if got, want := last.Type, events.WorkerOperation; got != want {
		t.Fatalf("last event type = %q, want %q", got, want)
	}
}
