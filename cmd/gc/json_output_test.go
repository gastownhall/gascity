package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteJSONErrorIncludesExitCodeOnStdoutAndStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := writeJSONError(&stdout, &stderr, "config_load_failed", "gc status: loading config failed", 7)
	if code != 7 {
		t.Fatalf("code = %d, want 7", code)
	}

	var out cliJSONErrorOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if out.SchemaVersion != "1" || out.OK || out.Error.Code != "config_load_failed" || out.Error.ExitCode != 7 {
		t.Fatalf("stdout error payload = %+v", out)
	}

	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr lines = %d, want 1; stderr=%q", len(lines), stderr.String())
	}
	var diag cliJSONDiagnostic
	if err := json.Unmarshal([]byte(lines[0]), &diag); err != nil {
		t.Fatalf("stderr is not JSONL diagnostic: %v\n%s", err, stderr.String())
	}
	if diag.SchemaVersion != "1" || diag.Level != "error" || diag.Code != "config_load_failed" || diag.ExitCode != 7 {
		t.Fatalf("stderr diagnostic = %+v", diag)
	}
}
