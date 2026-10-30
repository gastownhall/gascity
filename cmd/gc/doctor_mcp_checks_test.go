package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestMCPConfigDoctorCheckReportsTemplateExpansionErrors(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeProjectedMCPCity(t, cityDir, `
[workspace]
provider = "gemini"

[beads]
provider = "file"

[providers.gemini]
command = "echo"
prompt_mode = "none"
`, `
name = "mayor"
provider = "gemini"
scope = "city"
`)

	writeCatalogFile(t, cityDir, "mcp/remote.template.toml", `
name = "remote"
url = "https://example.com/{{.Missing}}"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	result := newMCPConfigDoctorCheck(cityDir, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error", result.Status)
	}
	if !strings.Contains(result.Message, "agent ") {
		t.Fatalf("message = %q, want agent context", result.Message)
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "remote.template.toml") {
		t.Fatalf("details = %#v, want template filename", result.Details)
	}
}

func TestMCPConfigDoctorCheckReportsUndeliverableTargets(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeProjectedMCPCity(t, cityDir, `
[workspace]
provider = "gemini"

[beads]
provider = "file"

[session]
provider = "subprocess"

[daemon]
formula_v2 = false

[providers.gemini]
command = "echo"
prompt_mode = "none"
`, `
name = "mayor"
provider = "gemini"
scope = "city"
work_dir = "worktrees/mayor"
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "npx"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	result := newMCPConfigDoctorCheck(cityDir, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error", result.Status)
	}
	if !strings.Contains(result.Message, "cannot be delivered") {
		t.Fatalf("message = %q, want delivery failure", result.Message)
	}
}

func TestMCPSharedTargetDoctorCheckReportsConflicts(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeProjectedMCPCity(t, cityDir, `
[workspace]
provider = "gemini"

[beads]
provider = "file"

[session]
provider = "tmux"

[providers.gemini]
command = "echo"
prompt_mode = "none"
`)
	writeCatalogFile(t, cityDir, "agents/mayor/agent.toml", `
name = "mayor"
provider = "gemini"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "agents/deputy/agent.toml", `
name = "deputy"
provider = "gemini"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "agents/mayor/mcp/notes.toml", `
name = "notes"
command = "npx"
`)
	writeCatalogFile(t, cityDir, "agents/deputy/mcp/notes.toml", `
name = "notes"
url = "https://example.com/deputy"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	result := newMCPSharedTargetDoctorCheck(cityDir, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error", result.Status)
	}
	if len(result.Details) == 0 {
		t.Fatal("expected target-conflict details")
	}
	if !strings.Contains(result.Details[0], "mayor") || !strings.Contains(result.Details[0], "deputy") {
		t.Fatalf("details = %#v, want both agents named", result.Details)
	}
}

// mcp-shared-target counts stage-1 claimants as the start gate does, so doctor
// reports a stage-1 conflict exactly when gc start refuses the city.
func TestMCPSharedTargetDoctorCheckAgreesWithStage1Gate(t *testing.T) {
	claude := map[string]config.ProviderSpec{"claude": {Command: "echo", PromptMode: "none"}}
	acp := true
	acpProviders := builtinProviderAliasesForTest("claude")
	acpProviders["relay"] = config.ProviderSpec{Base: ptrString("builtin:claude"), Command: "echo", PromptMode: "none", SupportsACP: &acp, ACPCommand: "echo"}
	implicit := config.Agent{Name: "claude", Provider: "claude", Implicit: true}

	tests := []struct {
		name     string
		sources  map[string]string
		cfg      func(cityPath string) *config.City
		conflict bool
	}{
		{
			name:    "city catalog shared by implicit and explicit agents",
			sources: map[string]string{"mcp/notes.toml": "name = \"notes\"\ncommand = \"uvx\"\n"},
			cfg: func(cityPath string) *config.City {
				return &config.City{
					PackMCPDir: filepath.Join(cityPath, "mcp"),
					Session:    config.SessionConfig{Provider: "tmux"},
					Providers:  claude,
					Agents:     []config.Agent{{Name: "mayor", Scope: "city", Provider: "claude"}, implicit},
				}
			},
		},
		{
			name:    "agent-local catalog against the implicit agent",
			sources: map[string]string{"agents/mayor/mcp/notes.toml": "name = \"notes\"\ncommand = \"uvx\"\n"},
			cfg: func(cityPath string) *config.City {
				return &config.City{
					Session:   config.SessionConfig{Provider: "tmux"},
					Providers: claude,
					Agents: []config.Agent{
						{Name: "mayor", Scope: "city", Provider: "claude", MCPDir: filepath.Join(cityPath, "agents", "mayor", "mcp")},
						implicit,
					},
				}
			},
			conflict: true,
		},
		{
			name:    "agent-local catalog against an explicit empty-catalog peer",
			sources: map[string]string{"agents/mayor/mcp/notes.toml": "name = \"notes\"\ncommand = \"uvx\"\n"},
			cfg: func(cityPath string) *config.City {
				return &config.City{
					Session:   config.SessionConfig{Provider: "tmux"},
					Providers: builtinProviderAliasesForTest("claude"),
					Agents: []config.Agent{
						{Name: "mayor", Scope: "city", Provider: "claude", MCPDir: filepath.Join(cityPath, "agents", "mayor", "mcp")},
						{Name: "deputy", Scope: "city", Provider: "claude"},
					},
				}
			},
			conflict: true,
		},
		{
			name: "ACP session beside a stage-1 agent",
			sources: map[string]string{
				"mcp/notes.toml":                "name = \"notes\"\ncommand = \"uvx\"\n",
				"agents/relay/mcp/private.toml": "name = \"private\"\ncommand = \"private-mcp\"\n",
			},
			cfg: func(cityPath string) *config.City {
				return &config.City{
					PackMCPDir: filepath.Join(cityPath, "mcp"),
					Session:    config.SessionConfig{Provider: "tmux"},
					Providers:  acpProviders,
					Agents: []config.Agent{
						{Name: "mayor", Scope: "city", Provider: "claude"},
						{Name: "relay", Scope: "city", Provider: "relay", Session: "acp", MCPDir: filepath.Join(cityPath, "agents", "relay", "mcp")},
					},
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			for rel, body := range tt.sources {
				writeMCPSource(t, filepath.Join(cityPath, rel), body)
			}
			cfg := tt.cfg(cityPath)

			_, startErr := buildStage1MCPTargets(cityPath, cfg, stubLookPath, io.Discard)
			result := newMCPSharedTargetDoctorCheck(cityPath, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityPath})
			if (startErr != nil) != tt.conflict {
				t.Fatalf("buildStage1MCPTargets err = %v, want conflict %v", startErr, tt.conflict)
			}
			if (result.Status == doctor.StatusError) != tt.conflict {
				t.Fatalf("mcp-shared-target = %v (%s; %v), want conflict %v to match gc start (err = %v)",
					result.Status, result.Message, result.Details, tt.conflict, startErr)
			}
		})
	}
}

// A city-scoped agent with its own mcp/ catalog conflicts with the implicit
// agent gc injects for its provider at the city root, so gc start refuses the
// city; mcp-shared-target reports it and names the agent-local directory.
func TestMCPSharedTargetDoctorCheckReportsImplicitAgentLocalConflict(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeProjectedMCPCity(t, cityDir, `
[workspace]
provider = "claude"

[beads]
provider = "file"

[session]
provider = "tmux"

[providers.claude]
command = "echo"
prompt_mode = "none"
`, `
name = "mayor"
provider = "claude"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "agents/mayor/mcp/notes.toml", `
name = "notes"
command = "uvx"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	var implicit *config.Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Implicit && cfg.Agents[i].Provider == "claude" {
			implicit = &cfg.Agents[i]
		}
	}
	if implicit == nil {
		t.Fatalf("loadCityConfig injected no implicit claude agent: %+v", cfg.Agents)
	}

	result := newMCPSharedTargetDoctorCheck(cityDir, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v (%q), want error: gc start rejects this city with an MCP target conflict", result.Status, result.Message)
	}
	target := filepath.Join(cityDir, ".mcp.json")
	mcpDir := filepath.Join(cityDir, "agents", "mayor", "mcp")
	for _, detail := range result.Details {
		if strings.Contains(detail, target) && strings.Contains(detail, "mayor") &&
			strings.Contains(detail, mcpAgentLabel(implicit)) && strings.Contains(detail, mcpDir) {
			return
		}
	}
	t.Fatalf("details = %#v, want one naming %s, mayor, %s and %s", result.Details, target, mcpAgentLabel(implicit), mcpDir)
}
