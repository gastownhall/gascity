package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestDoConfigShowMissingRemoteImportSuggestsInstall(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GC_CITY_PATH", dir)
	if err := os.MkdirAll(".gc", 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)

	var stdout, stderr bytes.Buffer
	code := doConfigShow(false, false, false, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected failure for missing remote import")
	}
	if got := stderr.String(); !bytes.Contains([]byte(got), []byte(`run "gc import install"`)) {
		t.Fatalf("stderr = %q, want install remediation", got)
	}
}

func TestConfigShowJSON(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GC_CITY_PATH", dir)
	if err := os.MkdirAll(".gc", 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(config show --json) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string `json:"schema_version"`
		CityPath      string `json:"city_path"`
		Warnings      []string
		Config        struct {
			Workspace struct {
				Name string
			}
		}
		Validation struct {
			OK       bool `json:"ok"`
			Warnings []string
			Errors   []string
		} `json:"validation"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" || payload.CityPath != dir || payload.Config.Workspace.Name != "demo" || !payload.Validation.OK {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Warnings == nil || payload.Validation.Warnings == nil || payload.Validation.Errors == nil {
		t.Fatalf("warnings/errors must be JSON arrays, got %+v", payload)
	}
	validateConfigShowJSONSchema(t, stdout.Bytes())
}

func TestConfigShowValidateJSONReturnsNonzeroForInvalidConfig(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GC_CITY_PATH", dir)
	if err := os.MkdirAll(".gc", 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n\n[[rigs]]\nname = \"broken\"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", "--validate", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(config show --validate --json) = 0, want nonzero; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	var payload struct {
		Validation struct {
			OK     bool     `json:"ok"`
			Errors []string `json:"errors"`
		} `json:"validation"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if payload.Validation.OK || len(payload.Validation.Errors) == 0 {
		t.Fatalf("validation payload = %+v, want ok=false with errors", payload.Validation)
	}
	validateConfigShowJSONSchema(t, stdout.Bytes())
}

func validateConfigShowJSONSchema(t *testing.T, data []byte) {
	t.Helper()

	rawSchema, err := readBuiltinSchema([]string{"config", "show"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read config show schema: %v", err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("config-show.schema.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := compiler.Compile("config-show.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("payload does not match config show schema: %v\n%s", err, string(data))
	}
}

// absentProviderCommand names a provider binary no host installs, so gc
// start skips the provider's agents while validation still resolves them.
const absentProviderCommand = "gc-test-absent-provider-binary"

// writeAgentLocalMCPConflictCity writes a tmux city whose city-scoped
// "worker" agent has its own mcp/ catalog, which the implicit agent of its
// provider, sharing the city root's .mcp.json, never receives (#8267). It
// makes the city the current one and returns its directory.
func writeAgentLocalMCPConflictCity(t *testing.T, providerCommand string) string {
	t.Helper()
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Chdir(cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	writeProjectedMCPCity(t, cityDir, `
[workspace]
provider = "claude"

[beads]
provider = "file"

[session]
provider = "tmux"

[providers.claude]
base = "builtin:claude"
command = "`+providerCommand+`"
prompt_mode = "none"
`)
	writeCatalogFile(t, cityDir, "agents/worker/agent.toml", `
provider = "claude"
scope = "city"
`)
	writeCatalogFile(t, cityDir, "agents/worker/mcp/notes.toml", `
name = "notes"
command = "uvx"
`)
	return cityDir
}

// gc start refuses a city whose agent-local MCP catalog conflicts with the
// implicit agent at the city root; gc config show --validate reports the
// same conflict, its cause and remedy before start, from config alone: a
// provider binary missing on this host does not hide it.
func TestConfigShowValidateReportsImplicitAgentLocalMCPConflict(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command string
	}{
		{name: "provider installed", command: "echo"},
		{name: "provider binary absent", command: absentProviderCommand},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := writeAgentLocalMCPConflictCity(t, tt.command)
			if tt.command == absentProviderCommand {
				if _, err := exec.LookPath(tt.command); err == nil {
					t.Fatalf("%s resolves on this host; the case needs it absent", tt.command)
				}
			}

			var stdout, stderr bytes.Buffer
			if code := run([]string{"config", "show", "--validate"}, &stdout, &stderr); code != 1 {
				t.Fatalf("gc config show --validate = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), "Config valid.") {
				t.Fatalf("stdout = %q, want no Config valid.", stdout.String())
			}
			for _, want := range []string{
				"gc config show: MCP target conflict at " + filepath.Join(cityDir, ".mcp.json") + " (claude)",
				`"worker" defines an agent-local MCP catalog at ` + filepath.Join(cityDir, "agents", "worker", "mcp"),
				`"claude" (the implicit agent for provider "claude")`,
				`give "worker" its own work_dir, or move its servers to an mcp/ directory both receive`,
			} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
		})
	}
}

// gc config show --validate --json fails the same conflicting city, with
// gc start's conflict error, unchanged, among its validation errors.
func TestConfigShowValidateJSONReportsImplicitAgentLocalMCPConflict(t *testing.T) {
	cityDir := writeAgentLocalMCPConflictCity(t, "echo")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "show", "--validate", "--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("gc config show --validate --json = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	validateConfigShowJSONSchema(t, stdout.Bytes())
	var payload struct {
		Validation struct {
			OK     bool     `json:"ok"`
			Errors []string `json:"errors"`
		} `json:"validation"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if payload.Validation.OK {
		t.Fatalf("validation.ok = true, want false:\n%s", stdout.String())
	}

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	_, startErr := buildStage1MCPTargets(cityDir, cfg, stubLookPath, io.Discard)
	if startErr == nil {
		t.Fatal("buildStage1MCPTargets: want the MCP target conflict gc start refuses, got nil")
	}
	for _, want := range []string{"MCP target conflict at ", filepath.Join(cityDir, "agents", "worker", "mcp")} {
		if !strings.Contains(startErr.Error(), want) {
			t.Fatalf("start error %q, want it to contain %q", startErr, want)
		}
	}
	found := false
	for _, e := range payload.Validation.Errors {
		found = found || e == startErr.Error()
	}
	if !found {
		t.Fatalf("validation.errors = %#v, want gc start's error %q", payload.Validation.Errors, startErr)
	}
}
