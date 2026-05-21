package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestDirectJSONWriterPayloadsValidateDeclaredSchemas(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityPath := t.TempDir()
	writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n")
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	loop, err := store.Create(beads.Bead{
		Title: "test convergence",
		Type:  "convergence",
		Metadata: map[string]string{
			convergence.FieldState:         "active",
			convergence.FieldIteration:     "1",
			convergence.FieldMaxIterations: "3",
			convergence.FieldGateMode:      "manual",
			convergence.FieldFormula:       "review",
			convergence.FieldTarget:        "worker",
		},
	})
	if err != nil {
		t.Fatalf("create convergence bead: %v", err)
	}
	gatePath := filepath.Join(cityPath, "pass-gate.sh")
	if err := os.WriteFile(gatePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write gate script: %v", err)
	}
	conditionLoop, err := store.Create(beads.Bead{
		Title: "condition convergence",
		Type:  "convergence",
		Metadata: map[string]string{
			convergence.FieldState:         "active",
			convergence.FieldIteration:     "1",
			convergence.FieldMaxIterations: "3",
			convergence.FieldGateMode:      convergence.GateModeCondition,
			convergence.FieldGateCondition: gatePath,
			convergence.FieldFormula:       "review",
			convergence.FieldTarget:        "worker",
		},
	})
	if err != nil {
		t.Fatalf("create condition convergence bead: %v", err)
	}

	tests := []struct {
		name      string
		command   []string
		args      []string
		wantJSONL bool
	}{
		{
			name:    "status",
			command: []string{"status"},
			args:    []string{"status", "--json"},
		},
		{
			name:    "dolt cleanup",
			command: []string{"dolt-cleanup"},
			args:    []string{"dolt-cleanup", "--json", "--max-orphan-dbs", "-1"},
		},
		{
			name:    "converge status",
			command: []string{"converge", "status"},
			args:    []string{"converge", "status", loop.ID, "--json"},
		},
		{
			name:    "converge list",
			command: []string{"converge", "list"},
			args:    []string{"converge", "list", "--json"},
		},
		{
			name:      "converge test gate",
			command:   []string{"converge", "test-gate"},
			args:      []string{"converge", "test-gate", conditionLoop.ID, "--json"},
			wantJSONL: true,
		},
		{
			name:      "sling dry run",
			command:   []string{"sling"},
			args:      []string{"sling", "dog-1", conditionLoop.ID, "--dry-run", "--json"},
			wantJSONL: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"--city", cityPath}, tc.args...), &stdout, &stderr)
			if code != 0 && tc.name != "dolt cleanup" {
				t.Fatalf("run %v = %d; stderr=%q stdout=%q", tc.args, code, stderr.String(), stdout.String())
			}
			if strings.Contains(stdout.String(), "Testing gate:") {
				t.Fatalf("stdout contains human gate text in JSON mode:\n%s", stdout.String())
			}
			if tc.wantJSONL && strings.Count(stdout.String(), "\n") != 1 {
				got := strings.Count(stdout.String(), "\n")
				t.Fatalf("stdout newline count = %d, want one JSONL record:\n%s", got, stdout.String())
			}
			assertTopLevelOKTrue(t, stdout.Bytes())
			validateJSONAgainstResultSchema(t, tc.command, stdout.Bytes())
		})
	}
}

func TestJSONResultStructsExposeExplicitOKField(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "runtime drain-check", value: runtimeDrainCheckJSON{}},
		{name: "runtime action", value: runtimeActionJSON{}},
		{name: "handoff", value: handoffJSONResult{}},
		{name: "build-image", value: buildImageJSONResult{}},
		{name: "mcp list", value: projectedMCPJSON{}},
		{name: "formula list", value: formulaListJSON{}},
		{name: "formula show", value: formulaShowJSON{}},
		{name: "event emit", value: eventEmitJSONResult{}},
		{name: "init", value: initJSONResult{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			field, ok := reflect.TypeOf(tc.value).FieldByName("OK")
			if !ok {
				t.Fatalf("%s JSON result does not expose explicit OK field", tc.name)
			}
			if field.Type.Kind() != reflect.Bool {
				t.Fatalf("%s OK field kind = %s, want bool", tc.name, field.Type.Kind())
			}
			if got := field.Tag.Get("json"); got != "ok" {
				t.Fatalf("%s OK json tag = %q, want ok", tc.name, got)
			}
		})
	}
}

func TestFixedResultSchemasPinSchemaVersion(t *testing.T) {
	for _, command := range [][]string{
		{"handoff"},
		{"build-image"},
		{"formula", "list"},
		{"formula", "show"},
		{"event", "emit"},
		{"init"},
	} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			rawSchema, err := readBuiltinSchema(command, jsonSchemaResultRole)
			if err != nil {
				t.Fatalf("read schema for %v: %v", command, err)
			}
			var schema struct {
				Properties map[string]map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(rawSchema, &schema); err != nil {
				t.Fatalf("parse schema for %v: %v", command, err)
			}
			version, ok := schema.Properties["schema_version"]
			if !ok {
				t.Fatalf("schema for %v lacks schema_version property", command)
			}
			if got := version["const"]; got != "1" {
				t.Fatalf("schema_version const = %#v, want %q", got, "1")
			}
		})
	}
}

func assertTopLevelOKTrue(t *testing.T, data []byte) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("payload is not a top-level object: %v\n%s", err, string(data))
	}
	if payload["ok"] != true {
		t.Fatalf("payload ok = %#v, want true\n%s", payload["ok"], string(data))
	}
}

func validateJSONAgainstResultSchema(t *testing.T, command []string, data []byte) {
	t.Helper()
	rawSchema, err := readBuiltinSchema(command, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read schema for %v: %v", command, err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		t.Fatalf("parse schema for %v: %v", command, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse payload for %v: %v\n%s", command, err, string(data))
	}
	compiler := jsonschema.NewCompiler()
	schemaURL := strings.Join(command, "/") + "/result.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDoc); err != nil {
		t.Fatalf("add schema resource for %v: %v", command, err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile schema for %v: %v", command, err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Fatalf("payload for %v does not validate: %v\n%s", command, err, string(data))
	}
}
