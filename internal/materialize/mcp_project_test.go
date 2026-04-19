package materialize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestBuildMCPProjectionTargetsAndStableHash(t *testing.T) {
	serversA := []MCPServer{
		{
			Name:      "zeta",
			Transport: MCPTransportHTTP,
			URL:       "https://example.com/mcp",
			Headers:   map[string]string{"B": "2", "A": "1"},
		},
		{
			Name:      "alpha",
			Transport: MCPTransportStdio,
			Command:   "uvx",
			Args:      []string{"pkg"},
			Env:       map[string]string{"Y": "2", "X": "1"},
		},
	}
	serversB := []MCPServer{
		{
			Name:      "alpha",
			Transport: MCPTransportStdio,
			Command:   "uvx",
			Args:      []string{"pkg"},
			Env:       map[string]string{"X": "1", "Y": "2"},
		},
		{
			Name:      "zeta",
			Transport: MCPTransportHTTP,
			URL:       "https://example.com/mcp",
			Headers:   map[string]string{"A": "1", "B": "2"},
		},
	}

	claudeA, err := BuildMCPProjection(MCPProviderClaude, "/work", serversA)
	if err != nil {
		t.Fatalf("BuildMCPProjection(claude): %v", err)
	}
	claudeB, err := BuildMCPProjection(MCPProviderClaude, "/work", serversB)
	if err != nil {
		t.Fatalf("BuildMCPProjection(claude): %v", err)
	}
	if got, want := claudeA.Target, filepath.Join("/work", ".mcp.json"); got != want {
		t.Fatalf("claude target = %q, want %q", got, want)
	}
	if claudeA.Hash() != claudeB.Hash() {
		t.Fatalf("projection hash must be stable across input ordering: %q vs %q", claudeA.Hash(), claudeB.Hash())
	}

	codex, err := BuildMCPProjection(MCPProviderCodex, "/work", nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection(codex): %v", err)
	}
	if got, want := codex.Target, filepath.Join("/work", ".codex", "config.toml"); got != want {
		t.Fatalf("codex target = %q, want %q", got, want)
	}

	gemini, err := BuildMCPProjection(MCPProviderGemini, "/work", nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection(gemini): %v", err)
	}
	if got, want := gemini.Target, filepath.Join("/work", ".gemini", "settings.json"); got != want {
		t.Fatalf("gemini target = %q, want %q", got, want)
	}
}

func TestBuildMCPProjectionRejectsUnsupportedProvider(t *testing.T) {
	if _, err := BuildMCPProjection("cursor", "/work", nil); err == nil {
		t.Fatal("expected unsupported provider error")
	}
}

func TestApplyMCPProjectionClaudeWritesManagedFile(t *testing.T) {
	dir := t.TempDir()
	proj, err := BuildMCPProjection(MCPProviderClaude, dir, []MCPServer{
		{
			Name:      "alpha",
			Transport: MCPTransportStdio,
			Command:   "uvx",
			Args:      []string{"pkg"},
			Env:       map[string]string{"TOKEN": "secret"},
		},
		{
			Name:      "remote",
			Transport: MCPTransportHTTP,
			URL:       "https://mcp.example.com",
			Headers:   map[string]string{"Authorization": "Bearer token"},
		},
	})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	if err := proj.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal .mcp.json: %v", err)
	}
	if _, ok := doc.MCPServers["alpha"]["command"]; !ok {
		t.Fatalf("stdio server missing command: %+v", doc.MCPServers["alpha"])
	}
	if got := doc.MCPServers["remote"]["type"]; got != "http" {
		t.Fatalf("remote type = %v, want http", got)
	}

	info, err := os.Stat(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("stat .mcp.json: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf(".mcp.json perms = %o, want 600", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc", "mcp-managed", "claude.json")); err != nil {
		t.Fatalf("managed marker missing: %v", err)
	}

	empty, err := BuildMCPProjection(MCPProviderClaude, dir, nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection(empty): %v", err)
	}
	if err := empty.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply(empty): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf(".mcp.json should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc", "mcp-managed", "claude.json")); !os.IsNotExist(err) {
		t.Fatalf("managed marker should be removed, stat err = %v", err)
	}
}

func TestApplyMCPProjectionGeminiPreservesNonMCPSettings(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{
  "theme": "ocean",
  "mcpServers": {
    "stale": {
      "command": "old"
    }
  }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	proj, err := BuildMCPProjection(MCPProviderGemini, dir, []MCPServer{
		{
			Name:      "stdio",
			Transport: MCPTransportStdio,
			Command:   "uvx",
			Args:      []string{"pkg"},
			Env:       map[string]string{"TOKEN": "secret"},
		},
		{
			Name:      "remote",
			Transport: MCPTransportHTTP,
			URL:       "https://mcp.example.com",
			Headers:   map[string]string{"Authorization": "Bearer token"},
		},
	})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	if err := proj.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal settings.json: %v", err)
	}
	if got := doc["theme"]; got != "ocean" {
		t.Fatalf("theme = %v, want ocean", got)
	}
	mcpServers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong type: %+v", doc["mcpServers"])
	}
	remote, ok := mcpServers["remote"].(map[string]any)
	if !ok {
		t.Fatalf("remote server missing: %+v", mcpServers)
	}
	if got := remote["httpUrl"]; got != "https://mcp.example.com" {
		t.Fatalf("remote httpUrl = %v, want https://mcp.example.com", got)
	}

	empty, err := BuildMCPProjection(MCPProviderGemini, dir, nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection(empty): %v", err)
	}
	if err := empty.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply(empty): %v", err)
	}
	data, err = os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(after cleanup): %v", err)
	}
	if strings.Contains(string(data), "mcpServers") {
		t.Fatalf("mcpServers should be removed after cleanup:\n%s", string(data))
	}
}

func TestApplyMCPProjectionCodexPreservesNonMCPConfig(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`
model = "gpt-5"

[mcp_servers.stale]
command = "old"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	proj, err := BuildMCPProjection(MCPProviderCodex, dir, []MCPServer{
		{
			Name:      "stdio",
			Transport: MCPTransportStdio,
			Command:   "uvx",
			Args:      []string{"pkg"},
			Env:       map[string]string{"TOKEN": "secret"},
		},
		{
			Name:      "remote",
			Transport: MCPTransportHTTP,
			URL:       "https://mcp.example.com",
			Headers:   map[string]string{"Authorization": "Bearer token"},
		},
	})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	if err := proj.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc map[string]any
	if _, err := toml.Decode(string(data), &doc); err != nil {
		t.Fatalf("decode codex config: %v", err)
	}
	if got := doc["model"]; got != "gpt-5" {
		t.Fatalf("model = %v, want gpt-5", got)
	}
	mcpServers, ok := doc["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("mcp_servers missing or wrong type: %#v", doc["mcp_servers"])
	}
	remote, ok := mcpServers["remote"].(map[string]any)
	if !ok {
		t.Fatalf("remote server missing: %#v", mcpServers)
	}
	if got := remote["url"]; got != "https://mcp.example.com" {
		t.Fatalf("remote url = %v, want https://mcp.example.com", got)
	}
	if _, ok := remote["http_headers"]; !ok {
		t.Fatalf("remote http_headers missing: %#v", remote)
	}

	empty, err := BuildMCPProjection(MCPProviderCodex, dir, nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection(empty): %v", err)
	}
	if err := empty.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply(empty): %v", err)
	}
	data, err = os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(after cleanup): %v", err)
	}
	doc = nil
	if _, err := toml.Decode(string(data), &doc); err != nil {
		t.Fatalf("decode cleaned codex config: %v", err)
	}
	if _, ok := doc["mcp_servers"]; ok {
		t.Fatalf("mcp_servers should be removed after cleanup: %#v", doc)
	}
}

func TestApplyMCPProjectionCodexRemovesManagedFileWhenItOnlyContainsMCP(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".codex", "config.toml")
	proj, err := BuildMCPProjection(MCPProviderCodex, dir, []MCPServer{
		{Name: "alpha", Transport: MCPTransportStdio, Command: "uvx"},
	})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	if err := proj.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply(non-empty): %v", err)
	}

	empty, err := BuildMCPProjection(MCPProviderCodex, dir, nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection(empty): %v", err)
	}
	if err := empty.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply(empty): %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("codex config should be removed, stat err = %v", err)
	}
}

func TestApplyMCPProjectionClaudeLeavesUnmanagedFileWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(target, []byte(`{"mcpServers":{"user":{"command":"custom"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	empty, err := BuildMCPProjection(MCPProviderClaude, dir, nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection(empty): %v", err)
	}
	if err := empty.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply(empty): %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target): %v", err)
	}
	if !strings.Contains(string(data), `"user"`) {
		t.Fatalf("unmanaged .mcp.json should be preserved, got:\n%s", string(data))
	}
}

func TestApplyMCPProjectionNormalizesPermissionsOnRewrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(target, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	proj, err := BuildMCPProjection(MCPProviderClaude, dir, []MCPServer{
		{Name: "alpha", Transport: MCPTransportStdio, Command: "uvx"},
	})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	if err := proj.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("target perms = %o, want 600", got)
	}
}

func TestApplyMCPProjectionFakeCallsChmod(t *testing.T) {
	fake := fsys.NewFake()
	proj, err := BuildMCPProjection(MCPProviderClaude, "/work", []MCPServer{
		{Name: "alpha", Transport: MCPTransportStdio, Command: "uvx"},
	})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	if err := proj.Apply(fake); err != nil {
		t.Fatalf("Apply(fake): %v", err)
	}

	var sawChmod bool
	for _, call := range fake.Calls {
		if call.Method == "Chmod" && call.Path == filepath.Join("/work", ".mcp.json") {
			sawChmod = true
			break
		}
	}
	if !sawChmod {
		t.Fatalf("expected Chmod call for managed file, calls = %#v", fake.Calls)
	}
}

func TestNormalizeMCPProjectionServerOrdering(t *testing.T) {
	proj, err := BuildMCPProjection(MCPProviderClaude, "/work", []MCPServer{
		{Name: "b", Transport: MCPTransportStdio, Command: "two"},
		{Name: "a", Transport: MCPTransportStdio, Command: "one"},
	})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	names := make([]string, 0, len(proj.Servers))
	for _, server := range proj.Servers {
		names = append(names, server.Name)
	}
	if !reflect.DeepEqual(names, []string{"a", "b"}) {
		t.Fatalf("projection servers ordered = %v, want [a b]", names)
	}
}
