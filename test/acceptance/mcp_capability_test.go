//go:build acceptance_a

// MCP capability acceptance tests.
//
// An imported pack attaches its MCP servers to every agent in the city. These
// tests drive the real gc binary through the surfaces an operator uses when
// one agent's provider cannot receive such a server: the city must still
// start, and every surface that shows MCP must say the agent starts without
// the server instead of presenting it as projected.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// writeHTTPMCPServerPack writes a pack whose mcp/ provides one remote HTTP
// server, the shape a third-party pack ships.
func writeHTTPMCPServerPack(t *testing.T, dir, server string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "mcp"), 0o755); err != nil {
		t.Fatalf("mkdir pack mcp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \""+server+"\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	body := "name = \"" + server + "\"\nurl = \"https://mcp.example.test/" + server + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "mcp", server+".toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write mcp server: %v", err)
	}
}

// TestImportedHTTPMCPServerSkipsAgentsThatCannotReceiveIt imports a pack
// providing an HTTP MCP server into a city with an agent on the cursor
// provider family, whose MCP config carries only stdio servers.
func TestImportedHTTPMCPServerSkipsAgentsThatCannotReceiveIt(t *testing.T) {
	// The pack the city imports outlives the city: its temp dir is made
	// first, so its cleanup runs after the city's gc stop, which loads the
	// city's config.
	packDir := filepath.Join(helpers.TempDir(t), "search")
	c := helpers.NewCity(t, testEnv)
	c.InitNoStart("claude")
	c.AppendToConfig(`
[providers.editor]
base = "builtin:cursor"
command = "echo"
prompt_mode = "none"
`)
	c.WriteV2AgentDir("researcher", `provider = "editor"`)

	writeHTTPMCPServerPack(t, packDir, "search")

	const skipped = `start without MCP server "search" (http, from import "search"): gc projects only stdio MCP servers for the "cursor" harness`

	out, err := c.GC("import", "add", packDir)
	if err != nil {
		t.Fatalf("gc import add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "gc import add: warning: ") || !strings.Contains(out, skipped) || !strings.Contains(out, `"researcher"`) {
		t.Fatalf("gc import add did not warn that researcher starts without the server:\n%s", out)
	}

	out, err = c.GC("config", "show", "--validate")
	if err != nil {
		t.Fatalf("gc config show --validate: %v\n%s", err, out)
	}
	if !strings.Contains(out, skipped) {
		t.Fatalf("gc config show --validate did not warn about the skipped server:\n%s", out)
	}

	out, err = c.GCStdout("mcp", "list", "--agent", "researcher")
	if err != nil {
		t.Fatalf("gc mcp list --agent researcher: %v\n%s", err, out)
	}
	projected, notProjected, found := strings.Cut(out, "Not projected")
	if !found || strings.Contains(projected, "search") || !strings.Contains(notProjected, "search") {
		t.Fatalf("gc mcp list must list search only under Not projected:\n%s", out)
	}

	// The start gate: before, stage-1 projection failed the whole city.
	out, err = c.GC("start", "--dry-run")
	if err != nil {
		t.Fatalf("gc start --dry-run failed; one agent's provider must not stop the city: %v\n%s", err, out)
	}
	if strings.Contains(out, "does not support http server") || !strings.Contains(out, skipped) {
		t.Fatalf("gc start --dry-run did not skip the server for the agent:\n%s", out)
	}
}
