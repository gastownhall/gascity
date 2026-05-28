package api

import (
	"path/filepath"
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
		"/tmp/test-city",
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
		"/tmp/test-city",
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

func TestSessionCreateHintsSeedsRuntimeEnv(t *testing.T) {
	sessionEnv := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "api-create-anthropic-token",
		"ANTHROPIC_BASE_URL":   "https://resolved.example.test",
		"OLLAMA_API_KEY":       "api-create-ollama-token",
		"GC_CITY":              "/tmp/test-city",
	}

	hints := sessionCreateHints(&config.ResolvedProvider{Name: "stub"}, sessionEnv, nil)

	for key, want := range sessionEnv {
		if got := hints.Env[key]; got != want {
			t.Errorf("Hints.Env[%s] = %q, want %q", key, got, want)
		}
	}
}

// TestResolvedSessionConfigForProviderSeedsCityRuntimeEnv is a
// regression test for upstream gastownhall/gascity#101 (re-opened):
// session-create paths through the API resolver dropped the
// city-anchored env vars (GC_CITY, GC_CITY_PATH, GC_CITY_RUNTIME_DIR)
// because they only forwarded resolved.Env (provider-only). The
// spawned shell then could not locate the city, so bd, mailboxes, and
// related tooling failed. Non-conflicting provider env vars are
// preserved; this test documents the merge contract.
func TestResolvedSessionConfigForProviderSeedsCityRuntimeEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "api-anthropic-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://process.example.test")
	t.Setenv("OLLAMA_API_KEY", "api-ollama-token")
	t.Setenv("GC_RIG", "caller-rig")
	t.Setenv("GC_SESSION_NAME", "caller-session")

	cityPath := t.TempDir()
	cfg, err := resolvedSessionConfigForProvider(
		cityPath,
		"worker",
		"",
		"myrig/worker",
		"Worker",
		"",
		nil,
		&config.ResolvedProvider{
			Name:    "stub",
			Command: "/bin/echo",
			Env: map[string]string{
				"ANTHROPIC_BASE_URL": "https://resolved.example.test",
				"PROVIDER_TOKEN":     "ok",
			},
		},
		"",
		cityPath,
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedSessionConfigForProvider: %v", err)
	}
	if got := cfg.Runtime.SessionEnv["GC_CITY"]; got != cityPath {
		t.Errorf("SessionEnv[GC_CITY] = %q, want %q", got, cityPath)
	}
	if got := cfg.Runtime.SessionEnv["GC_CITY_PATH"]; got != cityPath {
		t.Errorf("SessionEnv[GC_CITY_PATH] = %q, want %q", got, cityPath)
	}
	wantRuntimeDir := filepath.Join(cityPath, ".gc", "runtime")
	if got := cfg.Runtime.SessionEnv["GC_CITY_RUNTIME_DIR"]; got != wantRuntimeDir {
		t.Errorf("SessionEnv[GC_CITY_RUNTIME_DIR] = %q, want %q", got, wantRuntimeDir)
	}
	if got := cfg.Runtime.SessionEnv["PROVIDER_TOKEN"]; got != "ok" {
		t.Errorf("SessionEnv[PROVIDER_TOKEN] = %q, want %q (provider env preserved)", got, "ok")
	}
	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "api-anthropic-token",
		"ANTHROPIC_BASE_URL":   "https://resolved.example.test",
		"OLLAMA_API_KEY":       "api-ollama-token",
	} {
		if got := cfg.Runtime.SessionEnv[key]; got != want {
			t.Errorf("SessionEnv[%s] = %q, want %q", key, got, want)
		}
		if got := cfg.Runtime.Hints.Env[key]; got != want {
			t.Errorf("Hints.Env[%s] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"GC_RIG", "GC_SESSION_NAME"} {
		if got, present := cfg.Runtime.SessionEnv[key]; present {
			t.Errorf("SessionEnv[%s] = %q present, want absent caller context", key, got)
		}
		if got, present := cfg.Runtime.Hints.Env[key]; present {
			t.Errorf("Hints.Env[%s] = %q present, want absent caller context", key, got)
		}
	}
	// Identity-only contract (per Copilot review): GC_CONTROL_DISPATCHER_TRACE_DEFAULT
	// must NOT be seeded by the city-anchor reseed because it has to stay
	// per-dispatcher-qualified. template_resolve.go owns the qualified
	// override on the CLI create path; the API resume/create path must
	// not clobber it with the city-uniform default.
	if got, present := cfg.Runtime.SessionEnv["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; present {
		t.Errorf("SessionEnv[GC_CONTROL_DISPATCHER_TRACE_DEFAULT] = %q present, want absent (identity-only)", got)
	}
	if got, present := cfg.Runtime.Hints.Env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; present {
		t.Errorf("Hints.Env[GC_CONTROL_DISPATCHER_TRACE_DEFAULT] = %q present, want absent (identity-only)", got)
	}
	if got := cfg.Runtime.Hints.Env["GC_CITY"]; got != cityPath {
		t.Errorf("Hints.Env[GC_CITY] = %q, want %q", got, cityPath)
	}
	if got := cfg.Runtime.Hints.Env["GC_CITY_PATH"]; got != cityPath {
		t.Errorf("Hints.Env[GC_CITY_PATH] = %q, want %q", got, cityPath)
	}
	if got := cfg.Runtime.Hints.Env["GC_CITY_RUNTIME_DIR"]; got != wantRuntimeDir {
		t.Errorf("Hints.Env[GC_CITY_RUNTIME_DIR] = %q, want %q", got, wantRuntimeDir)
	}
	if got := cfg.Runtime.Hints.Env["PROVIDER_TOKEN"]; got != "ok" {
		t.Errorf("Hints.Env[PROVIDER_TOKEN] = %q, want %q (provider env preserved)", got, "ok")
	}
}

// TestResolvedSessionConfigForProviderCityAnchorsBeatConflictingProviderEnv
// locks in the precedence contract: when the resolved provider env
// carries its own GC_CITY (e.g. left over from a stale config), the
// city-anchored reseed must win. Future refactors that reverse the
// merge order would re-introduce upstream #101 from the other side;
// this test fails fast on that regression.
func TestResolvedSessionConfigForProviderCityAnchorsBeatConflictingProviderEnv(t *testing.T) {
	cityPath := t.TempDir()
	cfg, err := resolvedSessionConfigForProvider(
		cityPath,
		"worker",
		"",
		"myrig/worker",
		"Worker",
		"",
		nil,
		&config.ResolvedProvider{
			Name:    "stub",
			Command: "/bin/echo",
			Env: map[string]string{
				"GC_CITY":      "/wrong/city",
				"GC_CITY_PATH": "/wrong/city",
			},
		},
		"",
		cityPath,
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedSessionConfigForProvider: %v", err)
	}
	if got := cfg.Runtime.SessionEnv["GC_CITY"]; got != cityPath {
		t.Errorf("SessionEnv[GC_CITY] = %q, want %q (city anchor must win over provider env)", got, cityPath)
	}
	if got := cfg.Runtime.SessionEnv["GC_CITY_PATH"]; got != cityPath {
		t.Errorf("SessionEnv[GC_CITY_PATH] = %q, want %q (city anchor must win over provider env)", got, cityPath)
	}
}

func TestCityAnchoredSessionEnvSkipsCityAnchorsWhenCityPathEmpty(t *testing.T) {
	providerEnv := map[string]string{
		"GC_CITY":        "/provider/city",
		"PROVIDER_TOKEN": "ok",
	}

	got := cityAnchoredSessionEnv(" \t\n ", providerEnv)
	if got["GC_CITY"] != "/provider/city" {
		t.Fatalf("GC_CITY = %q, want provider value", got["GC_CITY"])
	}
	if got["PROVIDER_TOKEN"] != "ok" {
		t.Fatalf("PROVIDER_TOKEN = %q, want ok", got["PROVIDER_TOKEN"])
	}
	if _, ok := got["GC_CITY_PATH"]; ok {
		t.Fatalf("GC_CITY_PATH = %q, want absent when city path is empty", got["GC_CITY_PATH"])
	}
	if _, ok := got["GC_CITY_RUNTIME_DIR"]; ok {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want absent when city path is empty", got["GC_CITY_RUNTIME_DIR"])
	}

	providerEnv["PROVIDER_TOKEN"] = "mutated"
	if got["PROVIDER_TOKEN"] != "ok" {
		t.Fatalf("result env aliases provider env: PROVIDER_TOKEN = %q, want ok", got["PROVIDER_TOKEN"])
	}
}

func TestResolvedSessionConfigForProviderSkipsStoredMCPMetadataForTmuxTransport(t *testing.T) {
	cfg, err := resolvedSessionConfigForProvider(
		"/tmp/test-city",
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
