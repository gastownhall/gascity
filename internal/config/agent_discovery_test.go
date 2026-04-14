package config

// Tests for V2 convention-based agent discovery from agents/ directories.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestAgentDiscovery_BasicDirectory(t *testing.T) {
	// An agents/<name>/ directory with just prompt.md should produce an agent.
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "worker")

	mustMkdirAll(t, agentDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, agentDir, "prompt.md", `You are a worker agent.`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	found := false
	for _, a := range explicit {
		if a.Name == "worker" {
			found = true
			if !strings.HasSuffix(a.PromptTemplate, "prompt.md") {
				t.Errorf("worker PromptTemplate = %q, want suffix prompt.md", a.PromptTemplate)
			}
			break
		}
	}
	if !found {
		t.Error("worker agent not discovered from agents/ directory")
	}
}

func TestAgentDiscovery_CanonicalTemplateSuffix(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "worker")

	mustMkdirAll(t, agentDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, agentDir, "prompt.template.md", `You are {{ .AgentName }}.`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "worker" {
			if !strings.HasSuffix(a.PromptTemplate, "prompt.template.md") {
				t.Errorf("worker PromptTemplate = %q, want suffix prompt.template.md", a.PromptTemplate)
			}
			return
		}
	}
	t.Error("worker agent not discovered from agents/ directory")
}

func TestAgentDiscovery_LegacyTemplateSuffixStillLoads(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "worker")

	mustMkdirAll(t, agentDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, agentDir, "prompt.md.tmpl", `legacy`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "worker" {
			if !strings.HasSuffix(a.PromptTemplate, "prompt.md.tmpl") {
				t.Errorf("worker PromptTemplate = %q, want suffix prompt.md.tmpl", a.PromptTemplate)
			}
			return
		}
	}
	t.Error("worker agent not discovered from agents/ directory")
}

func TestAgentDiscovery_PrefersCanonicalTemplateSuffix(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "worker")

	mustMkdirAll(t, agentDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, agentDir, "prompt.template.md", `canonical`)
	writeTestFile(t, agentDir, "prompt.md.tmpl", `legacy`)
	writeTestFile(t, agentDir, "prompt.md", `plain`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "worker" {
			if !strings.HasSuffix(a.PromptTemplate, "prompt.template.md") {
				t.Errorf("worker PromptTemplate = %q, want canonical prompt.template.md", a.PromptTemplate)
			}
			return
		}
	}
	t.Error("worker agent not discovered from agents/ directory")
}

func TestAgentDiscovery_WithAgentToml(t *testing.T) {
	// agents/<name>/agent.toml provides per-agent config.
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "coder")

	mustMkdirAll(t, agentDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, agentDir, "agent.toml", `
scope = "city"
provider = "codex"
`)
	writeTestFile(t, agentDir, "prompt.md", `You are a coder.`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "coder" {
			if a.Scope != "city" {
				t.Errorf("coder Scope = %q, want %q", a.Scope, "city")
			}
			if a.Provider != "codex" {
				t.Errorf("coder Provider = %q, want %q", a.Provider, "codex")
			}
			return
		}
	}
	t.Error("coder agent not discovered from agents/ directory")
}

func TestAgentDiscovery_TomlAgentTakesPrecedence(t *testing.T) {
	// When both [[agent]] in pack.toml and agents/<name>/ exist,
	// the TOML declaration wins (convention agent skipped).
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "mayor")

	mustMkdirAll(t, agentDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
provider = "claude"
`)
	writeTestFile(t, agentDir, "agent.toml", `
scope = "rig"
provider = "codex"
`)
	writeTestFile(t, agentDir, "prompt.md", `Convention prompt.`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	mayorCount := 0
	for _, a := range explicit {
		if a.Name == "mayor" {
			mayorCount++
			// TOML version should win.
			if a.Provider != "claude" {
				t.Errorf("mayor Provider = %q, want %q (TOML should win over convention)", a.Provider, "claude")
			}
			if a.Scope != "city" {
				t.Errorf("mayor Scope = %q, want %q (TOML should win)", a.Scope, "city")
			}
		}
	}
	if mayorCount != 1 {
		t.Errorf("expected exactly 1 mayor, got %d", mayorCount)
	}
}

func TestAgentDiscovery_WithOverlay(t *testing.T) {
	// agents/<name>/overlay/ is discovered as the per-agent overlay dir.
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "helper")
	overlayDir := filepath.Join(agentDir, "overlay")

	mustMkdirAll(t, overlayDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, agentDir, "prompt.md", `Helper agent.`)
	writeTestFile(t, overlayDir, "CLAUDE.md", `# Helper overlay`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "helper" {
			if !strings.HasSuffix(a.OverlayDir, "overlay") {
				t.Errorf("helper OverlayDir = %q, want suffix 'overlay'", a.OverlayDir)
			}
			return
		}
	}
	t.Error("helper agent not discovered from agents/ directory")
}

func TestAgentDiscovery_NoAgentsDir(t *testing.T) {
	// A pack with no agents/ directory should work fine (no agents discovered).
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	mustMkdirAll(t, packDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 0 {
		t.Errorf("expected no agents, got %d", len(explicit))
	}
}

func TestAgentDiscovery_WithImport(t *testing.T) {
	// Convention-discovered agents from an imported pack should get
	// binding names like any other imported agent.
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	agentDir := filepath.Join(packDir, "agents", "assist")
	mustMkdirAll(t, agentDir, 0o755)

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, agentDir, "agent.toml", `
scope = "city"
`)
	writeTestFile(t, agentDir, "prompt.md", `Assist agent.`)

	cityDir := filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.helper]
source = "../mypk"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	found := map[string]bool{}
	for _, a := range explicit {
		found[a.QualifiedName()] = true
	}

	if !found["helper.assist"] {
		t.Errorf("missing helper.assist; got: %v", found)
	}
}
