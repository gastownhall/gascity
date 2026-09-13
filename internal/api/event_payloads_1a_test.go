package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestWorkerOperationEventPayload1aFieldsRoundTrip verifies the 1a-added
// fields (#1252) survive JSON round-trip with the documented JSON tag
// names. Wire compatibility check — the worker package uses the same
// JSON shape on its internal payload struct.
func TestWorkerOperationEventPayload1aFieldsRoundTrip(t *testing.T) {
	original := WorkerOperationEventPayload{
		OpID:                "op-1",
		Operation:           "message",
		Result:              "succeeded",
		StartedAt:           time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
		FinishedAt:          time.Date(2026, 4, 25, 0, 0, 1, 0, time.UTC),
		DurationMs:          1000,
		Model:               "claude-opus-4-7",
		AgentName:           "rig/polecat-1",
		PromptVersion:       "v3",
		PromptSHA:           "abc123def456",
		BeadID:              "rig-42",
		PromptTokens:        500,
		CompletionTokens:    250,
		CacheReadTokens:     5000,
		CacheCreationTokens: 1000,
		LatencyMs:           890,
		CostUSDEstimate:     0.01875,
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, expected := range []string{
		`"model":"claude-opus-4-7"`,
		`"agent_name":"rig/polecat-1"`,
		`"prompt_version":"v3"`,
		`"prompt_sha":"abc123def456"`,
		`"bead_id":"rig-42"`,
		`"prompt_tokens":500`,
		`"completion_tokens":250`,
		`"cache_read_tokens":5000`,
		`"cache_creation_tokens":1000`,
		`"latency_ms":890`,
		`"cost_usd_estimate":0.01875`,
	} {
		if !strings.Contains(string(raw), expected) {
			t.Errorf("expected payload to contain %q, got %s", expected, raw)
		}
	}

	var got WorkerOperationEventPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q", got.Model)
	}
	if got.PromptTokens != 500 {
		t.Errorf("PromptTokens = %d", got.PromptTokens)
	}
	if got.CostUSDEstimate != 0.01875 {
		t.Errorf("CostUSDEstimate = %v", got.CostUSDEstimate)
	}
}

// TestWorkerOperationEventPayload1aFieldsOmitEmpty verifies that the new
// fields are omitted from the JSON when at zero value. Keeps events
// compact for operations that lack data sources (lifecycle ops, internal
// polling).
func TestWorkerOperationEventPayload1aFieldsOmitEmpty(t *testing.T) {
	minimal := WorkerOperationEventPayload{
		OpID:       "op-1",
		Operation:  "stop",
		Result:     "succeeded",
		StartedAt:  time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 4, 25, 0, 0, 1, 0, time.UTC),
		DurationMs: 1000,
	}
	raw, err := json.Marshal(minimal)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, banned := range []string{
		"model", "agent_name", "prompt_version", "prompt_sha", "bead_id",
		"prompt_tokens", "completion_tokens", "cache_read_tokens",
		"cache_creation_tokens", "latency_ms", "cost_usd_estimate",
	} {
		if strings.Contains(string(raw), `"`+banned+`"`) {
			t.Errorf("minimal payload contains %q without source data: %s", banned, raw)
		}
	}
}

func TestWorkerOperationEventPayloadMatchesWorkerJSONShape(t *testing.T) {
	apiFields := jsonFieldNamesFromReflectType(t, reflect.TypeOf(WorkerOperationEventPayload{}))
	workerFields := jsonFieldNamesFromSourceStruct(t, filepath.Join("..", "worker", "operation_events.go"), "operationEventPayload")

	if got, want := strings.Join(workerFields, ","), strings.Join(apiFields, ","); got != want {
		t.Fatalf("worker.operation payload JSON fields drifted\nworker: %s\napi:    %s", got, want)
	}
}

func jsonFieldNamesFromReflectType(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := jsonTagName(field.Tag.Get("json"), field.Name)
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func jsonFieldNamesFromSourceStruct(t *testing.T, path, typeName string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != typeName {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("%s is not a struct", typeName)
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
				for _, name := range field.Names {
					jsonName := jsonTagName(tag.Get("json"), name.Name)
					if jsonName != "" {
						out = append(out, jsonName)
					}
				}
			}
			sort.Strings(out)
			return out
		}
	}
	t.Fatalf("type %s not found in %s", typeName, path)
	return nil
}

func jsonTagName(tag, fallback string) string {
	if tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return fallback
	}
	return name
}
