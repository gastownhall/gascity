//go:build acceptance_a

// gc start --dry-run acceptance tests.
//
// A dry run is a preview. These tests drive the real gc binary against a
// city with a rig whose repository commits its own provider-native MCP
// config, and assert that the preview reports what a start would change
// while leaving the rig's working tree exactly as it was.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestStartDryRunLeavesARigsCommittedMCPConfigAlone(t *testing.T) {
	// The rig and the pack the city imports outlive the city: their temp
	// dirs are made first, so their cleanups run after the city's gc stop,
	// which loads the city's config.
	rigDir := filepath.Join(helpers.TempDir(t), "frontend")
	packDir := filepath.Join(helpers.TempDir(t), "notes")
	c := helpers.NewCity(t, testEnv)
	c.InitNoStart("claude")

	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	teamMCP := `{"mcpServers":{"team-db":{"command":"team-db-mcp"}}}` + "\n"
	if err := os.WriteFile(filepath.Join(rigDir, ".mcp.json"), []byte(teamMCP), 0o644); err != nil {
		t.Fatalf("writing the rig's .mcp.json: %v", err)
	}
	git(t, rigDir, "init")
	git(t, rigDir, "add", ".")
	git(t, rigDir, "commit", "-m", "team MCP config")
	c.RigAdd(rigDir, "")

	// A pack that provides an MCP server, imported at city level: every
	// agent in the city and its rigs gets it.
	if err := os.MkdirAll(filepath.Join(packDir, "mcp"), 0o755); err != nil {
		t.Fatalf("creating pack: %v", err)
	}
	for path, body := range map[string]string{
		filepath.Join(packDir, "pack.toml"):         "[pack]\nname = \"notes\"\nschema = 2\n",
		filepath.Join(packDir, "mcp", "notes.toml"): "name = \"notes\"\ncommand = \"notes-mcp\"\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	if out, err := c.GC("import", "add", packDir); err != nil {
		t.Fatalf("gc import add: %v\n%s", err, out)
	}

	statusBefore := git(t, rigDir, "status", "--porcelain", "--untracked-files=all")
	out, err := c.GC("start", "--dry-run")
	if err != nil {
		t.Fatalf("gc start --dry-run: %v\n%s", err, out)
	}

	// The rig's own server keeps gc from adopting its .mcp.json, and the
	// preview says so.
	wantPlan := "leave " + filepath.Join(rigDir, ".mcp.json") + ` (claude) as it is: gc does not manage it yet, and adopting it would drop MCP server "team-db"`
	if !strings.Contains(out, wantPlan) {
		t.Fatalf("preview does not report what it would do with the rig's .mcp.json; want %q in:\n%s", wantPlan, out)
	}
	if strings.Contains(out, "adopting provider-native MCP") {
		t.Fatalf("the dry run adopted the rig's MCP config:\n%s", out)
	}
	if got := git(t, rigDir, "status", "--porcelain", "--untracked-files=all"); got != statusBefore {
		t.Fatalf("the dry run changed the rig's working tree:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}
	data, err := os.ReadFile(filepath.Join(rigDir, ".mcp.json"))
	if err != nil || string(data) != teamMCP {
		t.Fatalf("the rig's .mcp.json changed: %q (err=%v)", data, err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc", "mcp-adopted")); !os.IsNotExist(err) {
		t.Fatalf("the dry run took an adoption snapshot, stat err = %v", err)
	}
}
