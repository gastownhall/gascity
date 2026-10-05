package scripts_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Cross-phase test skipping is measured from Build Event Protocol files:
// every `bazel test` invocation in bazel-test.yml writes one
// (--build_event_json_file), and the "Bazel test cache report (BEP)" step
// feeds each of them to scripts/bazel-bep-summary.go, which renders cached vs
// executed test-target counts into the job summary and writes a JSON report
// that the upload step keeps as an artifact. A new `bazel test` invocation
// without a BEP file, or a BEP file the report step does not read, would
// silently drop that phase from the trend. bazel.yml does the same per lane:
// each lane uploads its BEP file and the bep-summary job reports them all,
// one phase per lane. A raw BEP file holds the expanded command line
// (--remote_executor, the client key path) and bytestream:// URIs naming the
// remote cache, so a lane uploads only its redact.jq projection. Every upload
// is continue-on-error: an artifact-service failure never fails a job.

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
	if upload.If != "always()" || !upload.ContinueOnError || !strings.HasPrefix(upload.Uses, "actions/upload-artifact@") || upload.With["path"] != bepReportJSONOut {
		t.Errorf("%q must be an always(), continue-on-error actions/upload-artifact of %s, got if=%q continue-on-error=%t uses=%q path=%q",
			bepUploadStep, bepReportJSONOut, upload.If, upload.ContinueOnError, upload.Uses, upload.With["path"])
	}

	// The report tool itself must exist where the step runs it.
	src := readFile(t, root, bepReportScript)
	if !strings.Contains(src, "bepsummary.Run(") {
		t.Errorf("%s must call bepsummary.Run", bepReportScript)
	}
}

// bazel.yml's BEP plumbing: lane job step "test" writes laneBEPFile, step
// "Redact BEP file" projects it with laneBEPRedact into laneBEPUploadPath,
// step "Upload BEP file" keeps that as bazel-bep-<lane>-<attempt>, and the
// bep-summary job downloads them all and reports one phase per lane.
const (
	laneBEPFile        = `--build_event_json_file="$RUNNER_TEMP/bazel-bep.json"`
	laneBEPRedactStep  = "Redact BEP file"
	laneBEPRedact      = `jq -R -c -f internal/testpolicy/bepsummary/redact.jq "$RUNNER_TEMP/bazel-bep.json"`
	laneBEPUploadPath  = "${{ runner.temp }}/bep-upload/bazel-bep.json"
	laneBEPUploadStep  = "Upload BEP file"
	laneBEPArtifact    = "bazel-bep-${{ matrix.lane }}-${{ github.run_attempt }}"
	bepSummaryJob      = "bep-summary"
	bepSummaryDownload = "Download lane BEP files"
)

// bepWorkflowStep is a step with the fields the BEP checks read.
type bepWorkflowStep struct {
	ID              string            `yaml:"id"`
	Name            string            `yaml:"name"`
	If              string            `yaml:"if"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	With            map[string]string `yaml:"with"`
}

type bepWorkflowJob struct {
	If    string            `yaml:"if"`
	Needs yaml.Node         `yaml:"needs"`
	Steps []bepWorkflowStep `yaml:"steps"`
}

// needs returns the job's needs as a list (YAML allows a bare string).
func (j bepWorkflowJob) needs() []string {
	if j.Needs.Kind == yaml.ScalarNode {
		return []string{j.Needs.Value}
	}
	var list []string
	for _, n := range j.Needs.Content {
		list = append(list, n.Value)
	}
	return list
}

func bepStepIndex(steps []bepWorkflowStep, match func(bepWorkflowStep) bool) int {
	return slices.IndexFunc(steps, match)
}

func TestBazelMultiLaneWorkflowEveryLaneFeedsTheCacheReport(t *testing.T) {
	var wf struct {
		Jobs map[string]bepWorkflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), bazelMultiLaneWorkflow)), &wf); err != nil {
		t.Fatalf("parse %s: %v", bazelMultiLaneWorkflow, err)
	}

	// Every lane is one `bazel test`, and the test step writes its BEP file.
	for lane, cmd := range multiLaneCommands {
		if !strings.HasPrefix(cmd, "test ") {
			t.Errorf("lane %s runs %q; the cache report counts `bazel test` invocations only", lane, cmd)
		}
	}
	lane := wf.Jobs["lane"]
	testIdx := bepStepIndex(lane.Steps, func(s bepWorkflowStep) bool { return s.ID == "test" })
	if testIdx < 0 {
		t.Fatalf("%s: the lane job has no step with id test", bazelMultiLaneWorkflow)
	}
	if !strings.Contains(lane.Steps[testIdx].Run, laneBEPFile) {
		t.Errorf("%s lane step %q must pass %s", bazelMultiLaneWorkflow, lane.Steps[testIdx].Name, laneBEPFile)
	}
	redactIdx := bepStepIndex(lane.Steps, func(s bepWorkflowStep) bool { return s.Name == laneBEPRedactStep })
	uploadIdx := bepStepIndex(lane.Steps, func(s bepWorkflowStep) bool { return s.Name == laneBEPUploadStep })
	if redactIdx < testIdx || uploadIdx < redactIdx {
		t.Fatalf("%s: the lane job needs %q then %q steps after the test step", bazelMultiLaneWorkflow, laneBEPRedactStep, laneBEPUploadStep)
	}
	red := lane.Steps[redactIdx]
	// The redacted file appears only once jq has finished (temp file, then
	// mv), so a failed redaction uploads nothing rather than a partial file.
	if !strings.HasPrefix(red.If, "always()") || !red.ContinueOnError || !strings.Contains(red.Run, laneBEPRedact) ||
		!strings.Contains(red.Run, `mv -f "$out.tmp" "$out"`) || !strings.Contains(red.Run, `out="$RUNNER_TEMP/bep-upload/bazel-bep.json"`) {
		t.Errorf("%q must be an always(), continue-on-error step that runs %s into $RUNNER_TEMP/bep-upload/bazel-bep.json via a temp file, got if=%q continue-on-error=%t run:\n%s",
			laneBEPRedactStep, laneBEPRedact, red.If, red.ContinueOnError, red.Run)
	}
	up := lane.Steps[uploadIdx]
	if !strings.HasPrefix(up.If, "always()") || !up.ContinueOnError || !strings.HasPrefix(up.Uses, "actions/upload-artifact@") ||
		up.With["name"] != laneBEPArtifact || up.With["path"] != laneBEPUploadPath {
		t.Errorf("%q must be an always(), continue-on-error upload of the redacted BEP file %s as %s, got if=%q continue-on-error=%t uses=%q name=%q path=%q",
			laneBEPUploadStep, laneBEPUploadPath, laneBEPArtifact, up.If, up.ContinueOnError, up.Uses, up.With["name"], up.With["path"])
	}
	// The raw BEP file never leaves the runner.
	for name, job := range wf.Jobs {
		for _, s := range job.Steps {
			if strings.HasPrefix(s.Uses, "actions/upload-artifact@") && strings.Contains(s.With["path"], "bazel-bep.json") && s.With["path"] != laneBEPUploadPath {
				t.Errorf("%s job %s step %q uploads %q; only the redacted %s may be uploaded", bazelMultiLaneWorkflow, name, s.Name, s.With["path"], laneBEPUploadPath)
			}
		}
	}

	// No other job runs `bazel test` outside the lanes (it would be missing
	// from the report).
	for name, job := range wf.Jobs {
		if name == "lane" {
			continue
		}
		for _, s := range job.Steps {
			if bazelTestInvocation.MatchString(strings.ReplaceAll(s.Run, "\\\n", " ")) {
				t.Errorf("%s job %s step %q runs `bazel test` outside the lane job; the cache report would miss it", bazelMultiLaneWorkflow, name, s.Name)
			}
		}
	}

	job, ok := wf.Jobs[bepSummaryJob]
	if !ok {
		t.Fatalf("%s has no %s job", bazelMultiLaneWorkflow, bepSummaryJob)
	}
	if !slices.Contains(job.needs(), "lane") || !strings.HasPrefix(job.If, "always()") {
		t.Errorf("%s job must need lane and run if: always() (failed lanes are reported too), got needs=%v if=%q", bepSummaryJob, job.needs(), job.If)
	}
	if slices.Contains(wf.Jobs["gate"].needs(), bepSummaryJob) {
		t.Errorf("the gate needs %s; the cache report is reporting only", bepSummaryJob)
	}
	dl := bepStepIndex(job.Steps, func(s bepWorkflowStep) bool { return s.Name == bepSummaryDownload })
	report := bepStepIndex(job.Steps, func(s bepWorkflowStep) bool { return s.Name == bepReportStep })
	upload := bepStepIndex(job.Steps, func(s bepWorkflowStep) bool { return s.Name == bepUploadStep })
	if dl < 0 || report < dl || upload < report {
		t.Fatalf("%s job needs steps %q, %q, %q in that order", bepSummaryJob, bepSummaryDownload, bepReportStep, bepUploadStep)
	}
	d := job.Steps[dl]
	if !strings.HasPrefix(d.Uses, "actions/download-artifact@") || d.With["pattern"] != "bazel-bep-*-${{ github.run_attempt }}" || d.With["path"] != "${{ runner.temp }}/bazel-bep" {
		t.Errorf("%q must download every lane's artifact of this attempt into ${{ runner.temp }}/bazel-bep, got uses=%q with=%v", bepSummaryDownload, d.Uses, d.With)
	}
	r := job.Steps[report]
	if !r.ContinueOnError {
		t.Errorf("%q must be continue-on-error: a report failure never fails the run", bepReportStep)
	}
	for _, want := range []string{bepReportScript, `"$GITHUB_STEP_SUMMARY"`, `--json-out "$dir/summary.json"`, "--allow-missing", `dir="$RUNNER_TEMP/bazel-bep"`} {
		if !strings.Contains(r.Run, want) {
			t.Errorf("%q run must contain %q", bepReportStep, want)
		}
	}
	// One phase per lane, named after it, read from the lane's artifact.
	for _, name := range multiLaneLaneNames() {
		want := name + `="$dir/bazel-bep-` + name + `-$ATTEMPT/bazel-bep.json"`
		if !strings.Contains(r.Run, want) {
			t.Errorf("%q does not report lane %s as %s", bepReportStep, name, want)
		}
	}
	u := job.Steps[upload]
	if u.If != "always()" || !u.ContinueOnError || !strings.HasPrefix(u.Uses, "actions/upload-artifact@") || u.With["path"] != "${{ runner.temp }}/bazel-bep/summary.json" ||
		u.With["name"] == "" || strings.HasPrefix(u.With["name"], "bazel-bep-") {
		t.Errorf("%q must be an always(), continue-on-error upload of the JSON report under a name the download pattern cannot match, got if=%q continue-on-error=%t uses=%q with=%v",
			bepUploadStep, u.If, u.ContinueOnError, u.Uses, u.With)
	}
}
