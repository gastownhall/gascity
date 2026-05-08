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

	opencode, err := BuildMCPProjection(MCPProviderOpenCode, "/work", nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection(opencode): %v", err)
	}
	if got, want := opencode.Target, filepath.Join("/work", "opencode.json"); got != want {
		t.Fatalf("opencode target = %q, want %q", got, want)
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

func TestApplyMCPProjectionOpenCodePreservesNonMCPConfig(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(target, []byte(`{"theme":"system","mcp":{"old":{"type":"local","command":["old"]}}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	proj, err := BuildMCPProjection(MCPProviderOpenCode, dir, []MCPServer{
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

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc struct {
		Theme string `json:"theme"`
		MCP   map[string]struct {
			Type    string            `json:"type"`
			Command []string          `json:"command"`
			URL     string            `json:"url"`
			Env     map[string]string `json:"environment"`
			Headers map[string]string `json:"headers"`
			Enabled bool              `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if doc.Theme != "system" {
		t.Fatalf("theme = %q, want system", doc.Theme)
	}
	if _, ok := doc.MCP["old"]; ok {
		t.Fatalf("old MCP entry was preserved: %+v", doc.MCP)
	}
	if got, want := doc.MCP["alpha"].Type, "local"; got != want {
		t.Fatalf("alpha.type = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(doc.MCP["alpha"].Command, []string{"uvx", "pkg"}) {
		t.Fatalf("alpha.command = %#v, want uvx/pkg", doc.MCP["alpha"].Command)
	}
	if got := doc.MCP["alpha"].Env["TOKEN"]; got != "secret" {
		t.Fatalf("alpha.environment TOKEN = %q, want secret", got)
	}
	if got, want := doc.MCP["remote"].Type, "remote"; got != want {
		t.Fatalf("remote.type = %q, want %q", got, want)
	}
	if got, want := doc.MCP["remote"].URL, "https://mcp.example.com"; got != want {
		t.Fatalf("remote.url = %q, want %q", got, want)
	}
	if got := doc.MCP["remote"].Headers["Authorization"]; got != "Bearer token" {
		t.Fatalf("remote.headers Authorization = %q, want Bearer token", got)
	}
	if !doc.MCP["alpha"].Enabled || !doc.MCP["remote"].Enabled {
		t.Fatalf("projected OpenCode MCP entries should be enabled: %+v", doc.MCP)
	}

	empty, err := BuildMCPProjection(MCPProviderOpenCode, dir, nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection(empty): %v", err)
	}
	if err := empty.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply(empty): %v", err)
	}
	data, err = os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile after empty apply: %v", err)
	}
	var after map[string]any
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("Unmarshal after empty apply: %v", err)
	}
	if _, ok := after["mcp"]; ok {
		t.Fatalf("mcp key remained after empty projection: %s", data)
	}
	if got := after["theme"]; got != "system" {
		t.Fatalf("theme after empty apply = %v, want system", got)
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

func TestApplyMCPProjectionFakeChmodsBeforeRename(t *testing.T) {
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

	// Verify pre-rename chmod: every Chmod on a managed file must target
	// a temp path (.tmp.*) and precede the matching Rename of that temp
	// path to the final path. Any Chmod on the final path would reopen
	// the write-then-chmod window this test guards against.
	var sawTempChmodBeforeRename bool
	var pendingTempChmod string
	targetFinal := filepath.Join("/work", ".mcp.json")
	for _, call := range fake.Calls {
		if call.Method == "Chmod" {
			if call.Path == targetFinal {
				t.Fatalf("Chmod on final path is a write-then-chmod window regression: %s", call.Path)
			}
			if strings.Contains(call.Path, targetFinal+".tmp.") {
				pendingTempChmod = call.Path
			}
		}
		if call.Method == "Rename" && pendingTempChmod != "" && call.Path == pendingTempChmod {
			sawTempChmodBeforeRename = true
			break
		}
	}
	if !sawTempChmodBeforeRename {
		t.Fatalf("expected Chmod(temp) before Rename(temp,final), calls = %#v", fake.Calls)
	}
}

func TestApplyMCPProjectionSnapshotsExistingContentBeforeFirstAdoption(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".mcp.json")
	existing := []byte(`{"mcpServers":{"user-authored":{"command":"custom"}}}` + "\n")
	if err := os.WriteFile(target, existing, 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	restore := SetAdoptionStderr(&stderr)
	defer restore()

	proj, err := BuildMCPProjection(MCPProviderClaude, dir, []MCPServer{
		{Name: "alpha", Transport: MCPTransportStdio, Command: "uvx"},
	})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	if err := proj.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The pre-existing hand-authored file must have been preserved under
	// .gc/mcp-adopted/claude/<timestamp>.json before being overwritten.
	adoptedDir := filepath.Join(dir, ".gc", "mcp-adopted", "claude")
	entries, err := os.ReadDir(adoptedDir)
	if err != nil {
		t.Fatalf("ReadDir(adopted): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 adoption snapshot, got %d: %v", len(entries), entries)
	}
	snapshot, err := os.ReadFile(filepath.Join(adoptedDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile(snapshot): %v", err)
	}
	if !reflect.DeepEqual(snapshot, existing) {
		t.Fatalf("snapshot content mismatch\n  got:  %q\n  want: %q", snapshot, existing)
	}

	// Second Apply (already managed) must NOT create a second snapshot.
	proj2, _ := BuildMCPProjection(MCPProviderClaude, dir, []MCPServer{
		{Name: "beta", Transport: MCPTransportStdio, Command: "uvx"},
	})
	if err := proj2.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply(second): %v", err)
	}
	entries2, err := os.ReadDir(adoptedDir)
	if err != nil {
		t.Fatalf("ReadDir(adopted) second pass: %v", err)
	}
	if len(entries2) != 1 {
		t.Fatalf("adoption snapshot must only be taken once; got %d: %v", len(entries2), entries2)
	}

	// The stderr warning must name both paths so operators can recover.
	warning := stderr.String()
	if !strings.Contains(warning, "adopting provider-native MCP at "+target) {
		t.Fatalf("stderr missing target path: %q", warning)
	}
	if !strings.Contains(warning, "snapshotted to ") {
		t.Fatalf("stderr missing snapshot path: %q", warning)
	}
}

func TestApplyMCPProjectionDoesNotSnapshotWhenApplyIsNoop(t *testing.T) {
	// An empty catalog against an unmanaged target must NOT take an
	// adoption snapshot — no write will happen, so no backup is owed.
	// Prior bug: snapshot fired unconditionally, filling
	// .gc/mcp-adopted/ with spurious backups of unchanged files on
	// every stage-2 pre-start that resolved to zero servers.
	dir := t.TempDir()
	target := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(target, []byte(`{"mcpServers":{"user":{"command":"custom"}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	restore := SetAdoptionStderr(&stderr)
	defer restore()

	empty, err := BuildMCPProjection(MCPProviderClaude, dir, nil)
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	if err := empty.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply(empty, unmanaged): %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".gc", "mcp-adopted")); !os.IsNotExist(err) {
		t.Fatalf(".gc/mcp-adopted must not exist after no-op apply; stat err = %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("no adoption stderr expected on no-op apply, got: %q", stderr.String())
	}
	// The original user-authored file must be untouched.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target): %v", err)
	}
	if !strings.Contains(string(got), `"user"`) {
		t.Fatalf("unmanaged target mutated: %q", got)
	}
}

func TestApplyMCPProjectionRejectsSymlinkedTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".mcp.json")
	// Point the managed target at an attacker-controlled file outside the
	// workdir. Apply must refuse rather than read/write through the link.
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("sensitive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, target); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	proj, err := BuildMCPProjection(MCPProviderClaude, dir, []MCPServer{
		{Name: "alpha", Transport: MCPTransportStdio, Command: "uvx"},
	})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	err = proj.Apply(fsys.OSFS{})
	if err == nil {
		t.Fatal("expected symlink rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "symlinked path") {
		t.Fatalf("unexpected error: %v", err)
	}
	// Victim file content must be untouched.
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("ReadFile(victim): %v", err)
	}
	if string(got) != "sensitive" {
		t.Fatalf("victim mutated: %q", got)
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
