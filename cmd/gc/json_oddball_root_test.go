package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestOddballRootJSONPrimeUsesSessionTemplateIdentity(t *testing.T) {
	t.Setenv("GC_ALIAS", "rigrepo/furiosa")
	t.Setenv("GC_AGENT", "rigrepo/furiosa")
	t.Setenv("GC_TEMPLATE", "rigrepo/polecat")
	t.Setenv("GC_SESSION_NAME", "rigrepo--furiosa")

	var stdout, stderr bytes.Buffer
	code := run([]string{"prime", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run prime --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["agent"] != "rigrepo/polecat" {
		t.Fatalf("agent = %v, want GC_TEMPLATE identity in %+v", got["agent"], got)
	}
}

func TestOddballRootJSONEventEmitBestEffort(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityPath := writeOddballMinimalCity(t, "city")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "event", "emit", "custom.test", "--subject", "thing", "--message", "hello", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run event emit --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["event_type"] != "custom.test" || got["subject"] != "thing" {
		t.Fatalf("event emit JSON = %+v", got)
	}
	if got["submitted"] != true || got["has_payload"] != false {
		t.Fatalf("event emit best-effort JSON = %+v", got)
	}
	if _, ok := got["recorded"]; ok {
		t.Fatalf("event emit JSON still exposes recorded: %+v", got)
	}
	if _, ok := got["payload"]; ok {
		t.Fatalf("event emit JSON still exposes ambiguous payload boolean: %+v", got)
	}
}

func TestOddballRootJSONEventEmitOpenFailure(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	missingCity := filepath.Join(t.TempDir(), "missing-city")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", missingCity, "event", "emit", "custom.test", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run event emit open failure --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["submitted"] != false {
		t.Fatalf("event emit open-failure JSON = %+v", got)
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr empty, want provider-open diagnostic")
	}
}

func TestOddballRootJSONEventEmitInvalidPayload(t *testing.T) {
	t.Setenv("GC_EVENTS", "fake")
	var stdout, stderr bytes.Buffer
	code := run([]string{"event", "emit", "custom.test", "--payload", "{not json", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run event emit invalid payload --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["submitted"] != false || got["has_payload"] != true {
		t.Fatalf("event emit invalid-payload JSON = %+v", got)
	}
	if !strings.Contains(stderr.String(), "not valid JSON") {
		t.Fatalf("stderr = %q, want invalid JSON diagnostic", stderr.String())
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
	cityPath := writeOddballMinimalCity(t, "city")
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
	contextDir, ok := buildJSON["context_dir"].(string)
	if !ok || contextDir == "" {
		t.Fatalf("build-image JSON missing context_dir: %+v", buildJSON)
	}
	if _, err := os.Stat(contextDir); err != nil {
		t.Fatalf("context_dir %q is not a real directory: %v", contextDir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(contextDir) })
}

func TestOddballRootJSONBuildImageNonContextOnly(t *testing.T) {
	cityPath := writeOddballMinimalCity(t, "city")
	oldBuild := buildImageBuild
	buildImageBuild = func(_ context.Context, _ string, tag string, stdout, _ io.Writer) error {
		fmt.Fprintf(stdout, "docker build for %s\n", tag) //nolint:errcheck // test writer is bytes.Buffer
		return nil
	}
	t.Cleanup(func() { buildImageBuild = oldBuild })

	var stdout, stderr bytes.Buffer
	code := run([]string{"build-image", cityPath, "--tag", "example:test", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run build-image non-context --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["tag"] != "example:test" || got["context_only"] != false {
		t.Fatalf("build-image JSON = %+v", got)
	}
	if _, ok := got["context_dir"]; ok {
		t.Fatalf("non-context build-image JSON should omit deleted context_dir: %+v", got)
	}
	for _, want := range []string{"Building image example:test", "docker build for example:test", "Image built: example:test"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestOddballRootJSONInitSummaryWriter(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	var stdout bytes.Buffer
	err := writeInitJSONOrExit(0, true, []string{cityPath}, "custom-name", "minimal", "codex", []string{"codex"}, "k8s-cell", "provider", &stdout)
	if err != nil {
		t.Fatalf("writeInitJSONOrExit: %v", err)
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["city_path"] != cityPath || got["city_name"] != "custom-name" || got["provider"] != "codex" {
		t.Fatalf("init JSON = %+v", got)
	}
}

func TestOddballRootJSONInitFromFileRun(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	source := filepath.Join(t.TempDir(), "source-city.toml")
	if err := os.WriteFile(source, []byte("[workspace]\nname = \"source\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cityPath := filepath.Join(t.TempDir(), "city")

	var stdout, stderr bytes.Buffer
	code := run([]string{"init", "--file", source, "--skip-provider-readiness", cityPath, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run init --file --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["mode"] != "file" || got["city_path"] != cityPath {
		t.Fatalf("init --file JSON = %+v", got)
	}
}

func TestOddballRootJSONInitDefaultSkipsTTYWizard(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	oldIsTerminal := isTerminalFunc
	isTerminalFunc = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminalFunc = oldIsTerminal })

	stdin := writeTempInput(t, "\n\n")
	oldStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = stdin.Close()
	})

	cityPath := filepath.Join(t.TempDir(), "city")
	var stdout, stderr bytes.Buffer
	code := run([]string{"init", "--skip-provider-readiness", cityPath, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run init default --json = %d; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	got := requireSingleJSONLine(t, &stdout)
	if got["mode"] != "default" || got["provider"] != nil {
		t.Fatalf("init default JSON = %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "provider =") {
		t.Fatalf("init --json used the interactive provider wizard; city.toml:\n%s", string(data))
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
		assertManifestOmitsTransport(t, stdout.Bytes())
		if got["json_supported"] != true {
			t.Fatalf("%v json_supported = %v, want true: %+v", args, got["json_supported"], got)
		}
	}
}

func writeOddballMinimalCity(t *testing.T, name string) string {
	t.Helper()
	cityPath := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \""+name+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func writeTempInput(t *testing.T, content string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return f
}
