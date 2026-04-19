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

func TestRunStage1MCPProjectionPropagatesManagedDirReadErrors(t *testing.T) {
	// Permission / corruption errors reading .gc/mcp-managed/ must
	// surface rather than be silently treated as "nothing to do" —
	// hiding them would leave stale managed state unreconciled and
	// rob operators of diagnostic output.
	if os.Geteuid() == 0 {
		t.Skip("cannot test permission-denied ReadDir as root")
	}
	cityPath := t.TempDir()
	markersDir := filepath.Join(cityPath, ".gc", "mcp-managed")
	if err := os.MkdirAll(markersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Revoke read permission so ReadDir returns EACCES.
	if err := os.Chmod(markersDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(markersDir, 0o755)
	})

	cfg := &config.City{Session: config.SessionConfig{Provider: "tmux"}}
	err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected EACCES propagation, got nil")
	}
	if !strings.Contains(err.Error(), "scanning") {
		t.Fatalf("error should cite scan context, got: %v", err)
	}
}

func TestRunStage1MCPProjectionCleansOrphanedManagedMarkers(t *testing.T) {
	cityPath := t.TempDir()
	// Plant an orphaned managed marker + its target: neither is referenced
	// by any agent in the current config, so stage-1 must clean both up.
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "mcp-managed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "mcp-managed", "gemini.json"),
		[]byte(`{"managed_by":"gc","provider":"gemini"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	geminiTarget := filepath.Join(cityPath, ".gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(geminiTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(geminiTarget, []byte(`{"mcpServers":{"stale":{"command":"old"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Config has no agents — any managed marker under this root is orphaned.
	cfg := &config.City{
		Session: config.SessionConfig{Provider: "tmux"},
	}

	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "mcp-managed", "gemini.json")); !os.IsNotExist(err) {
		t.Fatalf("orphaned managed marker should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(geminiTarget); !os.IsNotExist(err) {
		t.Fatalf("orphaned managed target should be removed, stat err = %v", err)
	}
	if !strings.Contains(stderr.String(), "orphan MCP marker gemini") {
		t.Fatalf("stderr missing orphan cleanup note: %q", stderr.String())
	}
}

func TestValidateStage2TargetClaimantsResolvesEachAgentsOwnWorkdir(t *testing.T) {
	// Two stage-2 agents with distinct per-agent MCP and distinct WorkDir
	// templates: they never actually land on the same provider-native
	// target, so the caller's stage-2 launch must not be blocked with a
	// bogus "conflict" error.
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

	cfg := &config.City{
		Workspace: config.Workspace{Provider: "gemini"},
		Providers: map[string]config.ProviderSpec{
			"gemini": {Command: "echo", PromptMode: "none"},
		},
		Session: config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "alpha", Scope: "city", Provider: "gemini", WorkDir: ".gc/worktrees/{{.Agent}}", MaxActiveSessions: intPtr(2), MCPDir: filepath.Dir(alphaMCP)},
			{Name: "beta", Scope: "city", Provider: "gemini", WorkDir: ".gc/worktrees/{{.Agent}}", MaxActiveSessions: intPtr(2), MCPDir: filepath.Dir(betaMCP)},
		},
	}

	// Simulate alpha invoking stage-2 pre-start: resolve alpha's workdir
	// and projection, then validate against other claimants.
	alphaWorkdir := filepath.Join(cityPath, ".gc", "worktrees", "alpha")
	_, alphaProj, err := resolveAgentMCPProjection(cityPath, cfg, &cfg.Agents[0], "alpha", alphaWorkdir, "gemini")
	if err != nil {
		t.Fatalf("resolveAgentMCPProjection(alpha): %v", err)
	}
	if err := validateStage2TargetClaimants(cityPath, cfg, &cfg.Agents[0], alphaProj, stubLookPath); err != nil {
		t.Fatalf("validateStage2TargetClaimants must allow disjoint-workdir peers, got: %v", err)
	}
}

func TestValidateStage2TargetClaimantsSkipsMixedProviderPeers(t *testing.T) {
	// A Gemini caller sharing a workdir template with a Claude peer
	// must NOT see a false-positive conflict: their provider-native
	// targets live in disjoint subtrees
	// (`.mcp.json` vs `.gemini/settings.json`) and cannot collide.
	// Prior bug: validator reused caller's providerKind for every
	// peer, projecting Claude's catalog as if it would land on
	// Gemini's target, producing a bogus hash mismatch.
	cityPath := t.TempDir()
	geminiMCP := filepath.Join(cityPath, "agents", "mayor", "mcp", "notes.toml")
	claudeMCP := filepath.Join(cityPath, "agents", "deputy", "mcp", "notes.toml")
	writeMCPSource(t, geminiMCP, `
name = "notes"
command = "uvx"
args = ["mayor-notes"]
`)
	writeMCPSource(t, claudeMCP, `
name = "notes"
command = "uvx"
args = ["deputy-notes"]
`)

	shared := ".gc/worktrees/shared"
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "gemini"},
		Providers: map[string]config.ProviderSpec{
			"gemini": {Command: "echo", PromptMode: "none"},
			"claude": {Command: "echo", PromptMode: "none"},
		},
		Session: config.SessionConfig{Provider: "tmux"},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "gemini", WorkDir: shared, MaxActiveSessions: intPtr(2), MCPDir: filepath.Dir(geminiMCP)},
			{Name: "deputy", Scope: "city", Provider: "claude", WorkDir: shared, MaxActiveSessions: intPtr(2), MCPDir: filepath.Dir(claudeMCP)},
		},
	}

	sharedWorkdir := filepath.Join(cityPath, shared)
	_, mayorProj, err := resolveAgentMCPProjection(cityPath, cfg, &cfg.Agents[0], "mayor", sharedWorkdir, "gemini")
	if err != nil {
		t.Fatalf("resolveAgentMCPProjection(mayor): %v", err)
	}
	if err := validateStage2TargetClaimants(cityPath, cfg, &cfg.Agents[0], mayorProj, stubLookPath); err != nil {
		t.Fatalf("mixed-provider peers in same workdir must not conflict, got: %v", err)
	}
}

func TestValidateStage2TargetClaimantsRejectsRealConflicts(t *testing.T) {
	// Two agents sharing the same WorkDir template with different MCP
	// catalogs must conflict — this is the safety contract we are
	// preserving even after fixing the false-positive path.
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

	// Both agents point at the same shared workdir template, so their
	// projections resolve to the same provider-native target.
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

	alphaWorkdir := filepath.Join(cityPath, shared)
	_, alphaProj, err := resolveAgentMCPProjection(cityPath, cfg, &cfg.Agents[0], "alpha", alphaWorkdir, "gemini")
	if err != nil {
		t.Fatalf("resolveAgentMCPProjection(alpha): %v", err)
	}
	err = validateStage2TargetClaimants(cityPath, cfg, &cfg.Agents[0], alphaProj, stubLookPath)
	if err == nil {
		t.Fatal("expected conflict for two agents with same workdir and divergent MCP, got nil")
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
