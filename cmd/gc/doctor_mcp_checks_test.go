package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

func TestMCPConfigDoctorCheckReportsTemplateExpansionErrors(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.gemini]
command = "echo"
prompt_mode = "none"

[[agent]]
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
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "subprocess"

[providers.gemini]
command = "echo"
prompt_mode = "none"

[[agent]]
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
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "tmux"

[providers.gemini]
command = "echo"
prompt_mode = "none"

[[agent]]
name = "mayor"
provider = "gemini"
scope = "city"

[[agent]]
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
