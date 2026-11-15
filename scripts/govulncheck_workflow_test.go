package scripts_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// govulncheckGateStep is the govulncheck.yml step that reports findings and
// gates on reachable ones.
const govulncheckGateStep = "Summarize findings and enforce reachable vulnerability policy"

// TestGovulncheckGateReportsWhatItFails runs the gate step as Actions runs it
// (bash -e: Actions' default shell, whatever the script itself sets) against
// a stub govulncheck. Whatever govulncheck's exit status, the findings reach
// the job log and the step summary, and the status stands as the step's own.
// Under bash -e a bare `govulncheck ...; status=$?` ends the step at exit 3,
// before the summary and the log are written, so a red check showed only
// "Process completed with exit code 3" and no finding.
func TestGovulncheckGateReportsWhatItFails(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), ".github/workflows/govulncheck.yml")), &workflow); err != nil {
		t.Fatalf("parsing govulncheck.yml: %v", err)
	}
	var script string
	for _, step := range workflow.Jobs["govulncheck"].Steps {
		if step.Name == govulncheckGateStep {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatalf("govulncheck.yml job govulncheck has no step %q", govulncheckGateStep)
	}

	for _, tc := range []struct {
		name    string
		status  int
		verdict string
	}{
		{"a reachable vulnerability", 3, "A reachable vulnerability was found"},
		{"no reachable vulnerability", 0, "No vulnerable symbol is reachable"},
		{"a scanner failure", 1, "govulncheck did not complete: exit status 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			for _, d := range []string{bin, filepath.Join(dir, "govulncheck-results")} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub := "#!/bin/sh\necho 'Vulnerability #1: GO-2026-0001'\necho '    stub finding body'\nexit \"$STUB_STATUS\"\n"
			if err := os.WriteFile(filepath.Join(bin, "govulncheck"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			summary := filepath.Join(dir, "summary")
			out, err := runWorkflowStepScript(t, dir, script, map[string]string{
				"PATH":                bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"GITHUB_STEP_SUMMARY": summary,
				"STUB_STATUS":         strconv.Itoa(tc.status),
			})
			got := 0
			var exitErr interface{ ExitCode() int }
			if errors.As(err, &exitErr) {
				got = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("running the gate step: %v\n%s", err, out)
			}
			if got != tc.status {
				t.Errorf("the gate step exited %d, want govulncheck's own %d:\n%s", got, tc.status, out)
			}
			if !strings.Contains(out, "GO-2026-0001") {
				t.Errorf("the job log does not show the finding:\n%s", out)
			}
			data, err := os.ReadFile(summary)
			if err != nil {
				t.Fatalf("the gate step wrote no step summary: %v\n%s", err, out)
			}
			for _, want := range []string{tc.verdict, "GO-2026-0001"} {
				if !strings.Contains(string(data), want) {
					t.Errorf("the step summary does not say %q:\n%s", want, data)
				}
			}
		})
	}
}
