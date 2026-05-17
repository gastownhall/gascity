package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requireSingleJSONLine(t *testing.T, stdout *bytes.Buffer) map[string]any {
	t.Helper()
	out := strings.TrimSuffix(stdout.String(), "\n")
	if out == "" {
		t.Fatalf("stdout empty, want JSON line")
	}
	if strings.Contains(out, "\n") {
		t.Fatalf("stdout has multiple lines, want one JSON line:\n%s", stdout.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got["schema_version"] != "1" {
		t.Fatalf("schema_version = %v, want 1 in %v", got["schema_version"], got)
	}
	return got
}

func TestOddballRootJSONPrimeNoCity(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"prime", "worker", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run prime --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["agent"] != "worker" || got["content"] == "" {
		t.Fatalf("prime JSON = %+v", got)
	}
}

func TestOddballRootJSONEventEmitBestEffort(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"event", "emit", "custom.test", "--subject", "thing", "--message", "hello", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run event emit --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["event_type"] != "custom.test" || got["subject"] != "thing" {
		t.Fatalf("event emit JSON = %+v", got)
	}
}

func TestOddballRootJSONVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run version --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["version"] == "" || got["commit"] == "" || got["date"] == "" {
		t.Fatalf("version JSON = %+v", got)
	}
}

func TestOddballRootJSONInitSkillListAndBuildImageContext(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, "skills", "sample"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "skills", "sample", "SKILL.md"), []byte("# Sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var skillOut, skillErr bytes.Buffer
	code := run([]string{"--city", cityPath, "skill", "list", "--json"}, &skillOut, &skillErr)
	if code != 0 {
		t.Fatalf("run skill list --json = %d; stderr=%q stdout=%q", code, skillErr.String(), skillOut.String())
	}
	skillJSON := requireSingleJSONLine(t, &skillOut)
	if _, ok := skillJSON["entries"].([]any); !ok {
		t.Fatalf("skill JSON missing entries array: %+v", skillJSON)
	}

	var buildOut, buildErr bytes.Buffer
	code = run([]string{"build-image", cityPath, "--context-only", "--json"}, &buildOut, &buildErr)
	if code != 0 {
		t.Fatalf("run build-image --json = %d; stderr=%q stdout=%q", code, buildErr.String(), buildOut.String())
	}
	buildJSON := requireSingleJSONLine(t, &buildOut)
	if buildJSON["context_only"] != true {
		t.Fatalf("build-image JSON = %+v", buildJSON)
	}
}

func TestOddballRootJSONInitSummaryWriter(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	var stdout bytes.Buffer
	err := writeInitJSONOrExit(0, true, []string{cityPath}, "custom-name", "codex", "k8s-cell", "provider", &stdout)
	if err != nil {
		t.Fatalf("writeInitJSONOrExit: %v", err)
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["city_path"] != cityPath || got["city_name"] != "custom-name" || got["provider"] != "codex" {
		t.Fatalf("init JSON = %+v", got)
	}
}

func TestOddballRootJSONSchemaManifests(t *testing.T) {
	for _, args := range [][]string{
		{"init", "--json-schema"},
		{"event", "emit", "--json-schema"},
		{"prime", "--json-schema"},
		{"handoff", "--json-schema"},
		{"skill", "list", "--json-schema"},
		{"build-image", "--json-schema"},
		{"version", "--json-schema"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run %v = %d; stderr=%q stdout=%q", args, code, stderr.String(), stdout.String())
		}
		got := requireSingleJSONLine(t, &stdout)
		if got["json_supported"] != true {
			t.Fatalf("%v json_supported = %v, want true: %+v", args, got["json_supported"], got)
		}
	}
}
