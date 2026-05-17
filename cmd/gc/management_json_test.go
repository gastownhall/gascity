package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManagementJSONTestCity(t *testing.T, cityPath string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityPath, body)
	writePackToml(t, cityPath, "[pack]\nname = \"test-city\"\nschema = 2\n")
}

func decodeOneJSONLine(t *testing.T, stdout *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1: %q", len(lines), stdout.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	return payload
}

func TestAgentAddJSONEmitsOnlySummary(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "agent", "add", "--name", "worker", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run agent add --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	payload := decodeOneJSONLine(t, &stdout)
	if payload["schema_version"] != "1" || payload["ok"] != true || payload["command"] != "agent add" || payload["name"] != "worker" {
		t.Fatalf("payload = %+v", payload)
	}
	if strings.Contains(stdout.String(), "Scaffolded agent") {
		t.Fatalf("stdout contains human text: %q", stdout.String())
	}
}

func TestRigSuspendResumeRemoveJSONEmitOnlySummaries(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"frontend\"\nprefix = \"fe\"\n")

	for _, tc := range []struct {
		args      []string
		command   string
		action    string
		suspended any
	}{
		{[]string{"rig", "suspend", "frontend", "--json"}, "rig suspend", "suspend", true},
		{[]string{"rig", "resume", "frontend", "--json"}, "rig resume", "resume", false},
		{[]string{"rig", "remove", "frontend", "--json"}, "rig remove", "remove", nil},
	} {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"--city", cityPath}, tc.args...), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run %v = %d; stderr=%q stdout=%q", tc.args, code, stderr.String(), stdout.String())
		}
		payload := decodeOneJSONLine(t, &stdout)
		if payload["schema_version"] != "1" || payload["ok"] != true || payload["command"] != tc.command || payload["action"] != tc.action || payload["rig"] != "frontend" {
			t.Fatalf("%v payload = %+v", tc.args, payload)
		}
		if tc.suspended != nil && payload["suspended"] != tc.suspended {
			t.Fatalf("%v suspended = %v, want %v", tc.args, payload["suspended"], tc.suspended)
		}
	}
}

func TestRigAddJSONEmitsOnlySummary(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	writeManagementJSONTestCity(t, cityPath, "[workspace]\nname = \"test-city\"\n")
	rigPath := filepath.Join(t.TempDir(), "frontend")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "rig", "add", rigPath, "--prefix", "fe", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run rig add --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	payload := decodeOneJSONLine(t, &stdout)
	if payload["schema_version"] != "1" || payload["ok"] != true || payload["command"] != "rig add" || payload["rig"] != "frontend" || payload["prefix"] != "fe" {
		t.Fatalf("payload = %+v", payload)
	}
	if strings.Contains(stdout.String(), "Adding rig") || strings.Contains(stdout.String(), "Rig added") {
		t.Fatalf("stdout contains human text: %q", stdout.String())
	}
}

func TestManagementJSONSchemasDeclared(t *testing.T) {
	commands := [][]string{
		{"agent", "add"},
		{"agent", "suspend"},
		{"agent", "resume"},
		{"rig", "add"},
		{"rig", "suspend"},
		{"rig", "resume"},
		{"rig", "remove"},
		{"rig", "set-endpoint"},
		{"wait", "cancel"},
		{"wait", "ready"},
		{"service", "restart"},
	}
	for _, command := range commands {
		args := append(append([]string{}, command...), "--json-schema=result")
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run %v = %d; stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
		}
		var schema map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
			t.Fatalf("%v schema is not JSON: %v\n%s", command, err, stdout.String())
		}
		if schema["$schema"] == "" {
			t.Fatalf("%v schema missing $schema: %+v", command, schema)
		}
	}
}
