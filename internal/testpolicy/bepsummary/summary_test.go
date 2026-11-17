package bepsummary

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The testdata/*.bep.json fixtures are real Bazel 9.2.0
// --build_event_json_file output, filtered to the event kinds this package
// reads (started, testResult, testSummary, buildMetrics.actionSummary,
// finished) with local paths and hostnames dropped. They come from one tiny
// workspace of five script tests (pass, slow pass, fail, flaky with
// tags=["no-remote"], and a two-shard pass) run against rbe-west:
//
//	cold.bep.json             first run: everything executed (4 remote, flaky local)
//	warm_local_cache.bep.json same command again: "(cached) PASSED" from the
//	                          client's action cache; fail_test reruns
//	disk_cache.bep.json       after `bazel clean`: --disk_cache hits
//	remote_cache.bep.json     after `bazel clean` with --disk_cache=: remote
//	                          cache hits; no-remote flaky_test runs locally
//
// Each table row's counts match Bazel's own "Executed N out of 5 tests" line
// for that invocation.

func fixture(t *testing.T, name string) Phase {
	t.Helper()
	path := filepath.Join("testdata", name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	p, err := SummarizePhase(strings.TrimSuffix(name, ".bep.json"), path, f)
	if err != nil {
		t.Fatalf("SummarizePhase(%s): %v", name, err)
	}
	return p
}

func TestSummarizePhaseFixtures(t *testing.T) {
	tests := []struct {
		file     string
		counts   TargetCounts
		outcomes map[string]Outcome
		attempts map[string]int
	}{
		{
			file: "cold.bep.json",
			counts: TargetCounts{
				Total: 5, Executed: 5, ExecutedRemote: 4, ExecutedLocal: 1,
				Passed: 3, Flaky: 1, Failed: 1,
				ByStatus:       map[string]int{"PASSED": 3, "FLAKY": 1, "FAILED": 1},
				ExecutedMillis: 291 + 1272 + 3224 + 1303 + 1317 + 38 + 39,
			},
			outcomes: map[string]Outcome{
				"//:pass_test": OutcomeExecutedRemote, "//:slow_pass_test": OutcomeExecutedRemote,
				"//:fail_test": OutcomeExecutedRemote, "//:sharded_test": OutcomeExecutedRemote,
				"//:flaky_test": OutcomeExecutedLocal,
			},
			attempts: map[string]int{"remote": 5, "processwrapper-sandbox": 2},
		},
		{
			file: "warm_local_cache.bep.json",
			counts: TargetCounts{
				Total: 5, Cached: 4, CachedLocal: 4, Executed: 1, ExecutedRemote: 1,
				Passed: 3, Flaky: 1, Failed: 1,
				ByStatus:       map[string]int{"PASSED": 3, "FLAKY": 1, "FAILED": 1},
				ExecutedMillis: 242,
				CachedMillis:   3224 + 1317 + 1303 + 1272 + 39,
			},
			outcomes: map[string]Outcome{
				"//:pass_test": OutcomeCachedLocal, "//:flaky_test": OutcomeCachedLocal,
				"//:fail_test": OutcomeExecutedRemote,
			},
			attempts: map[string]int{"local cache": 5, "remote": 1},
		},
		{
			file: "disk_cache.bep.json",
			counts: TargetCounts{
				Total: 5, Cached: 4, CachedDisk: 4, Executed: 1, ExecutedRemote: 1,
				Passed: 4, Failed: 1,
				ByStatus: map[string]int{"PASSED": 4, "FAILED": 1},
			},
			outcomes: map[string]Outcome{
				"//:sharded_test": OutcomeCachedDisk, "//:fail_test": OutcomeExecutedRemote,
			},
			attempts: map[string]int{"disk cache hit": 5, "remote": 1},
		},
		{
			file: "remote_cache.bep.json",
			counts: TargetCounts{
				Total: 5, Cached: 3, CachedRemote: 3, Executed: 2, ExecutedRemote: 1, ExecutedLocal: 1,
				Passed: 3, Flaky: 1, Failed: 1,
				ByStatus:     map[string]int{"PASSED": 3, "FLAKY": 1, "FAILED": 1},
				CachedMillis: 3224 + 1272 + 1317 + 1303,
			},
			outcomes: map[string]Outcome{
				"//:pass_test": OutcomeCachedRemote, "//:sharded_test": OutcomeCachedRemote,
				"//:fail_test": OutcomeExecutedRemote, "//:flaky_test": OutcomeExecutedLocal,
			},
			attempts: map[string]int{"remote cache hit": 4, "remote": 1, "processwrapper-sandbox": 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			p := fixture(t, tc.file)
			got := p.Targets
			if tc.counts.ExecutedMillis == 0 {
				tc.counts.ExecutedMillis = got.ExecutedMillis
			}
			if tc.counts.CachedMillis == 0 {
				tc.counts.CachedMillis = got.CachedMillis
			}
			if !reflect.DeepEqual(got, tc.counts) {
				t.Errorf("counts:\n got %+v\nwant %+v", got, tc.counts)
			}
			if got.Cached+got.Executed+got.NotRun != got.Total {
				t.Errorf("cached+executed+not_run = %d, total %d", got.Cached+got.Executed+got.NotRun, got.Total)
			}
			byLabel := map[string]TestTarget{}
			for _, tt := range p.Tests {
				byLabel[tt.Label] = tt
			}
			for label, want := range tc.outcomes {
				if byLabel[label].Outcome != want {
					t.Errorf("%s outcome = %q, want %q", label, byLabel[label].Outcome, want)
				}
			}
			if !reflect.DeepEqual(p.Attempts, tc.attempts) {
				t.Errorf("attempts by strategy = %v, want %v", p.Attempts, tc.attempts)
			}
			if p.Command != "test" || p.BazelVersion != "9.2.0" || p.InvocationID == "" {
				t.Errorf("started fields: command=%q version=%q uuid=%q", p.Command, p.BazelVersion, p.InvocationID)
			}
			if p.ExitCode != "TESTS_FAILED" {
				t.Errorf("exit code = %q, want TESTS_FAILED", p.ExitCode)
			}
			if p.Truncated {
				t.Error("fixture reported truncated")
			}
		})
	}
}

func TestSummarizePhaseShardAndFlakyDetails(t *testing.T) {
	p := fixture(t, "cold.bep.json")
	byLabel := map[string]TestTarget{}
	for _, tt := range p.Tests {
		byLabel[tt.Label] = tt
	}
	sharded := byLabel["//:sharded_test"]
	if sharded.Shards != 2 || sharded.Results != 2 || sharded.ExecutedMillis != 1303+1317 {
		t.Errorf("sharded_test = %+v, want 2 shards, 2 results, 2620ms", sharded)
	}
	flaky := byLabel["//:flaky_test"]
	if flaky.Status != "FLAKY" || flaky.Attempts != 2 || !reflect.DeepEqual(flaky.Strategies, []string{"processwrapper-sandbox"}) {
		t.Errorf("flaky_test = %+v", flaky)
	}
}

func TestActionSummaryFromBuildMetrics(t *testing.T) {
	a := fixture(t, "cold.bep.json").Actions
	if a == nil {
		t.Fatal("no action summary")
	}
	want := ActionSummary{
		Created: 36, Executed: 17,
		RemoteCacheHits: 3, RemoteExecuted: 7, LocalExecuted: 4, Internal: 11,
		ActionCacheHits: 15, ActionCacheMisses: 17, HasActionCacheStats: true,
	}
	got := *a
	got.Runners = nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("action summary:\n got %+v\nwant %+v", got, want)
	}
	if len(a.Runners) != 5 || a.Runners[0].Name != "total" || a.Runners[0].Count != 17 {
		t.Errorf("runners = %+v", a.Runners)
	}
}

func TestSummarizePhaseSynthetic(t *testing.T) {
	// Hand-built events for cases the fixtures do not cover; field names are
	// the proto3 JSON names from build_event_stream.proto.
	lines := []string{
		`{"id":{"started":{}},"started":{"uuid":"u1","command":"test"}}`,
		// Failed to build: a summary with no testResult.
		`{"id":{"testSummary":{"label":"//a:broken_test"}},"testSummary":{"overallStatus":"FAILED_TO_BUILD"}}`,
		// Result with no summary (stream cut before the summary event); int64 as number.
		`{"id":{"testResult":{"label":"//a:orphan_test","run":1,"shard":1,"attempt":1}},"testResult":{"status":"PASSED","testAttemptDurationMillis":1500,"executionInfo":{"strategy":"linux-sandbox"}}}`,
		// Remote cache hit without a strategy name.
		`{"id":{"testResult":{"label":"//a:hit_test","run":1,"shard":1,"attempt":1}},"testResult":{"status":"PASSED","testAttemptDurationMillis":"700","executionInfo":{"cachedRemotely":true}}}`,
		`{"id":{"testSummary":{"label":"//a:hit_test"}},"testSummary":{"overallStatus":"PASSED","attemptCount":1}}`,
		// Same label in a second configuration is a separate target.
		`{"id":{"testResult":{"label":"//a:hit_test","run":1,"shard":1,"attempt":1,"configuration":{"id":"exec"}}},"testResult":{"status":"TIMEOUT","testAttemptDurationMillis":"9000","executionInfo":{"strategy":"remote"}}}`,
		`{"id":{"testSummary":{"label":"//a:hit_test","configuration":{"id":"exec"}}},"testSummary":{"overallStatus":"TIMEOUT","attemptCount":1}}`,
		`{"id":{"buildFinished":{}},"finished":{"finishTimeMillis":"1"}}`,
	}
	p, err := SummarizePhase("synthetic", "inline", strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	want := TargetCounts{
		Total: 4, Cached: 1, CachedRemote: 1, Executed: 2, ExecutedRemote: 1, ExecutedLocal: 1, NotRun: 1,
		Passed: 1, Failed: 3,
		ByStatus:       map[string]int{"PASSED": 1, "FAILED_TO_BUILD": 1, "NO_STATUS": 1, "TIMEOUT": 1},
		ExecutedMillis: 1500 + 9000,
		CachedMillis:   700,
	}
	if !reflect.DeepEqual(p.Targets, want) {
		t.Errorf("counts:\n got %+v\nwant %+v", p.Targets, want)
	}
	if p.ExitCode != "SUCCESS" {
		t.Errorf("finished without exitCode should be SUCCESS, got %q", p.ExitCode)
	}
	if p.Actions != nil {
		t.Errorf("no buildMetrics event, got actions %+v", p.Actions)
	}
}

func TestSummarizePhaseTruncatedFinalLine(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "cold.bep.json"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// Keep the first three events intact and cut the fourth mid-object.
	cut := strings.Join(lines[:3], "\n") + "\n" + lines[3][:len(lines[3])/2]
	p, err := SummarizePhase("cut", "inline", strings.NewReader(cut))
	if err != nil {
		t.Fatalf("truncated final line should not fail: %v", err)
	}
	if !p.Truncated {
		t.Error("Truncated = false, want true")
	}
	if p.Targets.Total == 0 {
		t.Error("events before the cut were dropped")
	}
}

func TestSummarizePhaseMalformedMiddleLineFails(t *testing.T) {
	in := `{"id":{"started":{}},"started":{"uuid":"u"}}` + "\n" + `{not json` + "\n" + `{"id":{"started":{}}}` + "\n"
	_, err := SummarizePhase("bad", "inline", strings.NewReader(in))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want a line 2 parse error", err)
	}
}

func TestSummarizePhaseBadIntegerFails(t *testing.T) {
	in := `{"id":{"testResult":{"label":"//a:t"}},"testResult":{"testAttemptDurationMillis":"abc"}}` + "\n" +
		`{"id":{"started":{}}}` + "\n"
	if _, err := SummarizePhase("bad", "inline", strings.NewReader(in)); err == nil {
		t.Fatal("want an error for a non-integer int64 field")
	}
}

func TestBuildReportTotalsAndSlowest(t *testing.T) {
	phases := []Phase{fixture(t, "cold.bep.json"), fixture(t, "remote_cache.bep.json")}
	rep := BuildReport("pr", phases, 3)
	if rep.Schema != ReportSchema || rep.Context != "pr" {
		t.Errorf("schema/context = %d/%q", rep.Schema, rep.Context)
	}
	if rep.Totals.Total != 10 || rep.Totals.Cached != 3 || rep.Totals.Executed != 7 {
		t.Errorf("totals = %+v", rep.Totals)
	}
	if got := rep.Totals.CacheHitRate(); got != 0.3 {
		t.Errorf("hit rate = %v, want 0.3", got)
	}
	want := []string{"cold //:slow_pass_test", "cold //:sharded_test", "cold //:pass_test"}
	var got []string
	for _, s := range rep.Slowest {
		got = append(got, s.Phase+" "+s.Label)
		if !s.Outcome.IsExecuted() {
			t.Errorf("slowest lists a cached target: %+v", s)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("slowest = %v, want %v", got, want)
	}
}

func TestBuildReportNothingRan(t *testing.T) {
	rep := BuildReport("", nil, 10)
	if rep.Totals.CacheHitRate() != 0 || rep.Slowest == nil || len(rep.Slowest) != 0 {
		t.Errorf("empty report = %+v", rep)
	}
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No test executed") {
		t.Errorf("markdown:\n%s", buf.String())
	}
}

func TestRunWritesMarkdownAndJSON(t *testing.T) {
	dir := t.TempDir()
	jsonOut := filepath.Join(dir, "summary.json")
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--context", "main", "--json-out", jsonOut, "--allow-missing", "--top", "2",
		"unit=" + filepath.Join("testdata", "warm_local_cache.bep.json"),
		"acceptance=" + filepath.Join("testdata", "remote_cache.bep.json"),
		"integration=" + filepath.Join(dir, "absent.json"),
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	md := stdout.String()
	for _, want := range []string{
		"## Bazel test cache report (main)",
		"| unit | 5 | 4 (4 / 0 / 0) | 1 (1 / 0) | 0 | 3 | 1 | 1 | 80.0% |",
		"| acceptance | 5 | 3 (0 / 3 / 0) | 2 (1 / 1) | 0 | 3 | 1 | 1 | 60.0% |",
		"| integration | _no BEP file",
		"| **total** | 10 | 7 (4 / 3 / 0) | 3 (2 / 1) |",
		"### Actions",
		"### Slowest executed tests (top 2)",
		"| acceptance | `//:fail_test` | remote | FAILED | 1 | 0.6s |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	if !strings.Contains(stderr.String(), `phase "integration": no BEP file`) {
		t.Errorf("missing phase not reported on stderr: %s", stderr.String())
	}
	data, err := os.ReadFile(jsonOut)
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Schema != 1 || len(rep.Phases) != 3 || !rep.Phases[2].Missing || rep.Totals.Cached != 7 || len(rep.Slowest) != 2 {
		t.Errorf("JSON report = %+v", rep)
	}
}

func TestRunMissingFileWithoutAllowMissingFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"unit=" + filepath.Join(t.TempDir(), "absent.json")}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "absent.json") {
		t.Errorf("exit %d stderr %q, want 1 naming the file", code, stderr.String())
	}
}

func TestRunUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"just-a-path.json"},
		{"=x.json"},
		{"unit="},
		{"unit=a.json", "unit=b.json"},
		{"--top", "-1", "unit=a.json"},
		{"--bogus", "unit=a.json"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 {
			t.Errorf("Run(%q) = %d, want 2 (stderr %q)", args, code, stderr.String())
		}
	}
}

// validation_aspect.bep.json is a real Bazel 9.2.0 run of `bazel test
// --config=ci --keep_going //internal/doltversion/...` (test:ci sets
// --experimental_use_validation_aspect) with a deliberate errcheck finding in
// the package, passed through redact.jq. The test ran beside nogo and its
// testSummary says PASSED, yet Bazel exited BUILD_FAILURE ("1 fails to
// build"): the report must not count that target as passed.
func TestSummarizePhaseValidationAspectFailure(t *testing.T) {
	p := fixture(t, "validation_aspect.bep.json")
	if p.ExitCode != "BUILD_FAILURE" {
		t.Errorf("exit code = %q, want BUILD_FAILURE", p.ExitCode)
	}
	wantFailed := []string{"//internal/doltversion:doltversion", "//internal/doltversion:doltversion_test"}
	if !reflect.DeepEqual(p.ValidationFailed, wantFailed) {
		t.Errorf("validation failed = %v, want %v", p.ValidationFailed, wantFailed)
	}
	if len(p.Tests) != 1 || p.Tests[0].Status != StatusFailedValidation {
		t.Fatalf("tests = %+v, want one %s target", p.Tests, StatusFailedValidation)
	}
	if p.Targets.Passed != 0 || p.Targets.Failed != 1 {
		t.Errorf("passed/failed = %d/%d, want 0/1", p.Targets.Passed, p.Targets.Failed)
	}
	var md bytes.Buffer
	if err := WriteMarkdown(&md, BuildReport("pr/remote", []Phase{p}, 5)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "validation (nogo) failed for 2 target(s)") {
		t.Errorf("markdown does not report the validation failure:\n%s", md.String())
	}
}

func TestSummarizePhaseValidationAspectSynthetic(t *testing.T) {
	lines := []string{
		`{"id":{"targetCompleted":{"label":"//a:ok_test","aspect":"ValidateTarget"}},"completed":{"success":true}}`,
		`{"id":{"testSummary":{"label":"//a:ok_test"}},"testSummary":{"overallStatus":"PASSED"}}`,
		// Aborted: no completed payload; it never validated either.
		`{"id":{"targetCompleted":{"label":"//a:aborted_test","aspect":"ValidateTarget"}}}`,
		`{"id":{"testSummary":{"label":"//a:aborted_test"}},"testSummary":{"overallStatus":"FLAKY"}}`,
		// A failed test stays failed; its status is not rewritten.
		`{"id":{"targetCompleted":{"label":"//a:bad_test","aspect":"ValidateTarget"}},"completed":{}}`,
		`{"id":{"testSummary":{"label":"//a:bad_test"}},"testSummary":{"overallStatus":"FAILED"}}`,
		// A plain (non-aspect) failed completion is not a validation failure.
		`{"id":{"targetCompleted":{"label":"//a:lib"}},"completed":{}}`,
	}
	p, err := SummarizePhase("synthetic", "inline", strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"//a:aborted_test", "//a:bad_test"}; !reflect.DeepEqual(p.ValidationFailed, want) {
		t.Errorf("validation failed = %v, want %v", p.ValidationFailed, want)
	}
	want := map[string]int{"PASSED": 1, StatusFailedValidation: 1, "FAILED": 1}
	if !reflect.DeepEqual(p.Targets.ByStatus, want) {
		t.Errorf("by status = %v, want %v", p.Targets.ByStatus, want)
	}
}
