package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// One agent on a harness that cannot receive MCP must not take the whole city
// down: stage 1 projects every other agent and warns about the one it leaves
// without MCP. Here the servers reach that agent through the city pack, a
// shared layer, so the agent starts without them.
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
	for _, want := range []string{`agent "helper" starts without MCP server "notes"`, `"copilot" harness`, "the city pack"} {
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

	// The latch is keyed per message, not once per process: a newly added
	// agent that skips the same servers changes the line, so it warns once
	// more, naming both.
	cfg.Agents = append(cfg.Agents, config.Agent{Name: "helper2", Scope: "city", Provider: "copilot"})
	stderr.Reset()
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("third runStage1MCPProjection: %v", err)
	}
	if !strings.Contains(stderr.String(), `agents "helper", "helper2" start without MCP server "notes"`) {
		t.Fatalf("new skipped agent did not warn:\n%s", stderr.String())
	}
	stderr.Reset()
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("fourth runStage1MCPProjection: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unchanged skips re-warned:\n%s", stderr.String())
	}
}

// Servers declared in the agent's own mcp/ directory target it explicitly, so
// a harness that cannot receive them stays an error for that agent. Stage 1
// still projects the rest of the city.
func TestRunStage1MCPProjectionSkipsAgentDeclaringServersItsHarnessCannotReceive(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	helperMCP := filepath.Join(cityPath, "agents", "helper", "mcp")
	writeMCPSource(t, filepath.Join(helperMCP, "private.toml"), `
name = "private"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("gemini", "copilot"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "gemini"},
			{Name: "helper", Scope: "city", Provider: "copilot", MCPDir: helperMCP},
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
	for _, want := range []string{`skipping MCP projection for agent "helper"`, "private", `"copilot"`} {
		if !strings.Contains(warning, want) {
			t.Fatalf("warning missing %q:\n%s", want, warning)
		}
	}
	if strings.Contains(warning, "[private, notes]") || strings.Contains(warning, "[notes") {
		t.Fatalf("error names the shared server, want only the agent's own:\n%s", warning)
	}
}

func TestUnsupportedMCPHarnessErrorExplainsTheFix(t *testing.T) {
	cityPath := t.TempDir()
	helperMCP := filepath.Join(cityPath, "agents", "helper", "mcp")
	writeMCPSource(t, filepath.Join(helperMCP, "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		Session:   config.SessionConfig{Provider: "tmux"},
		Providers: builtinProviderAliasesForTest("copilot"),
	}
	agent := &config.Agent{Name: "helper", Scope: "city", Provider: "copilot", MCPDir: helperMCP}

	_, err := resolveAgentMCPProjection(cityPath, cfg, agent, agent.QualifiedName(), cityPath, "copilot")
	var harnessErr *unsupportedMCPHarnessError
	if !errors.As(err, &harnessErr) {
		t.Fatalf("err = %v, want *unsupportedMCPHarnessError", err)
	}
	msg := err.Error()
	for _, want := range []string{`"copilot"`, "notes", "claude", "codex", "provider", "own mcp/ directory"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "effective MCP") {
		t.Fatalf("error still uses internal jargon: %s", msg)
	}
}

// A harness without MCP support leaves out shared servers instead of failing:
// the agent resolves with nothing to project and the servers in Skipped.
func TestResolveAgentMCPProjectionSkipsSharedServersOnUnsupportedHarness(t *testing.T) {
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

	mcp, err := resolveAgentMCPProjection(cityPath, cfg, agent, agent.QualifiedName(), cityPath, "copilot")
	if err != nil {
		t.Fatalf("resolveAgentMCPProjection: %v", err)
	}
	if len(mcp.Catalog.Servers) != 0 || mcp.Projection.Provider != "" {
		t.Fatalf("resolution = %+v, want nothing to project", mcp)
	}
	if len(mcp.Skipped) != 1 || mcp.Skipped[0].Server.Name != "notes" {
		t.Fatalf("skipped = %+v, want notes", mcp.Skipped)
	}
	if !strings.Contains(mcp.Skipped[0].Reason, `"copilot" harness, which cannot receive MCP`) {
		t.Fatalf("reason = %q", mcp.Skipped[0].Reason)
	}
}

// An agent whose provider binary is not installed cannot start, and its own
// session start says so. Stage 1 must still project the rest of the city:
// gc init declares more than one provider, and each declared provider's
// implicit agent inherits the city's MCP servers.
func TestRunStage1MCPProjectionSkipsAgentWhoseProviderIsNotInstalled(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("gemini", "codex"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "gemini"},
			{Name: "codex", Scope: "city", Provider: "codex", Implicit: true},
		},
	}
	lookPath := func(name string) (string, error) {
		if name == "codex" {
			return "", exec.ErrNotFound
		}
		return "/bin/echo", nil
	}

	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, lookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gemini", "settings.json")); err != nil {
		t.Fatalf("the rest of the city was not projected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("skipped agent's target was written, stat err = %v", err)
	}
	for _, want := range []string{`skipping MCP projection for agent "codex"`, "provider not found in PATH"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("warning missing %q:\n%s", want, stderr.String())
		}
	}

	// Other stage-1 errors still fail the pass.
	if isProviderResolutionError(errors.New("resolving workdir")) {
		t.Fatal("isProviderResolutionError matched an unrelated error")
	}
}
