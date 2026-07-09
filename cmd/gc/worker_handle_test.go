package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type failingSessionLookupStore struct {
	beads.Store
	err error
}

func (s *failingSessionLookupStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, s.err
}

func (s *failingSessionLookupStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

func TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnFirstStart(t *testing.T) {
	skipSlowCmdGCTest(t, "waits through stale session-key detection; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
ready_prompt_prefix = "stub-ready>"
ready_delay_ms = 250

[providers.stub.env]
STUB_ENV = "present"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, store, sp, cfg)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{BeadOnly: true, Template: "worker", Title: "Probe", Command: "", WorkDir: t.TempDir(), Provider: "stub", Transport: "", Resume: session.ProviderResume{
		SessionIDFlag: "--old-session-id",
	}})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if strings.TrimSpace(info.SessionKey) == "" {
		t.Fatal("SessionKey is empty")
	}

	handle, err := workerHandleForSessionWithConfig(cityDir, store, sp, cfg, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSessionWithConfig: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("handle.Start: %v", err)
	}

	start := sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", info.SessionName)
	}
	wantArg := "--session-id " + info.SessionKey
	if !strings.Contains(start.Command, "/bin/echo") || !strings.Contains(start.Command, wantArg) {
		t.Fatalf("start command = %q, want /bin/echo with %q", start.Command, wantArg)
	}
	if strings.Contains(start.Command, "--old-session-id") {
		t.Fatalf("start command = %q, still used stale session id flag", start.Command)
	}
	if start.ReadyPromptPrefix != "stub-ready>" {
		t.Fatalf("ReadyPromptPrefix = %q, want stub-ready>", start.ReadyPromptPrefix)
	}
	if start.ReadyDelayMs != 250 {
		t.Fatalf("ReadyDelayMs = %d, want 250", start.ReadyDelayMs)
	}
	if start.Env["STUB_ENV"] != "present" {
		t.Fatalf("Env[STUB_ENV] = %q, want present", start.Env["STUB_ENV"])
	}
}

func TestResolvedWorkerRuntimeWithConfigUsesProviderLaunchCommand(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	claude := config.BuiltinProviders()["claude"]
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "claude",
		}},
		Providers: map[string]config.ProviderSpec{
			"claude": claude,
		},
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if !strings.Contains(resolved.Command, "--dangerously-skip-permissions") {
		t.Fatalf("Command = %q, want unrestricted default", resolved.Command)
	}
	if !strings.Contains(resolved.Command, "--effort max") {
		t.Fatalf("Command = %q, want effort max default", resolved.Command)
	}
	if !strings.Contains(resolved.Command, "--settings") {
		t.Fatalf("Command = %q, want settings arg", resolved.Command)
	}
}

// TestResolvedWorkerRuntimeResumesPoolSessionPreservesLaunchFlags is a
// regression test for gastownhall/gascity#799: a pool-agent session
// resumed through the control-dispatcher path must reconstruct the full
// launch command (--dangerously-skip-permissions, --settings, schema
// defaults) even when the persisted session command is the bare
// provider name. The pre-fix path dropped those flags and caused pool
// workers resumed via `claude --resume <uuid>` to wedge on interactive
// permission prompts.
func TestResolvedWorkerRuntimeResumesPoolSessionPreservesLaunchFlags(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	claude := config.BuiltinProviders()["claude"]
	maxActive := 3
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "perspective_planner",
			Provider:          "claude",
			MaxActiveSessions: &maxActive,
		}},
		Providers: map[string]config.ProviderSpec{
			"claude": claude,
		},
	}

	// Simulate a pool-instance session bead whose persisted command is
	// the bare provider name — the shape produced before the April 2026
	// worker-boundary refactor when the API created the bead with
	// sessionCreateAgentCommand(resolved) before the reconciler synced
	// the full tp.Command.
	runtimeCfg, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "perspective_planner",
		Command:  "claude",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if !strings.Contains(runtimeCfg.Command, "--dangerously-skip-permissions") {
		t.Fatalf("resumed pool Command = %q, want --dangerously-skip-permissions", runtimeCfg.Command)
	}
	if !strings.Contains(runtimeCfg.Command, "--effort max") {
		t.Fatalf("resumed pool Command = %q, want --effort max default", runtimeCfg.Command)
	}
	if !strings.Contains(runtimeCfg.Command, "--settings") {
		t.Fatalf("resumed pool Command = %q, want --settings arg", runtimeCfg.Command)
	}
}

// TestResolvedWorkerRuntimeWithConfigSeedsCityRuntimeEnv is a regression
// test for upstream gastownhall/gascity#101 (re-opened): on session
// restart, the worker resolver reseeded the session env from
// resolved.Env (provider-only). That dropped the city-anchored env vars
// (GC_CITY, GC_CITY_PATH, GC_CITY_RUNTIME_DIR), so spawned/restarted
// agent sessions could not locate their city — bd commands failed,
// mailboxes resolved against the wrong path, and downstream tooling
// behaved as if no city was configured. The CLI-side defense in
// cmd/gc/main.go resolveContext (#2062) masked the symptom; this
// resolver-level fix is the root cause: the resolved runtime must
// always carry the city anchor vars so any restart path is sound.
func TestResolvedWorkerRuntimeWithConfigSeedsCityRuntimeEnv(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	claude := config.BuiltinProviders()["claude"]
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "claude",
		}},
		Providers: map[string]config.ProviderSpec{
			"claude": claude,
		},
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}

	if got := resolved.SessionEnv["GC_CITY"]; got != cityDir {
		t.Errorf("SessionEnv[GC_CITY] = %q, want %q", got, cityDir)
	}
	if got := resolved.SessionEnv["GC_CITY_PATH"]; got != cityDir {
		t.Errorf("SessionEnv[GC_CITY_PATH] = %q, want %q", got, cityDir)
	}
	wantRuntimeDir := filepath.Join(cityDir, ".gc", "runtime")
	if got := resolved.SessionEnv["GC_CITY_RUNTIME_DIR"]; got != wantRuntimeDir {
		t.Errorf("SessionEnv[GC_CITY_RUNTIME_DIR] = %q, want %q", got, wantRuntimeDir)
	}
	// Identity-only contract (per Copilot review): the dispatcher trace
	// default must NOT be seeded by the resume reseed, because it has to
	// stay per-dispatcher-qualified (template_resolve.go owns the
	// qualified override). Seeding the city-uniform default here would
	// regress trace files for control-dispatcher sessions on restart.
	if got, present := resolved.SessionEnv["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; present {
		t.Errorf("SessionEnv[GC_CONTROL_DISPATCHER_TRACE_DEFAULT] = %q present, want absent (identity-only reseed)", got)
	}
}

func TestResolvedWorkerRuntimeWithConfigIncludesProviderAuthPassthrough(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "test-anthropic-auth-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://ollama.example.test")
	t.Setenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "kimi-k2.5")
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "kimi-k2.5")
	t.Setenv("OLLAMA_API_KEY", "test-ollama-token")
	t.Setenv("GC_RIG", "caller-rig")
	t.Setenv("GC_SESSION_NAME", "caller-session")

	claude := config.BuiltinProviders()["claude"]
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "claude",
		}},
		Providers: map[string]config.ProviderSpec{
			"claude": claude,
		},
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN":           "test-anthropic-auth-token",
		"ANTHROPIC_BASE_URL":             "https://ollama.example.test",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "kimi-k2.5",
		"CLAUDE_CODE_SUBAGENT_MODEL":     "kimi-k2.5",
		"OLLAMA_API_KEY":                 "test-ollama-token",
	} {
		if got := resolved.SessionEnv[key]; got != want {
			t.Errorf("SessionEnv[%s] = %q, want %q", key, got, want)
		}
		if got := resolved.Hints.Env[key]; got != want {
			t.Errorf("Hints.Env[%s] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"GC_RIG", "GC_SESSION_NAME"} {
		if got, ok := resolved.SessionEnv[key]; ok {
			t.Errorf("SessionEnv[%s] = %q, want absent caller context", key, got)
		}
		if got, ok := resolved.Hints.Env[key]; ok {
			t.Errorf("Hints.Env[%s] = %q, want absent caller context", key, got)
		}
	}
	if got := resolved.SessionEnv["GC_CITY"]; got != cityDir {
		t.Errorf("SessionEnv[GC_CITY] = %q, want %q", got, cityDir)
	}
	if got := resolved.Hints.Env["GC_CITY"]; got != cityDir {
		t.Errorf("Hints.Env[GC_CITY] = %q, want %q", got, cityDir)
	}
}

// TestResolvedWorkerRuntimeWithConfigCityAnchorsBeatConflictingProviderEnv
// pins the precedence contract: when the resolved provider env carries
// its own GC_CITY (e.g. left over from a stale pool entry, or a
// provider that hard-codes one), the city-anchored reseed must win.
// Without this assertion, future refactors could accidentally reverse
// the merge order and re-introduce upstream #101 from the other side.
func TestResolvedWorkerRuntimeWithConfigCityAnchorsBeatConflictingProviderEnv(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	claude := config.BuiltinProviders()["claude"]
	// Force a conflicting GC_CITY in the provider env so we can prove
	// the reseed wins. We can't reach into resolved.Env directly, so we
	// instead pin the worker's env on its own ProviderSpec via the
	// pool entry's runtime env section (which feeds resolved.Env).
	claude.Env = map[string]string{
		"GC_CITY":      "/wrong/city",
		"GC_CITY_PATH": "/wrong/city",
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "claude",
		}},
		Providers: map[string]config.ProviderSpec{
			"claude": claude,
		},
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got := resolved.SessionEnv["GC_CITY"]; got != cityDir {
		t.Errorf("SessionEnv[GC_CITY] = %q, want %q (city anchor must win over provider env)", got, cityDir)
	}
	if got := resolved.SessionEnv["GC_CITY_PATH"]; got != cityDir {
		t.Errorf("SessionEnv[GC_CITY_PATH] = %q, want %q (city anchor must win over provider env)", got, cityDir)
	}
}

func TestResolvedWorkerRuntimeWithConfigSkipsCityAnchorsWhenCityPathEmpty(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "stub",
		}},
		Providers: map[string]config.ProviderSpec{
			"stub": {
				Command: "/bin/echo",
				Env: map[string]string{
					"GC_CITY":        "/provider/city",
					"PROVIDER_TOKEN": "ok",
				},
			},
		},
	}

	resolved, err := resolvedWorkerRuntimeWithConfigAndMetadata("", cfg, session.Info{
		Template: "worker",
		WorkDir:  "/tmp/work",
	}, "", nil)
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfigAndMetadata: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfigAndMetadata() = nil")
	}
	if got := resolved.SessionEnv["GC_CITY"]; got != "/provider/city" {
		t.Fatalf("SessionEnv[GC_CITY] = %q, want provider value", got)
	}
	if got := resolved.SessionEnv["PROVIDER_TOKEN"]; got != "ok" {
		t.Fatalf("SessionEnv[PROVIDER_TOKEN] = %q, want ok", got)
	}
	if _, ok := resolved.SessionEnv["GC_CITY_PATH"]; ok {
		t.Fatalf("SessionEnv[GC_CITY_PATH] = %q, want absent when city path is empty", resolved.SessionEnv["GC_CITY_PATH"])
	}
	if _, ok := resolved.SessionEnv["GC_CITY_RUNTIME_DIR"]; ok {
		t.Fatalf("SessionEnv[GC_CITY_RUNTIME_DIR] = %q, want absent when city path is empty", resolved.SessionEnv["GC_CITY_RUNTIME_DIR"])
	}
}

// TestResolvedWorkerSessionConfigWithConfigSeedsCityAnchorsOnCreatePath
// covers the CLI session-create path (called by `gc session start` /
// `gc session new` etc. through newWorkerSessionHandleForResolvedRuntimeWithConfig).
// Before this fix, the create path passed resolved.Env directly as
// SessionEnv, so direct CLI creates landed without GC_CITY anchors —
// the same upstream #101 symptom as the resume path, just through a
// different door. Companion to the resume-path regression test above.
func TestResolvedWorkerSessionConfigWithConfigSeedsCityAnchorsOnCreatePath(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := resolvedWorkerSessionConfigWithConfig(
		cityDir,
		"",
		"",
		cityDir,
		"worker",
		"",
		"worker",
		"Worker",
		"",
		&config.ResolvedProvider{Name: "claude"},
		map[string]string{"session_origin": "test"},
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig: %v", err)
	}
	env := cfg.Runtime.SessionEnv
	if got := env["GC_CITY"]; got != cityDir {
		t.Errorf("Runtime.SessionEnv[GC_CITY] = %q, want %q", got, cityDir)
	}
	if got := env["GC_CITY_PATH"]; got != cityDir {
		t.Errorf("Runtime.SessionEnv[GC_CITY_PATH] = %q, want %q", got, cityDir)
	}
	if env["GC_CITY_RUNTIME_DIR"] == "" {
		t.Error("Runtime.SessionEnv[GC_CITY_RUNTIME_DIR] = empty, want set")
	}
	if got, present := env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; present {
		t.Errorf("Runtime.SessionEnv[GC_CONTROL_DISPATCHER_TRACE_DEFAULT] = %q present, want absent (identity-only)", got)
	}
}

func TestResolvedWorkerSessionConfigWithConfigIncludesProviderAuthPassthrough(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "test-anthropic-auth-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://ollama.example.test")
	t.Setenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "kimi-k2.5")
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "kimi-k2.5")
	t.Setenv("OLLAMA_API_KEY", "test-ollama-token")
	t.Setenv("GC_RIG", "caller-rig")
	t.Setenv("GC_SESSION_NAME", "caller-session")

	cfg, err := resolvedWorkerSessionConfigWithConfig(
		cityDir,
		"",
		"",
		cityDir,
		"worker",
		"",
		"worker",
		"Worker",
		"",
		&config.ResolvedProvider{Name: "claude"},
		map[string]string{"session_origin": "test"},
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig: %v", err)
	}
	env := cfg.Runtime.SessionEnv
	hintsEnv := cfg.Runtime.Hints.Env
	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN":           "test-anthropic-auth-token",
		"ANTHROPIC_BASE_URL":             "https://ollama.example.test",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "kimi-k2.5",
		"CLAUDE_CODE_SUBAGENT_MODEL":     "kimi-k2.5",
		"OLLAMA_API_KEY":                 "test-ollama-token",
	} {
		if got := env[key]; got != want {
			t.Errorf("Runtime.SessionEnv[%s] = %q, want %q", key, got, want)
		}
		if got := hintsEnv[key]; got != want {
			t.Errorf("Runtime.Hints.Env[%s] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"GC_RIG", "GC_SESSION_NAME"} {
		if got, ok := env[key]; ok {
			t.Errorf("Runtime.SessionEnv[%s] = %q, want absent caller context", key, got)
		}
		if got, ok := hintsEnv[key]; ok {
			t.Errorf("Runtime.Hints.Env[%s] = %q, want absent caller context", key, got)
		}
	}
	if got := env["GC_CITY"]; got != cityDir {
		t.Errorf("Runtime.SessionEnv[GC_CITY] = %q, want %q", got, cityDir)
	}
	if got := hintsEnv["GC_CITY"]; got != cityDir {
		t.Errorf("Runtime.Hints.Env[GC_CITY] = %q, want %q", got, cityDir)
	}
}

func TestShouldPreserveStoredRuntimeCommandForTransportRejectsExecutableOnlyMatch(t *testing.T) {
	if shouldPreserveStoredRuntimeCommandForTransport(
		"claude",
		"claude --settings /tmp/settings.json",
		"",
		nil,
	) {
		t.Fatal("shouldPreserveStoredRuntimeCommandForTransport() = true, want false")
	}
}

func TestResolvedWorkerRuntimeWithConfigUsesStoredTemplateACPTransport(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"
session = "acp"

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)
	writeCatalogFile(t, cityDir, "mcp/filesystem.toml", `
name = "filesystem"
command = "/bin/mcp"
args = ["--stdio"]

[env]
TOKEN = "abc"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template:  "worker",
		Command:   "/bin/echo",
		Transport: "acp",
		WorkDir:   cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := resolved.Command, "/bin/echo acp"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if len(resolved.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(resolved.Hints.MCPServers))
	}
	if got, want := resolved.Hints.MCPServers[0].Name, "filesystem"; got != want {
		t.Fatalf("Hints.MCPServers[0].Name = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigDoesNotInferConfiguredTransportWithoutStoredTemplateACPMetadata(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"
session = "acp"

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
		Command:  "/bin/echo",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := resolved.Command, "/bin/echo"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeTransportUsesResumeMetadataForLegacyACPWithSameCommand(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Command:    "/bin/echo",
		ACPCommand: "/bin/echo",
	}

	got := resolvedWorkerRuntimeTransport(session.Info{
		Command: "/bin/echo",
	}, resolved, "acp", map[string]string{
		"resume_flag": "--resume",
	})
	if got != "acp" {
		t.Fatalf("resolvedWorkerRuntimeTransport() = %q, want acp", got)
	}
}

func TestResolvedWorkerRuntimeTransportUsesConfiguredTmuxForCommandOnlyBead(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:        "kimi",
		Command:     "aimux",
		Args:        []string{"run", "kimi"},
		SupportsACP: true,
		ACPCommand:  "kimi-acp",
	}

	got := resolvedWorkerRuntimeTransport(session.Info{
		Template: "gascity/workflows.kimi",
		Provider: "kimi",
		Command:  "aimux run kimi -- --yolo --no-thinking --model kimi-k2.6",
	}, resolved, config.SessionTransportTmux, nil)
	if got != config.SessionTransportTmux {
		t.Fatalf("resolvedWorkerRuntimeTransport() = %q, want tmux", got)
	}
}

func TestResolvedWorkerRuntimeTransportUsesStoredACPCommandBeforeConfiguredTmux(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:        "kimi",
		Command:     "aimux",
		Args:        []string{"run", "kimi"},
		SupportsACP: true,
		ACPCommand:  "kimi-acp",
		ACPArgs:     []string{"run", "kimi"},
	}

	got := resolvedWorkerRuntimeTransport(session.Info{
		Template: "gascity/workflows.kimi",
		Provider: "kimi",
		Command:  "kimi-acp run kimi --resume session-1",
	}, resolved, config.SessionTransportTmux, nil)
	if got != config.SessionTransportACP {
		t.Fatalf("resolvedWorkerRuntimeTransport() = %q, want acp", got)
	}
}

func TestResolvedWorkerRuntimeWithConfigErrorsForAmbiguousLegacyACPTransportWithSameCommand(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"
session = "acp"

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	_, err = resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
		Command:  "/bin/echo",
		WorkDir:  cityDir,
	}, "")
	if err == nil || !strings.Contains(err.Error(), "legacy session transport is ambiguous") {
		t.Fatalf("resolvedWorkerRuntimeWithConfig() error = %v, want ambiguous legacy ACP transport", err)
	}
}

func TestResolvedWorkerRuntimeWithConfigUsesStartedConfigHashForLegacyProviderACPWithSameCommand(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.custom-acp]
command = "/bin/echo"
path_check = "true"
supports_acp = true
acp_command = "/bin/echo"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	cfg.PackMCPDir = filepath.Join(cityDir, "mcp")
	if err := os.MkdirAll(cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	info := session.Info{
		Template: "custom-acp",
		Command:  "/bin/echo",
		Provider: "custom-acp",
		WorkDir:  cityDir,
	}
	resolved, _ := resolveWorkerRuntimeProviderWithConfig(cfg, info, "provider")
	mcpServers, err := resolvedRuntimeMCPServersWithConfig(
		cityDir,
		cfg,
		info.Alias,
		info.Template,
		info.Provider,
		info.WorkDir,
		"acp",
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedRuntimeMCPServersWithConfig: %v", err)
	}
	startedHash := runtime.CoreFingerprint(runtime.Config{
		Command:    resolved.ACPCommandString(),
		Env:        resolved.Env,
		MCPServers: mcpServers,
	})

	runtimeCfg, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, info, "provider", map[string]string{
		"started_config_hash": startedHash,
	})
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfigAndMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfigAndMetadata() = nil")
	}
	if len(runtimeCfg.Hints.MCPServers) != 1 {
		t.Fatalf("len(runtimeCfg.Hints.MCPServers) = %d, want 1", len(runtimeCfg.Hints.MCPServers))
	}
}

func TestResolveWorkerRuntimeProviderWithConfigProviderKindPrefersPersistedProvider(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"stored-provider": {
				Command: "true",
				Args:    []string{"stored"},
			},
			"template-provider": {
				Command: "true",
				Args:    []string{"template"},
			},
		},
	}
	info := session.Info{
		Template: "template-provider",
		Provider: "stored-provider",
	}

	resolved, _ := resolveWorkerRuntimeProviderWithConfig(cfg, info, "provider")
	if resolved == nil {
		t.Fatal("resolveWorkerRuntimeProviderWithConfig() = nil")
	}
	if got := resolved.Name; got != "stored-provider" {
		t.Fatalf("resolved.Name = %q, want stored-provider", got)
	}
}

func TestResolveWorkerRuntimeProviderWithConfigMetadataIdentifiesProviderSession(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", Provider: "agent-provider"},
		},
		Providers: map[string]config.ProviderSpec{
			"stored-provider": {
				Command: "true",
				Args:    []string{"stored"},
			},
			"agent-provider": {
				Command: "true",
				Args:    []string{"agent"},
			},
		},
	}
	info := session.Info{
		Template: "myrig/worker",
		Provider: "stored-provider",
	}

	resolved, _ := resolveWorkerRuntimeProviderWithConfigAndMetadata(cfg, info, "", map[string]string{
		"session_origin": "manual",
	})
	if resolved == nil {
		t.Fatal("resolveWorkerRuntimeProviderWithConfigAndMetadata() = nil")
	}
	if got := resolved.Name; got != "stored-provider" {
		t.Fatalf("resolved.Name = %q, want stored-provider", got)
	}

	legacyAgentInfo := session.Info{
		Template: "myrig/worker",
		Provider: "agent-provider",
	}
	resolved, _ = resolveWorkerRuntimeProviderWithConfigAndMetadata(cfg, legacyAgentInfo, "", map[string]string{
		"session_origin": "manual",
	})
	if resolved == nil {
		t.Fatal("resolveWorkerRuntimeProviderWithConfigAndMetadata(legacy agent) = nil")
	}
	if got := resolved.Name; got != "agent-provider" {
		t.Fatalf("legacy agent resolved.Name = %q, want agent-provider", got)
	}

	resolved, _ = resolveWorkerRuntimeProviderWithConfigAndMetadata(cfg, info, "", map[string]string{
		"session_origin": "manual",
		"agent_name":     "myrig/worker",
	})
	if resolved == nil {
		t.Fatal("resolveWorkerRuntimeProviderWithConfigAndMetadata(agent) = nil")
	}
	if got := resolved.Name; got != "agent-provider" {
		t.Fatalf("agent resolved.Name = %q, want agent-provider", got)
	}
}

func TestResolvedWorkerRuntimeWithConfigUsesCurrentAgentProviderOverStaleSessionProvider(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", Provider: "agent-provider"},
		},
		Providers: map[string]config.ProviderSpec{
			"stale-provider": {
				Command: "true",
				Args:    []string{"stale"},
			},
			"agent-provider": {
				Command: "true",
				Args:    []string{"agent"},
			},
		},
	}

	resolved, err := resolvedWorkerRuntimeWithConfigAndMetadata("", cfg, session.Info{
		Template: "myrig/worker",
		Provider: "stale-provider",
		WorkDir:  "/tmp/work",
	}, "", map[string]string{
		"session_origin": "ephemeral",
		"agent_name":     "myrig/worker",
	})
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfigAndMetadata: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfigAndMetadata() = nil")
	}
	if got := resolved.Provider; got != "agent-provider" {
		t.Fatalf("resolved.Provider = %q, want agent-provider", got)
	}
}

func TestResolvedWorkerRuntimeWithConfigKeepsDefaultTransportWithoutExplicitACPTemplate(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
		Command:  "/bin/echo",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := resolved.Command, "/bin/echo"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigUsesStoredACPTransportForProviderSession(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.opencode]
command = "/bin/echo"
path_check = "true"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template:  "opencode",
		Command:   "/bin/echo",
		Transport: "acp",
		WorkDir:   cityDir,
	}, "provider")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := resolved.Command, "/bin/echo acp"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigUsesStoredACPTransportForLegacyProviderSessionWithoutMetadata(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.opencode]
command = "/bin/echo"
path_check = "true"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "opencode",
		Command:  "/bin/echo acp",
		WorkDir:  cityDir,
	}, "provider")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := resolved.Command, "/bin/echo acp"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigUsesStoredACPTransportForLegacyProviderSessionOnACPEnabledCustomProvider(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.custom-acp]
command = "/bin/echo"
path_check = "true"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "custom-acp",
		Command:  "/bin/echo acp",
		WorkDir:  cityDir,
	}, "provider")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := resolved.Command, "/bin/echo acp"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigUsesProviderACPDefaultForAgentTemplateWithoutSessionOverride(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
dir = "myrig"
provider = "custom-acp"

[providers.custom-acp]
command = "/bin/echo"
path_check = "true"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "myrig/worker",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := resolved.Command, "/bin/echo acp"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigReplaysTemplateOverridesOnResume(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Dir:      "myrig",
			Provider: "custom",
		}},
		Providers: map[string]config.ProviderSpec{
			"custom": {
				Command:       "/bin/echo",
				ResumeCommand: "/bin/echo --resume {{.SessionKey}} --effort low",
				ResumeFlag:    "--resume",
				ResumeStyle:   "flag",
				PathCheck:     "true",
				OptionsSchema: []config.ProviderOption{{
					Key:  "effort",
					Type: "select",
					Choices: []config.OptionChoice{{
						Value:    "high",
						FlagArgs: []string{"--effort", "high"},
					}, {
						Value:    "low",
						FlagArgs: []string{"--effort", "low"},
					}},
				}},
			},
		},
	}

	resolved, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, session.Info{
		Template:   "myrig/worker",
		Command:    "/bin/echo",
		WorkDir:    cityDir,
		SessionKey: "abc-123",
	}, "", map[string]string{
		"template_overrides": `{"effort":"high","initial_message":"hello"}`,
	})
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfigAndMetadata: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfigAndMetadata() = nil")
	}
	if got, want := resolved.Command, "/bin/echo --effort high"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := resolved.Resume.ResumeCommand, "/bin/echo --resume {{.SessionKey}} --effort high"; got != want {
		t.Fatalf("Resume.ResumeCommand = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigFallsBackToStoredCommandWhenTemplateOverridesInvalid(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Dir:      "myrig",
			Provider: "custom",
		}},
		Providers: map[string]config.ProviderSpec{
			"custom": {
				Command:   "/bin/echo",
				PathCheck: "true",
			},
		},
	}

	resolved, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, session.Info{
		Template: "myrig/worker",
		Command:  "/bin/echo --stored",
		WorkDir:  cityDir,
	}, "", map[string]string{
		"template_overrides": `{`,
	})
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfigAndMetadata: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfigAndMetadata() = nil")
	}
	if got, want := resolved.Command, "/bin/echo --stored"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

func TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnResume(t *testing.T) {
	skipSlowCmdGCTest(t, "waits through stale session-key detection; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, store, sp, cfg)
	info, err := mgr.CreateSession(
		context.Background(), session.CreateOptions{Template: "worker", Title: "Probe", Command: "legacy-agent", WorkDir: t.TempDir(), Provider: "stub", Env: nil, Resume: session.ProviderResume{
			ResumeFlag:    "--old-resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	handle, err := workerHandleForSessionWithConfig(cityDir, store, sp, cfg, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSessionWithConfig: %v", err)
	}

	sp.Calls = nil
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("handle.Start: %v", err)
	}

	start := sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", info.SessionName)
	}
	wantArg := "--resume " + info.SessionKey
	if !strings.Contains(start.Command, "/bin/echo") || !strings.Contains(start.Command, wantArg) {
		t.Fatalf("start command = %q, want /bin/echo with %q", start.Command, wantArg)
	}
	if strings.Contains(start.Command, "--old-resume") {
		t.Fatalf("start command = %q, still used stale resume flag", start.Command)
	}
}

func TestWorkerHandleForSessionTargetWithConfigResolvesSessionName(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, store, sp, cfg)
	info, err := mgr.CreateSession(
		context.Background(), session.CreateOptions{Template: "worker", Title: "Probe", Command: "", WorkDir: t.TempDir(), Provider: "stub", Env: nil, Resume: session.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	handle, err := workerHandleForSessionTargetWithConfig(cityDir, store, sp, cfg, info.SessionName)
	if err != nil {
		t.Fatalf("workerHandleForSessionTargetWithConfig: %v", err)
	}
	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("handle.Kill: %v", err)
	}
	if stop := sp.Calls[len(sp.Calls)-1]; stop.Method != "Stop" || stop.Name != info.SessionName {
		t.Fatalf("last runtime call = %#v, want Stop %q", stop, info.SessionName)
	}
}

// TestWorkerObserveSessionTargetWithConfigDoesNotFetchSessionBeadMoreThanTwice
// guards the dedup invariant. The Observe path used to load the same session
// bead five times per call (resolve, factory.Get, factory metadata Get,
// LiveObservation Get, ObserveRuntime Get); this PR collapses that to two:
// once for ResolveSessionBeadByExactID and once for LiveObservation's
// freshness re-load. Each redundant fetch is a `bd show` CLI fork on real
// (non-mem) stores, so the supervisor's nudge poll loop pays for every Get
// directly in idle-city CPU.
func TestWorkerObserveSessionTargetWithConfigDoesNotFetchSessionBeadMoreThanTwice(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	backing, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, backing, sp, cfg)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Probe", Command: "/bin/echo", WorkDir: t.TempDir(), Provider: "stub", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{Command: "/bin/echo"}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	store := &sessionGetSpyStore{Store: backing}
	obs, err := workerObserveSessionTargetWithConfig(cityDir, store, sp, cfg, info.ID)
	if err != nil {
		t.Fatalf("workerObserveSessionTargetWithConfig: %v", err)
	}
	if !obs.Running {
		t.Fatalf("obs.Running = false, want true after Create started runtime")
	}

	var hits int
	for _, id := range store.getIDs {
		if id == info.ID {
			hits++
		}
	}
	if hits > 2 {
		t.Fatalf("store.Get(%q) called %d times, want at most 2; all Get IDs: %v", info.ID, hits, store.getIDs)
	}
}

func TestWorkerObserveSessionTargetWithConfigFallsBackToRunningRuntimeHandle(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
		},
	}

	target := cliSessionName("/home/user/city", cfg.Workspace.Name, "mayor", cfg.Workspace.SessionTemplate)
	obs, err := workerObserveSessionTargetWithConfig("/home/user/city", nil, sp, cfg, target)
	if err != nil {
		t.Fatalf("workerObserveSessionTargetWithConfig: %v", err)
	}
	if !obs.Running {
		t.Fatalf("obs.Running = false, want true for %q", target)
	}
}

func TestWorkerObserveSessionTargetWithConfigIgnoresStoreLookupFailuresForRuntimeFallback(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
		},
	}

	target := cliSessionName("/home/user/city", cfg.Workspace.Name, "mayor", cfg.Workspace.SessionTemplate)
	store := &failingSessionLookupStore{err: fmt.Errorf("store lookup failed")}
	obs, err := workerObserveSessionTargetWithConfig("/home/user/city", store, sp, cfg, target)
	if err != nil {
		t.Fatalf("workerObserveSessionTargetWithConfig: %v", err)
	}
	if !obs.Running {
		t.Fatalf("obs.Running = false, want true for %q when runtime session is live", target)
	}
}

func TestWorkerKillSessionTargetWithConfigResolvesRuntimeSessionMeta(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, store, sp, cfg)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Probe", Command: "stub", WorkDir: t.TempDir(), Provider: "stub", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := workerKillSessionTargetWithConfig(cityDir, store, sp, cfg, info.SessionName); err != nil {
		t.Fatalf("workerKillSessionTargetWithConfig: %v", err)
	}
	last := sp.Calls[len(sp.Calls)-1]
	if last.Method != "Stop" || last.Name != info.SessionName {
		t.Fatalf("last runtime call = %#v, want Stop %q", last, info.SessionName)
	}
}

func TestWorkerDeliveryIntentForSubmitIntent(t *testing.T) {
	tests := []struct {
		name   string
		intent session.SubmitIntent
		want   worker.DeliveryIntent
	}{
		{name: "default", intent: session.SubmitIntentDefault, want: worker.DeliveryIntentDefault},
		{name: "follow up", intent: session.SubmitIntentFollowUp, want: worker.DeliveryIntentFollowUp},
		{name: "interrupt now", intent: session.SubmitIntentInterruptNow, want: worker.DeliveryIntentInterruptNow},
		{name: "empty defaults", intent: "", want: worker.DeliveryIntentDefault},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workerDeliveryIntentForSubmitIntent(tt.intent); got != tt.want {
				t.Fatalf("workerDeliveryIntentForSubmitIntent(%q) = %q, want %q", tt.intent, got, tt.want)
			}
		})
	}
}

func TestWorkerNudgeDeliveryForMode(t *testing.T) {
	tests := []struct {
		name string
		mode nudgeDeliveryMode
		want worker.NudgeDelivery
		ok   bool
	}{
		{name: "immediate", mode: nudgeDeliveryImmediate, want: worker.NudgeDeliveryImmediate, ok: true},
		{name: "wait idle", mode: nudgeDeliveryWaitIdle, want: worker.NudgeDeliveryWaitIdle, ok: true},
		{name: "queue", mode: nudgeDeliveryQueue, want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := workerNudgeDeliveryForMode(tt.mode)
			if ok != tt.ok {
				t.Fatalf("workerNudgeDeliveryForMode(%q) ok = %v, want %v", tt.mode, ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("workerNudgeDeliveryForMode(%q) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

func TestResolvedWorkerSessionConfigWithConfigFallsBackToResolvedProviderNameForCommand(t *testing.T) {
	cfg, err := resolvedWorkerSessionConfigWithConfig(
		"",
		"",
		"",
		"/tmp/work",
		"worker",
		"",
		"worker",
		"Worker",
		"",
		&config.ResolvedProvider{
			Name: "custom-provider",
		},
		map[string]string{"session_origin": "test"},
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig: %v", err)
	}
	if got, want := cfg.Runtime.Command, "custom-provider"; got != want {
		t.Fatalf("Runtime.Command = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Provider, "custom-provider"; got != want {
		t.Fatalf("Runtime.Provider = %q, want %q", got, want)
	}
}

func TestResolvedWorkerSessionConfigWithConfigFallsBackToProviderArgForCommand(t *testing.T) {
	cfg, err := resolvedWorkerSessionConfigWithConfig(
		"",
		"",
		"legacy-provider",
		"/tmp/work",
		"worker",
		"",
		"worker",
		"Worker",
		"",
		&config.ResolvedProvider{},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig: %v", err)
	}
	if got, want := cfg.Runtime.Command, "legacy-provider"; got != want {
		t.Fatalf("Runtime.Command = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Provider, "legacy-provider"; got != want {
		t.Fatalf("Runtime.Provider = %q, want %q", got, want)
	}
}

func TestResolvedWorkerSessionConfigWithConfigPersistsStoredMCPMetadata(t *testing.T) {
	cfg, err := resolvedWorkerSessionConfigWithConfig(
		"",
		"",
		"legacy-provider",
		"/tmp/work",
		"worker",
		"",
		"worker",
		"Worker",
		"acp",
		&config.ResolvedProvider{
			Name: "custom-provider",
		},
		map[string]string{
			"session_origin": "test",
			"agent_name":     "myrig/worker-adhoc-123",
		},
		[]runtime.MCPServerConfig{{
			Name:      "filesystem",
			Transport: runtime.MCPTransportStdio,
			Command:   "/bin/mcp",
			Args:      []string{"--stdio"},
		}},
	)
	if err != nil {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig: %v", err)
	}
	if got, want := cfg.Metadata[session.MCPIdentityMetadataKey], "myrig/worker-adhoc-123"; got != want {
		t.Fatalf("Metadata[mcp_identity] = %q, want %q", got, want)
	}
	if got := cfg.Metadata[session.MCPServersSnapshotMetadataKey]; got == "" {
		t.Fatal("Metadata[mcp_servers_snapshot] = empty, want persisted snapshot")
	}
}

func TestResolvedWorkerSessionConfigWithConfigSkipsStoredMCPMetadataForTmuxTransport(t *testing.T) {
	cfg, err := resolvedWorkerSessionConfigWithConfig(
		"",
		"",
		"legacy-provider",
		"/tmp/work",
		"worker",
		"",
		"worker",
		"Worker",
		"",
		&config.ResolvedProvider{
			Name: "custom-provider",
		},
		map[string]string{
			"session_origin": "test",
			"agent_name":     "myrig/worker-adhoc-123",
		},
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig: %v", err)
	}
	if got := cfg.Metadata[session.MCPIdentityMetadataKey]; got != "" {
		t.Fatalf("Metadata[mcp_identity] = %q, want empty for tmux transport", got)
	}
	if got := cfg.Metadata[session.MCPServersSnapshotMetadataKey]; got != "" {
		t.Fatalf("Metadata[mcp_servers_snapshot] = %q, want empty for tmux transport", got)
	}
}

func TestResolvedWorkerRuntimeWithConfigFallsBackToCityPathAndSyncsHintsWorkDir(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
ready_prompt_prefix = "stub-ready>"
ready_delay_ms = 250
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	runtimeCfg, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := runtimeCfg.WorkDir, cityDir; got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.WorkDir, cityDir; got != want {
		t.Fatalf("Hints.WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, "stub"; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Command, "/bin/echo"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

// TestResolvedWorkerRuntimeWithConfigPopulatesSessionLiveOnResume guards the
// ga-vtkhi fix: the `gc session attach` resume path must carry the agent's
// session_live commands (with templates expanded) into Hints.SessionLive so
// the recreated tmux runtime re-applies the status-bar/keybinding theme the
// same way reconciler-started sessions do.
func TestResolvedWorkerRuntimeWithConfigPopulatesSessionLiveOnResume(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, fmt.Sprintf(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[rigs]]
name = "myrig"
path = %q

[[agent]]
name = "worker"
provider = "stub"
session_live = ["theme apply {{.Session}}"]

[[agent]]
name = "rig-worker"
dir = "myrig"
provider = "stub"
session_live = ["theme rig={{.Rig}} root={{.RigRoot}} base={{.AgentBase}} city={{.CityName}} work={{.WorkDir}} config={{.ConfigDir}}"]

[[agent]]
name = "polecat"
dir = "myrig"
provider = "stub"
session_live = ["theme agent={{.Agent}} base={{.AgentBase}} rig={{.Rig}} root={{.RigRoot}}"]

[[agent]]
name = "plain"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`, rigDir))

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	tests := []struct {
		name string
		info session.Info
		want []string
	}{
		{
			name: "session name template",
			info: session.Info{
				Template:    "worker",
				AgentName:   "worker",
				SessionName: "test-city__worker",
			},
			want: []string{"theme apply test-city__worker"},
		},
		{
			name: "rig template context",
			info: session.Info{
				Template:    "myrig/rig-worker",
				AgentName:   "myrig/rig-worker",
				SessionName: "test-city__myrig__rig-worker",
				WorkDir:     filepath.Join(rigDir, "agents", "rig-worker"),
			},
			want: []string{"theme rig=myrig root=" + rigDir + " base=rig-worker city=test-city work=" + filepath.Join(rigDir, "agents", "rig-worker") + " config=" + cityDir},
		},
		{
			name: "pool instance uses concrete agent name",
			info: session.Info{
				Template:    "myrig/polecat",
				AgentName:   "myrig/polecat__furiosa-1",
				SessionName: "test-city__myrig__polecat__furiosa-1",
			},
			want: []string{"theme agent=myrig/polecat__furiosa-1 base=polecat__furiosa-1 rig=myrig root=" + rigDir},
		},
		{
			name: "missing session live stays empty",
			info: session.Info{
				Template:    "plain",
				AgentName:   "plain",
				SessionName: "test-city__plain",
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtimeCfg, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, tt.info, "")
			if err != nil {
				t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
			}
			if runtimeCfg == nil {
				t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
			}
			if got, want := runtimeCfg.Hints.SessionLive, tt.want; !slicesEqual(got, want) {
				t.Fatalf("Hints.SessionLive = %v, want %v", got, want)
			}
		})
	}
}

func TestResolvedWorkerRuntimeWithConfigIgnoresMCPResolutionErrorForACPResume(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"
session = "acp"

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)
	writeCatalogFile(t, cityDir, "mcp/filesystem.toml", `
name = "filesystem"
command = [broken
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template:  "worker",
		Transport: "acp",
		WorkDir:   cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := resolved.Command, "/bin/echo acp"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if len(resolved.Hints.MCPServers) != 0 {
		t.Fatalf("Hints.MCPServers len = %d, want 0", len(resolved.Hints.MCPServers))
	}
}

func TestResolvedWorkerRuntimeWithConfigIgnoresMCPResolutionErrorWithoutACPTransport(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`)
	writeCatalogFile(t, cityDir, "mcp/filesystem.toml", `
name = "filesystem"
command = [broken
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := resolved.Command, "/bin/echo"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if len(resolved.Hints.MCPServers) != 0 {
		t.Fatalf("Hints.MCPServers len = %d, want 0", len(resolved.Hints.MCPServers))
	}
}

func TestResolvedWorkerRuntimeWithConfigUsesStoredAgentNameForResumeMCPMaterialization(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "ant"
dir = "myrig"
provider = "stub"
session = "acp"
work_dir = ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}"
min_active_sessions = 0
max_active_sessions = 4

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)
	writeCatalogFile(t, cityDir, "mcp/identity.template.toml", `
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	workDir := filepath.Join(cityDir, ".gc", "worktrees", "myrig", "ants", "ant")
	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template:  "myrig/ant",
		Alias:     "ant",
		AgentName: "myrig/ant-adhoc-123",
		Transport: "acp",
		WorkDir:   workDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if len(resolved.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(resolved.Hints.MCPServers))
	}
	if got, want := resolved.Hints.MCPServers[0].Args[0], "myrig/ant-adhoc-123"; got != want {
		t.Fatalf("Args[0] = %q, want %q", got, want)
	}
	if got, want := resolved.Hints.MCPServers[0].Args[1], workDir; got != want {
		t.Fatalf("Args[1] = %q, want %q", got, want)
	}
	if got, want := resolved.Hints.MCPServers[0].Args[2], "myrig/ant"; got != want {
		t.Fatalf("Args[2] = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigFallsBackToStoredMCPServersWhenCatalogBreaks(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "ant"
dir = "myrig"
provider = "stub"
session = "acp"
work_dir = ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}"
min_active_sessions = 0
max_active_sessions = 4

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)
	writeCatalogFile(t, cityDir, "mcp/identity.template.toml", `
name = "identity"
command = [broken
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	workDir := filepath.Join(cityDir, ".gc", "worktrees", "myrig", "ants", "ant")
	metadata, err := session.WithStoredMCPMetadata(nil, "myrig/ant-adhoc-123", []runtime.MCPServerConfig{{
		Name:      "identity",
		Transport: runtime.MCPTransportStdio,
		Command:   "/bin/mcp",
		Args:      []string{"myrig/ant-adhoc-123", workDir, "myrig/ant"},
	}})
	if err != nil {
		t.Fatalf("WithStoredMCPMetadata: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, session.Info{
		Template:  "myrig/ant",
		Alias:     "ant",
		AgentName: "myrig/ant-adhoc-123",
		Transport: "acp",
		WorkDir:   workDir,
	}, "", metadata)
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfigAndMetadata: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfigAndMetadata() = nil")
	}
	if len(resolved.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(resolved.Hints.MCPServers))
	}
	if got, want := resolved.Hints.MCPServers[0].Args[0], "myrig/ant-adhoc-123"; got != want {
		t.Fatalf("Args[0] = %q, want %q", got, want)
	}
	if got, want := resolved.Hints.MCPServers[0].Args[1], workDir; got != want {
		t.Fatalf("Args[1] = %q, want %q", got, want)
	}
	if got, want := resolved.Hints.MCPServers[0].Args[2], "myrig/ant"; got != want {
		t.Fatalf("Args[2] = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigFallsBackToRuntimeMCPServersSnapshotWhenCatalogBreaks(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "ant"
dir = "myrig"
provider = "stub"
session = "acp"
work_dir = ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}"
min_active_sessions = 0
max_active_sessions = 4

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)
	writeCatalogFile(t, cityDir, "mcp/identity.template.toml", `
name = "identity"
command = [broken
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	workDir := filepath.Join(cityDir, ".gc", "worktrees", "myrig", "ants", "ant")
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
	if err := session.PersistRuntimeMCPServersSnapshot(cityDir, "sess-1", servers); err != nil {
		t.Fatalf("PersistRuntimeMCPServersSnapshot: %v", err)
	}

	resolved, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, session.Info{
		ID:        "sess-1",
		Template:  "myrig/ant",
		Alias:     "ant",
		AgentName: "myrig/ant-adhoc-123",
		Transport: "acp",
		WorkDir:   workDir,
	}, "", metadata)
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfigAndMetadata: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfigAndMetadata() = nil")
	}
	if len(resolved.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(resolved.Hints.MCPServers))
	}
	if got, want := resolved.Hints.MCPServers[0].Args[1], "super-secret"; got != want {
		t.Fatalf("Args[1] = %q, want %q", got, want)
	}
	if got, want := resolved.Hints.MCPServers[0].Env["API_TOKEN"], "super-secret"; got != want {
		t.Fatalf("Env[API_TOKEN] = %q, want %q", got, want)
	}
	if got, want := resolved.Hints.MCPServers[0].Headers["Authorization"], "Bearer secret"; got != want {
		t.Fatalf("Headers[Authorization] = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigFallsBackToSanitizedStoredMCPServersWhenRuntimeSnapshotMissing(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "ant"
dir = "myrig"
provider = "stub"
session = "acp"
work_dir = ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}"
min_active_sessions = 0
max_active_sessions = 4

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`)
	writeCatalogFile(t, cityDir, "mcp/identity.template.toml", `
name = "identity"
command = [broken
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	workDir := filepath.Join(cityDir, ".gc", "worktrees", "myrig", "ants", "ant")
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

	resolved, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, session.Info{
		ID:        "sess-1",
		Template:  "myrig/ant",
		Alias:     "ant",
		AgentName: "myrig/ant-adhoc-123",
		Transport: "acp",
		WorkDir:   workDir,
	}, "", metadata)
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfigAndMetadata: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfigAndMetadata() = nil")
	}
	if len(resolved.Hints.MCPServers) != 1 {
		t.Fatalf("Hints.MCPServers len = %d, want 1", len(resolved.Hints.MCPServers))
	}
	if got, want := resolved.Hints.MCPServers[0].Args, []string{"--serve"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("Args = %#v, want %#v", got, want)
	}
	if len(resolved.Hints.MCPServers[0].Env) != 0 {
		t.Fatalf("Env = %#v, want empty", resolved.Hints.MCPServers[0].Env)
	}
	if len(resolved.Hints.MCPServers[0].Headers) != 0 {
		t.Fatalf("Headers = %#v, want empty", resolved.Hints.MCPServers[0].Headers)
	}
	if got, want := resolved.Hints.MCPServers[0].URL, "https://example.invalid/mcp"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestWorkerSessionRuntimeResolverWithConfigFallsBackToProviderNameWhenResolvedCommandMissing(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "stub",
		}},
		Providers: map[string]config.ProviderSpec{
			"stub": {},
		},
	}

	resolver := workerSessionRuntimeResolverWithConfig(t.TempDir(), cfg)
	if resolver == nil {
		t.Fatal("workerSessionRuntimeResolverWithConfig() = nil")
	}

	runtimeCfg, err := resolver(session.Info{Template: "worker"}, "", nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolver() = nil")
	}
	if got, want := runtimeCfg.Command, "stub"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, "stub"; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
}

func TestWorkerSessionRuntimeResolverWithConfigFallsBackToPersistedRuntimeOnIncompleteResolvedConfig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "stub",
		}},
		Providers: map[string]config.ProviderSpec{
			"stub": {
				ReadyPromptPrefix: "resolved-ready>",
				ReadyDelayMs:      321,
			},
		},
	}

	resolver := workerSessionRuntimeResolverWithConfig(t.TempDir(), cfg)
	if resolver == nil {
		t.Fatal("workerSessionRuntimeResolverWithConfig() = nil")
	}

	info := session.Info{
		Template:      "worker",
		Command:       "persisted-worker --dangerously-skip-permissions",
		Provider:      "persisted-provider",
		WorkDir:       "/tmp/persisted-workdir",
		ResumeFlag:    "--resume-persisted",
		ResumeStyle:   "subcommand",
		ResumeCommand: "persisted resume {{.SessionKey}}",
	}

	runtimeCfg, err := resolver(info, "", nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolver() = nil")
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

func TestWorkerSessionRuntimeResolverWithConfigFallsBackToPersistedProviderWhenCommandMissing(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "resolved-provider",
		}},
		Providers: map[string]config.ProviderSpec{
			"resolved-provider": {
				ReadyPromptPrefix: "resolved-ready>",
			},
		},
	}

	resolver := workerSessionRuntimeResolverWithConfig(t.TempDir(), cfg)
	if resolver == nil {
		t.Fatal("workerSessionRuntimeResolverWithConfig() = nil")
	}

	info := session.Info{
		Template: "worker",
		Provider: "persisted-provider",
	}

	runtimeCfg, err := resolver(info, "", nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolver() = nil")
	}
	if got, want := runtimeCfg.Command, info.Provider; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, info.Provider; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
}

// TestWorkerSessionCreateHintsEnablesMouse locks ga-c4w finding #1 for the
// UNMANAGED `gc session new` direct-start path (controller down): the CLI builds
// its runtime hints via workerSessionCreateHints, NOT the internal/api
// sessionCreateHints seam the original fix patched. Without MouseOn here the
// wheel→scrollback feature never reaches `gc session new` when the city runs
// unmanaged. Pool/headless agents never use this function — they resolve MouseOn
// via the reconciler's templateParamsToConfig (guarded by
// TestResolveTemplateHeadlessAgentStaysMouseOff), so this stays poll-safe.
func TestWorkerSessionCreateHintsEnablesMouse(t *testing.T) {
	hints := workerSessionCreateHints(&config.ResolvedProvider{Name: "stub"})
	if !hints.MouseOn {
		t.Error("workerSessionCreateHints().MouseOn = false, want true (gc session new unmanaged-direct wheel→scrollback, ga-c4w)")
	}
}

// piVllmRigCity builds an in-memory city with a single rig-scoped agent
// "myrig/polecat" running a custom provider whose base = "builtin:pi", plus a
// rig overlay dir carrying the per-provider/pi/ hooks. It mirrors the real
// pi-vllm hybrid shape used in gc-6bw8o. The qualified template name
// ("myrig/polecat") is the identity the resume path persists for a rig agent.
func piVllmRigCity(t *testing.T) (cityDir, overlayDir string, cfg *config.City) {
	t.Helper()
	cityDir = t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	overlayDir = filepath.Join(cityDir, "packs", "myrig", "overlay")
	// per-provider/pi/.pi/extensions/gc-hooks.js is the slot OverlayProviderNames
	// must resolve to for the harness to stage its ready-signal hook.
	if err := os.MkdirAll(filepath.Join(overlayDir, "per-provider", "pi", ".pi", "extensions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "per-provider", "pi", ".pi", "extensions", "gc-hooks.js"), []byte("// hook"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := "builtin:pi"
	cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "polecat",
			Provider:          "pi-vllm",
			Scope:             "rig",
			Dir:               "myrig",
			InstallAgentHooks: []string{"pi"}, // declares the per-provider/pi/ overlay slot
		}},
		Providers: map[string]config.ProviderSpec{
			// Explicit command so resolution does not depend on a real `pi`
			// binary in the test host PATH; base still yields BuiltinAncestor=pi.
			"pi-vllm": {Base: &base, Command: "/bin/echo"},
		},
		Rigs:           []config.Rig{{Name: "myrig", Path: filepath.Join(cityDir, "myrig")}},
		RigOverlayDirs: map[string][]string{"myrig": {overlayDir}},
	}
	return cityDir, overlayDir, cfg
}

// TestResolvedWorkerRuntimeStagesProviderOverlayForRigBasePiProvider is the
// regression test for gc-6bw8o. The worker resume resolver
// (resolvedWorkerRuntimeWithConfigAndMetadata) builds runtime.Config directly
// and never routes through resolveTemplate, so before the fix it left
// ProviderOverlayName/PackOverlayDirs empty. OverlayProviderNames then fell back
// to ProviderName="" and the per-provider/pi/ hooks (gc-hooks.js) never staged,
// the harness never signaled ready, and the controller churned into a
// fall-back-to-claude loop. The fix populates these via applyWorkerOverlayHints,
// mirroring the reconciler create path's sourcing in template_resolve.go.
func TestResolvedWorkerRuntimeStagesProviderOverlayForRigBasePiProvider(t *testing.T) {
	cityDir, overlayDir, cfg := piVllmRigCity(t)

	runtimeCfg, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "myrig/polecat",
		Command:  "pi-vllm",
		WorkDir:  cityDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}

	// Concrete provider name drives the per-provider overlay slot.
	if got := strings.TrimSpace(runtimeCfg.Hints.ProviderOverlayName); got != "pi-vllm" {
		t.Fatalf("resume Hints.ProviderOverlayName = %q, want %q (concrete provider name)", got, "pi-vllm")
	}
	// Launch family is the base (pi), mirroring the reconciler create path.
	if got := strings.TrimSpace(runtimeCfg.Hints.ProviderName); got != "pi" {
		t.Fatalf("resume Hints.ProviderName = %q, want %q (launch family)", got, "pi")
	}
	// Rig overlay dir must be staged so per-provider/pi/ hooks reach the workdir.
	if len(runtimeCfg.Hints.PackOverlayDirs) == 0 {
		t.Fatalf("resume Hints.PackOverlayDirs is empty, want the rig overlay dir %q", overlayDir)
	}
	foundOverlay := false
	for _, od := range runtimeCfg.Hints.PackOverlayDirs {
		if od == overlayDir {
			foundOverlay = true
		}
	}
	if !foundOverlay {
		t.Fatalf("resume Hints.PackOverlayDirs = %v, want to include rig overlay dir %q", runtimeCfg.Hints.PackOverlayDirs, overlayDir)
	}
	// Sanity: the effective overlay slot list must be non-empty, otherwise
	// StageProviderOverlayDir would copy no per-provider/<slot>/ content.
	slots := runtime.OverlayProviderNames(runtimeCfg.Hints)
	if len(slots) == 0 {
		t.Fatal("runtime.OverlayProviderNames(resume Hints) is empty; per-provider overlay would never stage")
	}

	// End-to-end proof of the gc-6bw8o regression: actually stage the workdir
	// and confirm the per-provider/pi/ hook lands. Stage into a fresh temp
	// workdir rather than the city dir so the assertion is unambiguous.
	stageCfg := runtimeCfg.Hints
	stageCfg.WorkDir = t.TempDir()
	if err := runtime.StageSessionWorkDir(stageCfg); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	hookPath := filepath.Join(stageCfg.WorkDir, ".pi", "extensions", "gc-hooks.js")
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatalf("per-provider/pi hook not staged at %s: %v", hookPath, err)
	}
}

// TestResolvedWorkerSessionConfigStagesProviderOverlayForRigBasePiProvider
// covers the CLI create path for the same gc-6bw8o rig pi-vllm agent. It first
// documents the gap (resolvedWorkerSessionConfigWithConfig, which only sees
// `resolved`, leaves the overlay fields empty), then asserts that
// applyWorkerOverlayHints — the exact call
// newWorkerSessionHandleForResolvedRuntimeWithConfig makes onto
// sessionCfg.Runtime.Hints before handing the spec to the worker factory —
// populates them so the per-provider/pi/ hooks would stage. The end-to-end
// create wiring is hard to assert (the resulting worker.Handle does not expose
// its Hints); the resume-path test above is the integration regression proof.
func TestResolvedWorkerSessionConfigStagesProviderOverlayForRigBasePiProvider(t *testing.T) {
	cityDir, overlayDir, cfg := piVllmRigCity(t)

	resolved, _ := resolveWorkerRuntimeProviderWithConfigAndMetadata(cfg, session.Info{Template: "myrig/polecat"}, "", nil)
	if resolved == nil {
		t.Fatal("resolveWorkerRuntimeProviderWithConfigAndMetadata() = nil")
	}

	sessionCfg, err := resolvedWorkerSessionConfigWithConfig(
		cityDir,
		resolved.CommandString(),
		"pi-vllm",
		cityDir,
		"myrig/polecat",
		"",
		"myrig/polecat",
		"polecat",
		"",
		resolved,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig: %v", err)
	}
	// The bare builder must NOT set overlay fields — that is the gap the create
	// call site closes. If this ever starts populating them on its own, the
	// create-path applyWorkerOverlayHints call is redundant and this guard flags it.
	if got := strings.TrimSpace(sessionCfg.Runtime.Hints.ProviderOverlayName); got != "" {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig set ProviderOverlayName = %q on its own, expected empty (overlay is caller-applied)", got)
	}

	// Apply the overlay hints exactly as
	// newWorkerSessionHandleForResolvedRuntimeWithConfig does before the factory call.
	applyWorkerOverlayHints(&sessionCfg.Runtime.Hints, cfg, cityDir, "myrig/polecat", resolved)

	if got := strings.TrimSpace(sessionCfg.Runtime.Hints.ProviderOverlayName); got != "pi-vllm" {
		t.Fatalf("create Hints.ProviderOverlayName = %q, want %q", got, "pi-vllm")
	}
	if len(sessionCfg.Runtime.Hints.PackOverlayDirs) == 0 {
		t.Fatalf("create Hints.PackOverlayDirs is empty, want rig overlay dir %q", overlayDir)
	}
	if slots := runtime.OverlayProviderNames(sessionCfg.Runtime.Hints); len(slots) == 0 {
		t.Fatal("runtime.OverlayProviderNames(create Hints) is empty; per-provider overlay would never stage")
	}
}
