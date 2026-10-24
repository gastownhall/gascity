package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

const teamMCPJSON = `{"mcpServers":{"team-db":{"command":"team-db-mcp"}}}` + "\n"

// A city-level MCP server reaches every rig. A rig repository that commits
// its own .mcp.json keeps it: gc does not adopt a file that holds servers it
// does not manage, and says which agents start without gc's servers there.
func TestRunStage1MCPProjectionLeavesAFileWithTheUsersOwnServersAlone(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude"),
		Rigs:       []config.Rig{{Name: "frontend", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "coder", Dir: "frontend", Provider: "claude"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(rigPath, ".mcp.json"))
	if err != nil || string(data) != teamMCPJSON {
		t.Fatalf("the rig's own .mcp.json changed: %q (err=%v)", data, err)
	}
	for _, rel := range []string{".gc/mcp-adopted", ".gc/mcp-managed", ".gc/mcp-locks", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(rigPath, rel)); !os.IsNotExist(err) {
			t.Fatalf("refused adoption wrote %s in the rig (stat err = %v)", rel, err)
		}
	}
	if _, ok := mcpServersInFile(t, filepath.Join(cityPath, ".mcp.json"))["notes"]; !ok {
		t.Fatal("the city root was not projected")
	}
	want := `gc: warning: agent "frontend/coder" starts without gc's MCP server "notes": not adopting ` + filepath.Join(rigPath, ".mcp.json") +
		`: gc does not manage it yet, and adopting it would drop MCP server "team-db"; to let gc manage it, ` +
		"define the servers you still need in gc's MCP catalog (an mcp/ directory) and remove those entries from the file\n"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("warning missing %q:\n%s", want, stderr.String())
	}

	stderr.Reset()
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("second runStage1MCPProjection: %v", err)
	}
	if strings.Contains(stderr.String(), "not adopting") {
		t.Fatalf("an unchanged refusal warned again:\n%s", stderr.String())
	}

	// Once the catalog defines the team server the way the file does, the
	// file holds nothing gc does not manage, and gc adopts it.
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "team-db.toml"), `
name = "team-db"
command = "team-db-mcp"
`)
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("third runStage1MCPProjection: %v", err)
	}
	servers := mcpServersInFile(t, filepath.Join(rigPath, ".mcp.json"))
	if _, ok := servers["notes"]; !ok {
		t.Fatalf("rig .mcp.json not adopted once its servers are in the catalog: %v", servers)
	}
	if _, ok := servers["team-db"]; !ok {
		t.Fatalf("adopted rig .mcp.json lost team-db: %v", servers)
	}
}

// gc's .gitignore entries cover the files it projects and no others: at a
// root where it leaves the Claude file alone but projects Gemini's, only
// .gemini/settings.json is ignored. The dry-run plan says the same.
func TestStage1MCPGitignoreCoversOnlyTheFilesGCProjects(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude", "gemini"),
		Rigs:       []config.Rig{{Name: "frontend", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "coder", Dir: "frontend", Provider: "claude"},
			{Name: "analyst", Dir: "frontend", Provider: "gemini"},
		},
	}

	lines, err := planStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard)
	if err != nil {
		t.Fatalf("planStage1MCPProjection: %v", err)
	}
	plan := strings.Join(lines, "\n")
	if want := "create " + filepath.Join(rigPath, ".gitignore") + ": add .gemini/settings.json\n"; !strings.Contains(plan+"\n", want) {
		t.Fatalf("plan missing %q:\n%s", want, plan)
	}
	if want := "create " + filepath.Join(cityPath, ".gitignore") + ": add .mcp.json\n"; !strings.Contains(plan+"\n", want) {
		t.Fatalf("plan missing %q:\n%s", want, plan)
	}

	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	for root, want := range map[string]string{
		rigPath:  "# Gas City\n.gemini/settings.json\n",
		cityPath: "# Gas City\n.mcp.json\n",
	} {
		got, err := os.ReadFile(filepath.Join(root, ".gitignore"))
		if err != nil || string(got) != want {
			t.Fatalf("%s/.gitignore = %q (err %v), want %q", root, got, err, want)
		}
	}
	if lines, err := planStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil || strings.Contains(strings.Join(lines, "\n"), ".gitignore") {
		t.Fatalf("plan after the pass = %q (err %v), want no .gitignore change", lines, err)
	}
}

// A target that fails fails the pass, but the files gc projected before it
// stay ignored: .gitignore is written on the way out.
func TestRunStage1MCPProjectionIgnoresWhatItProjectedWhenALaterTargetFails(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".gc", "mcp-managed"), 0o755); err != nil {
		t.Fatal(err)
	}
	// gc manages the rig's .mcp.json, which has since become a symlink:
	// projecting there fails, after the city root (which sorts first).
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rigPath, ".mcp.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".gc", "mcp-managed", "claude.json"), []byte(`{"managed_by":"gc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude"),
		Rigs:       []config.Rig{{Name: "frontend", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "coder", Dir: "frontend", Provider: "claude"},
		},
	}
	err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "symlinked path") {
		t.Fatalf("runStage1MCPProjection = %v, want the rig target's symlink error", err)
	}
	if _, ok := mcpServersInFile(t, filepath.Join(cityPath, ".mcp.json"))["notes"]; !ok {
		t.Fatal("the city root was not projected before the failure")
	}
	if got, err := os.ReadFile(filepath.Join(cityPath, ".gitignore")); err != nil || string(got) != "# Gas City\n.mcp.json\n" {
		t.Fatalf("city .gitignore = %q (err %v), want the projected .mcp.json ignored", got, err)
	}
}

// A project may link one provider's MCP file to another's. With no MCP
// servers in scope, gc has nothing to write there and must not fail the
// city's start over the link; with servers in scope, it leaves the link
// alone and warns.
func TestRunStage1MCPProjectionLeavesASymlinkedProviderFileAlone(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".mcp.json"), filepath.Join(cityPath, ".cursor", "mcp.json")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Session:   config.SessionConfig{Provider: "tmux"},
		Providers: builtinProviderAliasesForTest("cursor"),
		Agents:    []config.Agent{{Name: "editor", Scope: "city", Provider: "cursor"}},
	}
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("runStage1MCPProjection with no MCP servers in scope: %v", err)
	}

	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg.PackMCPDir = filepath.Join(cityPath, "mcp")
	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection with a server in scope: %v", err)
	}
	if want := filepath.Join(cityPath, ".cursor", "mcp.json") + " is a symlink"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("warning missing %q:\n%s", want, stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(cityPath, ".mcp.json")); err != nil || string(data) != teamMCPJSON {
		t.Fatalf("the linked .mcp.json changed: %q (err=%v)", data, err)
	}
}

// The stage-2 pre-start leaves a work dir's own MCP config alone the same
// way, and lets the session start.
func TestInternalProjectMCPLeavesTheWorkDirsOwnConfigAlone(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[providers.claude]
base = "builtin:claude"
command = "echo"
prompt_mode = "none"
`, `
provider = "claude"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "uvx"
`)
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"internal", "project-mcp", "--agent", "mayor", "--workdir", workdir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (the session starts): stderr=%q", code, stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(workdir, ".mcp.json")); err != nil || string(data) != teamMCPJSON {
		t.Fatalf("the work dir's own .mcp.json changed: %q (err=%v)", data, err)
	}
	want := `gc internal project-mcp: warning: agent "mayor" starts without gc's MCP server "notes": not adopting ` + filepath.Join(workdir, ".mcp.json")
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("refused adoption wrote .gitignore in the work dir (stat err = %v)", err)
	}
}

// A runtime discards a successful pre-start's output, so resolveTemplate,
// which wires the stage-2 pre-start, also says when the work dir's file will
// be left alone: once per process, where the controller logs.
func TestResolveTemplateWarnsWhenAStage2TargetIsNotAdopted(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	workDir := filepath.Join(cityPath, ".gc", "worktrees", "worker-1")
	team := `{"theme":"dark","mcpServers":{"team-db":{"command":"team-db-mcp"}}}`
	if err := os.MkdirAll(filepath.Join(workDir, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".gemini", "settings.json"), []byte(team), 0o644); err != nil {
		t.Fatal(err)
	}
	cityCfg := &config.City{
		Workspace:  config.Workspace{Provider: "gemini"},
		Providers:  map[string]config.ProviderSpec{"gemini": {Command: "echo", PromptMode: "none"}},
		PackMCPDir: filepath.Join(cityPath, "mcp"),
	}
	var stderr bytes.Buffer
	params := &agentBuildParams{
		city:            cityCfg,
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &cityCfg.Workspace,
		providers:       cityCfg.Providers,
		lookPath:        stubLookPath,
		fs:              fsys.OSFS{},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          &stderr,
		sessionProvider: "tmux",
	}
	agent := &config.Agent{Name: "worker", Scope: "city", Provider: "gemini", WorkDir: ".gc/worktrees/worker-1"}
	for range 2 {
		if _, err := resolveTemplate(params, agent, agent.QualifiedName(), nil); err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
	}
	want := `gc: warning: agent "worker" starts without gc's MCP server "notes": not adopting ` +
		filepath.Join(workDir, ".gemini", "settings.json") + `: gc does not manage it yet, and adopting it would drop MCP server "team-db"`
	if got := strings.Count(stderr.String(), want); got != 1 {
		t.Fatalf("warning %q appeared %d times, want once:\n%s", want, got, stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(workDir, ".gemini", "settings.json")); err != nil || string(data) != team {
		t.Fatalf("resolveTemplate changed the work dir's settings: %q (err %v)", data, err)
	}
}

func TestMCPAdoptionDoctorCheck(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude", "gemini"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "analyst", Scope: "city", Provider: "gemini"},
			// Stage-2 agents: one whose work dir holds a file of its own,
			// one whose work dir does not exist yet.
			{Name: "worker", Scope: "city", Provider: "claude", WorkDir: "workspaces/worker"},
			{Name: "fresh", Scope: "city", Provider: "claude", WorkDir: "workspaces/fresh"},
		},
	}
	check := newMCPAdoptionDoctorCheck(cityPath, cfg, stubLookPath)
	if result := check.Run(&doctor.CheckContext{CityPath: cityPath}); result.Status != doctor.StatusOK {
		t.Fatalf("status = %v (%s), want OK with nothing to adopt", result.Status, result.Message)
	}

	// The city's .mcp.json holds a server of the user's own: gc leaves it.
	if err := os.WriteFile(filepath.Join(cityPath, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// So does the worker's work dir.
	workerDir := filepath.Join(cityPath, "workspaces", "worker")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, ".mcp.json"), []byte(teamMCPJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// The gemini settings were adopted by an earlier gc that replaced the
	// user's server; its definition survives only in the snapshot.
	snapshotDir := filepath.Join(cityPath, ".gc", "mcp-adopted", "gemini")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "20260101T000000Z.json"), []byte(`{"mcpServers":{"wiki":{"command":"wiki-mcp"},"notes":{"command":"old"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, io.Discard); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(cityPath, "workspaces", "fresh")); err != nil {
		t.Fatal(err)
	}

	result := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		`agent "mayor" starts without gc's MCP server "notes": not adopting ` + filepath.Join(cityPath, ".mcp.json"),
		`agent "worker" starts without gc's MCP server "notes": not adopting ` + filepath.Join(workerDir, ".mcp.json"),
		filepath.Join(cityPath, ".gemini", "settings.json") + ` (gemini): gc adopted this file and replaced MCP servers it held that gc does not project ("wiki")`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	if strings.Contains(details, `"notes")`) {
		t.Fatalf("a server gc projects is reported as dropped:\n%s", details)
	}
	if _, err := os.Stat(filepath.Join(cityPath, "workspaces", "fresh")); !os.IsNotExist(err) {
		t.Fatalf("the doctor check created a work dir (stat err = %v)", err)
	}
}

// gc mcp list shows the servers an agent's config would carry. When gc leaves
// that file alone, it says so, and why, in text and JSON.
func TestMcpListReportsAFileGCLeavesAlone(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeProjectedMCPCity(t, cityDir, `[beads]
provider = "file"

[session]
provider = "tmux"

[providers.gemini]
command = "echo"
prompt_mode = "none"
`, `
provider = "gemini"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "uvx"
`)
	writeCatalogFile(t, cityDir, ".gemini/settings.json", `{"mcpServers":{"team-db":{"command":"team-db-mcp"},"notes":{"command":"team-notes"}}}`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"mcp", "list", "--agent", "mayor"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc mcp list exited %d: %s", code, stderr.String())
	}
	want := "Not delivered: gc leaves " + filepath.Join(cityDir, ".gemini", "settings.json") +
		` as it is, so the agent starts without these servers: gc does not manage it yet, and adopting it would drop MCP server "team-db" and its own definition of MCP server "notes"; to let gc manage it,`
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("mcp list output missing %q:\n%s", want, stdout.String())
	}

	stdout.Reset()
	if code := run([]string{"mcp", "list", "--agent", "mayor", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc mcp list --json exited %d: %s", code, stderr.String())
	}
	contentOpValidate(t, "schemas/mcp/list/result.schema.json", stdout.String())
	var got projectedMCPJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	notAdopted := got.Projection.NotAdopted
	if notAdopted == nil || strings.Join(notAdopted.UnmanagedServers, ",") != "team-db" ||
		strings.Join(notAdopted.RedefinedServers, ",") != "notes" || notAdopted.Remedy == "" {
		t.Fatalf("projection.not_adopted = %+v", notAdopted)
	}
}

// mcpServersInFile returns the mcpServers object of a Claude-shaped MCP file.
func mcpServersInFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", path, err, data)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	return servers
}
