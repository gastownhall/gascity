package main

import (
	"bytes"
	"errors"
	"io"
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
	for _, want := range []string{`agent "helper" cannot start: MCP servers [private]`, `"copilot"`} {
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

	_, err := resolveAgentMCPProjection(cityPath, cfg, agent, agent.QualifiedName(), cityPath, "copilot", false)
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

	mcp, err := resolveAgentMCPProjection(cityPath, cfg, agent, agent.QualifiedName(), cityPath, "copilot", false)
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
	// codex is the implicit agent gc adds for the declared codex provider,
	// which the user never defined; the warning says so.
	for _, want := range []string{`agent "codex" (the implicit agent for provider "codex") cannot start: provider not found in PATH`} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("warning missing %q:\n%s", want, stderr.String())
		}
	}

	// Other stage-1 errors still fail the pass.
	if isProviderResolutionError(errors.New("resolving workdir")) {
		t.Fatal("isProviderResolutionError matched an unrelated error")
	}
}

// A skipped agent cannot start, but stage 1 leaves the target it last
// projected as it was instead of cleaning it up as an orphan: the skip may be
// transient, and a session already running may read that file.
func TestRunStage1MCPProjectionLeavesASkippedAgentsTargetAsItWas(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("codex", "cursor"),
		Agents: []config.Agent{
			{Name: "coder", Scope: "city", Provider: "codex"},
			{Name: "editor", Scope: "city", Provider: "cursor"},
		},
	}
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("first runStage1MCPProjection: %v", err)
	}
	codexTarget := filepath.Join(cityPath, ".codex", "config.toml")
	cursorTarget := filepath.Join(cityPath, ".cursor", "mcp.json")
	before := map[string][]byte{}
	for _, path := range []string{codexTarget, cursorTarget} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("first pass did not project %s: %v", path, err)
		}
		before[path] = data
	}

	// The codex binary goes missing, and the cursor agent gains an http
	// server of its own that its provider cannot receive: stage 1 skips
	// both agents.
	editorMCP := filepath.Join(cityPath, "agents", "editor", "mcp")
	writeMCPSource(t, filepath.Join(editorMCP, "private.toml"), `
name = "private"
url = "https://mcp.example.test/private"
`)
	cfg.Agents[1].MCPDir = editorMCP
	noCodex := func(name string) (string, error) {
		if name == "codex" {
			return "", exec.ErrNotFound
		}
		return "/bin/echo", nil
	}
	// gc start --dry-run's plan agrees: it does not list their targets for
	// cleanup.
	lines, err := planStage1MCPProjection(cityPath, cfg, noCodex, io.Discard)
	if err != nil {
		t.Fatalf("planStage1MCPProjection: %v", err)
	}
	if plan := strings.Join(lines, "\n"); strings.Contains(plan, "no agent claims it") {
		t.Fatalf("the plan lists a skipped agent's target for cleanup:\n%s", plan)
	}
	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, noCodex, &stderr); err != nil {
		t.Fatalf("second runStage1MCPProjection: %v", err)
	}
	if strings.Contains(stderr.String(), "cleaned up orphan") {
		t.Fatalf("a skipped agent's target was cleaned up as an orphan:\n%s", stderr.String())
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("skipped agent's target %s is gone: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("skipped agent's target %s changed:\n got: %s\nwant: %s", path, got, want)
		}
	}
	for _, provider := range []string{"codex", "cursor"} {
		if _, err := os.Stat(filepath.Join(cityPath, ".gc", "mcp-managed", provider+".json")); err != nil {
			t.Fatalf("%s managed marker is gone: %v", provider, err)
		}
	}

	// Once nothing claims the target, cleanup removes it as before, and the
	// plan says so.
	cfg.Agents = cfg.Agents[:0]
	lines, err = planStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard)
	if err != nil {
		t.Fatalf("planStage1MCPProjection without agents: %v", err)
	}
	if plan := strings.Join(lines, "\n"); !strings.Contains(plan, "remove "+cursorTarget+" (cursor) (no agent claims it)") {
		t.Fatalf("the plan does not list the unclaimed target for cleanup:\n%s", plan)
	}
	stderr.Reset()
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("third runStage1MCPProjection: %v", err)
	}
	if _, err := os.Stat(cursorTarget); !os.IsNotExist(err) {
		t.Fatalf("orphaned target survived once no agent claims it, stat err = %v", err)
	}
}

// gc start --dry-run's plan resolves a skipped agent's work dir, to find the
// target it holds, without creating the directory.
func TestPlanStage1MCPProjectionLeavesASkippedAgentsWorkDirUncreated(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("codex"),
		Agents:     []config.Agent{{Name: "coder", Scope: "city", Provider: "codex", WorkDir: "worktrees/coder"}},
	}
	noCodex := func(string) (string, error) { return "", exec.ErrNotFound }
	var stderr bytes.Buffer
	if _, err := planStage1MCPProjection(cityPath, cfg, noCodex, &stderr); err != nil {
		t.Fatalf("planStage1MCPProjection: %v", err)
	}
	if !strings.Contains(stderr.String(), `agent "coder" cannot start`) {
		t.Fatalf("the plan did not skip the agent:\n%s", stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(cityPath, "worktrees")); !os.IsNotExist(err) {
		t.Fatalf("the plan created the skipped agent's work dir (err=%v)", err)
	}
}
