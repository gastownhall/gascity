package scripts_test

import (
	"regexp"
	"strings"
	"testing"
)

// Cross-phase test skipping is measured from Build Event Protocol files:
// every `bazel test` invocation in bazel-test.yml writes one
// (--build_event_json_file), and the "Bazel test cache report (BEP)" step
// feeds each of them to scripts/bazel-bep-summary.go, which renders cached vs
// executed test-target counts into the job summary and writes a JSON report
// that the upload step keeps as an artifact. A new `bazel test` invocation
// without a BEP file, or a BEP file the report step does not read, would
// silently drop that phase from the trend.

const (
	bepReportStep    = "Bazel test cache report (BEP)"
	bepUploadStep    = "Upload bazel test cache report"
	bepReportScript  = "scripts/bazel-bep-summary.go"
	bepReportJSONOut = "/tmp/bazel-bep/summary.json"
)

var (
	// A bazel invocation whose command is `test` (not build/coverage/run),
	// after shell line continuations are joined.
	bazelTestInvocation = regexp.MustCompile(`(?m)^\s*bazel\s(?:[^\n]*?\s)?test\s[^\n]*$`)
	bepFileFlag         = regexp.MustCompile(`--build_event_json_file=(\S+)`)
)

func TestBazelTestWorkflowEveryTestInvocationFeedsTheCacheReport(t *testing.T) {
	root := repoRoot(t)
	steps := bazelTestWorkflowSteps(t, root)

	reportIdx, uploadIdx := -1, -1
	lastTestStep := -1
	var bepFiles []string
	seen := map[string]string{}
	for i, step := range steps {
		switch step.Name {
		case bepReportStep:
			reportIdx = i
		case bepUploadStep:
			uploadIdx = i
		}
		joined := strings.ReplaceAll(step.Run, "\\\n", " ")
		for _, inv := range bazelTestInvocation.FindAllString(joined, -1) {
			lastTestStep = i
			m := bepFileFlag.FindStringSubmatch(inv)
			if m == nil {
				t.Errorf("step %q runs `bazel test` without --build_event_json_file:\n%s", step.Name, strings.TrimSpace(inv))
				continue
			}
			if prev, dup := seen[m[1]]; dup {
				t.Errorf("BEP file %s is written by two invocations (%q and %q); each phase needs its own", m[1], prev, step.Name)
			}
			seen[m[1]] = step.Name
			bepFiles = append(bepFiles, m[1])
		}
	}
	if len(bepFiles) == 0 {
		t.Fatalf("found no `bazel test` invocations in %s; the invocation pattern no longer matches", bazelTestWorkflow)
	}
	if reportIdx < 0 {
		t.Fatalf("%s has no %q step", bazelTestWorkflow, bepReportStep)
	}
	report := steps[reportIdx]
	if report.If != "always()" {
		t.Errorf("%q must run with if: always() so failed or canceled test steps are still reported, got %q", bepReportStep, report.If)
	}
	if reportIdx < lastTestStep {
		t.Errorf("%q (step %d) must run after the last `bazel test` step (%d)", bepReportStep, reportIdx, lastTestStep)
	}
	for _, want := range []string{bepReportScript, `"$GITHUB_STEP_SUMMARY"`, "--json-out " + bepReportJSONOut, "--allow-missing"} {
		if !strings.Contains(report.Run, want) {
			t.Errorf("%q run must contain %q", bepReportStep, want)
		}
	}
	for _, f := range bepFiles {
		if !regexp.MustCompile(`\s[A-Za-z0-9_-]+=` + regexp.QuoteMeta(f) + `(\s|$)`).MatchString(report.Run) {
			t.Errorf("%q does not read BEP file %s (written by %q) as PHASE=%s", bepReportStep, f, seen[f], f)
		}
	}

	if uploadIdx < reportIdx {
		t.Fatalf("%s needs an %q step after %q", bazelTestWorkflow, bepUploadStep, bepReportStep)
	}
	upload := steps[uploadIdx]
	if upload.If != "always()" || !strings.HasPrefix(upload.Uses, "actions/upload-artifact@") || upload.With["path"] != bepReportJSONOut {
		t.Errorf("%q must be an always() actions/upload-artifact of %s, got if=%q uses=%q path=%q",
			bepUploadStep, bepReportJSONOut, upload.If, upload.Uses, upload.With["path"])
	}

	// The report tool itself must exist where the step runs it.
	src := readFile(t, root, bepReportScript)
	if !strings.Contains(src, "bepsummary.Run(") {
		t.Errorf("%s must call bepsummary.Run", bepReportScript)
	}
}
