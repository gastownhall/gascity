package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestLintValidPackPasses(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "valid", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "Agent {{.AgentName}} work {{.WorkQuery}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "ok") {
		t.Fatalf("stdout missing ok status: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestLintUsesRuntimeMissingKeyPolicyForUnknownVariables(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "typo", "witness", "prompts/witness.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "witness.template.md"), "runtime-compatible {{.CommitSha}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestLintPromptContextDoesNotInjectAlias(t *testing.T) {
	packDir := t.TempDir()
	data := buildTemplateData(lintPromptContext(packDir, config.Agent{Name: "worker"}, nil))
	if _, ok := data["Alias"]; ok {
		t.Fatalf("lint prompt data injected Alias = %q, want no lint-only Alias key", data["Alias"])
	}

	data = buildTemplateData(lintPromptContext(packDir, config.Agent{
		Name: "worker",
		Env:  map[string]string{"Alias": "configured"},
	}, nil))
	if got := data["Alias"]; got != "configured" {
		t.Fatalf("Alias = %q, want configured env value", got)
	}
}

func TestLintReportsMalformedTemplateActionWithLine(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "bad-template", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "broken {{if .AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "worker.template.md:") || !strings.Contains(errText, "unexpected EOF") {
		t.Fatalf("stderr missing line-numbered malformed template path:\n%s", errText)
	}
}

func TestLintReportsMalformedPackTOMLWithLine(t *testing.T) {
	packDir := t.TempDir()
	writeFile(t, filepath.Join(packDir, "pack.toml"), "[pack]\nname = \"broken\"\nschema =\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "pack.toml:3:") {
		t.Fatalf("stderr missing line-numbered pack.toml error:\n%s", errText)
	}
}

func TestLintDotWalksPackTOMLDirectories(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	first := filepath.Join(root, "packs", "first")
	writeLintPack(t, first, "first", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(first, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	second := filepath.Join(root, "packs", "second")
	writeLintPack(t, second, "second", "reviewer", "prompts/reviewer.template.md")
	writeLintFile(t, filepath.Join(second, "prompts", "reviewer.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", "."}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint . = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "2 pack(s) ok") {
		t.Fatalf("stdout missing recursive pack count: %q", stdout.String())
	}
}

func TestLintDotHandlesCityRootPackDefaults(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	writeLintFile(t, filepath.Join(root, "pack.toml"), `[pack]
name = "city-root"
version = "0.1.0"
schema = 2

[imports.worker]
source = "packs/worker"

[defaults.rig.imports.worker]
source = "packs/worker"
`)
	writeLintPack(t, filepath.Join(root, "packs", "worker"), "worker", "builder", "prompts/builder.template.md")
	writeLintFile(t, filepath.Join(root, "packs", "worker", "prompts", "builder.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", "."}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint . = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}

func TestLintEmitsLoaderWarnings(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "warns", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")
	appendLintFile(t, filepath.Join(packDir, "pack.toml"), "\n[agents]\nwake_mode = \"resume\"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want warnings-only success\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "warning") || !strings.Contains(errText, "deprecated compatibility alias") {
		t.Fatalf("stderr missing loader warning:\n%s", errText)
	}
}

func TestLintPromptDiscoverySkipsIgnoredDirs(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "skip-dirs", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")
	writeLintFile(t, filepath.Join(packDir, ".gc", "bad.template.md"), "broken {{if .AgentName}}\n")
	writeLintFile(t, filepath.Join(packDir, "node_modules", "dep", "bad.template.md"), "broken {{if .AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want ignored dirs skipped\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}

func TestLintReportsMissingInjectFragment(t *testing.T) {
	packDir := t.TempDir()
	writeLintFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "missing-frag"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
inject_fragments = ["missing-footer"]
`)
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `inject_fragment "missing-footer"`) {
		t.Fatalf("stderr missing inject fragment diagnostic:\n%s", stderr.String())
	}
}

func TestLintJSONReportsDiagnostics(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "json-bad", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "broken {{if .AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir, "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint --json succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want diagnostics in JSON stdout only", stderr.String())
	}

	var report struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Passed        bool   `json:"passed"`
		ErrorCount    int    `json:"error_count"`
		Packs         []struct {
			Path        string `json:"path"`
			OK          bool   `json:"ok"`
			Diagnostics []struct {
				Path    string `json:"path"`
				Line    int    `json:"line"`
				Message string `json:"message"`
			} `json:"diagnostics"`
		} `json:"packs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	validateLintJSONSchema(t, stdout.Bytes())
	if report.SchemaVersion != "2" {
		t.Fatalf("schema_version = %q, want 2", report.SchemaVersion)
	}
	if !report.OK {
		t.Fatalf("report.OK = false, want true transport discriminator: %+v", report)
	}
	if report.Passed {
		t.Fatalf("report.Passed = true, want false: %+v", report)
	}
	if report.ErrorCount != 1 {
		t.Fatalf("error_count = %d, want 1: %+v", report.ErrorCount, report)
	}
	if len(report.Packs) != 1 || len(report.Packs[0].Diagnostics) != 1 {
		t.Fatalf("unexpected JSON diagnostics: %+v", report)
	}
	diag := report.Packs[0].Diagnostics[0]
	if diag.Line == 0 || !strings.Contains(diag.Message, "unexpected EOF") {
		t.Fatalf("diagnostic = %+v, want line-numbered template error", diag)
	}
}

func TestLintRecursiveJSONReportsSchemaBackedPassAndFail(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	passing := filepath.Join(root, "packs", "passing")
	writeLintPack(t, passing, "passing", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(passing, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", ".", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint . --json = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	validateLintJSONSchema(t, stdout.Bytes())

	failing := filepath.Join(root, "packs", "failing")
	writeLintPack(t, failing, "failing", "reviewer", "prompts/reviewer.template.md")
	writeLintFile(t, filepath.Join(failing, "prompts", "reviewer.template.md"), "broken {{if .AgentName}}\n")

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"lint", ".", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint . --json succeeded, want failing report\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	validateLintJSONSchema(t, stdout.Bytes())
}

func TestLintHelpDocumentsJSONFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint --help = %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "gc lint <pack>") || !strings.Contains(out, "--json") {
		t.Fatalf("help missing lint usage or --json flag:\n%s", out)
	}
}

func writeLintPack(t *testing.T, dir, packName, agentName, promptTemplate string) {
	t.Helper()
	writeLintFile(t, filepath.Join(dir, "pack.toml"), `[pack]
name = "`+packName+`"
version = "0.1.0"
schema = 2

[[agent]]
name = "`+agentName+`"
prompt_template = "`+promptTemplate+`"
`)
}

func writeLintFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLintFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func validateLintJSONSchema(t *testing.T, data []byte) {
	t.Helper()
	rawSchema, err := readBuiltinSchema([]string{"lint"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read lint schema: %v", err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		t.Fatalf("parse lint schema: %v", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse lint payload: %v\n%s", err, string(data))
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("lint/result.schema.json", schemaDoc); err != nil {
		t.Fatalf("add lint schema: %v", err)
	}
	compiled, err := compiler.Compile("lint/result.schema.json")
	if err != nil {
		t.Fatalf("compile lint schema: %v", err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Fatalf("lint payload does not validate: %v\n%s", err, string(data))
	}
}
