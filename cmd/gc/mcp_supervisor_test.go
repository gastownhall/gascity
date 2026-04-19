package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func writeMCPSource(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stubLookPath(_ string) (string, error) {
	return "/bin/echo", nil
}

func TestRunStage1MCPProjectionCityScoped(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
args = ["notes-mcp"]
`)

	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "gemini"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cityPath, ".gemini", "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile(settings.json): %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal settings.json: %v", err)
	}
	if _, ok := doc["mcpServers"]; !ok {
		t.Fatalf("mcpServers missing from projected gemini settings:\n%s", string(data))
	}

	gitignore, err := os.ReadFile(filepath.Join(cityPath, ".gitignore"))
	if err != nil {
		t.Fatalf("ReadFile(.gitignore): %v", err)
	}
	for _, want := range managedMCPGitignoreEntries {
		if !strings.Contains(string(gitignore), want) {
			t.Fatalf(".gitignore missing %q:\n%s", want, string(gitignore))
		}
	}
}

func TestRunStage1MCPProjectionRemovesStaleManagedTarget(t *testing.T) {
	cityPath := t.TempDir()
	target := filepath.Join(cityPath, ".mcp.json")
	if err := os.WriteFile(target, []byte(`{"mcpServers":{"stale":{"command":"old"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "mcp-managed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "mcp-managed", "claude.json"), []byte(`{"managed_by":"gc"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Session: config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
		},
	}

	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &bytes.Buffer{}); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("stale .mcp.json should be removed, stat err = %v", err)
	}
}

func TestBuildStage1MCPTargetsRejectsConflictingSharedTarget(t *testing.T) {
	cityPath := t.TempDir()
	agentLocal := filepath.Join(cityPath, "agents", "mayor", "mcp", "notes.toml")
	writeMCPSource(t, agentLocal, `
name = "notes"
command = "uvx"
`)

	cfg := &config.City{
		Session: config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude", MCPDir: filepath.Dir(agentLocal)},
			{Name: "deputy", Scope: "city", Provider: "claude"},
		},
	}

	_, err := buildStage1MCPTargets(cityPath, cfg, stubLookPath)
	if err == nil {
		t.Fatal("expected MCP target conflict, got nil")
	}
	if !strings.Contains(err.Error(), "MCP target conflict") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildStage1MCPTargetsSkipsStage2OnlyAgents(t *testing.T) {
	cityPath := t.TempDir()
	mayorMCP := filepath.Join(cityPath, "agents", "mayor", "mcp", "notes.toml")
	deputyMCP := filepath.Join(cityPath, "agents", "deputy", "mcp", "notes.toml")
	writeMCPSource(t, mayorMCP, `
name = "notes"
command = "uvx"
`)
	writeMCPSource(t, deputyMCP, `
name = "notes"
url = "https://example.com/deputy"
`)

	cfg := &config.City{
		Workspace: config.Workspace{Provider: "gemini"},
		Providers: map[string]config.ProviderSpec{
			"gemini": {Command: "echo", PromptMode: "none"},
		},
		Session: config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "gemini", WorkDir: ".gc/worktrees/{{.Agent}}", MaxActiveSessions: intPtr(2), MCPDir: filepath.Dir(mayorMCP)},
			{Name: "deputy", Scope: "city", Provider: "gemini", WorkDir: ".gc/worktrees/{{.Agent}}", MaxActiveSessions: intPtr(2), MCPDir: filepath.Dir(deputyMCP)},
		},
	}

	targets, err := buildStage1MCPTargets(cityPath, cfg, stubLookPath)
	if err != nil {
		t.Fatalf("buildStage1MCPTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("stage2-only agents should not contribute stage1 targets, got %+v", targets)
	}
}
