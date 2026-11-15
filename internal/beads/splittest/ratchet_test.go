package splittest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleBaseline = "# comment\n" +
	"internal/api/residency_by_id.go\tapi.(*Server).resolveBeadOwner\tget\twork\tsessions\tapi.(*Server).humaHandleBeadReopen\t1\t1\t3\t*\tmisroute\tS3: by-id through the work leg\n" +
	"\n" +
	"cmd/gc/sling.go\tgc.attach\tcreate\twork\tgraph\t-\t2\t0\t1\t*\tknown-violation\tS4: rig-store graph write\n" +
	"cmd/gc/fix.go\tgc.fix\tget\twork\tsessions\tgc.fixCaller\t1\t1\t1\tTestFixtureShape\tfixture\tthe test passes the work store\n" +
	"cmd/gc/open.go\tgc.open\tget\twork\tgraph\t-\t1\t0\t1\t*\ttriage\tnot yet ruled on\n"

func resolveKey() SiteKey {
	return SiteKey{File: "internal/api/residency_by_id.go", Func: "api.(*Server).resolveBeadOwner", Op: "get", View: "work", Class: "sessions", Caller: "api.(*Server).humaHandleBeadReopen"}
}

func fixtureKey() SiteKey {
	return SiteKey{File: "cmd/gc/fix.go", Func: "gc.fix", Op: "get", View: "work", Class: "sessions", Caller: "gc.fixCaller"}
}

func TestParseBaseline(t *testing.T) {
	entries, err := ParseBaseline(strings.NewReader(sampleBaseline))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %+v", entries)
	}
	e := entries[0]
	if e.Key != resolveKey() || e.MaxLines != 1 || e.MaxCallerLines != 1 || e.Tests != 3 || !e.anyTest() || e.Verdict != VerdictMisroute || e.Note != "S3: by-id through the work leg" {
		t.Fatalf("entry = %+v", e)
	}
	if f := entries[2]; f.anyTest() || len(f.Scope) != 1 || f.Scope[0] != "TestFixtureShape" {
		t.Fatalf("fixture entry scope = %v, want [TestFixtureShape]", f.Scope)
	}
}

func TestParseBaselineRejectsMalformedEntries(t *testing.T) {
	row := func(fields ...string) string { return strings.Join(fields, "\t") + "\n" }
	for name, text := range map[string]string{
		"fields":         row("a.go", "f", "get", "work", "graph", "-", "1", "0", "1", "*", "misroute"),
		"emptykey":       row("a.go", "", "get", "work", "graph", "-", "1", "0", "1", "*", "misroute", "n"),
		"lines":          row("a.go", "f", "get", "work", "graph", "-", "0", "0", "1", "*", "misroute", "n"),
		"callerlines":    row("a.go", "f", "get", "work", "graph", "-", "1", "x", "1", "*", "misroute", "n"),
		"tests":          row("a.go", "f", "get", "work", "graph", "-", "1", "0", "x", "*", "misroute", "n"),
		"scope":          row("a.go", "f", "get", "work", "graph", "-", "1", "0", "1", "", "misroute", "n"),
		"unboundfixture": row("a.go", "f", "get", "work", "graph", "-", "1", "0", "1", "*", "fixture", "n"),
		"verdict":        row("a.go", "f", "get", "work", "graph", "-", "1", "0", "1", "*", "ok", "n"),
		"note":           row("a.go", "f", "get", "work", "graph", "-", "1", "0", "1", "*", "misroute", " "),
		"duplicate":      row("a.go", "f", "get", "work", "graph", "-", "1", "0", "1", "*", "misroute", "n") + row("a.go", "f", "get", "work", "graph", "-", "1", "0", "1", "TestA", "fixture", "n"),
	} {
		if _, err := ParseBaseline(strings.NewReader(text)); err == nil {
			t.Errorf("%s: ParseBaseline accepted %q", name, text)
		}
	}
}

func TestCompareToBaseline(t *testing.T) {
	baseline, err := ParseBaseline(strings.NewReader(sampleBaseline))
	if err != nil {
		t.Fatal(err)
	}
	known := SiteSummary{Key: resolveKey(), Lines: []int{90}, CallerLines: []int{12}, Tests: 5, TestNames: []string{"TestA"}}
	t.Run("known key passes, line drift is fine", func(t *testing.T) {
		r := CompareToBaseline([]SiteSummary{known}, baseline)
		if r.Failed() || len(r.Grown) != 1 {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("same function, different op is new", func(t *testing.T) {
		other := known
		other.Key.Op = "list"
		if r := CompareToBaseline([]SiteSummary{other}, baseline); len(r.New) != 1 {
			t.Fatalf("New = %+v", r.New)
		}
	})
	t.Run("same site, new caller in the package is new", func(t *testing.T) {
		other := known
		other.Key.Caller = "api.(*Server).humaHandleBeadClose"
		if r := CompareToBaseline([]SiteSummary{other}, baseline); len(r.New) != 1 {
			t.Fatalf("New = %+v", r.New)
		}
	})
	t.Run("a second offending line in a known function fails", func(t *testing.T) {
		two := known
		two.Lines = []int{82, 95}
		r := CompareToBaseline([]SiteSummary{two}, baseline)
		if len(r.LineCap) != 1 || r.LineCap[0].Cap != 1 || r.LineCap[0].Caller {
			t.Fatalf("LineCap = %+v", r.LineCap)
		}
	})
	t.Run("a second calling line in a known caller fails", func(t *testing.T) {
		two := known
		two.CallerLines = []int{12, 40}
		r := CompareToBaseline([]SiteSummary{two}, baseline)
		if len(r.LineCap) != 1 || !r.LineCap[0].Caller || !strings.Contains(r.Report("b"), "LINES caller") {
			t.Fatalf("LineCap = %+v", r.LineCap)
		}
	})
	t.Run("a fixture row reached from another test is new", func(t *testing.T) {
		fix := SiteSummary{Key: fixtureKey(), Lines: []int{3}, CallerLines: []int{9}, Tests: 2, TestNames: []string{"TestFixtureShape", "TestNewCaller"}}
		r := CompareToBaseline([]SiteSummary{fix}, baseline)
		if len(r.OutOfScope) != 1 || r.OutOfScope[0].Tests[0] != "TestNewCaller" || !r.Failed() {
			t.Fatalf("OutOfScope = %+v", r.OutOfScope)
		}
		fix.TestNames = []string{"TestFixtureShape"}
		fix.Tests = 1
		if r := CompareToBaseline([]SiteSummary{fix}, baseline); r.Failed() {
			t.Fatalf("the bound test failed the ratchet: %+v", r)
		}
	})
	t.Run("a triage row fails", func(t *testing.T) {
		tri := SiteSummary{Key: SiteKey{File: "cmd/gc/open.go", Func: "gc.open", Op: "get", View: "work", Class: "graph", Caller: "-"}, Lines: []int{3}, Tests: 1, TestNames: []string{"TestX"}}
		r := CompareToBaseline([]SiteSummary{tri}, baseline)
		if len(r.Triage) != 1 || !r.Failed() || !strings.Contains(r.Report("b"), "TRIAGE") {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("report names the new key", func(t *testing.T) {
		fresh := SiteSummary{Key: SiteKey{File: "internal/api/new.go", Func: "api.fresh", Op: "get", View: "work", Class: "graph", Caller: "-"}, Lines: []int{7}, Tests: 1, SampleTest: "TestX"}
		report := CompareToBaseline([]SiteSummary{fresh}, baseline).Report("testdata/b.tsv")
		for _, want := range []string{"NEW internal/api/new.go:7 api.fresh", "TestX", "beads.FederatedRead"} {
			if !strings.Contains(report, want) {
				t.Errorf("report lacks %q:\n%s", want, report)
			}
		}
	})
}

func TestParseKnownFailuresAndFlaky(t *testing.T) {
	got, err := ParseKnownFailures(strings.NewReader("# known\nTestA\treads a session through the work view\n\nTestB\tfixture shape\n"))
	if err != nil || got["TestA"] == "" || got["TestB"] == "" || len(got) != 2 {
		t.Fatalf("ParseKnownFailures = %v, %v", got, err)
	}
	for _, bad := range []string{"TestA\n", "TestA/sub\tr\n", "notatest\tr\n", "TestA\tr\nTestA\tr\n", "TestA\t \n"} {
		if _, err := ParseKnownFailures(strings.NewReader(bad)); err == nil {
			t.Errorf("ParseKnownFailures accepted %q", bad)
		}
	}
	flaky, err := ParseKnownFlaky(strings.NewReader("TestF\tmc-19h7i\ttiming under remote load\n"))
	if err != nil || !strings.Contains(flaky["TestF"], "mc-19h7i") {
		t.Fatalf("ParseKnownFlaky = %v, %v", flaky, err)
	}
	if _, err := ParseKnownFlaky(strings.NewReader("TestF\ttiming\n")); err == nil {
		t.Error("ParseKnownFlaky accepted an entry without an owner bead")
	}
}

func TestGateRunObservesTopLevelTests(t *testing.T) {
	g := newTestGateRun(t)
	for _, line := range []string{
		"=== RUN   TestA",
		"=== RUN   TestA/sub",
		"=== PAUSE TestB",
		"--- FAIL: TestA (0.10s)",
		"    --- FAIL: TestA/sub (0.00s)",
		"=== RUN   TestB",
		"=== PAUSE TestB",
	} {
		g.observe(line)
	}
	if !g.failed["TestA"] || len(g.failed) != 1 {
		t.Fatalf("failed = %v, want TestA only (subtests are not top-level)", g.failed)
	}
	if len(g.running) != 0 {
		t.Fatalf("running = %v; a paused test's clock must stop", g.running)
	}
	g.observe("=== CONT  TestB")
	if _, ok := g.running["TestB"]; !ok {
		t.Fatal("a continued test is not running")
	}
}

func newTestGateRun(t *testing.T) *gateRun {
	t.Helper()
	return &gateRun{dir: t.TempDir(), done: make(chan struct{}), stop: make(chan struct{}), running: map[string]time.Time{}, ran: map[string]bool{}, failed: map[string]bool{}}
}

func TestGateFinishReportsRatchetAndTestsSeparately(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	t.Setenv(StrictReportEnv, "")
	dir := t.TempDir()
	baselinePath := filepath.Join(dir, "baseline.tsv")
	knownPath := filepath.Join(dir, "known.txt")
	adjudicatedOnly := strings.Replace(sampleBaseline, "cmd/gc/open.go\tgc.open\tget\twork\tgraph\t-\t1\t0\t1\t*\ttriage\tnot yet ruled on\n", "", 1)
	if adjudicatedOnly == sampleBaseline {
		t.Fatal("the triage row was not removed from the sample baseline")
	}
	if err := os.WriteFile(baselinePath, []byte(adjudicatedOnly), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownPath, []byte("TestKnown\tfixture shape\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	flakyPath := filepath.Join(dir, "flaky.list")
	if err := os.WriteFile(flakyPath, []byte("TestFlaky\tmc-19h7i\ttiming under load\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gateSink.reset()
	t.Cleanup(gateSink.reset)
	files := GateFiles{Baseline: baselinePath, KnownFailures: knownPath, KnownFlaky: flakyPath}

	run := newTestGateRun(t)
	close(run.done)
	for _, line := range []string{"=== RUN   TestKnown", "--- FAIL: TestKnown (0.00s)", "=== RUN   TestOK", "--- PASS: TestOK (0.00s)", "=== RUN   TestFlaky", "--- FAIL: TestFlaky (0.00s)"} {
		run.observe(line)
	}
	var out bytes.Buffer
	if code := finishGate(1, false, run, files, &out); code != 0 {
		t.Fatalf("a run whose only failure is known exited %d:\n%s", code, out.String())
	}
	for _, want := range []string{"SPLIT-STRICT RATCHET: PASS", "SPLIT-STRICT TESTS: PASS", "KNOWN FLAKY TestFlaky failed (mc-19h7i: timing under load)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}

	run = newTestGateRun(t)
	close(run.done)
	run.observe("=== RUN   TestNew")
	run.observe("--- FAIL: TestNew (0.00s)")
	gateSink.record(SplitViolation{View: "work", Op: "get", Class: "graph", ID: "gcg-1", Site: Site{File: "internal/api/new.go", Line: 3, Func: "api.fresh"}, Test: "TestNew"})
	out.Reset()
	if code := finishGate(0, false, run, files, &out); code != 1 {
		t.Fatalf("a run with a new key and a new failing test exited %d", code)
	}
	for _, want := range []string{"SPLIT-STRICT RATCHET: FAIL", "SPLIT-STRICT TESTS: FAIL", "NEW FAILING TestNew"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), logTableBegin+observedReportName+"\n# ") || !strings.Contains(out.String(), "internal/api/new.go\tapi.fresh") {
		t.Fatalf("the observed table is not in the log:\n%s", out.String())
	}
	result, err := os.ReadFile(filepath.Join(run.dir, resultReportName))
	if err != nil || !strings.Contains(string(result), "ratchet=FAIL") || !strings.Contains(string(result), "tests=FAIL") {
		t.Fatalf("result file = %q, %v", result, err)
	}
	table, err := os.ReadFile(filepath.Join(run.dir, observedReportName))
	if err != nil || !strings.Contains(string(table), "internal/api/new.go\tapi.fresh\tget\twork\tgraph\t-\t3\t\t1\t1\tTestNew\t") {
		t.Fatalf("observed table = %q, %v", table, err)
	}

	// A fixture row reached from a test it is not bound to fails the
	// ratchet, and the summary line counts it.
	gateSink.reset()
	run = newTestGateRun(t)
	close(run.done)
	gateSink.record(SplitViolation{View: "work", Op: "get", Class: "sessions", ID: "gcs-1", Site: Site{File: "cmd/gc/fix.go", Line: 3, Func: "gc.fix"}, Pkg: Site{File: "cmd/gc/caller.go", Line: 9, Func: "gc.fixCaller"}, Test: "TestElsewhere"})
	out.Reset()
	if code := finishGate(0, false, run, files, &out); code != 1 {
		t.Fatalf("a run reaching a fixture row out of its scope exited %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "SPLIT-STRICT RATCHET: FAIL (1 key(s) observed, 0 new, 0 triage, 0 past their line cap, 1 out of scope)") {
		t.Fatalf("the summary does not count the out-of-scope fixture row:\n%s", out.String())
	}
}

// TestGateFinishFailsAnUntriagedKnownFailure: regeneration lists a test that
// newly fails under the gate with an UNTRIAGED reason (its first logged
// message), which is not a ruling. Its failure fails the TESTS verdict until
// someone replaces the reason with why the test fails; a ruled entry passes.
func TestGateFinishFailsAnUntriagedKnownFailure(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	t.Setenv(StrictReportEnv, "")
	if !Untriaged("UNTRIAGED: want 1, got 0") || Untriaged("fails under the gate: want 1, got 0") {
		t.Fatal("Untriaged does not tell a placeholder from a ruling")
	}
	dir := t.TempDir()
	files := GateFiles{Baseline: filepath.Join(dir, "baseline.tsv"), KnownFailures: filepath.Join(dir, "known.list")}
	if err := os.WriteFile(files.Baseline, []byte("# empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.KnownFailures, []byte("TestRuled\tfixture shape\nTestNew\tUNTRIAGED: want 1, got 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gateSink.reset()
	t.Cleanup(gateSink.reset)
	finish := func(failing string) (int, string) {
		run := newTestGateRun(t)
		close(run.done)
		run.observe("=== RUN   " + failing)
		run.observe("--- FAIL: " + failing + " (0.00s)")
		var out bytes.Buffer
		code := finishGate(1, false, run, files, &out)
		return code, out.String()
	}
	if code, out := finish("TestRuled"); code != 0 || !strings.Contains(out, "SPLIT-STRICT TESTS: PASS") {
		t.Fatalf("a ruled known failure = exit %d:\n%s", code, out)
	}
	code, out := finish("TestNew")
	if code == 0 || !strings.Contains(out, "SPLIT-STRICT TESTS: FAIL") || !strings.Contains(out, "1 listed UNTRIAGED") || !strings.Contains(out, "UNTRIAGED TestNew") {
		t.Fatalf("an untriaged known failure = exit %d; want the TESTS verdict to fail and name it:\n%s", code, out)
	}
}

// TestGateFinishRunsOncePerRun: the watchdog's hang path and GateMain both
// finish the run when a hung test ends while the watchdog dumps stacks. The
// later call must not finish it again (close its stop channel twice, which
// panics, or print a second report); it gets the first call's code.
func TestGateFinishRunsOncePerRun(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	t.Setenv(StrictReportEnv, "")
	dir := t.TempDir()
	files := GateFiles{Baseline: filepath.Join(dir, "baseline.tsv"), KnownFailures: filepath.Join(dir, "known.list")}
	for _, p := range []string{files.Baseline, files.KnownFailures} {
		if err := os.WriteFile(p, []byte("# empty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gateSink.reset()
	t.Cleanup(gateSink.reset)
	run := newTestGateRun(t)
	close(run.done)
	run.observe("=== RUN   TestNew")
	run.observe("--- FAIL: TestNew (0.00s)")
	var first, second bytes.Buffer
	if code := finishGate(1, false, run, files, &first); code != 1 {
		t.Fatalf("first finish = %d, want 1 (a new failing test):\n%s", code, first.String())
	}
	if code := finishGate(0, false, run, files, &second); code != 1 || second.Len() != 0 {
		t.Fatalf("second finish = %d with output %q; want the first finish's 1 and no second report", code, second.String())
	}
}

func TestGateFinishFoldsInHelperProcessEvents(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	t.Setenv(StrictReportEnv, "")
	dir := t.TempDir()
	baselinePath := filepath.Join(dir, "baseline.tsv")
	knownPath := filepath.Join(dir, "known.list")
	_ = os.WriteFile(baselinePath, nil, 0o644)
	_ = os.WriteFile(knownPath, nil, 0o644)
	gateSink.reset()
	t.Cleanup(gateSink.reset)
	run := newTestGateRun(t)
	close(run.done)
	helper := sighting{kind: kindViolation, key: SiteKey{File: "cmd/gc/x.go", Func: "gc.x", Op: "get", View: "work", Class: "graph", Caller: "-"}, line: 4, test: "TestHelper"}
	if err := os.WriteFile(filepath.Join(run.dir, "split-strict-events.999.tsv"), []byte(formatSighting(helper)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := finishGate(0, false, run, GateFiles{Baseline: baselinePath, KnownFailures: knownPath}, &out); code != 1 || !strings.Contains(out.String(), "NEW cmd/gc/x.go:4 gc.x") {
		t.Fatalf("a helper process's key was not ratcheted (code %d):\n%s", code, out.String())
	}
}

func TestGateMainIsANoOpWhenTheGateIsOff(t *testing.T) {
	t.Setenv(StrictEnv, "")
	var out bytes.Buffer
	if code := GateMain(7, 7, GateFiles{Baseline: "/nonexistent"}, &out); code != 7 || out.Len() != 0 {
		t.Fatalf("GateMain off = %d, %q", code, out.String())
	}
	// Off the gate the checks ran over the tests' own code, so their answer
	// is the exit code, as without the gate.
	if code := GateMain(0, 1, GateFiles{Baseline: "/nonexistent"}, &out); code != 1 || out.Len() != 0 {
		t.Fatalf("GateMain off with a failed check = %d, %q; want 1", code, out.String())
	}
	if got := GateChecksBase(3); got != 3 {
		t.Fatalf("GateChecksBase off = %d, want the tests' code 3", got)
	}
}

// TestGateMainFailsTestsOnAFailedCheckBesideKnownFailures: a check TestMain
// runs around the tests (a leak guard, the no-city or session-key guard)
// raises a 0 to 1 but leaves a non-zero code alone, so over a known failure's
// 1 its own failure would not show. Under the gate the checks run over
// GateChecksBase's 0, and GateMain takes their code apart from the tests':
// a failed check fails the TESTS verdict and the exit code even when every
// failing test is a known one.
func TestGateMainFailsTestsOnAFailedCheckBesideKnownFailures(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	t.Setenv(StrictReportEnv, "")
	if got := GateChecksBase(1); got != 0 {
		t.Fatalf("GateChecksBase under the gate = %d, want 0 so each check answers for itself", got)
	}
	dir := t.TempDir()
	files := GateFiles{Baseline: filepath.Join(dir, "baseline.tsv"), KnownFailures: filepath.Join(dir, "known.list")}
	if err := os.WriteFile(files.Baseline, []byte("# empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.KnownFailures, []byte("TestKnown\tfixture shape\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gateSink.reset()
	t.Cleanup(gateSink.reset)
	gateRunMu.Lock()
	saved := activeRun
	gateRunMu.Unlock()
	t.Cleanup(func() {
		gateRunMu.Lock()
		activeRun = saved
		gateRunMu.Unlock()
	})
	finish := func(checks int) (int, string) {
		run := newTestGateRun(t)
		close(run.done)
		run.observe("=== RUN   TestKnown")
		run.observe("--- FAIL: TestKnown (0.00s)")
		gateRunMu.Lock()
		activeRun = run
		gateRunMu.Unlock()
		var out bytes.Buffer
		code := GateMain(1, checks, files, &out)
		return code, out.String()
	}
	if code, out := finish(0); code != 0 || !strings.Contains(out, "SPLIT-STRICT TESTS: PASS") {
		t.Fatalf("a known failure with every check passing = exit %d:\n%s", code, out)
	}
	code, out := finish(1)
	if code == 0 {
		t.Fatalf("a known failure beside a failed check exited 0:\n%s", out)
	}
	if !strings.Contains(out, "SPLIT-STRICT TESTS: FAIL") || !strings.Contains(out, "a TestMain check failed") {
		t.Fatalf("a failed check did not fail the tests verdict:\n%s", out)
	}
}

func TestFormatSightingRoundTrips(t *testing.T) {
	sg := sighting{kind: kindRelic, key: resolveKey(), line: 82, callerLine: 7, test: "TestA", via: "a.go:1 f"}
	got, ok := parseSighting(formatSighting(sg))
	if !ok || got != sg {
		t.Fatalf("round trip = %+v, %v; want %+v", got, ok, sg)
	}
}

// TestOffGoroutineSightingsAreChargedToTheSoleRunningTest: a sighting with no
// test frame (a controller loop, a handler goroutine) is charged to the one
// top-level test running, so fixture scopes bind it; with several running it
// stays "?".
func TestOffGoroutineSightingsAreChargedToTheSoleRunningTest(t *testing.T) {
	g := newTestGateRun(t)
	gateRunMu.Lock()
	saved := activeRun
	activeRun = g
	gateRunMu.Unlock()
	t.Cleanup(func() {
		gateRunMu.Lock()
		activeRun = saved
		gateRunMu.Unlock()
	})
	site := Site{File: "internal/api/loop.go", Line: 7, Func: "api.loop"}
	testOf := func() string {
		sink := newViolationSink()
		sink.record(SplitViolation{View: "work", Op: "get", Class: "graph", ID: "gcg-1", Site: site})
		if len(sink.order) != 1 {
			t.Fatalf("sightings = %d, want 1", len(sink.order))
		}
		return sink.order[0].test
	}
	g.observe("=== RUN   TestOnly")
	if got := testOf(); got != "TestOnly" {
		t.Fatalf("off-goroutine sighting charged to %q, want the sole running test", got)
	}
	g.observe("=== CONT  TestOther")
	if got := testOf(); got != "?" {
		t.Fatalf("with two tests running the sighting was charged to %q, want ?", got)
	}
}

// TestGateFinishNeverPassesWhatItCouldNotJudge: an events file it cannot
// read fails the ratchet verdict, and a non-zero exit with no failing test
// (a panic or TestMain error outside any test) fails the tests verdict.
func TestGateFinishNeverPassesWhatItCouldNotJudge(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	t.Setenv(StrictReportEnv, "")
	dir := t.TempDir()
	baselinePath := filepath.Join(dir, "baseline.tsv")
	knownPath := filepath.Join(dir, "known.list")
	flakyPath := filepath.Join(dir, "flaky.list")
	for _, p := range []string{baselinePath, knownPath, flakyPath} {
		if err := os.WriteFile(p, []byte("# empty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := GateFiles{Baseline: baselinePath, KnownFailures: knownPath, KnownFlaky: flakyPath}
	gateSink.reset()
	t.Cleanup(gateSink.reset)

	run := newTestGateRun(t)
	close(run.done)
	// A directory where an events file should be: opening works, reading
	// does not (whoever runs the test).
	if err := os.Mkdir(filepath.Join(run.dir, strings.Replace(eventsFilePattern, "*", "1", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := finishGate(0, false, run, files, &out); code == 0 {
		t.Fatalf("an unreadable events file exited 0:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "SPLIT-STRICT RATCHET: FAIL") {
		t.Fatalf("an unreadable events file did not fail the ratchet verdict:\n%s", out.String())
	}

	run = newTestGateRun(t)
	close(run.done)
	run.observe("=== RUN   TestOK")
	run.observe("--- PASS: TestOK (0.00s)")
	out.Reset()
	if code := finishGate(2, false, run, files, &out); code == 0 {
		t.Fatalf("exit 2 with no failing test exited 0:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "SPLIT-STRICT TESTS: FAIL") || !strings.Contains(out.String(), "exit 2 with no failing test") {
		t.Fatalf("exit 2 with no failing test did not fail the tests verdict:\n%s", out.String())
	}
}
