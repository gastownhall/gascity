package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePackDiffFixture writes a small pack into a new temporary directory.
// files maps pack-relative paths to contents.
func writePackDiffFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func packDiffBaseFiles() map[string]string {
	return map[string]string{
		"pack.toml":                             "[pack]\nname = \"fixture\"\nversion = \"0.1.0\"\n",
		"commands/status/run.sh":                "#!/bin/sh\necho ok\n",
		"agents/worker/prompt.template.md":      "Never claim work through gc bd ready; claim it with gc hook --claim.\n",
		"template-fragments/report.template.md": "Write a short report when you finish.\n",
		"formulas/do-work.toml":                 "formula = \"do-work\"\n",
	}
}

func packDiffFilesWith(changes map[string]string, remove ...string) map[string]string {
	files := packDiffBaseFiles()
	for k, v := range changes {
		files[k] = v
	}
	for _, k := range remove {
		delete(files, k)
	}
	return files
}

// TestPackDiffExitCodesAndJSON proves the CLI maps each verdict to its exit
// code in both output modes and that --json output matches the result schema.
func TestPackDiffExitCodesAndJSON(t *testing.T) {
	old := writePackDiffFixture(t, packDiffBaseFiles())
	tests := []struct {
		name     string
		files    map[string]string
		verdict  string
		wantCode int
	}{
		{"identical", packDiffBaseFiles(), "NONE", 0},
		{"formula added", packDiffFilesWith(map[string]string{"formulas/review.toml": "formula = \"review\"\n"}), "ADDITIVE", 0},
		{"prose reworded", packDiffFilesWith(map[string]string{"template-fragments/report.template.md": "Write a short report, with links.\n"}), "UNCLASSIFIED", 1},
		{"command removed", packDiffFilesWith(nil, "commands/status/run.sh"), "BREAKING", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newer := writePackDiffFixture(t, tc.files)

			var stdout, stderr bytes.Buffer
			code := run([]string{"pack", "diff", old, newer}, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d\nstdout=%s\nstderr=%s", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "VERDICT  "+tc.verdict) {
				t.Fatalf("stdout lacks VERDICT  %s:\n%s", tc.verdict, stdout.String())
			}

			args := []string{"pack", "diff", old, newer, "--json"}
			runPackRegistryJSONAndValidateExitCode(t, args, tc.wantCode)
			stdout.Reset()
			stderr.Reset()
			run(args, &stdout, &stderr)
			var got packDiffJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
			}
			if string(got.Verdict) != tc.verdict || got.ExitCode != tc.wantCode {
				t.Fatalf("JSON verdict=%s exit_code=%d, want %s %d", got.Verdict, got.ExitCode, tc.verdict, tc.wantCode)
			}
		})
	}
}

func TestPackDiffBreakingNamesTheMandate(t *testing.T) {
	old := writePackDiffFixture(t, packDiffBaseFiles())
	newer := writePackDiffFixture(t, packDiffFilesWith(map[string]string{
		"agents/worker/prompt.template.md": "Never claim work through gc bd ready; claim it with gc work claim.\n",
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pack", "diff", old, newer}, &stdout, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2\n%s%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"MANDATES no longer stated (1)", "gc hook --claim", "MANDATES newly stated (1)", "gc work claim"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestPackDiffRefusals(t *testing.T) {
	pack := writePackDiffFixture(t, packDiffBaseFiles())
	missing := filepath.Join(t.TempDir(), "missing")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"pack", "diff", pack}, &stdout, &stderr); code != 1 {
		t.Fatalf("one argument: code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "accepts 2 arg(s)") {
		t.Fatalf("one argument: stderr = %q, want an argument-count error", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"pack", "diff", pack, missing}, &stdout, &stderr); code != 1 {
		t.Fatalf("missing pack: code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gc pack diff: reading pack directory") {
		t.Fatalf("missing pack: stderr = %q", stderr.String())
	}

	runPackRegistryJSONFailureAndValidate(t, []string{"pack", "diff", pack, missing, "--json"})
	runPackRegistryJSONFailureAndValidate(t, []string{"pack", "capability", missing, "--json"})
}

func TestPackCapabilityOutput(t *testing.T) {
	pack := writePackDiffFixture(t, packDiffBaseFiles())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"pack", "capability", pack}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"PROVIDES\tcommand:status\n", "MANDATES\tgc hook --claim\n", "PROVIDES\tformula:do-work\n"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}

	runPackRegistryJSONAndValidate(t, []string{"pack", "capability", pack, "--json"})
}

func TestPackDiffSchemasHaveDescriptions(t *testing.T) {
	for _, command := range [][]string{{"pack", "diff"}, {"pack", "capability"}} {
		var stdout, stderr bytes.Buffer
		args := append(append([]string{}, command...), "--json-schema=result")
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s schema code=%d stderr=%q", strings.Join(command, " "), code, stderr.String())
		}
		var schema map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
			t.Fatalf("%s schema not JSON: %v", strings.Join(command, " "), err)
		}
		assertSchemaDescriptions(t, strings.Join(command, " "), schema)
	}
}
