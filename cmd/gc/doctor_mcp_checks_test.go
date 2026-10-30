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
command = "npx"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	_, gateErr := buildStage1MCPTargets(cityDir, cfg, stubLookPath, io.Discard)
	if gateErr == nil {
		t.Fatal("buildStage1MCPTargets accepted a city whose agent-local catalog conflicts with the implicit provider agent")
	}
	result := newMCPSharedTargetDoctorCheck(cityDir, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true})
	if result.Status != doctor.StatusError {
		t.Fatalf("mcp-shared-target = %v (%q; %v), want error: gc start rejects this city", result.Status, result.Message, result.Details)
	}
	joined := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"mayor",
		`(the implicit agent for provider "claude")`,
		filepath.Join("agents", "mayor", "mcp"),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("details %q missing %q", joined, want)
		}
	}
}

func TestMCPSharedTargetDoctorCheckAgreesWithStage1Gate(t *testing.T) {
	// mcp-shared-target must flag a stage-1 conflict exactly when the start
	// gate rejects the city: each row asserts the two verdicts match.
	rows := []struct {
		name string
		cfg  func(t *testing.T, cityPath string) *config.City
	}{
		{
			name: "shared city catalog keeps implicit and explicit agents OK",
			cfg: func(t *testing.T, cityPath string) *config.City {
				writeMCPSource(t, filepath.Join(cityPath, "mcp", "kb.toml"), `
name = "kb"
command = "uvx"
`)
				return &config.City{
					PackMCPDir: filepath.Join(cityPath, "mcp"),
					Session:    config.SessionConfig{Provider: "tmux"},
					Providers:  builtinProviderAliasesForTest("claude"),
					Agents: []config.Agent{
						{Name: "mayor", Scope: "city", Provider: "claude"},
						{Name: "claude", Scope: "city", Provider: "claude", Implicit: true},
					},
				}
			},
		},
		{
			name: "agent-local catalog against implicit agent fails both",
			cfg: func(t *testing.T, cityPath string) *config.City {
				agentLocal := filepath.Join(cityPath, "agents", "mayor", "mcp", "notes.toml")
				writeMCPSource(t, agentLocal, `
name = "notes"
command = "uvx"
`)
				return &config.City{
					Session:   config.SessionConfig{Provider: "tmux"},
					Providers: builtinProviderAliasesForTest("claude"),
					Agents: []config.Agent{
						{Name: "mayor", Scope: "city", Provider: "claude", MCPDir: filepath.Dir(agentLocal)},
						{Name: "claude", Scope: "city", Provider: "claude", Implicit: true},
					},
				}
			},
		},
		{
			name: "agent-local catalog against empty-catalog peer fails both",
			cfg: func(t *testing.T, cityPath string) *config.City {
				agentLocal := filepath.Join(cityPath, "agents", "mayor", "mcp", "notes.toml")
				writeMCPSource(t, agentLocal, `
name = "notes"
command = "uvx"
`)
				return &config.City{
					Session:   config.SessionConfig{Provider: "tmux"},
					Providers: builtinProviderAliasesForTest("claude"),
					Agents: []config.Agent{
						{Name: "mayor", Scope: "city", Provider: "claude", MCPDir: filepath.Dir(agentLocal)},
						{Name: "deputy", Scope: "city", Provider: "claude"},
					},
				}
			},
		},
		{
			name: "acp session shares no target with a stage-1 agent",
			cfg: func(t *testing.T, cityPath string) *config.City {
				writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
				writeMCPSource(t, filepath.Join(cityPath, "agents", "relay", "mcp", "private.toml"), `
name = "private"
command = "private-mcp"
`)
				acp := true
				providers := builtinProviderAliasesForTest("claude")
				providers["relay"] = config.ProviderSpec{Base: ptrString("builtin:claude"), Command: "echo", PromptMode: "none", SupportsACP: &acp, ACPCommand: "echo"}
				return &config.City{
					PackMCPDir: filepath.Join(cityPath, "mcp"),
					Session:    config.SessionConfig{Provider: "tmux"},
					Providers:  providers,
					Agents: []config.Agent{
						{Name: "mayor", Scope: "city", Provider: "claude"},
						{Name: "relay", Scope: "city", Provider: "relay", Session: "acp", MCPDir: filepath.Join(cityPath, "agents", "relay", "mcp")},
					},
				}
			},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := row.cfg(t, cityPath)
			_, gateErr := buildStage1MCPTargets(cityPath, cfg, stubLookPath, io.Discard)
			result := newMCPSharedTargetDoctorCheck(cityPath, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityPath})
			if gateRejected := gateErr != nil; gateRejected != (result.Status == doctor.StatusError) {
				t.Fatalf("start gate rejected = %v (%v), doctor status = %v (%q; %v); the two must agree",
					gateRejected, gateErr, result.Status, result.Message, result.Details)
			}
		})
	}
}

func TestMCPConflictMessagesStayGenericForTemplateDivergence(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.template.toml"), `
name = "notes"
command = "uvx"
args = ["{{.AgentName}}"]
`)

	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "deputy", Scope: "city", Provider: "claude"},
		},
	}

	_, err := buildStage1MCPTargets(cityPath, cfg, stubLookPath, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "MCP target conflict") {
		t.Fatalf("buildStage1MCPTargets err = %v, want MCP target conflict", err)
	}
	if strings.Contains(err.Error(), "mcp/") || strings.Contains(err.Error(), "agent-local") {
		t.Fatalf("template-only divergence must keep the generic message, got: %v", err)
	}

	result := newMCPSharedTargetDoctorCheck(cityPath, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityPath})
	if result.Status != doctor.StatusError {
		t.Fatalf("mcp-shared-target = %v (%q; %v), want error", result.Status, result.Message, result.Details)
	}
	joined := strings.Join(result.Details, "\n")
	if strings.Contains(joined, "mcp/") || strings.Contains(joined, "agent-local") {
		t.Fatalf("template-only divergence must keep the generic detail, got: %s", joined)
	}
}

func TestMCPSharedTargetDoctorCheckReportsStage2WorkDirConflict(t *testing.T) {
	cityPath := t.TempDir()
	alphaMCP := filepath.Join(cityPath, "agents", "alpha", "mcp", "notes.toml")
	betaMCP := filepath.Join(cityPath, "agents", "beta", "mcp", "notes.toml")
	writeMCPSource(t, alphaMCP, `
name = "notes"
command = "uvx"
args = ["alpha-notes"]
`)
	writeMCPSource(t, betaMCP, `
name = "notes"
command = "uvx"
args = ["beta-notes"]
`)

	shared := ".gc/worktrees/shared"
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "gemini"},
		Providers: map[string]config.ProviderSpec{
			"gemini": {Command: "echo", PromptMode: "none"},
		},
		Session: config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "alpha", Scope: "city", Provider: "gemini", WorkDir: shared, MaxActiveSessions: intPtr(2), MCPDir: filepath.Dir(alphaMCP)},
			{Name: "beta", Scope: "city", Provider: "gemini", WorkDir: shared, MaxActiveSessions: intPtr(2), MCPDir: filepath.Dir(betaMCP)},
		},
	}

	result := newMCPSharedTargetDoctorCheck(cityPath, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityPath})
	if result.Status != doctor.StatusError {
		t.Fatalf("mcp-shared-target = %v (%q; %v), want error for divergent catalogs in one work dir", result.Status, result.Message, result.Details)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "alpha") || !strings.Contains(joined, "beta") {
		t.Fatalf("details %q want both agents named", joined)
	}
}
