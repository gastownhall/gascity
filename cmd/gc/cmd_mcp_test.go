package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/session"
)

func TestMcpListRequiresTarget(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.gemini]
command = "echo"
prompt_mode = "none"

[[agent]]
name = "mayor"
provider = "gemini"
scope = "city"
max_active_sessions = 1
`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"mcp", "list"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected gc mcp list to fail without a target, stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "projected MCP is target-specific") {
		t.Fatalf("stderr = %q, want target-specific error", stderr.String())
	}
}

func TestMcpListAgentProjectedSummary(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "tmux"

[providers.gemini]
command = "echo"
prompt_mode = "none"

[[agent]]
name = "mayor"
provider = "gemini"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "npx"
args = ["@acme/notes"]

[env]
API_TOKEN = "super-secret"
`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"mcp", "list", "--agent", "mayor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc mcp list --agent exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Provider: gemini",
		filepath.ToSlash(filepath.Join(cityDir, ".gemini", "settings.json")),
		"notes",
		"stdio",
		"mcp/notes.toml",
		"API_TOKEN",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "super-secret") {
		t.Fatalf("mcp list output leaked env value:\n%s", out)
	}
}

func TestMcpListAgentRequiresSessionForMultiSessionTargets(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.gemini]
command = "echo"
prompt_mode = "none"

[[agent]]
name = "mayor"
provider = "gemini"
scope = "city"
max_active_sessions = 2
work_dir = "worktrees/{{.Agent}}"
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "npx"
`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"mcp", "list", "--agent", "mayor"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected gc mcp list --agent to fail for multi-session target, stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "session-specific MCP targets; use --session") {
		t.Fatalf("stderr = %q, want session-specific error", stderr.String())
	}
}

func TestMcpListSessionProjectedSummaryUsesSessionIdentity(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "tmux"

[providers.claude]
command = "echo"
prompt_mode = "none"

[[agent]]
name = "mayor"
provider = "claude"
scope = "city"
max_active_sessions = 1
`)
	writeCatalogFile(t, cityDir, "mcp/remote.template.toml", `
name = "remote"
url = "https://example.com/{{.AgentName}}"

[headers]
Authorization = "Bearer top-secret"
`)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionDir := filepath.Join(cityDir, "worktrees", "ops-mayor-2")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(sessionDir): %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "mayor session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":      "mayor",
			"session_name":  "s-mayor-2",
			"agent_name":    "ops/mayor-2",
			"provider":      "claude",
			"provider_kind": "claude",
			"work_dir":      sessionDir,
			"state":         "asleep",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"mcp", "list", "--session", bead.ID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc mcp list --session exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Provider: claude",
		filepath.ToSlash(filepath.Join(sessionDir, ".mcp.json")),
		"https://example.com/ops/mayor-2",
		"mcp/remote.template.toml",
		"Authorization",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "top-secret") {
		t.Fatalf("mcp list output leaked header value:\n%s", out)
	}
}

func TestMcpListErrorsOnUndeliverableTarget(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "subprocess"

[providers.gemini]
command = "echo"
prompt_mode = "none"

[[agent]]
name = "mayor"
provider = "gemini"
scope = "city"
work_dir = "worktrees/mayor"
max_active_sessions = 1
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "npx"
`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"mcp", "list", "--agent", "mayor"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected gc mcp list --agent to fail, stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "effective MCP cannot be delivered") {
		t.Fatalf("stderr = %q, want delivery error", stderr.String())
	}
}

func writeProjectedMCPCity(t *testing.T, dir, cityTOML string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(strings.TrimLeft(cityTOML, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
}

func TestDisplayMCPSourcePathCityRelative(t *testing.T) {
	cityDir := t.TempDir()
	got := displayMCPSourcePath(cityDir, filepath.Join(cityDir, "mcp", "notes.toml"))
	if want := filepath.ToSlash(filepath.Join("mcp", "notes.toml")); got != want {
		t.Fatalf("displayMCPSourcePath() = %q, want %q", got, want)
	}
	got = displayMCPSourcePath(cityDir, filepath.Join(t.TempDir(), "other.toml"))
	if !strings.HasSuffix(got, "other.toml") {
		t.Fatalf("displayMCPSourcePath(external) = %q, want absolute path suffix", got)
	}
}

func TestFormatMCPKeyNamesSorted(t *testing.T) {
	got := formatMCPKeyNames(map[string]string{"ZED": "1", "API_TOKEN": "2"})
	if got != "API_TOKEN,ZED" {
		t.Fatalf("formatMCPKeyNames() = %q", got)
	}
	if got := formatMCPKeyNames(nil); got != "-" {
		t.Fatalf("formatMCPKeyNames(nil) = %q, want -", got)
	}
}

func TestProjectedMCPWriterNoServers(t *testing.T) {
	var buf bytes.Buffer
	writeProjectedMCPView(&buf, t.TempDir(), resolvedMCPProjection{
		Projection: materialize.MCPProjection{
			Provider: "gemini",
			Target:   "/tmp/example/.gemini/settings.json",
		},
	})
	out := buf.String()
	if !strings.Contains(out, "No projected MCP servers.") {
		t.Fatalf("writeProjectedMCPView() missing empty-state message:\n%s", out)
	}
}
