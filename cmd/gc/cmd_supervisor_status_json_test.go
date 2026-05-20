package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSupervisorStatusJSON(t *testing.T) {
	clearGCEnv(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"supervisor", "status", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(supervisor status --json) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string   `json:"schema_version"`
		Running       bool     `json:"running"`
		PID           int      `json:"pid"`
		SocketPath    string   `json:"socket_path"`
		CheckedPaths  []string `json:"checked_paths"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" {
		t.Fatalf("schema_version = %q, want 1", payload.SchemaVersion)
	}
	if len(payload.CheckedPaths) == 0 {
		t.Fatalf("checked_paths empty: %+v", payload)
	}
	if !payload.Running && payload.PID != 0 {
		t.Fatalf("not running with pid = %d", payload.PID)
	}
}
