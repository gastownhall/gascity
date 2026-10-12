package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// One agent on a harness that cannot receive MCP must not take the whole city
// down: stage-1 projection skips it with a warning and projects every other
// agent. Regression for platform/substrate/trust sitting in init_failed because
// a zcode-family helper agent inherited the city MCP catalog.
func TestRunStage1MCPProjectionSkipsUnsupportedHarnessAgent(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
args = ["notes-mcp"]
`)

	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("gemini", "copilot"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "gemini"},
			{Name: "helper", Scope: "city", Provider: "copilot"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gemini", "settings.json")); err != nil {
		t.Fatalf("supported agent's projection was not written: %v", err)
	}
	warning := stderr.String()
	for _, want := range []string{`"helper"`, `"copilot"`, "notes", "skipping"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("warning missing %q:\n%s", want, warning)
		}
	}

	// The supervisor re-runs stage-1 every tick; the same skip warns once.
	stderr.Reset()
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("second runStage1MCPProjection: %v", err)
	}
	if strings.Contains(stderr.String(), `"helper"`) {
		t.Fatalf("unchanged skip re-warned on the next tick:\n%s", stderr.String())
	}

	// The latch is keyed per (city, agent, harness, servers), not once per
	// process: a newly added unsupported agent still warns while the already
	// reported one stays quiet.
	cfg.Agents = append(cfg.Agents, config.Agent{Name: "helper2", Scope: "city", Provider: "copilot"})
	stderr.Reset()
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("third runStage1MCPProjection: %v", err)
	}
	if !strings.Contains(stderr.String(), `"helper2"`) {
		t.Fatalf("new skipped agent did not warn:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), `"helper"`) {
		t.Fatalf("unchanged skip re-warned alongside the new one:\n%s", stderr.String())
	}
}

func TestUnsupportedMCPHarnessErrorExplainsTheFix(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("copilot"),
	}
	agent := &config.Agent{Name: "helper", Scope: "city", Provider: "copilot"}

	_, _, err := resolveAgentMCPProjection(cityPath, cfg, agent, agent.QualifiedName(), cityPath, "copilot")
	var harnessErr *unsupportedMCPHarnessError
	if !errors.As(err, &harnessErr) {
		t.Fatalf("err = %v, want *unsupportedMCPHarnessError", err)
	}
	msg := err.Error()
	for _, want := range []string{`"copilot"`, "notes", "claude", "codex", "provider"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "effective MCP") {
		t.Fatalf("error still uses internal jargon: %s", msg)
	}
}
