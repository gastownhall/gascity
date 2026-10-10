package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalParallelFailureReportNamesFailedJobs drives the real run_fan_out
// and report_failed_jobs bodies over a passing job, a job with failing tests,
// and a job that dies before any test runs. The summary is the only place a
// failure surfaces once the fan-out ends, so it must name each failed job and
// show what broke in it: the `--- FAIL:` lines when there are any, otherwise
// the log tail, because a build failure, a panic, or a timeout prints none.
// A marker left in a reused LOCAL_TEST_LOG_DIR by an earlier run must not be
// reported against this one.
func TestLocalParallelFailureReportNamesFailedJobs(t *testing.T) {
	content := localParallelScript(t)
	logDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(logDir, "stale-job.failed"), nil, 0o644); err != nil {
		t.Fatalf("seed stale marker: %v", err)
	}

	jobs := []string{
		`passing::echo all good`,
		`failing-tests::printf "%s\n" "=== RUN   TestBoom" "--- FAIL: TestBoom (0.00s)" "    boom_test.go:7: boom" FAIL; exit 1`,
		`build-failure::printf "%s\n" "# example.com/pkg" "pkg.go:3:2: undefined: missing" "FAIL	example.com/pkg [build failed]"; exit 2`,
	}
	quoted := make([]string, len(jobs))
	for i, job := range jobs {
		quoted[i] = shellQuote(job)
	}

	lines := []string{
		"#!/usr/bin/env bash",
		"set -euo pipefail",
		shellFunctionSource(t, content, "run_fan_out"),
		shellFunctionSource(t, content, "report_failed_jobs"),
		`gate_fd=""`,
		"local_jobs=1",
		"jobspecs=(" + strings.Join(quoted, " ") + ")",
		"gc_test_gitconfig=/dev/null",
		"export gc_test_gitconfig",
		"export LOCAL_TEST_LOG_DIR=" + shellQuote(logDir),
		`export TEST_LOCAL_NICE=""`,
		"export TEST_LOCAL_GOPATH=" + shellQuote(goEnvValue(t, "GOPATH")),
		"export TEST_LOCAL_GOCACHE=" + shellQuote(goEnvValue(t, "GOCACHE")),
		"export TEST_LOCAL_GOMODCACHE=" + shellQuote(goEnvValue(t, "GOMODCACHE")),
		"export TEST_LOCAL_GOTMPDIR=" + shellQuote(goEnvValue(t, "GOTMPDIR")),
		"export TEST_LOCAL_GOROOT=" + shellQuote(goEnvValue(t, "GOROOT")),
		"set +e",
		"run_fan_out",
		"status=$?",
		"set -e",
		`report_failed_jobs "$LOCAL_TEST_LOG_DIR"`,
		`exit "$status"`,
	}
	harnessPath := filepath.Join(t.TempDir(), "failure_report_harness.sh")
	if err := os.WriteFile(harnessPath, []byte(strings.Join(lines, "\n")+"\n"), 0o755); err != nil {
		t.Fatalf("write harness script: %v", err)
	}

	out, err := testCommand("bash", harnessPath).CombinedOutput()
	if err == nil {
		t.Fatalf("fan-out with failing jobs exited 0; output:\n%s", out)
	}
	report := string(out)
	if i := strings.Index(report, "=== FAILED JOBS ==="); i >= 0 {
		report = report[i:]
	} else {
		t.Fatalf("output has no failed-jobs summary:\n%s", out)
	}

	for _, want := range []string{
		"failing-tests (log: " + filepath.Join(logDir, "failing-tests.log") + ")",
		"--- FAIL: TestBoom (0.00s)",
		"boom_test.go:7: boom",
		"build-failure (log: " + filepath.Join(logDir, "build-failure.log") + ")",
		"pkg.go:3:2: undefined: missing",
		"Logs: " + logDir,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("summary is missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"passing", "stale-job"} {
		if strings.Contains(report, unwanted) {
			t.Errorf("summary reports %q, which did not fail in this run:\n%s", unwanted, out)
		}
	}
}

// shellFunctionSource returns the full `name() { ... }` definition from a
// shell script whose functions close with a `}` at column zero.
func shellFunctionSource(t *testing.T, content, name string) string {
	t.Helper()
	open := "\n" + name + "() {\n"
	start := strings.Index(content, open)
	if start == -1 {
		t.Fatalf("scripts/test-local-parallel: %s() not found", name)
	}
	start++
	end := strings.Index(content[start:], "\n}\n")
	if end == -1 {
		t.Fatalf("scripts/test-local-parallel: %s() closing brace not found", name)
	}
	return content[start : start+end+len("\n}")]
}
