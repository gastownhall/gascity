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
// directories, "-> <target>" for symlinks, which it does not follow), so a
// test can assert nothing was written.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "-> " + target
			return nil
		}
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

// existingProviderConfig is a hand-written provider-native file defining the
// "notes" server the tests project, the same way, and, where the format
// allows, other settings: gc adopts it without dropping anything it does not
// manage, and rewrites it in its own format.
var existingProviderConfig = map[string]string{
	MCPProviderClaude:   `{"mcpServers":{"notes":{"command":"uvx"}}}`,
	MCPProviderCodex:    "model = \"o3\"\n\n[mcp_servers.notes]\ncommand = \"uvx\"\n",
	MCPProviderGemini:   `{"theme":"dark","mcpServers":{"notes":{"command":"uvx"}}}`,
	MCPProviderOpenCode: `{"theme":"dark","mcp":{"notes":{"type":"local","command":["uvx"]}}}`,
	MCPProviderMimoCode: `{"theme":"dark","mcp":{"notes":{"type":"local","command":["uvx"]}}}`,
	MCPProviderCursor:   `{"theme":"dark","mcpServers":{"notes":{"command":"uvx"}}}`,
}

// teamProviderConfig is a hand-written provider-native file holding a server
// of the user's own, "team", which no catalog projects.
var teamProviderConfig = map[string]string{
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

// gc leaves a target it cannot read alone: for every provider, Plan reports
// the refusal Apply returns, and neither writes anything.
func TestMCPProjectionPlanReportsApplysRefusalOfAnUnreadableTarget(t *testing.T) {
	for provider := range existingProviderConfig {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			proj := notesProjection(t, provider, root)
			// A directory where the file should be: no one can read it as a
			// file, whoever runs the test.
			if err := os.MkdirAll(proj.Target, 0o755); err != nil {
				t.Fatal(err)
			}
			refused := assertRefusedAndUntouched(t, proj, root)
			if refused.ReadErr == nil || !strings.Contains(refused.Error(), "gc cannot read it") {
				t.Fatalf("refusal = %v, want a read refusal", refused)
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
