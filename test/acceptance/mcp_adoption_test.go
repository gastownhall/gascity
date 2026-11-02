//go:build acceptance_a

// MCP adoption acceptance tests.
//
// gc projects MCP servers into provider-native files such as .mcp.json. A
// work dir that already has such a file with servers of its own (a repository
// that commits its team's MCP config) keeps it: gc does not adopt a file it
// would have to drop servers from.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestProjectMCPLeavesARepositorysOwnMCPConfigAlone(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitNoStart("claude")
	if err := os.MkdirAll(filepath.Join(c.Dir, "mcp"), 0o755); err != nil {
		t.Fatalf("creating mcp/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "mcp", "notes.toml"), []byte("name = \"notes\"\ncommand = \"notes-mcp\"\n"), 0o644); err != nil {
		t.Fatalf("writing the city's MCP server: %v", err)
	}

	workdir := filepath.Join(helpers.TempDir(t), "repo")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("creating work dir: %v", err)
	}
	teamMCP := `{"mcpServers":{"team-db":{"command":"team-db-mcp"}}}` + "\n"
	if err := os.WriteFile(filepath.Join(workdir, ".mcp.json"), []byte(teamMCP), 0o644); err != nil {
		t.Fatalf("writing the work dir's .mcp.json: %v", err)
	}
	git(t, workdir, "init")
	git(t, workdir, "add", ".")
	git(t, workdir, "commit", "-m", "team MCP config")

	out, err := c.GC("internal", "project-mcp", "--agent", "mayor", "--workdir", workdir)
	if err != nil {
		t.Fatalf("gc internal project-mcp failed; the session must still start: %v\n%s", err, out)
	}
	if !strings.Contains(out, `agent "mayor" starts without gc's MCP server "notes": not adopting `+filepath.Join(workdir, ".mcp.json")) ||
		!strings.Contains(out, `MCP server "team-db"`) {
		t.Fatalf("project-mcp did not explain the refusal:\n%s", out)
	}
	if status := git(t, workdir, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("project-mcp changed the repository:\n%s", status)
	}
}
