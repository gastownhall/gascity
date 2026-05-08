package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveTemplateMCPIntegration(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
args = ["notes-mcp"]
`)

	cityCfg := &config.City{
		Workspace:  config.Workspace{Provider: "gemini"},
		Providers:  map[string]config.ProviderSpec{"gemini": {Command: "echo", PromptMode: "none"}, "cursor": {Command: "echo", PromptMode: "none"}},
		PackMCPDir: filepath.Join(cityPath, "mcp"),
	}

	buildParams := func(sessionProvider string) *agentBuildParams {
		return &agentBuildParams{
			city:            cityCfg,
			cityName:        "city",
			cityPath:        cityPath,
			workspace:       &cityCfg.Workspace,
			providers:       cityCfg.Providers,
			lookPath:        stubLookPath,
			fs:              fsys.OSFS{},
			rigs:            nil,
			beaconTime:      time.Unix(0, 0),
			beadNames:       make(map[string]string),
			stderr:          io.Discard,
			sessionProvider: sessionProvider,
		}
	}

	t.Run("scope root uses stage1 hash only", func(t *testing.T) {
		agent := &config.Agent{Name: "mayor", Scope: "city", Provider: "gemini"}
		tp, err := resolveTemplate(buildParams("tmux"), agent, agent.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
		if tp.FPExtra["mcp:gemini"] == "" {
			t.Fatalf("expected mcp fingerprint entry, got %+v", tp.FPExtra)
		}
		for _, entry := range tp.Hints.PreStart {
			if strings.Contains(entry, "internal project-mcp") {
				t.Fatalf("unexpected stage-2 project-mcp PreStart for scope-root workdir: %v", tp.Hints.PreStart)
			}
		}
	})

	t.Run("non scope root injects project-mcp prestart", func(t *testing.T) {
		agent := &config.Agent{
			Name:     "worker",
			Scope:    "city",
			Provider: "gemini",
			WorkDir:  ".gc/worktrees/worker-1",
		}
		tp, err := resolveTemplate(buildParams("tmux"), agent, agent.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
		if tp.FPExtra["mcp:gemini"] == "" {
			t.Fatalf("expected mcp fingerprint entry, got %+v", tp.FPExtra)
		}
		found := false
		for _, entry := range tp.Hints.PreStart {
			if strings.Contains(entry, "internal project-mcp") {
				found = true
				if !strings.Contains(entry, "--agent worker") || !strings.Contains(entry, "--identity worker") {
					t.Fatalf("project-mcp PreStart missing identity/template flags: %q", entry)
				}
			}
		}
		if !found {
			t.Fatalf("expected stage-2 project-mcp PreStart, got %v", tp.Hints.PreStart)
		}
	})

	t.Run("non acp runtime excludes mcp servers", func(t *testing.T) {
		agent := &config.Agent{Name: "mayor", Scope: "city", Provider: "gemini"}
		tp, err := resolveTemplate(buildParams("tmux"), agent, agent.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
		if len(tp.MCPServers) != 0 {
			t.Fatalf("TemplateParams.MCPServers len = %d, want 0", len(tp.MCPServers))
		}
		cfg := templateParamsToConfig(tp)
		if len(cfg.MCPServers) != 0 {
			t.Fatalf("runtime.Config.MCPServers len = %d, want 0", len(cfg.MCPServers))
		}
	})

	t.Run("undeliverable runtime hard errors", func(t *testing.T) {
		agent := &config.Agent{
			Name:     "worker",
			Scope:    "city",
			Provider: "gemini",
			WorkDir:  ".gc/worktrees/worker-1",
		}
		_, err := resolveTemplate(buildParams("subprocess"), agent, agent.QualifiedName(), nil)
		if err == nil {
			t.Fatal("expected undeliverable MCP error, got nil")
		}
		if !strings.Contains(err.Error(), "effective MCP cannot be delivered") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unsupported provider hard errors when MCP exists", func(t *testing.T) {
		agent := &config.Agent{Name: "worker", Scope: "city", Provider: "cursor"}
		_, err := resolveTemplate(buildParams("tmux"), agent, agent.QualifiedName(), nil)
		if err == nil {
			t.Fatal("expected unsupported provider error, got nil")
		}
		if !strings.Contains(err.Error(), "effective MCP requires a supported provider family") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("wrapped opencode provider accepts mcp", func(t *testing.T) {
		cityCfg.Providers["wrapped-opencode"] = config.ProviderSpec{
			Base:       stringPtr("builtin:opencode"),
			Command:    "echo",
			PromptMode: "none",
		}
		agent := &config.Agent{Name: "worker", Scope: "city", Provider: "wrapped-opencode"}
		tp, err := resolveTemplate(buildParams("tmux"), agent, agent.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
		if tp.FPExtra["mcp:opencode"] == "" {
			t.Fatalf("expected opencode mcp fingerprint entry, got %+v", tp.FPExtra)
		}
	})

	t.Run("nil city hard-errors when MCP would resolve non-empty", func(t *testing.T) {
		// Regression guard: the prior fallback silently constructed a
		// synthetic City that omitted imports/implicit/bootstrap layers,
		// so tests saw a different MCP precedence stack than production.
		// resolveTemplate now refuses to proceed when the synthetic
		// fallback would yield non-empty MCP — tests exercising MCP
		// must construct a real config.City.
		params := buildParams("tmux")
		params.city = nil
		params.packDirs = []string{cityPath}
		agent := &config.Agent{Name: "mayor", Scope: "city", Provider: "gemini"}
		_, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
		if err == nil {
			t.Fatal("expected hard error when synthetic fallback would resolve MCP, got nil")
		}
		if !strings.Contains(err.Error(), "resolveTemplate invoked without config.City") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
