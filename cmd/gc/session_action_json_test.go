package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionActionJSONLine(t *testing.T) {
	var stdout bytes.Buffer
	pinned := true
	writeSessionActionJSON(&stdout, sessionActionResult{
		Action:            "pin",
		SessionID:         "gc-1",
		Pinned:            &pinned,
		MaterializedNamed: true,
	})

	if strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("stdout = %q, want exactly one JSONL record", stdout.String())
	}
	var got struct {
		SchemaVersion     string `json:"schema_version"`
		OK                bool   `json:"ok"`
		Action            string `json:"action"`
		SessionID         string `json:"session_id"`
		Pinned            bool   `json:"pinned"`
		MaterializedNamed bool   `json:"materialized_named"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || !got.OK || got.Action != "pin" || got.SessionID != "gc-1" || !got.Pinned || !got.MaterializedNamed {
		t.Fatalf("payload = %+v", got)
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
				XGCJSONL map[string]any `json:"x-gc-jsonl"`
				Required []string       `json:"required"`
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
		})
	}
}
