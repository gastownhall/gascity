package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestPartitionAgentMCPCatalog(t *testing.T) {
	stdio := func(name, layer string) materialize.MCPServer {
		return materialize.MCPServer{Name: name, Transport: materialize.MCPTransportStdio, Command: "uvx", Layer: layer, Origin: layer}
	}
	http := func(name, layer string) materialize.MCPServer {
		return materialize.MCPServer{Name: name, Transport: materialize.MCPTransportHTTP, URL: "https://mcp.example.test/" + name, Layer: layer, Origin: layer}
	}
	catalogOf := func(servers ...materialize.MCPServer) materialize.MCPCatalog {
		catalog := materialize.MCPCatalog{ByName: map[string]materialize.MCPServer{}, ByLayer: map[string][]materialize.MCPServer{}}
		for _, server := range servers {
			catalog.Servers = append(catalog.Servers, server)
			catalog.ByName[server.Name] = server
			catalog.ByLayer[server.Layer] = append(catalog.ByLayer[server.Layer], server)
			catalog.RawOrder = append(catalog.RawOrder, server.Name)
		}
		return catalog
	}

	tests := []struct {
		name        string
		provider    string
		catalog     materialize.MCPCatalog
		wantKept    []string
		wantSkipped []string
		wantErr     any
		errContains []string
	}{
		{
			name:     "claude receives stdio and http",
			provider: "claude",
			catalog:  catalogOf(http("search", "import"), stdio("notes", "city")),
			wantKept: []string{"search", "notes"},
		},
		{
			name:        "cursor skips a shared http server and keeps stdio",
			provider:    "cursor",
			catalog:     catalogOf(stdio("notes", "city"), http("search", "import")),
			wantKept:    []string{"notes"},
			wantSkipped: []string{"search"},
		},
		{
			name:        "cursor skips http from the city pack and the rig pack alike",
			provider:    "cursor",
			catalog:     catalogOf(http("city-search", "city"), http("rig-search", "rig")),
			wantSkipped: []string{"city-search", "rig-search"},
		},
		{
			name:        "cursor errors on an http server in the agent's own mcp dir",
			provider:    "cursor",
			catalog:     catalogOf(http("private", materialize.MCPLayerAgent), http("search", "import")),
			wantErr:     &unsupportedMCPTransportError{},
			errContains: []string{"private (http)", `"cursor" harness`, "only stdio"},
		},
		{
			name:        "copilot skips every shared server",
			provider:    "copilot",
			catalog:     catalogOf(stdio("notes", "city"), http("search", "import")),
			wantSkipped: []string{"notes", "search"},
		},
		{
			name:        "copilot errors on servers in the agent's own mcp dir",
			provider:    "copilot",
			catalog:     catalogOf(stdio("private", materialize.MCPLayerAgent), stdio("notes", "city")),
			wantErr:     &unsupportedMCPHarnessError{},
			errContains: []string{"[private]", `"copilot" harness`},
		},
		{
			name:     "empty catalog on an unsupported harness is fine",
			provider: "copilot",
			catalog:  catalogOf(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, skipped, err := partitionAgentMCPCatalog(tt.provider, tt.catalog)
			if tt.wantErr != nil {
				switch tt.wantErr.(type) {
				case *unsupportedMCPTransportError:
					var want *unsupportedMCPTransportError
					if !errors.As(err, &want) {
						t.Fatalf("err = %v, want *unsupportedMCPTransportError", err)
					}
				case *unsupportedMCPHarnessError:
					var want *unsupportedMCPHarnessError
					if !errors.As(err, &want) {
						t.Fatalf("err = %v, want *unsupportedMCPHarnessError", err)
					}
				}
				if !isAgentMCPCapabilityError(err) {
					t.Fatalf("isAgentMCPCapabilityError(%v) = false", err)
				}
				for _, want := range tt.errContains {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error missing %q: %v", want, err)
					}
				}
				if strings.Contains(err.Error(), "search") || strings.Contains(err.Error(), "notes") {
					t.Fatalf("error names a shared server: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("partitionAgentMCPCatalog: %v", err)
			}
			var keptNames []string
			for _, server := range kept.Servers {
				keptNames = append(keptNames, server.Name)
				if _, ok := kept.ByName[server.Name]; !ok {
					t.Fatalf("kept ByName missing %q", server.Name)
				}
			}
			if strings.Join(keptNames, ",") != strings.Join(tt.wantKept, ",") {
				t.Fatalf("kept = %v, want %v", keptNames, tt.wantKept)
			}
			if len(kept.ByName) != len(kept.Servers) || len(kept.RawOrder) != len(kept.Servers) {
				t.Fatalf("kept catalog not narrowed consistently: %+v", kept)
			}
			var skippedNames []string
			for _, skip := range skipped {
				skippedNames = append(skippedNames, skip.Server.Name)
				if skip.Reason == "" {
					t.Fatalf("skip %q has no reason", skip.Server.Name)
				}
			}
			if strings.Join(skippedNames, ",") != strings.Join(tt.wantSkipped, ",") {
				t.Fatalf("skipped = %v, want %v", skippedNames, tt.wantSkipped)
			}
		})
	}
}

func TestDescribeMCPOrigin(t *testing.T) {
	for origin, want := range map[string]string{
		"import:search":    `import "search"`,
		"rig-import:tools": `rig import "tools"`,
		"implicit:shared":  `implicit import "shared"`,
		"bootstrap:core":   `bootstrap import "core"`,
		"import":           "an import",
		"rig-import":       "a rig import",
		"implicit":         "an implicit import",
		"bootstrap":        "a bootstrap import",
		"city":             "the city pack",
		"rig":              "the rig pack",
		"agent":            "the agent's mcp/ directory",
	} {
		if got := describeMCPOrigin(materialize.MCPServer{Origin: origin}); got != want {
			t.Errorf("describeMCPOrigin(%q) = %q, want %q", origin, got, want)
		}
	}
}

// writeHTTPMCPPack writes a pack whose mcp/ provides one remote HTTP server,
// the shape a third-party pack ships.
func writeHTTPMCPPack(t *testing.T, dir, server string) {
	t.Helper()
	writeMCPSource(t, filepath.Join(dir, "mcp", server+".toml"), `
name = "`+server+`"
url = "https://mcp.example.test/`+server+`"
`)
}

// A city-level import attaches its MCP servers to every agent in the city.
// When one agent's provider cannot receive a server's transport (cursor's
// projection is stdio-only), stage 1 must project the rest of the city, give
// that agent its stdio servers, and warn that it starts without the server.
func TestRunStage1MCPProjectionSkipsSharedServerTheProviderCannotReceive(t *testing.T) {
	cityPath := t.TempDir()
	packDir := filepath.Join(t.TempDir(), "search-pack")
	writeHTTPMCPPack(t, packDir, "search")
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
args = ["notes-mcp"]
`)
	cfg := &config.City{
		Workspace:                 config.Workspace{Name: "my-city"},
		PackMCPDir:                filepath.Join(cityPath, "mcp"),
		ExplicitImportPackDirs:    []string{packDir},
		ExplicitImportMCPBindings: map[string]string{packDir: "search"},
		Session:                   config.SessionConfig{Provider: "tmux"},
		Providers:                 builtinProviderAliasesForTest("claude", "cursor"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "editor", Scope: "city", Provider: "cursor"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}

	claude := readMCPServersDoc(t, filepath.Join(cityPath, ".mcp.json"))
	if _, ok := claude["search"]; !ok {
		t.Fatalf("claude projection lost the http server: %v", claude)
	}
	if _, ok := claude["notes"]; !ok {
		t.Fatalf("claude projection lost the stdio server: %v", claude)
	}
	cursor := readMCPServersDoc(t, filepath.Join(cityPath, ".cursor", "mcp.json"))
	if _, ok := cursor["search"]; ok {
		t.Fatalf("cursor projection carries the http server it cannot receive: %v", cursor)
	}
	if _, ok := cursor["notes"]; !ok {
		t.Fatalf("cursor projection lost its stdio server: %v", cursor)
	}

	// The warning names its city: a supervisor's log interleaves every city
	// it runs.
	want := `gc: warning: city "my-city": agent "editor" starts without MCP server "search" (http, from import "search"): gc projects only stdio MCP servers for the "cursor" harness`
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("warning missing\n want: %s\n got: %s", want, stderr.String())
	}
	if strings.Contains(stderr.String(), `"mayor"`) {
		t.Fatalf("warning names an agent that receives every server:\n%s", stderr.String())
	}

	stderr.Reset()
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("second runStage1MCPProjection: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unchanged skip re-warned on the next tick:\n%s", stderr.String())
	}
}

// The same applies to a server a rig import attaches to the rig's agents.
func TestRunStage1MCPProjectionSkipsRigImportServerTheProviderCannotReceive(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	packDir := filepath.Join(t.TempDir(), "search-pack")
	writeHTTPMCPPack(t, packDir, "search")
	cfg := &config.City{
		Rigs:                 []config.Rig{{Name: "frontend", Path: rigPath}},
		RigImportPackDirs:    map[string][]string{"frontend": {packDir}},
		RigImportMCPBindings: map[string]map[string]string{"frontend": {packDir: "search"}},
		Session:              config.SessionConfig{Provider: "tmux"},
		Providers:            builtinProviderAliasesForTest("claude", "cursor"),
		Agents: []config.Agent{
			{Name: "coder", Dir: "frontend", Provider: "claude"},
			{Name: "editor", Dir: "frontend", Provider: "cursor"},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	if _, ok := readMCPServersDoc(t, filepath.Join(rigPath, ".mcp.json"))["search"]; !ok {
		t.Fatal("rig claude projection lost the rig-import server")
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".cursor", "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("cursor target written with nothing it can receive, stat err = %v", err)
	}
	want := `agent "frontend/editor" starts without MCP server "search" (http, from rig import "search")`
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("warning missing %q:\n%s", want, stderr.String())
	}
}

// A server in the agent's own mcp/ directory targets it explicitly: an
// unreceivable one stops that agent (its session start fails), and stage 1
// skips it with a warning while projecting the rest of the city.
func TestRunStage1MCPProjectionSkipsAgentDeclaringServerItsProviderCannotReceive(t *testing.T) {
	cityPath := t.TempDir()
	editorMCP := filepath.Join(cityPath, "agents", "editor", "mcp")
	writeHTTPMCPPack(t, filepath.Dir(editorMCP), "private")
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("claude", "cursor"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "editor", Scope: "city", Provider: "cursor", MCPDir: editorMCP},
		},
	}

	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	if _, ok := readMCPServersDoc(t, filepath.Join(cityPath, ".mcp.json"))["notes"]; !ok {
		t.Fatal("the rest of the city was not projected")
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".cursor", "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("skipped agent's target was written, stat err = %v", err)
	}
	for _, want := range []string{`agent "editor" cannot start: MCP servers [private (http)]`, "own mcp/ directory"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("warning missing %q:\n%s", want, stderr.String())
		}
	}
}

// readMCPServersDoc returns the mcpServers object of a Claude- or
// Cursor-shaped MCP config.
func readMCPServersDoc(t *testing.T, path string) map[string]any {
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

// cursorAgentCity writes a city whose only agent, mayor, runs on a provider
// in the cursor family, plus a city-pack http server and a stdio server.
func cursorAgentCity(t *testing.T) string {
	t.Helper()
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeProjectedMCPCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "tmux"

[providers.editor]
base = "builtin:cursor"
command = "echo"
prompt_mode = "none"
`, `
provider = "editor"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "mcp/search.toml", `
name = "search"
url = "https://mcp.example.test/search"
`)
	writeCatalogFile(t, cityDir, "mcp/notes.toml", `
name = "notes"
command = "uvx"
`)
	return cityDir
}

func TestMcpListAgentShowsServersItsProviderCannotReceive(t *testing.T) {
	cityDir := cursorAgentCity(t)
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"mcp", "list", "--agent", "mayor"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc mcp list --agent exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	projected, notProjected, ok := strings.Cut(out, "Not projected")
	if !ok {
		t.Fatalf("listing has no not-projected section:\n%s", out)
	}
	if strings.Contains(projected, "search") || !strings.Contains(projected, "notes") {
		t.Fatalf("projected section must list notes and not search:\n%s", out)
	}
	for _, want := range []string{`gc projects only stdio MCP servers for the "cursor" harness`, "search", "http", "mcp/search.toml"} {
		if !strings.Contains(notProjected, want) {
			t.Fatalf("not-projected section missing %q:\n%s", want, out)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"mcp", "list", "--agent", "mayor", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc mcp list --agent --json exited %d: %s", code, stderr.String())
	}
	validateManagementJSONPayload(t, []string{"mcp", "list"}, &stdout)
	var got projectedMCPJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if len(got.Servers) != 1 || got.Servers[0].Name != "notes" {
		t.Fatalf("servers = %+v, want only notes", got.Servers)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Name != "search" || got.Skipped[0].Transport != "http" ||
		got.Skipped[0].Origin != "city" || !strings.Contains(got.Skipped[0].Reason, "only stdio") {
		t.Fatalf("skipped = %+v, want search with its reason", got.Skipped)
	}
}

func TestMCPConfigDoctorCheckWarnsAboutServersAnAgentStartsWithout(t *testing.T) {
	cityDir := cursorAgentCity(t)
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	result := newMCPConfigDoctorCheck(cityDir, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %+v", result.Status, result)
	}
	// "editor" is the provider's implicit agent, which the user never
	// defined; it skips the same server, and the warning says what it is.
	if !strings.Contains(result.Message, `agents "editor" (the implicit agent for provider "editor"), "mayor" start without MCP server "search" (http, from the city pack)`) {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestConfigShowValidateReportsMCPServersAgentsCannotReceive(t *testing.T) {
	cityDir := cursorAgentCity(t)
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "show", "--validate"}, &stdout, &stderr); code != 0 {
		t.Fatalf("config show --validate = %d, want 0 (a shared server is a warning); stderr=%s", code, stderr.String())
	}
	want := `gc config show: warning: agents "editor" (the implicit agent for provider "editor"), "mayor" start without MCP server "search" (http, from the city pack)`
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
	}

	// The same server in the agent's own mcp/ directory keeps that agent
	// from starting. gc start starts the rest of the city, so validation
	// warns about it too rather than failing.
	writeCatalogFile(t, cityDir, "agents/mayor/mcp/private.toml", `
name = "private"
url = "https://mcp.example.test/private"
`)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"config", "show", "--validate"}, &stdout, &stderr); code != 0 {
		t.Fatalf("config show --validate = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `gc config show: warning: agent "mayor" cannot start: MCP servers [private (http)] are declared in this agent's own mcp/ directory (`) {
		t.Fatalf("stderr missing the agent warning:\n%s", stderr.String())
	}

	// Plain gc config show --json reports the same validation.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"config", "show", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("config show --json = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `cannot start: MCP servers [private (http)]`) {
		t.Fatalf("config show --json validation lacks the MCP warning:\n%s", stdout.String())
	}
}

func TestDoImportAddWarnsAboutMCPServersAgentsCannotReceive(t *testing.T) {
	cityDir := cursorAgentCity(t)
	packDir := filepath.Join(cityDir, "packs", "search")
	writeHTTPMCPPack(t, packDir, "websearch")
	writePackToml(t, packDir, "[pack]\nname = \"search\"\nschema = 2\n")

	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doImportAdd(fsys.OSFS{}, cityDir, "./packs/search", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("doImportAdd = %d, stderr = %s", code, stderr.String())
	}
	want := `gc import add: warning: agents "editor" (the implicit agent for provider "editor"), "mayor" start without MCP server "websearch" (http, from import "search"): gc projects only stdio MCP servers for the "cursor" harness`
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr missing\n want: %s\n got: %s", want, stderr.String())
	}
	// Only the new import's servers: the city pack's own http server is
	// not this command's news.
	if strings.Contains(stderr.String(), `"search" (http, from the city pack)`) {
		t.Fatalf("import add reported a server the import did not bring:\n%s", stderr.String())
	}
}

// cursorAgentCitySkipWarning is the stage-1 warning for cursorAgentCity.
const cursorAgentCitySkipWarning = `gc: warning: city "test-city": agents "editor" (the implicit agent for provider "editor"), "mayor" start without MCP server "search" (http, from the city pack): gc projects only stdio MCP servers for the "cursor" harness`

// requireNoStage1MCPWrites fails the test if cursorAgentCity holds what a
// stage-1 MCP pass writes there: the cursor target and its managed marker.
func requireNoStage1MCPWrites(t *testing.T, cityDir string) {
	t.Helper()
	for _, rel := range []string{".cursor/mcp.json", ".gc/mcp-managed"} {
		if _, err := os.Lstat(filepath.Join(cityDir, rel)); !os.IsNotExist(err) {
			t.Fatalf("%s exists after a pass that must write nothing (err=%v)", rel, err)
		}
	}
}

// Under a supervisor, stage 1 runs in the supervisor's process, which prints
// its warnings only to its log. gc start prints them too, from a pass that
// writes nothing.
func TestSupervisorStartPrintsStage1MCPSkipWarnings(t *testing.T) {
	cityDir := cursorAgentCity(t)
	t.Setenv("GC_DOLT", "skip")
	oldAlive, oldRegister, oldDryRun := supervisorAliveHook, registerCityWithSupervisorTestHook, dryRunMode
	t.Cleanup(func() {
		supervisorAliveHook, registerCityWithSupervisorTestHook, dryRunMode = oldAlive, oldRegister, oldDryRun
	})
	supervisorAliveHook = func() int { return 0 }
	dryRunMode = false
	// The supervisor's own start: it would run the real pass.
	registerCityWithSupervisorTestHook = func(string, string, io.Writer, io.Writer) (bool, int) { return true, 0 }

	var stdout, stderr bytes.Buffer
	if code := doStart([]string{cityDir}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doStart = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), cursorAgentCitySkipWarning) {
		t.Fatalf("stderr missing\n want: %s\n got: %s", cursorAgentCitySkipWarning, stderr.String())
	}
	if !strings.Contains(stdout.String(), "City started under supervisor.") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	requireNoStage1MCPWrites(t, cityDir)

	// With --json they go to stderr, and stdout holds only the JSON record.
	// Another city: each warning prints once per process.
	jsonCity := cursorAgentCity(t)
	stdout.Reset()
	stderr.Reset()
	if code := doStartJSON([]string{jsonCity}, false, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doStartJSON = %d; stderr=%s", code, stderr.String())
	}
	requireStage1MCPWarningsOnStderrOnly(t, &stdout, &stderr)
	requireNoStage1MCPWrites(t, jsonCity)
}

// requireStage1MCPWarningsOnStderrOnly fails the test unless a --json run
// printed cursorAgentCity's warning to stderr and one JSON record to stdout.
func requireStage1MCPWarningsOnStderrOnly(t *testing.T, stdout, stderr *bytes.Buffer) {
	t.Helper()
	if !strings.Contains(stderr.String(), cursorAgentCitySkipWarning) {
		t.Fatalf("stderr missing\n want: %s\n got: %s", cursorAgentCitySkipWarning, stderr.String())
	}
	var record map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &record); err != nil || strings.Count(strings.TrimSpace(stdout.String()), "\n") != 0 {
		t.Fatalf("stdout is not one JSON record (err=%v):\n%s", err, stdout.String())
	}
}

// gc reload prints the same warnings once the controller accepts the
// reload, since the controller's own pass prints them only to its log. A
// reload it does not accept prints none.
func TestCmdReloadPrintsStage1MCPSkipWarnings(t *testing.T) {
	cityDir := cursorAgentCity(t)
	oldSend, oldUnavailable := sendReloadControlRequestHook, reloadUnavailableMessageHook
	t.Cleanup(func() { sendReloadControlRequestHook, reloadUnavailableMessageHook = oldSend, oldUnavailable })
	reply := reloadControlReply{Outcome: reloadOutcomeBusy, Message: "controller is busy"}
	sendReloadControlRequestHook = func(string, reloadControlRequest) (reloadControlReply, error) { return reply, nil }
	reloadUnavailableMessageHook = func(string) string { return "" }

	var stdout, stderr bytes.Buffer
	if code := cmdReload([]string{cityDir}, false, false, false, "30s", true, &stdout, &stderr); code != 1 {
		t.Fatalf("busy cmdReload = %d, want 1; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "MCP server") {
		t.Fatalf("a reload the controller did not accept printed MCP warnings:\n%s", stderr.String())
	}

	reply = reloadControlReply{Outcome: reloadOutcomeApplied, Message: "Config reloaded: 1 agents, 0 rigs (rev abc123def456)"}
	stdout.Reset()
	stderr.Reset()
	if code := cmdReload([]string{cityDir}, false, false, false, "30s", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReload = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), cursorAgentCitySkipWarning) {
		t.Fatalf("stderr missing\n want: %s\n got: %s", cursorAgentCitySkipWarning, stderr.String())
	}
	requireNoStage1MCPWrites(t, cityDir)

	// With --json too, on stderr, as gc start --json prints them.
	jsonCity := cursorAgentCity(t)
	stdout.Reset()
	stderr.Reset()
	if code := cmdReload([]string{jsonCity}, false, false, true, "30s", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdReload --json = %d; stderr=%s", code, stderr.String())
	}
	requireStage1MCPWarningsOnStderrOnly(t, &stdout, &stderr)
	requireNoStage1MCPWrites(t, jsonCity)
}

// writeMCPReloadTestConfig writes a city whose mayor runs on a claude-family
// provider that resolves in tests (its command is echo).
func writeMCPReloadTestConfig(t *testing.T, tomlPath string, extra ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(tomlPath), "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	var buf strings.Builder
	buf.WriteString("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"tmux\"\n\n")
	buf.WriteString("[providers.claude]\nbase = \"builtin:claude\"\ncommand = \"echo\"\n\n")
	buf.WriteString("[[agent]]\nname = \"mayor\"\nscope = \"city\"\nprovider = \"claude\"\n\n")
	for _, fragment := range extra {
		buf.WriteString(fragment)
	}
	if err := os.WriteFile(tomlPath, []byte(buf.String()), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// Stage-1 MCP projection also runs on a live config reload. Before, it ran
// only when a city started: a server added to a running city changed the
// agents' fingerprints (draining them) while their config kept the old
// servers, and an agent stage 1 had skipped never got its config once fixed.
func TestCityRuntimeReloadProjectsMCPForARunningCity(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	clearInheritedBeadsEnv(t)
	requireNoLeakedDoltAfterForPaths(t, cityPath)
	writeMCPReloadTestConfig(t, tomlPath)
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
	if err := runStage1MCPProjection(cityPath, cfg, exec.LookPath, io.Discard); err != nil {
		t.Fatalf("stage 1 at city start: %v", err)
	}

	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:     cityPath,
		CityName:     "test-city",
		TomlPath:     tomlPath,
		WatchTargets: config.WatchTargets(nil, cfg, cityPath),
		ConfigRev:    configRev,
		ConfigDirty:  &atomic.Bool{},
		Cfg:          cfg,
		SP:           sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		PokeCh: make(chan struct{}, 8),
		Stdout: &stdout,
		Stderr: &stderr,
	})

	// Add a server and change a live-reloadable field, so the reload takes
	// the applied branch.
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "docs.toml"), `
name = "docs"
command = "docs-mcp"
`)
	writeMCPReloadTestConfig(t, tomlPath, "[orders]\nskip = [\"reaper\"]\n")
	lastProviderName := "tmux"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceWatch)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reload outcome = %q; stderr=%s", reply.Outcome, stderr.String())
	}
	servers := readMCPServersDoc(t, filepath.Join(cityPath, ".mcp.json"))
	if _, ok := servers["docs"]; !ok {
		t.Fatalf("the reload did not project the new server: %v\nstderr=%s", servers, stderr.String())
	}
}

// A stage-1 pass that fails on a config reload warns instead of refusing the
// reload: the reload's other changes apply, and the targets the pass did not
// reach stay as they were. A city start still fails on the same error.
func TestCityRuntimeReloadWarnsWhenMCPProjectionFails(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	clearInheritedBeadsEnv(t)
	requireNoLeakedDoltAfterForPaths(t, cityPath)
	writeMCPReloadTestConfig(t, tomlPath)
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
	if err := runStage1MCPProjection(cityPath, cfg, exec.LookPath, io.Discard); err != nil {
		t.Fatalf("stage 1 at city start: %v", err)
	}

	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:     cityPath,
		CityName:     "test-city",
		TomlPath:     tomlPath,
		WatchTargets: config.WatchTargets(nil, cfg, cityPath),
		ConfigRev:    configRev,
		ConfigDirty:  &atomic.Bool{},
		Cfg:          cfg,
		SP:           sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		PokeCh: make(chan struct{}, 8),
		Stdout: &stdout,
		Stderr: &stderr,
	})

	// The managed .mcp.json has become a symlink, which projection refuses
	// to write through.
	target := filepath.Join(cityPath, ".mcp.json")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(elsewhere, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, target); err != nil {
		t.Fatal(err)
	}
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "docs.toml"), `
name = "docs"
command = "docs-mcp"
`)
	writeMCPReloadTestConfig(t, tomlPath, "[orders]\nskip = [\"reaper\"]\n")
	lastProviderName := "tmux"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceWatch)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reload outcome = %q, want applied despite the MCP failure; stderr=%s", reply.Outcome, stderr.String())
	}
	warned := false
	for _, warning := range reply.Warnings {
		if strings.HasPrefix(warning, "MCP projection: ") && strings.Contains(warning, "symlinked path") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("reload warnings = %q, want the MCP projection failure", reply.Warnings)
	}
	if data, err := os.ReadFile(elsewhere); err != nil || string(data) != "{}\n" {
		t.Fatalf("the symlink's target changed: %q (err %v)", data, err)
	}
}

// An ACP session on a harness that has a file projection receives, in-band,
// the servers that projection cannot carry: a cursor-family agent whose
// sessions use ACP gets the city's HTTP server, and nothing is reported as
// skipped for it.
func TestACPSessionsOnAFileHarnessReceiveServersItsFileCannotCarry(t *testing.T) {
	cityPath := t.TempDir()
	packDir := filepath.Join(t.TempDir(), "search")
	writeHTTPMCPPack(t, packDir, "search")
	acp := true
	cfg := &config.City{
		ExplicitImportPackDirs:    []string{packDir},
		ExplicitImportMCPBindings: map[string]string{packDir: "search"},
		Providers: map[string]config.ProviderSpec{
			"editor": {Base: ptrString("builtin:cursor"), Command: "echo", PromptMode: "none", SupportsACP: &acp, ACPCommand: "echo"},
		},
		Session: config.SessionConfig{Provider: "tmux"},
	}
	agent := config.Agent{Name: "editor", Scope: "city", Provider: "editor", Session: "acp"}
	cfg.Agents = []config.Agent{agent}

	params := &agentBuildParams{
		city:            cfg,
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &cfg.Workspace,
		providers:       cfg.Providers,
		lookPath:        stubLookPath,
		fs:              fsys.OSFS{},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
		sessionProvider: "tmux",
	}
	tp, err := resolveTemplate(params, &agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(acp): %v", err)
	}
	if len(tp.MCPServers) != 1 || tp.MCPServers[0].Name != "search" {
		t.Fatalf("ACP session MCP servers = %+v, want the http server in-band", tp.MCPServers)
	}
	view, err := resolveConfiguredAgentMCPProjection(cityPath, cfg, &cfg.Agents[0], stubLookPath)
	if err != nil {
		t.Fatalf("resolveConfiguredAgentMCPProjection(acp): %v", err)
	}
	if view.ProviderKind != "cursor" || view.Delivery != "acp" || len(view.Skipped) != 0 || len(view.Catalog.Servers) != 1 {
		t.Fatalf("view = kind %q, delivery %q, catalog %d, skipped %d; want cursor, acp, 1, 0",
			view.ProviderKind, view.Delivery, len(view.Catalog.Servers), len(view.Skipped))
	}
	if report := collectMCPCapabilityReport(cityPath, cfg); len(report.skips) != 0 || len(report.blocked) != 0 {
		t.Fatalf("report = %+v, want nothing for an ACP session", report)
	}
}

// An ACP session writes no provider-native file, so it shares no target with
// the agents that do: mcp-shared-target must not call an explicit ACP agent
// with servers of its own a conflict with a stage-1 agent at the same root.
func TestMCPSharedTargetDoctorCheckIgnoresACPSessions(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	writeMCPSource(t, filepath.Join(cityPath, "agents", "relay", "mcp", "private.toml"), `
name = "private"
command = "private-mcp"
`)
	acp := true
	providers := builtinProviderAliasesForTest("claude")
	providers["relay"] = config.ProviderSpec{Base: ptrString("builtin:claude"), Command: "echo", PromptMode: "none", SupportsACP: &acp, ACPCommand: "echo"}
	cfg := &config.City{
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  providers,
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "claude"},
			{Name: "relay", Scope: "city", Provider: "relay", Session: "acp", MCPDir: filepath.Join(cityPath, "agents", "relay", "mcp")},
		},
	}
	result := newMCPSharedTargetDoctorCheck(cityPath, cfg, stubLookPath).Run(&doctor.CheckContext{CityPath: cityPath})
	if result.Status != doctor.StatusOK {
		t.Fatalf("mcp-shared-target = %v (%s; %v), want OK: the ACP agent writes no file", result.Status, result.Message, result.Details)
	}
}

// An agent whose provider does not resolve cannot start: its provider name is
// not in the catalog (a typo), or it is not set at all. Stage 1 skips it with
// a warning and projects the rest of the city, as it does an agent whose
// provider is not installed.
func TestRunStage1MCPProjectionSkipsAgentsWhoseProviderIsUnknownOrUnset(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	cfg := &config.City{
		Workspace:  config.Workspace{Name: "my-city"},
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
		Providers:  builtinProviderAliasesForTest("gemini"),
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Provider: "gemini"},
			{Name: "typo", Scope: "city", Provider: "gemnii"},
			{Name: "unset", Scope: "city"},
		},
	}
	var stderr bytes.Buffer
	if err := runStage1MCPProjection(cityPath, cfg, stubLookPath, &stderr); err != nil {
		t.Fatalf("runStage1MCPProjection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gemini", "settings.json")); err != nil {
		t.Fatalf("the rest of the city was not projected: %v", err)
	}
	for _, want := range []string{
		`gc: warning: city "my-city": agent "typo" cannot start: unknown provider`,
		`gc: warning: city "my-city": agent "unset" cannot start: unknown provider`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("warning missing %q:\n%s", want, stderr.String())
		}
	}
}

// An ACP session receives its MCP servers in session/new, not through a
// file, so a harness without a file projection still receives every server,
// shared or its own, and nothing is reported as skipped.
func TestACPSessionsReceiveEveryMCPServerInBand(t *testing.T) {
	cityPath := t.TempDir()
	writeMCPSource(t, filepath.Join(cityPath, "mcp", "notes.toml"), `
name = "notes"
command = "uvx"
`)
	agentMCP := filepath.Join(cityPath, "agents", "relay", "mcp")
	writeHTTPMCPPack(t, filepath.Dir(agentMCP), "private")
	acp := true
	cfg := &config.City{
		Workspace:  config.Workspace{Provider: "relay"},
		Providers:  map[string]config.ProviderSpec{"relay": {Command: "echo", PromptMode: "none", SupportsACP: &acp, ACPCommand: "echo"}},
		PackMCPDir: filepath.Join(cityPath, "mcp"),
		Session:    config.SessionConfig{Provider: "tmux"},
	}
	agent := config.Agent{Name: "relay", Scope: "city", Provider: "relay", Session: "acp", MCPDir: agentMCP}
	cfg.Agents = []config.Agent{agent}

	params := &agentBuildParams{
		city:            cfg,
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &cfg.Workspace,
		providers:       cfg.Providers,
		lookPath:        stubLookPath,
		fs:              fsys.OSFS{},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
		sessionProvider: "tmux",
	}
	tp, err := resolveTemplate(params, &agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(acp): %v", err)
	}
	var names []string
	for _, server := range tp.MCPServers {
		names = append(names, server.Name)
	}
	if strings.Join(names, ",") != "notes,private" {
		t.Fatalf("ACP session MCP servers = %v, want notes and private in-band", names)
	}

	if report := collectMCPCapabilityReport(cityPath, cfg); len(report.skips) != 0 || len(report.blocked) != 0 {
		t.Fatalf("report = %+v, want nothing for an ACP session", report)
	}
	view, err := resolveConfiguredAgentMCPProjection(cityPath, cfg, &cfg.Agents[0], stubLookPath)
	if err != nil {
		t.Fatalf("resolveConfiguredAgentMCPProjection(acp): %v", err)
	}
	if len(view.Skipped) != 0 || len(view.Catalog.Servers) != 2 || view.Delivery != "acp" {
		t.Fatalf("view = delivery %q, catalog %d, skipped %d; want acp, 2, 0", view.Delivery, len(view.Catalog.Servers), len(view.Skipped))
	}
}

// gc import add compares the city before and after the add: servers an
// import shares a pack directory with, or that a same-named rig import
// brought in earlier, are not this add's news.
func TestMCPCapabilityReportSinceReportsOnlyWhatChanged(t *testing.T) {
	notes := mcpServerSkip{Server: materialize.MCPServer{Name: "notes", Transport: materialize.MCPTransportStdio, Origin: "city"}, Reason: "r"}
	search := mcpServerSkip{Server: materialize.MCPServer{Name: "search", Transport: materialize.MCPTransportHTTP, Origin: "import:search"}, Reason: "r"}
	// The report is keyed by agent label (mcpAgentLabel).
	before := mcpCapabilityReport{
		skips:   map[string][]mcpServerSkip{`"helper"`: {notes}},
		blocked: map[string]string{`"editor"`: "old reason"},
	}
	after := mcpCapabilityReport{
		skips:   map[string][]mcpServerSkip{`"helper"`: {notes, search}, `"cursor"`: {search}},
		blocked: map[string]string{`"editor"`: "old reason", `"coder"`: "new reason"},
	}
	got := strings.Join(after.since(before).lines(), "\n")
	want := `agents "cursor", "helper" start without MCP server "search" (http, from import "search"): r` + "\n" +
		`agent "coder" cannot start: new reason`
	if got != want {
		t.Fatalf("since() lines =\n%s\nwant\n%s", got, want)
	}
}
