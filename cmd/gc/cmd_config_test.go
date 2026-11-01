package main

import (
	"bytes"
	"encoding/json"
	"os"
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

// A malformed order file no longer fails the config load itself; gc config
// show --validate scans the orders and reports it, and plain gc config show
// still renders the config with the order error as a warning.
func TestConfigShowValidateReportsMalformedOrder(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GC_CITY_PATH", dir)
	if err := os.MkdirAll(".gc", 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n")
	if err := os.MkdirAll(filepath.Join(dir, "orders"), 0o755); err != nil {
		t.Fatalf("MkdirAll(orders): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orders", "broken.toml"), []byte("[order\nformula = "), 0o644); err != nil {
		t.Fatalf("WriteFile(orders/broken.toml): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "show", "--validate"}, &stdout, &stderr); code == 0 {
		t.Fatalf("run(config show --validate) = 0, want nonzero for a malformed order; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "broken") {
		t.Fatalf("stderr = %q, want the malformed order named", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"config", "show"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(config show) = %d, want the config shown despite the malformed order; stderr=%q", code, stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "warning") || !strings.Contains(got, "broken") {
		t.Fatalf("stderr = %q, want the malformed order reported as a warning", got)
	}
}
