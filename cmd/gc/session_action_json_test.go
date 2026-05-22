package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

type failingSessionActionWriter struct {
	err error
}

func (w failingSessionActionWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestSessionActionJSONLine(t *testing.T) {
	var stdout bytes.Buffer
	pinned := true
	if err := writeSessionActionJSON(&stdout, sessionActionResult{
		Action:            "pin",
		SessionID:         "gc-1",
		Pinned:            &pinned,
		MaterializedNamed: true,
	}); err != nil {
		t.Fatalf("writeSessionActionJSON: %v", err)
	}

	if strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("stdout = %q, want exactly one JSONL record", stdout.String())
	}
	var got struct {
		SchemaVersion     string `json:"schema_version"`
		OK                bool   `json:"ok"`
		Command           string `json:"command"`
		Action            string `json:"action"`
		SessionID         string `json:"session_id"`
		Pinned            bool   `json:"pinned"`
		MaterializedNamed bool   `json:"materialized_named"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || !got.OK || got.Command != "session pin" || got.Action != "pin" || got.SessionID != "gc-1" || !got.Pinned || !got.MaterializedNamed {
		t.Fatalf("payload = %+v", got)
	}
}

func TestSessionActionJSONWriteError(t *testing.T) {
	wantErr := errors.New("stdout closed")
	err := writeSessionActionJSON(failingSessionActionWriter{err: wantErr}, sessionActionResult{
		Action:    "wake",
		SessionID: "gc-1",
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("writeSessionActionJSON error = %v, want %v", err, wantErr)
	}
}

func TestSessionMutationActionSchemasDeclared(t *testing.T) {
	for _, args := range [][]string{
		{"session", "wake", "--json-schema=result"},
		{"session", "suspend", "--json-schema=result"},
		{"session", "close", "--json-schema=result"},
		{"session", "kill", "--json-schema=result"},
		{"session", "rename", "--json-schema=result"},
		{"session", "prune", "--json-schema=result"},
		{"session", "reset", "--json-schema=result"},
		{"session", "pin", "--json-schema=result"},
		{"session", "unpin", "--json-schema=result"},
	} {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run(%v) = %d; stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			var schema struct {
				XGCJSONL             map[string]any             `json:"x-gc-jsonl"`
				Required             []string                   `json:"required"`
				Properties           map[string]json.RawMessage `json:"properties"`
				AdditionalProperties *bool                      `json:"additionalProperties"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
				t.Fatalf("schema is not JSON: %v\n%s", err, stdout.String())
			}
			if schema.XGCJSONL == nil {
				t.Fatalf("schema missing x-gc-jsonl: %s", stdout.String())
			}
			if strings.Join(schema.Required, ",") == "" {
				t.Fatalf("schema required is empty: %s", stdout.String())
			}
			if !slices.Contains(schema.Required, "command") {
				t.Fatalf("schema required = %v, want command", schema.Required)
			}
			if schema.AdditionalProperties == nil || !*schema.AdditionalProperties {
				t.Fatalf("schema additionalProperties = %v, want true", schema.AdditionalProperties)
			}
			var commandProperty struct {
				Const string `json:"const"`
			}
			if err := json.Unmarshal(schema.Properties["command"], &commandProperty); err != nil {
				t.Fatalf("schema command property is invalid: %v\n%s", err, stdout.String())
			}
			if want := strings.Join(args[:2], " "); commandProperty.Const != want {
				t.Fatalf("schema command const = %q, want %q", commandProperty.Const, want)
			}
		})
	}
}
