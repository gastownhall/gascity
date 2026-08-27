package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestConfigShowValidateRootFileUsesCandidateRootAndPreservesLiveConfig(t *testing.T) {
	oldExtraConfigFiles := extraConfigFiles
	extraConfigFiles = nil
	t.Cleanup(func() { extraConfigFiles = oldExtraConfigFiles })

	clearGCEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GC_CITY_PATH", dir)
	if err := os.MkdirAll(".gc", 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	live := []byte("[workspace]\nname = \"live\"\n")
	writeCityToml(t, dir, string(live))
	candidateDir := filepath.Join(dir, "candidate")
	if err := os.MkdirAll(candidateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(candidate): %v", err)
	}
	candidate := filepath.Join(candidateDir, "city.toml")
	if err := os.WriteFile(candidate, []byte("include = [\"fragment.toml\"]\n[workspace]\nname = \"candidate\"\n"), 0o644); err != nil {
		t.Fatalf("write candidate root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(candidateDir, "fragment.toml"), []byte("[[agent]]\nname = \"candidate-worker\"\n"), 0o644); err != nil {
		t.Fatalf("write candidate fragment: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "show", "--validate", "--root-file", candidate}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(config show --validate --root-file) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if got := stdout.String(); got != "Config valid.\n" {
		t.Fatalf("stdout = %q, want Config valid", got)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"config", "show", "--validate", "--file", candidate}, &stdout, &stderr); code == 0 {
		t.Fatalf("run(config show --validate --file candidate) = 0, want legacy fragment interpretation to fail; stderr=%q stdout=%q", stderr.String(), stdout.String())
	}
	gotLive, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("read live city.toml: %v", err)
	}
	if !bytes.Equal(gotLive, live) {
		t.Fatalf("live city.toml changed during validation: got %q want %q", gotLive, live)
	}
}

func TestConfigShowRootFileIncompatibleFlagsWriteDiagnostics(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "validate required",
			args: []string{"--root-file", "candidate.toml"},
			want: "gc config show: --root-file requires --validate\n",
		},
		{
			name: "file forbidden",
			args: []string{"--validate", "--root-file", "candidate.toml", "--file", "overlay.toml"},
			want: "gc config show: --root-file cannot be combined with --file\n",
		},
		{
			name: "provenance forbidden",
			args: []string{"--validate", "--root-file", "candidate.toml", "--provenance"},
			want: "gc config show: --root-file cannot be combined with --provenance\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldExtraConfigFiles := extraConfigFiles
			extraConfigFiles = nil
			t.Cleanup(func() { extraConfigFiles = oldExtraConfigFiles })

			var stdout, stderr bytes.Buffer
			cmd := newConfigShowCmd(&stdout, &stderr)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); !errors.Is(err, errExit) {
				t.Fatalf("Execute() error = %v, want errExit", err)
			}
			if got := stdout.String(); got != "" {
				t.Fatalf("stdout = %q, want empty", got)
			}
			if got := stderr.String(); got != tt.want {
				t.Fatalf("stderr = %q, want %q", got, tt.want)
			}
		})
	}
}

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
	code := doConfigShow(false, false, false, "", &stdout, &stderr)
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
