package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInternalProjectMCPProjectsGeminiConfigWithIdentityExpansion(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	cityToml := `[workspace]
name = "test-city"
provider = "gemini"

[beads]
provider = "file"

[providers.gemini]
command = "echo"
prompt_mode = "none"

[[agent]]
name = "mayor"
provider = "gemini"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
	writeMCPSource(t, filepath.Join(cityDir, "mcp", "notes.template.toml"), `
name = "notes"
command = "uvx"
args = ["notes-mcp"]

[env]
AGENT = "{{.AgentName}}"
`)

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "project-mcp",
		"--agent", "mayor",
		"--identity", "rig/mayor-2",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	data, err := os.ReadFile(filepath.Join(workdir, ".gemini", "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile(settings.json): %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal settings.json: %v", err)
	}
	mcpServers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing: %+v", doc)
	}
	notes, ok := mcpServers["notes"].(map[string]any)
	if !ok {
		t.Fatalf("notes server missing: %+v", mcpServers)
	}
	env, ok := notes["env"].(map[string]any)
	if !ok {
		t.Fatalf("notes env missing: %+v", notes)
	}
	if got := env["AGENT"]; got != "rig/mayor-2" {
		t.Fatalf("env.AGENT = %v, want rig/mayor-2", got)
	}
	if !strings.Contains(stdout.String(), "projected 1 MCP server") {
		t.Fatalf("stdout missing projection summary: %q", stdout.String())
	}

	gitignore, err := os.ReadFile(filepath.Join(workdir, ".gitignore"))
	if err != nil {
		t.Fatalf("ReadFile(.gitignore): %v", err)
	}
	for _, want := range managedMCPGitignoreEntries {
		if !strings.Contains(string(gitignore), want) {
			t.Fatalf(".gitignore missing %q:\n%s", want, string(gitignore))
		}
	}
}

func TestInternalProjectMCPMissingFlags(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"internal", "project-mcp", "--workdir", t.TempDir()}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected missing-agent failure, stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--agent is required") {
		t.Fatalf("stderr missing --agent error: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"internal", "project-mcp", "--agent", "mayor"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected missing-workdir failure, stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--workdir is required") {
		t.Fatalf("stderr missing --workdir error: %q", stderr.String())
	}
}
