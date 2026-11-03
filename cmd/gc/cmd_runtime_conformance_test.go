package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/runtimecontract"
)

func TestRuntimeConformanceCmd_ConformantExecutablePasses(t *testing.T) {
	script := conformantConformanceScript(t)
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeConformanceCmd(&stdout, &stderr)
	cmd.SetArgs([]string{script})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "PASS RPP-LIFECYCLE-001") {
		t.Errorf("output should list requirement-coded PASS lines; got:\n%s", out)
	}
	if !strings.Contains(out, "0 failed") {
		t.Errorf("conformant executable should report 0 failed; got:\n%s", out)
	}
}

func TestRuntimeConformanceCmd_BrokenExecutableExitsNonZero(t *testing.T) {
	// A script that never reports running fails RPP-LIFECYCLE-001.
	script := writeRPPScript(t, `
case "$1" in
  protocol)   echo '{"version":0,"capabilities":[]}' ;;
  start)      cat >/dev/null ;;
  stop)       ;;
  is-running) echo false ;;
  *) exit 2 ;;
esac
`)
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeConformanceCmd(&stdout, &stderr)
	cmd.SetArgs([]string{script})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("broken executable must exit non-zero; stdout:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "FAIL RPP-LIFECYCLE-001") {
		t.Errorf("output should flag the failed requirement; got:\n%s", stdout.String())
	}
}

func TestRuntimeConformanceCmd_JSONReport(t *testing.T) {
	script := conformantConformanceScript(t)
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeConformanceCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--json", script})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr:\n%s", err, stderr.String())
	}
	var report runtimecontract.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not a JSON Report: %v\n%s", err, stdout.String())
	}
	if report.Failed() {
		t.Errorf("conformant report should not be failed; summary=%+v", report.Summary)
	}
	if len(report.Results) != len(runtimecontract.Catalog()) {
		t.Errorf("report has %d results, want one per catalog requirement (%d)",
			len(report.Results), len(runtimecontract.Catalog()))
	}
}

// conformantConformanceScript writes a stateful RPP script that passes the
// full runtimecontract catalog.
func conformantConformanceScript(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	return writeRPPScript(t, `
S='`+state+`'
op="$1"; name="$2"
case "$op" in
  protocol)   echo '{"version":0,"capabilities":[]}' ;;
  start)      cat >/dev/null; if [ -e "$S/$name" ]; then exit 1; fi; : > "$S/$name" ;;
  stop)       rm -f "$S/$name" ;;
  is-running) if [ -e "$S/$name" ]; then echo true; else echo false; fi ;;
  exec)       sh -c "$(cat)" ;;
  provision)  cat >/dev/null ;;
  *) exit 2 ;;
esac
`)
}

// Kills a conformance report that hides whether the executable is
// v2-ready, or names the wrong gaps.
func TestRuntimeConformanceCmd_ReportsV2Readiness(t *testing.T) {
	for want, report := range map[string]runtimecontract.Report{
		"v2-ready: yes": {V2Ready: true},
		"v2-ready: no (missing observe.v1, report-attachment)":                          {V2Missing: []string{"observe.v1", "report-attachment"}},
		"v2-ready: no (gc already reads it through observe.v1; failed RPP-OBSERVE-003)": {V2Failed: []runtimecontract.Code{"RPP-OBSERVE-003"}},
		"v2-ready: no (missing report-attachment; failed RPP-LIFECYCLE-001)":            {V2Missing: []string{"report-attachment"}, V2Failed: []runtimecontract.Code{"RPP-LIFECYCLE-001"}},
	} {
		if got := v2ReadyLine(report); got != want {
			t.Errorf("v2ReadyLine = %q, want %q", got, want)
		}
	}
}
