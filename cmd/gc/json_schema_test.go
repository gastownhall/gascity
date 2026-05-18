package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func TestJSONSchemaManifestForSupportedCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"events", "--json-schema"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(events --json-schema) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1: %q", len(lines), stdout.String())
	}
	var manifest struct {
		SchemaVersion string                     `json:"schema_version"`
		Command       []string                   `json:"command"`
		Transport     string                     `json:"transport"`
		JSONSupported bool                       `json:"json_supported"`
		Schemas       map[string]json.RawMessage `json:"schemas"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
	}
	if manifest.SchemaVersion != "1" || manifest.Transport != "jsonl" || !manifest.JSONSupported {
		t.Fatalf("manifest metadata = %+v", manifest)
	}
	if got := strings.Join(manifest.Command, " "); got != "events" {
		t.Fatalf("command = %q, want events", got)
	}
	if !json.Valid(manifest.Schemas["result"]) {
		t.Fatalf("result schema missing or invalid: %s", manifest.Schemas["result"])
	}
	if !json.Valid(manifest.Schemas["failure"]) {
		t.Fatalf("failure schema missing or invalid: %s", manifest.Schemas["failure"])
	}
}

func TestJSONSchemaManifestForUnsupportedCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version", "--json-schema"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(version --json-schema) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var manifest struct {
		Command       []string                   `json:"command"`
		JSONSupported bool                       `json:"json_supported"`
		Schemas       map[string]json.RawMessage `json:"schemas"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
	}
	if got := strings.Join(manifest.Command, " "); got != "version" {
		t.Fatalf("command = %q, want version", got)
	}
	if manifest.JSONSupported {
		t.Fatalf("json_supported = true, want false")
	}
	if len(manifest.Schemas) != 0 {
		t.Fatalf("schemas = %+v, want empty", manifest.Schemas)
	}
}

func TestJSONSchemaRoleSpecificResult(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", "/tmp/example-city", "events", "--json-schema=result"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(events --json-schema=result) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var schema map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
		t.Fatalf("result schema is not JSON: %v\n%s", err, stdout.String())
	}
	if schema["$schema"] == "" {
		t.Fatalf("schema missing $schema: %+v", schema)
	}
	if _, ok := schema["x-gc-jsonl"].(map[string]any); !ok {
		t.Fatalf("schema missing x-gc-jsonl object: %+v", schema)
	}
}

func TestJSONSchemaRoleSpecificFailureUsesSharedDefault(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"events", "--json-schema", "failure"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(events --json-schema failure) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var schema struct {
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
		t.Fatalf("failure schema is not JSON: %v\n%s", err, stdout.String())
	}
	if !schema.AdditionalProperties {
		t.Fatalf("additionalProperties = false, want true")
	}
	if strings.Join(schema.Required, ",") != "schema_version,ok,error" {
		t.Fatalf("required = %v", schema.Required)
	}
}

func TestJSONSchemaUnavailableRoleFailureIsStructured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version", "--json-schema=result"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(version --json-schema=result) = 0, want nonzero")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Error         struct {
			Code     string `json:"code"`
			Message  string `json:"message"`
			ExitCode int    `json:"exit_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("failure payload is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.OK || payload.Error.ExitCode != code || payload.Error.Code == "" {
		t.Fatalf("payload = %+v, code=%d", payload, code)
	}
}

func TestJSONUnsupportedCommandFailureIsStructured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(version --json) = 0, want nonzero")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Error         struct {
			Code     string `json:"code"`
			Message  string `json:"message"`
			ExitCode int    `json:"exit_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("failure payload is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.OK || payload.Error.ExitCode != code || payload.Error.Code != "json_unsupported" {
		t.Fatalf("payload = %+v, code=%d", payload, code)
	}
}

func TestJSONContractAllowsBdPassthrough(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "bd [bd-args...]"})

	var stdout, stderr bytes.Buffer
	handled, code := handleJSONContractRequest(root, []string{"bd", "list", "--json"}, &stdout, &stderr)
	if handled || code != 0 {
		t.Fatalf("handled=%v code=%d stdout=%q", handled, code, stdout.String())
	}
}

func TestJSONContractResolvesCommandWithInterspersedBooleanFlags(t *testing.T) {
	schemaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(schemaDir, "result.schema.json"), []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object"
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	root := &cobra.Command{Use: "gc"}
	parent := &cobra.Command{Use: "tools"}
	parent.PersistentFlags().Bool("verbose", false, "verbose output")
	leaf := &cobra.Command{
		Use:         "run",
		Annotations: map[string]string{jsonSchemaDirAnnotation: schemaDir},
	}
	parent.AddCommand(leaf)
	root.AddCommand(parent)

	var stdout, stderr bytes.Buffer
	handled, code := handleJSONContractRequest(root, []string{"tools", "--verbose", "run", "--json"}, &stdout, &stderr)
	if handled || code != 0 {
		t.Fatalf("handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
}

func TestJSONContractMissingPackSchemaWarnsBeforeStrictRollout(t *testing.T) {
	t.Setenv("GC_JSON_CONTRACT_STRICT", "0")

	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{
		Use:         "packcmd",
		Annotations: map[string]string{jsonSchemaDirAnnotation: filepath.Join(t.TempDir(), "schemas")},
	})

	var stdout, stderr bytes.Buffer
	handled, code := handleJSONContractRequest(root, []string{"packcmd", "--json"}, &stdout, &stderr)
	if handled || code != 0 {
		t.Fatalf("handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not declare JSON support") {
		t.Fatalf("stderr = %q, want missing-schema warning", stderr.String())
	}
}

func TestJSONContractMissingPackSchemaCanBeStrict(t *testing.T) {
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")

	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{
		Use:         "packcmd",
		Annotations: map[string]string{jsonSchemaDirAnnotation: filepath.Join(t.TempDir(), "schemas")},
	})

	var stdout, stderr bytes.Buffer
	handled, code := handleJSONContractRequest(root, []string{"packcmd", "--json"}, &stdout, &stderr)
	if !handled || code == 0 {
		t.Fatalf("handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"code":"json_unsupported"`) {
		t.Fatalf("stdout = %q, want json_unsupported", stdout.String())
	}
}

func TestJSONExecutionDoesNotBufferJSONLCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd(&stdout, &stderr)

	for _, args := range [][]string{
		{"events", "--json"},
		{"events", "--follow", "--json"},
		{"events", "--watch", "--json"},
	} {
		if shouldBufferJSONExecution(root, args) {
			t.Fatalf("shouldBufferJSONExecution(%v) = true, want false for JSONL command", args)
		}
	}
}

func TestJSONExecutionFailureIsStructured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "explain", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(config explain --json) = 0, want nonzero")
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr empty, want command diagnostics")
	}

	var payload struct {
		OK    bool `json:"ok"`
		Error struct {
			Code     string `json:"code"`
			ExitCode int    `json:"exit_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("failure payload is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.OK || payload.Error.Code != "command_failed" || payload.Error.ExitCode != code {
		t.Fatalf("payload = %+v, code=%d", payload, code)
	}
}

func TestJSONSchemaManifestForDiscoveredPackCommand(t *testing.T) {
	dir := t.TempDir()
	commandDir := filepath.Join(dir, "commands", "review", "pr")
	if err := os.MkdirAll(filepath.Join(commandDir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commandDir, "schemas", "result.schema.json"), []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": { "ok": { "type": "boolean" } },
  "required": ["ok"]
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	root := &cobra.Command{Use: "gc"}
	configureJSONSchemaFlag(root)
	var stdout, stderr bytes.Buffer
	addDiscoveredCommandsToRoot(root, []config.DiscoveredCommand{{
		Command:     []string{"review", "pr"},
		Description: "Review a PR",
		SourceDir:   commandDir,
		PackDir:     dir,
		PackName:    "tools",
		BindingName: "tools",
	}}, dir, "testcity", &stdout, &stderr, true)

	handled, code := handleJSONSchemaRequest(root, []string{"tools", "review", "pr", "--json-schema"}, &stdout)
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d stderr=%q stdout=%q", handled, code, stderr.String(), stdout.String())
	}

	var manifest struct {
		Command       []string                   `json:"command"`
		JSONSupported bool                       `json:"json_supported"`
		Schemas       map[string]json.RawMessage `json:"schemas"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
	}
	if got := strings.Join(manifest.Command, " "); got != "tools review pr" {
		t.Fatalf("command = %q, want tools review pr", got)
	}
	if !manifest.JSONSupported || !json.Valid(manifest.Schemas["result"]) || !json.Valid(manifest.Schemas["failure"]) {
		t.Fatalf("manifest = %+v", manifest)
	}
}
