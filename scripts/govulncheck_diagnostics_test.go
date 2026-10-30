package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	govulncheckWorkflow = ".github/workflows/govulncheck.yml"
	govulncheckStepName = "Summarize findings and enforce reachable vulnerability policy"
	// govulncheckStubReport stands in for the scanner's verbose report; the
	// tests look for it in the job log and the step summary.
	govulncheckStubReport = "STUB-GOVULNCHECK-REPORT"
)

// govulncheckPolicyStep returns the run: body of the govulncheck workflow's
// summarize-and-enforce step.
func govulncheckPolicyStep(t *testing.T) string {
	t.Helper()
	wf := parseWorkflow(t, govulncheckWorkflow)
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if step.Name == govulncheckStepName {
				return step.Run
			}
		}
	}
	t.Fatalf("%s has no step named %q", govulncheckWorkflow, govulncheckStepName)
	return ""
}

// runGovulncheckPolicyStep runs the step body as Actions does (bash -eo
// pipefail) against a govulncheck stub that prints a report and exits with
// scannerExit. It returns the step's exit code, its job-log output and the
// $GITHUB_STEP_SUMMARY it wrote.
func runGovulncheckPolicyStep(t *testing.T, scannerExit string) (exitCode int, out, summary string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "govulncheck-results"), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/usr/bin/env bash\necho " + govulncheckStubReport + "\nexit " + scannerExit + "\n"
	if err := os.WriteFile(filepath.Join(bin, "govulncheck"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	summaryPath := filepath.Join(dir, "summary.md")
	out, err := runWorkflowStepScript(t, dir, govulncheckPolicyStep(t), map[string]string{
		"PATH":                bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GITHUB_STEP_SUMMARY": summaryPath,
	})
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running the step: %v\n%s", err, out)
		}
		exitCode = exitErr.ExitCode()
	}
	b, readErr := os.ReadFile(summaryPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	return exitCode, out, string(b)
}

// TestGovulncheckPolicyStepKeepsDiagnosticsOnFinding guards the govulncheck
// workflow's "Summarize findings and enforce reachable vulnerability policy"
// step. The step captures govulncheck's exit status with status=$? and then
// writes the step summary and the report to the job log, so it must not run
// under errexit: Actions' default "bash -e" would abort on the scanner's
// nonzero exit (3 = a reachable vulnerable symbol) and discard every
// diagnostic exactly when there is something to diagnose. The job must still
// fail with the scanner's own exit status.
func TestGovulncheckPolicyStepKeepsDiagnosticsOnFinding(t *testing.T) {
	tests := []struct {
		name        string
		scannerExit string
		wantExit    int
		wantSummary string
	}{
		{"reachable vulnerability", "3", 3, "A reachable vulnerability was found"},
		{"scanner failure", "2", 2, "govulncheck did not complete: exit status 2"},
		{"clean scan", "0", 0, "No vulnerable symbol is reachable from this tree."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exitCode, out, summary := runGovulncheckPolicyStep(t, tc.scannerExit)
			if exitCode != tc.wantExit {
				t.Errorf("step exit code = %d, want govulncheck's own status %d\n%s", exitCode, tc.wantExit, out)
			}
			if !strings.Contains(out, govulncheckStubReport) {
				t.Errorf("job log lacks the govulncheck report; the step died before printing it:\n%s", out)
			}
			if !strings.Contains(summary, tc.wantSummary) {
				t.Errorf("step summary lacks %q; the step died before writing it:\n%s", tc.wantSummary, summary)
			}
			if !strings.Contains(summary, govulncheckStubReport) {
				t.Errorf("step summary lacks the govulncheck report:\n%s", summary)
			}
		})
	}
}

// TestGovulncheckPolicyStepKeepsStrictness pins the options the errexit fix
// must leave alone: unset variables and pipeline failures still abort.
func TestGovulncheckPolicyStepKeepsStrictness(t *testing.T) {
	body := govulncheckPolicyStep(t)
	if !strings.Contains(body, "set -uo pipefail") {
		t.Error("step must keep `set -uo pipefail`")
	}
	if !strings.Contains(body, `exit "$status"`) {
		t.Error(`step must end by exiting with govulncheck's own status (exit "$status")`)
	}
}
