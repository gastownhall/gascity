package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gascity "github.com/gastownhall/gascity"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/santhosh-tekuri/jsonschema/v6"
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
		JSONSupported bool                       `json:"json_supported"`
		Schemas       map[string]json.RawMessage `json:"schemas"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
	}
	assertManifestOmitsTransport(t, stdout.Bytes())
	if manifest.SchemaVersion != "1" || !manifest.JSONSupported {
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

func TestJSONResultSchemasRequireSuccessDiscriminator(t *testing.T) {
	var missing []string
	var nonObject []string
	err := fs.WalkDir(gascity.BuiltinSchemas, "schemas", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "/result.schema.json") {
			return nil
		}
		data, err := gascity.BuiltinSchemas.ReadFile(path)
		if err != nil {
			return err
		}
		var schema struct {
			Type       string                     `json:"type"`
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			return err
		}
		if path == "schemas/bd/result.schema.json" {
			// gc bd is an explicit passthrough: bd owns the payload shape.
			return nil
		}
		if schema.Type != "object" {
			nonObject = append(nonObject, path)
			return nil
		}
		okSchema := schema.Properties["ok"]
		var okProperty struct {
			Const *bool `json:"const"`
		}
		if len(okSchema) > 0 {
			if err := json.Unmarshal(okSchema, &okProperty); err != nil {
				return err
			}
		}
		if !slices.Contains(schema.Required, "ok") || okProperty.Const == nil || !*okProperty.Const {
			missing = append(missing, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(nonObject) > 0 {
		t.Fatalf("result schemas must use an object root for the top-level ok:true discriminator:\n%s", strings.Join(nonObject, "\n"))
	}
	if len(missing) > 0 {
		t.Fatalf("result schemas missing required top-level ok:true discriminator:\n%s", strings.Join(missing, "\n"))
	}
}

func TestWithDefaultSuccessOKHandlesNilPayload(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("withDefaultSuccessOK(nil) panicked: %v", r)
		}
	}()
	if got := withDefaultSuccessOK(nil); got != nil {
		t.Fatalf("withDefaultSuccessOK(nil) = %#v, want nil", got)
	}
}

func TestJSONSchemaManifestForLifecycleActionCommands(t *testing.T) {
	commands := [][]string{
		{"start"},
		{"stop"},
		{"restart"},
		{"reload"},
		{"suspend"},
		{"resume"},
		{"register"},
		{"unregister"},
		{"supervisor", "start"},
		{"supervisor", "stop"},
		{"supervisor", "reload"},
	}
	for _, command := range commands {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			args := append(append([]string{}, command...), "--json-schema")
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run(%v) = %d, stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			var manifest struct {
				SchemaVersion string                     `json:"schema_version"`
				JSONSupported bool                       `json:"json_supported"`
				Schemas       map[string]json.RawMessage `json:"schemas"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
				t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
			}
			assertManifestOmitsTransport(t, stdout.Bytes())
			if manifest.SchemaVersion != "1" || !manifest.JSONSupported {
				t.Fatalf("manifest metadata = %+v", manifest)
			}
			if !json.Valid(manifest.Schemas["result"]) {
				t.Fatalf("result schema missing or invalid: %s", manifest.Schemas["result"])
			}
			if !json.Valid(manifest.Schemas["failure"]) {
				t.Fatalf("failure schema missing or invalid: %s", manifest.Schemas["failure"])
			}
		})
	}
}

func TestActionResultSchemasAllowExtensionFields(t *testing.T) {
	var checked []string
	err := fs.WalkDir(gascity.BuiltinSchemas, "schemas", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "/result.schema.json") {
			return nil
		}
		data, err := gascity.BuiltinSchemas.ReadFile(path)
		if err != nil {
			return err
		}
		var schema struct {
			Required             []string                   `json:"required"`
			AdditionalProperties json.RawMessage            `json:"additionalProperties"`
			Properties           map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			return err
		}
		var commandProp, actionProp struct {
			Const *string `json:"const"`
		}
		if raw := schema.Properties["command"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &commandProp); err != nil {
				return err
			}
		}
		if raw := schema.Properties["action"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &actionProp); err != nil {
				return err
			}
		}
		if commandProp.Const == nil || actionProp.Const == nil {
			return nil
		}
		checked = append(checked, path)
		if !slices.Contains(schema.Required, "command") || !slices.Contains(schema.Required, "action") {
			t.Errorf("%s required = %v, want command and action", path, schema.Required)
		}
		var additionalProperties bool
		if err := json.Unmarshal(schema.AdditionalProperties, &additionalProperties); err != nil || !additionalProperties {
			t.Errorf("%s additionalProperties = %s, want true", path, string(schema.AdditionalProperties))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(checked) == 0 {
		t.Fatal("no action result schemas discovered")
	}
}

func TestJSONSchemaManifestForActionSummaryCommands(t *testing.T) {
	for _, args := range [][]string{
		{"convoy", "create", "--json-schema"},
		{"convoy", "land", "--json-schema"},
		{"mail", "send", "--json-schema"},
		{"mail", "delete", "--json-schema"},
	} {
		t.Run(strings.Join(args[:len(args)-1], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run(%v) = %d, stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			var manifest struct {
				SchemaVersion string                     `json:"schema_version"`
				JSONSupported bool                       `json:"json_supported"`
				Schemas       map[string]json.RawMessage `json:"schemas"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
				t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
			}
			assertManifestOmitsTransport(t, stdout.Bytes())
			if manifest.SchemaVersion != "1" || !manifest.JSONSupported {
				t.Fatalf("manifest metadata = %+v", manifest)
			}
			if !json.Valid(manifest.Schemas["result"]) {
				t.Fatalf("result schema missing or invalid: %s", manifest.Schemas["result"])
			}
			if !json.Valid(manifest.Schemas["failure"]) {
				t.Fatalf("failure schema missing or invalid: %s", manifest.Schemas["failure"])
			}
		})
	}
}

func TestJSONSchemaManifestForUnsupportedCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"dashboard", "--json-schema"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(dashboard --json-schema) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
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
	assertManifestOmitsTransport(t, stdout.Bytes())
	if got := strings.Join(manifest.Command, " "); got != "dashboard" {
		t.Fatalf("command = %q, want dashboard", got)
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

func TestJSONSchemaRoleSpecificResultForRigAgentRoutingCommands(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "list", "--json-schema=result"},
		{"rig", "status", "--json-schema=result"},
	} {
		t.Run(strings.Join(args[:len(args)-1], "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run(%v) = %d, stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			var schema struct {
				Required   []string       `json:"required"`
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
				t.Fatalf("result schema is not JSON: %v\n%s", err, stdout.String())
			}
			if !slices.Contains(schema.Required, "schema_version") {
				t.Fatalf("required = %v, want schema_version", schema.Required)
			}
			if schema.Properties["schema_version"] == nil {
				t.Fatalf("schema missing schema_version property: %+v", schema.Properties)
			}
		})
	}
}

func TestJSONSchemaRoleSpecificResultForMailAndTraceShard(t *testing.T) {
	for _, args := range [][]string{
		{"mail", "inbox", "--json-schema=result"},
		{"mail", "read", "--json-schema=result"},
		{"mail", "peek", "--json-schema=result"},
		{"mail", "thread", "--json-schema=result"},
		{"mail", "count", "--json-schema=result"},
		{"trace", "status", "--json-schema=result"},
		{"trace", "show", "--json-schema=result"},
	} {
		t.Run(strings.Join(args[:len(args)-1], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run(%v) = %d, stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
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
		})
	}
}

func TestJSONSchemaManifestForSessionOrderShardCommands(t *testing.T) {
	commands := [][]string{
		{"session", "new"},
		{"session", "submit"},
		{"session", "nudge"},
		{"order", "check"},
		{"order", "run"},
	}
	for _, command := range commands {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			args := append(append([]string{}, command...), "--json-schema=result")
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run(%v) = %d, stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
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
		})
	}
}

func TestJSONSchemaResultForGraphConvergeOrderFormulaActions(t *testing.T) {
	commands := [][]string{
		{"graph"},
		{"converge", "create"},
		{"converge", "approve"},
		{"converge", "iterate"},
		{"converge", "stop"},
		{"converge", "test-gate"},
		{"converge", "test-trigger"},
		{"converge", "retry"},
		{"formula", "cook"},
		{"order", "history"},
	}
	for _, command := range commands {
		t.Run(strings.Join(command, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append(append([]string{}, command...), "--json-schema=result")
			code := run(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run(%s --json-schema=result) = %d, stderr=%q stdout=%q", strings.Join(command, " "), code, stderr.String(), stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			var schema map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
				t.Fatalf("schema is not JSON: %v\n%s", err, stdout.String())
			}
			props, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("schema missing properties: %+v", schema)
			}
			if _, ok := props["schema_version"]; !ok {
				t.Fatalf("schema missing schema_version property: %+v", schema)
			}
		})
	}
}

func validateJSONResultSchema(t *testing.T, commandPath []string, data []byte) {
	t.Helper()

	rawSchema, err := readBuiltinSchema(commandPath, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read %s result schema: %v", strings.Join(commandPath, " "), err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	schemaName := strings.Join(commandPath, "-") + "-result.schema.json"
	if err := compiler.AddResource(schemaName, schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := compiler.Compile(schemaName)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("payload does not match %s result schema: %v\n%s", strings.Join(commandPath, " "), err, string(data))
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
	code := run([]string{"dashboard", "--json-schema=result"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(dashboard --json-schema=result) = 0, want nonzero")
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
	code := run([]string{"dashboard", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(dashboard --json) = 0, want nonzero")
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

func TestJSONSchemaManifestForBdPassthrough(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bd", "--json-schema"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(bd --json-schema) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
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
	assertManifestOmitsTransport(t, stdout.Bytes())
	if got := strings.Join(manifest.Command, " "); got != "bd" {
		t.Fatalf("command = %q, want bd", got)
	}
	if !manifest.JSONSupported {
		t.Fatalf("manifest metadata = %+v", manifest)
	}
	if !json.Valid(manifest.Schemas["result"]) || !json.Valid(manifest.Schemas["failure"]) {
		t.Fatalf("manifest schemas = %+v", manifest.Schemas)
	}
}

func assertManifestOmitsTransport(t *testing.T, data []byte) {
	t.Helper()
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, string(data))
	}
	if _, ok := manifest["transport"]; ok {
		t.Fatalf("manifest exposes deprecated transport field: %s", string(data))
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

func TestJSONLExecutionFailureIsStructured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missingCity := filepath.Join(t.TempDir(), "missing-city")
	code := run([]string{"--city", missingCity, "session", "pin", "missing-session", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run(session pin --json) = 0, want nonzero")
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr empty, want command diagnostics")
	}

	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1 shared failure payload: %q", len(lines), stdout.String())
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
	assertManifestOmitsTransport(t, stdout.Bytes())
	if got := strings.Join(manifest.Command, " "); got != "tools review pr" {
		t.Fatalf("command = %q, want tools review pr", got)
	}
	if !manifest.JSONSupported || !json.Valid(manifest.Schemas["result"]) || !json.Valid(manifest.Schemas["failure"]) {
		t.Fatalf("manifest = %+v", manifest)
	}
}
