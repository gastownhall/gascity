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
	info, err := mgr.CreateBeadOnly("worker", "Probe", "", t.TempDir(), "stub", "", nil, session.ProviderResume{
		SessionIDFlag: "--old-session-id",
	})
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
				Command:   "/bin/echo",
				PathCheck: "true",
				OptionsSchema: []config.ProviderOption{{
					Key:  "effort",
					Type: "select",
					Choices: []config.OptionChoice{{
						Value:    "high",
						FlagArgs: []string{"--effort", "high"},
					}},
				}},
			},
		},
	}

	resolved, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, session.Info{
		Template: "myrig/worker",
		Command:  "/bin/echo",
		WorkDir:  cityDir,
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
	info, err := mgr.Create(
		context.Background(),
		"worker",
		"Probe",
		"legacy-agent",
		t.TempDir(),
		"stub",
		nil,
		session.ProviderResume{
			ResumeFlag:    "--old-resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		runtime.Config{},
	)
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
	info, err := mgr.Create(
		context.Background(),
		"worker",
		"Probe",
		"",
		t.TempDir(),
		"stub",
		nil,
		session.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"},
		runtime.Config{},
	)
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
	info, err := mgr.Create(context.Background(), "worker", "Probe", "/bin/echo", t.TempDir(), "stub", nil, session.ProviderResume{}, runtime.Config{Command: "/bin/echo"})
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
	info, err := mgr.Create(context.Background(), "worker", "Probe", "stub", t.TempDir(), "stub", nil, session.ProviderResume{}, runtime.Config{})
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
