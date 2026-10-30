package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestPrintDryRunPreview(t *testing.T) {
	ds := map[string]TemplateParams{
		"mayor":         {SessionName: "mayor", TemplateName: "mayor", Command: "echo hello"},
		"hw--polecat-1": {SessionName: "hw--polecat-1", TemplateName: "hw/polecat-1", Command: "echo hello"},
		"hw--polecat-2": {SessionName: "hw--polecat-2", TemplateName: "hw/polecat-2", Command: "echo hello"},
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "hw", MaxActiveSessions: intPtr(1)},
			{Name: "worker", Suspended: true},
		},
	}

	var stdout bytes.Buffer
	printDryRunPreview(ds, cfg, "test", nil, &stdout)
	out := stdout.String()

	if !strings.Contains(out, "3 agent(s) would start") {
		t.Errorf("should report 3 agents, got:\n%s", out)
	}
	if !strings.Contains(out, "mayor") {
		t.Errorf("should list mayor, got:\n%s", out)
	}
	if !strings.Contains(out, "hw/polecat-1") {
		t.Errorf("should list hw/polecat-1, got:\n%s", out)
	}
	if !strings.Contains(out, "1 agent(s) suspended") {
		t.Errorf("should mention 1 suspended, got:\n%s", out)
	}
	if !strings.Contains(out, dryRunFooter) {
		t.Errorf("should show dry-run footer, got:\n%s", out)
	}
}

func TestPrintDryRunPreviewEmpty(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "empty"},
	}

	var stdout bytes.Buffer
	printDryRunPreview(nil, cfg, "empty", nil, &stdout)
	out := stdout.String()

	if !strings.Contains(out, "0 agent(s) would start") {
		t.Errorf("should report 0 agents, got:\n%s", out)
	}
	if !strings.Contains(out, "(no agents to start)") {
		t.Errorf("should show empty message, got:\n%s", out)
	}
}

func TestStartDryRunFlagExists(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStartCmd(&stdout, &stderr)
	f := cmd.Flags().Lookup("dry-run")
	if f == nil {
		t.Fatal("missing --dry-run flag")
	}
	if f.Shorthand != "n" {
		t.Errorf("--dry-run shorthand = %q, want %q", f.Shorthand, "n")
	}
}

// dryRunTreeSnapshot maps every path under root to its content, a symlink's
// target, or "<dir>". Under gc's own .gc/ and .beads/ directories it records
// only what a dry run must not touch either (MCP bookkeeping, Claude
// settings, formula links), since the bead-store lifecycle, which a dry run
// still runs, writes the rest (its store files and their events).
func dryRunTreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	watchedInternal := []string{
		".gc/mcp-adopted", ".gc/mcp-managed", ".gc/mcp-locks",
		".gc/settings.json", ".beads/formulas",
	}
	internal := func(rel string) (watched, descend bool) {
		if rel != ".gc" && rel != ".beads" && !strings.HasPrefix(rel, ".gc/") && !strings.HasPrefix(rel, ".beads/") {
			return true, true
		}
		for _, prefix := range watchedInternal {
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true, true
			}
			if strings.HasPrefix(prefix, rel+"/") {
				return false, true
			}
		}
		return false, false
	}
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		slashed := filepath.ToSlash(rel)
		watched, descend := internal(slashed)
		if !watched {
			if d.IsDir() && !descend {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			out[slashed] = "-> " + target
		case d.IsDir():
			out[slashed] = "<dir>"
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[slashed] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// gc start --dry-run previews; it must not write into the city or its rigs.
// Before, stage 1 adopted and rewrote a rig's hand-written .mcp.json (moving
// the original under .gc/mcp-adopted/), linked skills into the provider
// sinks, extended .gitignore, and installed hooks, all while printing "No
// side effects executed". The preview now reports those changes instead.
func TestStartDryRunWritesNothingInTheCityOrItsRigs(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	clearInheritedBeadsEnv(t)
	requireNoLeakedDoltAfterForPaths(t, cityPath, rigPath)
	t.Chdir(t.TempDir())
	// The provider's implicit agents cover the city root and the rig root.
	// scribe has a work dir of its own, and coder, a rig agent whose named
	// session is always on, installs gemini hooks into its work dir: both
	// resolve while the preview lists the agents that would start.
	for _, dir := range []string{
		filepath.Join(cityPath, citylayout.RuntimeRoot), filepath.Join(cityPath, "mcp"), filepath.Join(cityPath, "skills", "plan"),
		filepath.Join(cityPath, "formulas"), filepath.Join(cityPath, "agents", "mayor"), filepath.Join(cityPath, "agents", "scribe"),
		filepath.Join(cityPath, "agents", "coder"), rigPath,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cityTOML := `[workspace]
name = "dry-run-purity"

[beads]
provider = "file"

[providers.claude]
base = "builtin:claude"
command = "echo"
prompt_mode = "none"

[providers.gemini]
base = "builtin:gemini"
command = "echo"
prompt_mode = "none"

[[rigs]]
name = "frontend"
`
	siteTOML := fmt.Sprintf("workspace_name = \"dry-run-purity\"\n\n[[rig]]\nname = \"frontend\"\npath = %q\n", rigPath)
	for path, body := range map[string]string{
		filepath.Join(cityPath, "city.toml"):                      cityTOML,
		filepath.Join(cityPath, ".gc", "site.toml"):               siteTOML,
		filepath.Join(cityPath, "pack.toml"):                      "[pack]\nname = \"dry-run-purity\"\nschema = 2\n\n[[named_session]]\ntemplate = \"coder\"\nscope = \"rig\"\nmode = \"always\"\n",
		filepath.Join(cityPath, "formulas", "hello.toml"):         "formula = \"hello\"\n\n[[steps]]\nid = \"greet\"\ntitle = \"Greet\"\n",
		filepath.Join(cityPath, "agents", "scribe", "agent.toml"): "provider = \"claude\"\nscope = \"city\"\nwork_dir = \"workspaces/scribe\"\n",
		filepath.Join(cityPath, "agents", "scribe", "prompt.md"):  "You are the scribe.\n",
		filepath.Join(cityPath, "agents", "coder", "agent.toml"):  "provider = \"gemini\"\nscope = \"rig\"\ninstall_agent_hooks = [\"gemini\"]\n",
		filepath.Join(cityPath, "agents", "coder", "prompt.md"):   "You are the coder.\n",
		filepath.Join(cityPath, "mcp", "notes.toml"):              "name = \"notes\"\ncommand = \"uvx\"\n",
		filepath.Join(cityPath, "skills", "plan", "SKILL.md"):     "---\nname: plan\ndescription: test\n---\nbody\n",
		filepath.Join(cityPath, "agents", "mayor", "agent.toml"):  "provider = \"claude\"\nscope = \"city\"\n",
		filepath.Join(cityPath, "agents", "mayor", "prompt.md"):   "You are the mayor.\n",
		// The rig's own, hand-written MCP config.
		filepath.Join(rigPath, ".mcp.json"): `{"mcpServers":{"team-db":{"command":"team-db-mcp"}}}` + "\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldBuild := buildSessionProviderByName
	oldDry := dryRunMode
	t.Cleanup(func() {
		buildSessionProviderByName = oldBuild
		dryRunMode = oldDry
	})
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	}
	dryRunMode = true

	cityBefore, rigBefore := dryRunTreeSnapshot(t, cityPath), dryRunTreeSnapshot(t, rigPath)
	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doStartStandalone exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if after := dryRunTreeSnapshot(t, cityPath); !reflect.DeepEqual(cityBefore, after) {
		t.Errorf("dry run wrote into the city:\n%s", describeTreeChanges(cityBefore, after))
	}
	if after := dryRunTreeSnapshot(t, rigPath); !reflect.DeepEqual(rigBefore, after) {
		t.Errorf("dry run wrote into the rig:\n%s", describeTreeChanges(rigBefore, after))
	}

	out := stdout.String()
	for _, want := range []string{
		"Would change in the city and its rigs:",
		// The rig's own server keeps gc from adopting its .mcp.json, so the
		// rig's .gitignore covers only the Gemini settings gc projects there.
		"leave " + filepath.Join(rigPath, ".mcp.json") + ` (claude) as it is: gc does not manage it yet, and adopting it would drop MCP server "team-db"; to let gc manage it, `,
		"create " + filepath.Join(cityPath, ".mcp.json") + " (claude) with 1 MCP server",
		"link 1 skill in " + filepath.Join(cityPath, ".claude", "skills"),
		"create " + filepath.Join(rigPath, ".gitignore") + ": add /.gemini/settings.json\n",
		"frontend/coder",
		"No agents started (--dry-run). Only gc's own state may have been written: the bead store, the pack cache, and .gc/ runtime directories.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(stderr.String(), "adopting provider-native MCP") {
		t.Errorf("dry run adopted an MCP target:\n%s", stderr.String())
	}
}

func describeTreeChanges(before, after map[string]string) string {
	var lines []string
	for path, was := range before {
		if now, ok := after[path]; !ok {
			lines = append(lines, "removed "+path)
		} else if now != was {
			lines = append(lines, "changed "+path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			lines = append(lines, "added "+path)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
