package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// The stage-1 MCP plan names every change the real pass would make, orphan
// cleanup included, and writes nothing.
func TestPlanStage1MCPProjectionReportsWithoutWriting(t *testing.T) {
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
			{Name: "coder", Scope: "city", Provider: "codex"},
		},
	}
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	// The codex agent goes away (its target is now an orphan), and the
	// gemini agent gains a second server.
	cfg.Agents = cfg.Agents[:1]
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "docs.toml"), `
name = "docs"
command = "docs-mcp"
`)
	before := map[string][]byte{}
	for _, rel := range []string{".gemini/settings.json", ".codex/config.toml", ".gc/mcp-managed/codex.json", ".gc/mcp-managed/gemini.json", ".gitignore"} {
		data, err := os.ReadFile(filepath.Join(cityPath, rel))
		if err != nil {
			t.Fatalf("first pass did not write %s: %v", rel, err)
		}
		before[rel] = data
	}

	lines, err := planStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard)
	if err != nil {
		t.Fatalf("planStage1MCPProjection: %v", err)
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"update " + filepath.Join(cityPath, ".gemini", "settings.json") + " (gemini) to 2 MCP servers",
		"remove " + filepath.Join(cityPath, ".codex", "config.toml") + " (codex) (no agent claims it)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, ".gitignore") {
		t.Errorf("plan reports a .gitignore update that is already done:\n%s", got)
	}
	for rel, want := range before {
		data, err := os.ReadFile(filepath.Join(cityPath, rel))
		if err != nil || !bytes.Equal(data, want) {
			t.Errorf("plan changed %s (err=%v)", rel, err)
		}
	}
}

// A preview fails where the start would: the MCP gate's errors come back.
func TestPlanStage1MCPProjectionReturnsTheStartGateError(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	agentLocal := filepath.Join(cityPath, "agents", "mayor", "mcp", "notes.toml")
	writeMCPSource(t, agentLocal, `
name = "notes"
command = "other"
`)
	cfg := &config.City{
		Session:   config.SessionConfig{Provider: "tmux"},
		Providers: builtinProviderAliasesForTest("claude"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude", MCPDir: filepath.Dir(agentLocal)},
			{Name: "deputy", Scope: "city", Provider: "claude"},
		},
	}
	_, planErr := planStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard)
	runErr := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard)
	if planErr == nil || runErr == nil || planErr.Error() != runErr.Error() {
		t.Fatalf("plan error = %v, run error = %v; want the same gate error", planErr, runErr)
	}
	if !strings.Contains(planErr.Error(), "MCP target conflict") {
		t.Fatalf("plan error = %v", planErr)
	}
}

func TestPlanStage1SkillMaterializationReportsWithoutWriting(t *testing.T) {
	cityPath := t.TempDir()
	skillDir := filepath.Join(cityPath, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Providers:     builtinProviderAliasesForTest("claude"),
		Agents:        []config.Agent{{Name: "mayor", Scope: "city", Provider: "claude"}},
	}

	var stderr bytes.Buffer
	lines := planStage1SkillMaterialization(cityPath, cfg, &stderr)
	sink := filepath.Join(cityPath, ".claude", "skills")
	if got := strings.Join(lines, "\n"); got != "link 1 skill in "+sink {
		t.Fatalf("plan = %q (stderr %q)", got, stderr.String())
	}
	if _, err := os.Stat(sink); !os.IsNotExist(err) {
		t.Fatalf("plan created the sink, stat err = %v", err)
	}

	if err := runStage1SkillMaterialization(cityPath, cfg, io.Discard); err != nil {
		t.Fatalf("runStage1SkillMaterialization: %v", err)
	}
	if lines := planStage1SkillMaterialization(cityPath, cfg, io.Discard); len(lines) != 0 {
		t.Fatalf("plan after the real pass = %q, want nothing to do", lines)
	}
}
