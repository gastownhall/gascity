package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	// The sling dry-run case targets a dog pool instance; the city defines
	// its own dog pool (builtin packs no longer supply a fallback dog).
	dogDir := filepath.Join(cityPath, "agents", "dog")
	if err := os.MkdirAll(dogDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(agents/dog): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dogDir, "agent.toml"), []byte("start_command = \"true\"\nmin_active_sessions = 0\nmax_active_sessions = 3\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(agents/dog/agent.toml): %v", err)
	}
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

	t.Run("status optional contracts", func(t *testing.T) {
		// Cover populated and omitted families independently of the empty-city
		// producer case above. Unknown fields remain forbidden at every level.
		rich := []byte(`{
			"schema_version":"1","ok":true,"city_name":"test-city",
			"workspace":{"name":"test-city","path":"/city"},"city_path":"/city",
			"controller":{"running":false},"running":false,"suspended":false,
			"health":{"usable":true,"degraded":false},"agents":[],
			"rigs":[{"name":"rig","path":"/city/rig","suspended":false,"default_sling_targets":["worker"]}],
			"summary":{"total_agents":0,"running_agents":0,"store_health":{
				"path":"/city/.beads","size_bytes":12,"live_rows":0,"live_rows_unknown":true,
				"ratio_mb_per_row":0,"warning":false,"threshold_mb_per_row":1,
				"last_gc_at":"","last_gc_status":""}},
			"_cache_age_s":2,
			"runtime_sessions":{"schema":"gascity.runtime-sessions/v1","observed_at":"2026-10-08T00:00:00Z",
				"provider_complete":false,"sessions":[{"id":"sid","template":"worker","agent_name":"worker",
					"runtime_name":"","provider":"fake","running":false}]},
			"conditional_writes":{"mode":"auto","origin":"config","effective":"degraded",
				"stores":[{"store_id":"city","kind":"bd","probe":"incapable","latch":"unlatched",
					"capable":false,"reason":"unsupported"}],
				"notices":[{"kind":"pending_restart","flag_key":"beads.conditional_writes",
					"env_var":"","config_value":"auto","env_value":"","message":"restart pending"}]}
		}`)
		variants := []struct {
			name   string
			mutate func(map[string]any)
			valid  bool
		}{
			{name: "rich", valid: true},
			{name: "omitted", valid: true, mutate: func(v map[string]any) {
				for _, key := range []string{"runtime_sessions", "conditional_writes", "_cache_age_s"} {
					delete(v, key)
				}
				delete(v["summary"].(map[string]any), "store_health")
				delete(v["rigs"].([]any)[0].(map[string]any), "default_sling_targets")
			}},
			{name: "null sessions", valid: true, mutate: func(v map[string]any) {
				v["runtime_sessions"].(map[string]any)["sessions"] = nil
			}},
			{name: "empty sessions", valid: true, mutate: func(v map[string]any) {
				v["runtime_sessions"].(map[string]any)["sessions"] = []any{}
			}},
			{name: "top unknown", mutate: func(v map[string]any) { v["unknown"] = true }},
			{name: "null projection", mutate: func(v map[string]any) { v["runtime_sessions"] = nil }},
			{name: "projection unknown", mutate: func(v map[string]any) {
				v["runtime_sessions"].(map[string]any)["unknown"] = true
			}},
			{name: "projection missing required", mutate: func(v map[string]any) {
				delete(v["runtime_sessions"].(map[string]any), "provider_complete")
			}},
			{name: "sessions wrong type", mutate: func(v map[string]any) {
				v["runtime_sessions"].(map[string]any)["sessions"] = "wrong"
			}},
			{name: "row unknown", mutate: func(v map[string]any) {
				v["runtime_sessions"].(map[string]any)["sessions"].([]any)[0].(map[string]any)["unknown"] = true
			}},
			{name: "row wrong type", mutate: func(v map[string]any) {
				v["runtime_sessions"].(map[string]any)["sessions"].([]any)[0].(map[string]any)["running"] = "false"
			}},
			{name: "store health wrong type", mutate: func(v map[string]any) {
				v["summary"].(map[string]any)["store_health"].(map[string]any)["live_rows"] = "0"
			}},
			{name: "conditional store unknown", mutate: func(v map[string]any) {
				v["conditional_writes"].(map[string]any)["stores"].([]any)[0].(map[string]any)["unknown"] = true
			}},
		}
		for _, variant := range variants {
			t.Run(variant.name, func(t *testing.T) {
				var payload map[string]any
				if err := json.Unmarshal(rich, &payload); err != nil {
					t.Fatal(err)
				}
				if variant.mutate != nil {
					variant.mutate(payload)
				}
				data, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				err = validateJSONAgainstResultSchemaE([]string{"status"}, data)
				if (err == nil) != variant.valid {
					t.Fatalf("valid = %v, want %v; error = %v", err == nil, variant.valid, err)
				}
			})
		}
	})
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
		{name: "formula catalog", value: formulaCatalogJSON{}},
		{name: "formula list", value: formulaListJSON{}},
		{name: "formula show", value: formulaShowJSON{}},
		{name: "event emit", value: eventEmitJSONResult{}},
		{name: "init", value: initJSONResult{}},
		{name: "import status", value: ImportStatusJSON{}},
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
		{"formula", "catalog"},
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

func TestFormulaWarningSchemasRequireCodeAndMessage(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		payload string
	}{
		{
			name:    "catalog",
			command: []string{"formula", "catalog"},
			payload: `{"schema_version":"1","ok":true,"formulas":[],"summary":{"count":0},"warnings":[{"message":"missing code"}]}`,
		},
		{
			name:    "list",
			command: []string{"formula", "list"},
			payload: `{"schema_version":"1","ok":true,"search_paths":[],"formulas":[],"summary":{"count":0},"warnings":[{"message":"missing code"}]}`,
		},
		{
			name:    "show",
			command: []string{"formula", "show"},
			payload: `{"schema_version":"1","ok":true,"name":"build","search_paths":[],"steps":[],"warnings":[{"message":"missing code"}]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateJSONAgainstResultSchemaE(tc.command, []byte(tc.payload)); err == nil {
				t.Fatalf("schema for %v accepted a warning without code", tc.command)
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
	if err := validateJSONAgainstResultSchemaE(command, data); err != nil {
		t.Fatalf("payload for %v does not validate: %v\n%s", command, err, string(data))
	}
}

func validateJSONAgainstResultSchemaE(command []string, data []byte) error {
	rawSchema, err := readBuiltinSchema(command, jsonSchemaResultRole)
	if err != nil {
		return fmt.Errorf("read schema for %v: %w", command, err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		return fmt.Errorf("parse schema for %v: %w", command, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse payload for %v: %w", command, err)
	}
	compiler := jsonschema.NewCompiler()
	schemaURL := strings.Join(command, "/") + "/result.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDoc); err != nil {
		return fmt.Errorf("add schema resource for %v: %w", command, err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return fmt.Errorf("compile schema for %v: %w", command, err)
	}
	if err := compiled.Validate(instance); err != nil {
		return err
	}
	return nil
}
