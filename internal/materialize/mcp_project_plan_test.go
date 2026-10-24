package materialize

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// treeSnapshot maps every path under root to its content ("<dir>" for
// directories), so a test can assert nothing was written.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			out[rel] = "<dir>"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// existingProviderConfig is a hand-written provider-native file holding the
// user's own MCP server and, where the format allows, other settings.
var existingProviderConfig = map[string]string{
	MCPProviderClaude:   `{"mcpServers":{"team":{"command":"team-mcp"}}}`,
	MCPProviderCodex:    "model = \"o3\"\n\n[mcp_servers.team]\ncommand = \"team-mcp\"\n",
	MCPProviderGemini:   `{"theme":"dark","mcpServers":{"team":{"command":"team-mcp"}}}`,
	MCPProviderOpenCode: `{"theme":"dark","mcp":{"team":{"type":"local","command":["team-mcp"]}}}`,
	MCPProviderMimoCode: `{"theme":"dark","mcp":{"team":{"type":"local","command":["team-mcp"]}}}`,
	MCPProviderCursor:   `{"theme":"dark","mcpServers":{"team":{"command":"team-mcp"}}}`,
}

func TestMCPProjectionPlanWritesNothing(t *testing.T) {
	for provider, existing := range existingProviderConfig {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			proj, err := BuildMCPProjection(provider, root, []MCPServer{{Name: "notes", Transport: MCPTransportStdio, Command: "uvx"}})
			if err != nil {
				t.Fatalf("BuildMCPProjection: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(proj.Target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(proj.Target, []byte(existing), 0o644); err != nil {
				t.Fatal(err)
			}
			before := treeSnapshot(t, root)

			plan, err := proj.Plan(fsys.OSFS{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if after := treeSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("Plan wrote to the filesystem:\nbefore: %v\nafter:  %v", before, after)
			}
			want := MCPProjectionPlan{
				Provider:    provider,
				Target:      proj.Target,
				Servers:     1,
				Action:      MCPPlanUpdate,
				Adopt:       true,
				SnapshotDir: filepath.Join(root, ".gc", "mcp-adopted", provider),
			}
			if plan != want {
				t.Fatalf("plan = %+v\nwant   %+v", plan, want)
			}
		})
	}
}

// Plan predicts each step Apply then takes, through create, update, a
// converged pass, and removal.
func TestMCPProjectionPlanMatchesApply(t *testing.T) {
	for provider := range existingProviderConfig {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			step := func(servers []MCPServer, want MCPPlanAction) {
				t.Helper()
				proj, err := BuildMCPProjection(provider, root, servers)
				if err != nil {
					t.Fatalf("BuildMCPProjection: %v", err)
				}
				plan, err := proj.Plan(fsys.OSFS{})
				if err != nil {
					t.Fatalf("Plan: %v", err)
				}
				if plan.Action != want || plan.Adopt {
					t.Fatalf("plan = %+v, want action %q and no adoption", plan, want)
				}
				_, statErr := os.Stat(proj.Target)
				existedBefore := statErr == nil
				if err := proj.Apply(fsys.OSFS{}); err != nil {
					t.Fatalf("Apply: %v", err)
				}
				_, statErr = os.Stat(proj.Target)
				existsAfter := statErr == nil
				switch want {
				case MCPPlanCreate:
					if existedBefore || !existsAfter {
						t.Fatalf("create: existed before %v, after %v", existedBefore, existsAfter)
					}
				case MCPPlanRemove:
					if !existedBefore || existsAfter {
						t.Fatalf("remove: existed before %v, after %v", existedBefore, existsAfter)
					}
				}
				again, err := proj.Plan(fsys.OSFS{})
				if err != nil {
					t.Fatalf("Plan after Apply: %v", err)
				}
				wantAfter := MCPPlanUnchanged
				if len(servers) == 0 {
					wantAfter = MCPPlanNone
				}
				if again.Action != wantAfter {
					t.Fatalf("plan after Apply = %q, want %q", again.Action, wantAfter)
				}
			}
			notes := MCPServer{Name: "notes", Transport: MCPTransportStdio, Command: "uvx"}
			docs := MCPServer{Name: "docs", Transport: MCPTransportStdio, Command: "docs-mcp"}
			step([]MCPServer{notes}, MCPPlanCreate)
			step([]MCPServer{notes, docs}, MCPPlanUpdate)
			step(nil, MCPPlanRemove)
		})
	}
}

// Apply saves a target gc does not manage before replacing it, so it fails
// on one it cannot read. Plan returns that error instead of planning an
// adoption, and writes nothing.
func TestMCPProjectionPlanReturnsApplysErrorForAnUnreadableTarget(t *testing.T) {
	for provider := range existingProviderConfig {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			proj, err := BuildMCPProjection(provider, root, []MCPServer{{Name: "notes", Transport: MCPTransportStdio, Command: "uvx"}})
			if err != nil {
				t.Fatalf("BuildMCPProjection: %v", err)
			}
			// A directory where the file should be: no one can read it as a
			// file, whoever runs the test.
			if err := os.MkdirAll(proj.Target, 0o755); err != nil {
				t.Fatal(err)
			}
			before := treeSnapshot(t, root)
			plan, planErr := proj.Plan(fsys.OSFS{})
			if after := treeSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("Plan wrote to the filesystem:\nbefore: %v\nafter:  %v", before, after)
			}
			applyErr := proj.Apply(fsys.OSFS{})
			if planErr == nil || applyErr == nil || planErr.Error() != applyErr.Error() {
				t.Fatalf("Plan = %+v, error %v; Apply error = %v; want Apply's error from both", plan, planErr, applyErr)
			}
			if !strings.Contains(planErr.Error(), "for adoption snapshot") {
				t.Fatalf("Plan error = %v", planErr)
			}
		})
	}
}

func TestMCPProjectionPlanReturnsApplyValidationErrors(t *testing.T) {
	root := t.TempDir()
	proj, err := BuildMCPProjection(MCPProviderCursor, root, []MCPServer{{Name: "search", Transport: MCPTransportHTTP, URL: "https://mcp.example.test/search"}})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	_, planErr := proj.Plan(fsys.OSFS{})
	applyErr := proj.Apply(fsys.OSFS{})
	if planErr == nil || applyErr == nil || planErr.Error() != applyErr.Error() {
		t.Fatalf("Plan error = %v, Apply error = %v; want the same validation error", planErr, applyErr)
	}
	if !strings.Contains(planErr.Error(), "does not support http server") {
		t.Fatalf("Plan error = %v", planErr)
	}
}
