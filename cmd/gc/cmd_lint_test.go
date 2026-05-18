package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintValidPackPasses(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "valid", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "Agent {{.AgentName}} alias {{.Alias}} work {{.WorkQuery}}\n")

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

func TestLintReportsMissingTemplateVariableWithLine(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "typo", "witness", "prompts/witness.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "witness.template.md"), "before\nbad {{.alias}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "witness.template.md:2:") {
		t.Fatalf("stderr missing line-numbered template path:\n%s", errText)
	}
	if !strings.Contains(errText, "alias") {
		t.Fatalf("stderr missing missing-key name:\n%s", errText)
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
	writeLintFile(t, filepath.Join(second, "prompts", "reviewer.template.md"), "hello {{.Alias}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", "."}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint . = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "2 pack(s) ok") {
		t.Fatalf("stdout missing recursive pack count: %q", stdout.String())
	}
}

func TestLintJSONReportsDiagnostics(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "json-bad", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "bad {{.alias}}\n")

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
	if report.SchemaVersion != "1" {
		t.Fatalf("schema_version = %q, want 1", report.SchemaVersion)
	}
	if report.OK {
		t.Fatalf("report.OK = true, want false: %+v", report)
	}
	if report.ErrorCount != 1 {
		t.Fatalf("error_count = %d, want 1: %+v", report.ErrorCount, report)
	}
	if len(report.Packs) != 1 || len(report.Packs[0].Diagnostics) != 1 {
		t.Fatalf("unexpected JSON diagnostics: %+v", report)
	}
	diag := report.Packs[0].Diagnostics[0]
	if diag.Line != 1 || !strings.Contains(diag.Message, "alias") {
		t.Fatalf("diagnostic = %+v, want line 1 alias error", diag)
	}
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
