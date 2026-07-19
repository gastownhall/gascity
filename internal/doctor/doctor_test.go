package doctor

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockCheck is a configurable Check for testing the runner.
type mockCheck struct {
	name     string
	status   CheckStatus
	severity CheckSeverity
	msg      string
	canFix   bool
	fixErr   error
	fixed    bool          // set by Fix
	block    chan struct{} // if non-nil, Run blocks on it until closed (models a wedge abandoned on timeout)
	fixCalls int           // incremented by Fix
}

func (m *mockCheck) Name() string { return m.name }
func (m *mockCheck) Run(_ *CheckContext) *CheckResult {
	if m.block != nil {
		<-m.block
	}
	st := m.status
	if m.fixed {
		st = StatusOK
	}
	return &CheckResult{
		Name:     m.name,
		Status:   st,
		Severity: m.severity,
		Message:  m.msg,
	}
}
func (m *mockCheck) CanFix() bool { return m.canFix }
func (m *mockCheck) Fix(_ *CheckContext) error {
	m.fixCalls++
	if m.fixErr != nil {
		return m.fixErr
	}
	m.fixed = true
	return nil
}

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (m *mockCheck) WarmupEligible() bool { return false }

func TestCheckWarmupEligibleDefaultsFalse(t *testing.T) {
	checks := []Check{
		&AgentSessionsCheck{},
		&BDSplitStoreCheck{},
		&BdBackupSizeCheck{},
		&BeadsRoleCheck{},
		&BeadsStoreCheck{},
		&BinaryCheck{},
		&BuiltinPackFamilyCheck{},
		&CityConfigCheck{},
		&CityStructureCheck{},
		&ConfigRefsCheck{},
		&ConfigSemanticsCheck{},
		&ConfigValidCheck{},
		&ControllerCheck{},
		&CustomTypesCheck{},
		&DeprecatedAttachmentFieldsCheck{},
		&DoltConfigCheck{},
		&DoltNomsSizeCheck{},
		&DoltServerCheck{},
		&DoltVersionCheck{},
		&DurationRangeCheck{},
		&EventLogSizeCheck{},
		&EventsLogCheck{},
		&ImplicitImportCacheCheck{},
		&NestedWorktreePruneCheck{},
		&OrphanSessionsCheck{},
		&OrderFiringCurrentCheck{},
		&PackCacheCheck{},
		&PreStartScriptsCheck{},
		&ProviderParityCheck{},
		&RigBeadsCheck{},
		&RigDoltServerCheck{},
		&RigGitCheck{},
		&RigPathCheck{},
		&SkillCollisionCheck{},
		&WorktreeCheck{},
		&WorktreeDiskSizeCheck{},
		&ZombieSessionsCheck{},
	}

	for _, c := range checks {
		t.Run(fmt.Sprintf("%T", c), func(t *testing.T) {
			if c.WarmupEligible() {
				t.Errorf("%T.WarmupEligible() = true, want false", c)
			}
		})
	}

	t.Run("pack_script_check_default_false", func(t *testing.T) {
		c := &PackScriptCheck{CheckName: "x:y"}
		if c.WarmupEligible() {
			t.Error("zero-value PackScriptCheck.WarmupEligible() = true, want false")
		}
	})

	t.Run("pack_script_check_opted_in", func(t *testing.T) {
		c := &PackScriptCheck{CheckName: "x:y", Warmup: true}
		if !c.WarmupEligible() {
			t.Error("PackScriptCheck{Warmup: true}.WarmupEligible() = false, want true")
		}
	})
}

func TestDoctor_AllPass(t *testing.T) {
	d := &Doctor{}
	d.Register(&mockCheck{name: "a", status: StatusOK, msg: "ok"})
	d.Register(&mockCheck{name: "b", status: StatusOK, msg: "ok"})

	var buf bytes.Buffer
	r := d.Run(&CheckContext{CityPath: "/tmp"}, &buf, false)

	if r.Passed != 2 {
		t.Errorf("Passed = %d, want 2", r.Passed)
	}
	if r.Warned != 0 || r.Failed != 0 || r.Fixed != 0 {
		t.Errorf("unexpected counts: warned=%d failed=%d fixed=%d", r.Warned, r.Failed, r.Fixed)
	}
	if !strings.Contains(buf.String(), "✓ a") {
		t.Errorf("output missing check a: %q", buf.String())
	}
}

func TestDoctor_MixedResults(t *testing.T) {
	d := &Doctor{}
	d.Register(&mockCheck{name: "ok-check", status: StatusOK, msg: "fine"})
	d.Register(&mockCheck{name: "warn-check", status: StatusWarning, msg: "hmm"})
	d.Register(&mockCheck{name: "fail-check", status: StatusError, msg: "bad"})

	var buf bytes.Buffer
	r := d.Run(&CheckContext{CityPath: "/tmp"}, &buf, false)

	if r.Passed != 1 {
		t.Errorf("Passed = %d, want 1", r.Passed)
	}
	if r.Warned != 1 {
		t.Errorf("Warned = %d, want 1", r.Warned)
	}
	if r.Failed != 1 {
		t.Errorf("Failed = %d, want 1", r.Failed)
	}

	out := buf.String()
	if !strings.Contains(out, "✓ ok-check") {
		t.Errorf("missing ok icon: %q", out)
	}
	if !strings.Contains(out, "⚠ warn-check") {
		t.Errorf("missing warning icon: %q", out)
	}
	if !strings.Contains(out, "✗ fail-check") {
		t.Errorf("missing error icon: %q", out)
	}
}

func TestDoctor_FixFlow(t *testing.T) {
	d := &Doctor{}
	d.Register(&mockCheck{name: "fixable", status: StatusWarning, msg: "problem", canFix: true})

	var buf bytes.Buffer
	r := d.Run(&CheckContext{CityPath: "/tmp"}, &buf, true)

	if r.Fixed != 1 {
		t.Errorf("Fixed = %d, want 1", r.Fixed)
	}
	if r.Passed != 1 {
		t.Errorf("Passed = %d, want 1 (fixed counts as passed)", r.Passed)
	}
	if !strings.Contains(buf.String(), "(fixed)") {
		t.Errorf("output missing (fixed): %q", buf.String())
	}
}

func TestDoctor_FixNotRequested(t *testing.T) {
	d := &Doctor{}
	d.Register(&mockCheck{name: "fixable", status: StatusWarning, msg: "problem", canFix: true})

	var buf bytes.Buffer
	r := d.Run(&CheckContext{CityPath: "/tmp"}, &buf, false)

	if r.Fixed != 0 {
		t.Errorf("Fixed = %d, want 0 (fix not requested)", r.Fixed)
	}
	if r.Warned != 1 {
		t.Errorf("Warned = %d, want 1", r.Warned)
	}
}

func TestDoctor_FixFails(t *testing.T) {
	d := &Doctor{}
	d.Register(&mockCheck{
		name: "broken-fix", status: StatusError, msg: "bad",
		canFix: true, fixErr: fmt.Errorf("fix failed"),
	})

	var buf bytes.Buffer
	r := d.Run(&CheckContext{CityPath: "/tmp"}, &buf, true)

	if r.Fixed != 0 {
		t.Errorf("Fixed = %d, want 0 (fix errored)", r.Fixed)
	}
	if r.Failed != 1 {
		t.Errorf("Failed = %d, want 1", r.Failed)
	}
	if !strings.Contains(buf.String(), "fix failed: fix failed") {
		t.Errorf("output missing fix error: %q", buf.String())
	}
}

func TestDoctor_FixSucceedsButCheckStillFails(t *testing.T) {
	d := &Doctor{}
	d.Register(&unchangedFixCheck{})

	var buf bytes.Buffer
	r := d.Run(&CheckContext{CityPath: "/tmp"}, &buf, true)

	if r.Fixed != 0 {
		t.Errorf("Fixed = %d, want 0", r.Fixed)
	}
	if r.Failed != 1 {
		t.Errorf("Failed = %d, want 1", r.Failed)
	}
	if !strings.Contains(buf.String(), "fix attempted; check still failing") {
		t.Errorf("output missing fix-attempt signal: %q", buf.String())
	}
}

func TestDoctor_NoChecks(t *testing.T) {
	d := &Doctor{}
	var buf bytes.Buffer
	r := d.Run(&CheckContext{CityPath: "/tmp"}, &buf, false)

	if r.Passed != 0 || r.Warned != 0 || r.Failed != 0 || r.Fixed != 0 {
		t.Errorf("empty doctor should have all zeros: %+v", r)
	}
}

func TestDoctor_VerboseDetails(t *testing.T) {
	d := &Doctor{}
	c := &mockCheck{name: "detail-check", status: StatusOK, msg: "ok"}
	d.Register(c)

	// We need a check that returns details — override with a custom one.
	d2 := &Doctor{}
	d2.Register(&detailCheck{})

	var buf bytes.Buffer
	d2.Run(&CheckContext{CityPath: "/tmp", Verbose: true}, &buf, false)

	if !strings.Contains(buf.String(), "extra info") {
		t.Errorf("verbose output missing details: %q", buf.String())
	}
}

func TestDoctor_VerboseHidden(t *testing.T) {
	d := &Doctor{}
	d.Register(&detailCheck{})

	var buf bytes.Buffer
	d.Run(&CheckContext{CityPath: "/tmp", Verbose: false}, &buf, false)

	if strings.Contains(buf.String(), "extra info") {
		t.Errorf("non-verbose output should hide details: %q", buf.String())
	}
}

func TestPrintSummary(t *testing.T) {
	tests := []struct {
		name   string
		report *Report
		want   string
	}{
		{"all pass", &Report{Passed: 3}, "3 passed"},
		{"mixed", &Report{Passed: 2, Warned: 1, Failed: 1}, "2 passed, 1 warnings, 1 failed"},
		{"with fixes", &Report{Passed: 2, Fixed: 1}, "2 passed, 1 fixed"},
		{"empty", &Report{}, "No checks ran."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			PrintSummary(&buf, tt.report)
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("summary = %q, want to contain %q", buf.String(), tt.want)
			}
		})
	}
}

func TestDoctor_ReportResultsInOrder(t *testing.T) {
	d := &Doctor{}
	d.Register(&mockCheck{name: "first", status: StatusOK, msg: "fine"})
	d.Register(&mockCheck{name: "second", status: StatusWarning, msg: "hmm"})
	d.Register(&mockCheck{name: "third", status: StatusError, msg: "bad"})

	var buf bytes.Buffer
	r := d.Run(&CheckContext{CityPath: "/tmp"}, &buf, false)

	if len(r.Results) != 3 {
		t.Fatalf("Results length = %d, want 3", len(r.Results))
	}
	names := []string{r.Results[0].Name, r.Results[1].Name, r.Results[2].Name}
	want := []string{"first", "second", "third"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("Results[%d].Name = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestDoctor_RunCollectSuppressesStreaming(t *testing.T) {
	d := &Doctor{}
	d.Register(&mockCheck{name: "silent", status: StatusError, msg: "bad"})

	r := d.RunCollect(&CheckContext{CityPath: "/tmp"}, false)

	if len(r.Results) != 1 || r.Results[0].Name != "silent" {
		t.Fatalf("Results = %#v, want one result named 'silent'", r.Results)
	}
	if r.Failed != 1 {
		t.Errorf("Failed = %d, want 1", r.Failed)
	}
}

func TestDoctor_FixHint(t *testing.T) {
	d := &Doctor{}
	d.Register(&hintCheck{})

	var buf bytes.Buffer
	d.Run(&CheckContext{CityPath: "/tmp"}, &buf, false)

	if !strings.Contains(buf.String(), "hint: try this") {
		t.Errorf("output missing fix hint: %q", buf.String())
	}
}

// detailCheck returns a result with Details for verbose testing.
type detailCheck struct{}

func (c *detailCheck) Name() string { return "detail-check" }
func (c *detailCheck) Run(_ *CheckContext) *CheckResult {
	return &CheckResult{
		Name:    "detail-check",
		Status:  StatusOK,
		Message: "ok",
		Details: []string{"extra info"},
	}
}
func (c *detailCheck) CanFix() bool              { return false }
func (c *detailCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *detailCheck) WarmupEligible() bool { return false }

// hintCheck returns a failing result with a FixHint.
type hintCheck struct{}

func (c *hintCheck) Name() string { return "hint-check" }
func (c *hintCheck) Run(_ *CheckContext) *CheckResult {
	return &CheckResult{
		Name:    "hint-check",
		Status:  StatusError,
		Message: "problem",
		FixHint: "try this",
	}
}
func (c *hintCheck) CanFix() bool              { return false }
func (c *hintCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *hintCheck) WarmupEligible() bool { return false }

type unchangedFixCheck struct{}

func (c *unchangedFixCheck) Name() string { return "unchanged-fix" }
func (c *unchangedFixCheck) Run(_ *CheckContext) *CheckResult {
	return &CheckResult{
		Name:    "unchanged-fix",
		Status:  StatusError,
		Message: "still bad",
	}
}
func (c *unchangedFixCheck) CanFix() bool              { return true }
func (c *unchangedFixCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *unchangedFixCheck) WarmupEligible() bool { return false }

// TestDoctor_BlockingFailedSeverityAccounting exercises the per-severity
// counters added so dispatch gates can ignore advisory failures.
func TestDoctor_BlockingFailedSeverityAccounting(t *testing.T) {
	tests := []struct {
		name              string
		checks            []Check
		wantPassed        int
		wantFailed        int
		wantBlockingFails int
	}{
		{
			name:              "pure-ok",
			checks:            []Check{&mockCheck{name: "a", status: StatusOK, msg: "ok"}},
			wantPassed:        1,
			wantFailed:        0,
			wantBlockingFails: 0,
		},
		{
			name:              "blocking-error",
			checks:            []Check{&mockCheck{name: "blocker", status: StatusError, severity: SeverityBlocking, msg: "blocked"}},
			wantPassed:        0,
			wantFailed:        1,
			wantBlockingFails: 1,
		},
		{
			name:              "advisory-error",
			checks:            []Check{&mockCheck{name: "advisor", status: StatusError, severity: SeverityAdvisory, msg: "info"}},
			wantPassed:        0,
			wantFailed:        1,
			wantBlockingFails: 0,
		},
		{
			name: "mixed-blocking-advisory",
			checks: []Check{
				&mockCheck{name: "ok", status: StatusOK, msg: "fine"},
				&mockCheck{name: "blocker", status: StatusError, severity: SeverityBlocking, msg: "blocked"},
				&mockCheck{name: "advisor", status: StatusError, severity: SeverityAdvisory, msg: "info"},
			},
			wantPassed:        1,
			wantFailed:        2,
			wantBlockingFails: 1,
		},
		{
			name: "default-severity-is-blocking",
			checks: []Check{
				// Severity field omitted; zero value must count as Blocking
				// so existing checks remain gate-relevant.
				&mockCheck{name: "legacy", status: StatusError, msg: "bad"},
			},
			wantPassed:        0,
			wantFailed:        1,
			wantBlockingFails: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Doctor{}
			for _, c := range tt.checks {
				d.Register(c)
			}
			var buf bytes.Buffer
			r := d.Run(&CheckContext{CityPath: "/tmp"}, &buf, false)

			if r.Passed != tt.wantPassed {
				t.Errorf("Passed = %d, want %d", r.Passed, tt.wantPassed)
			}
			if r.Failed != tt.wantFailed {
				t.Errorf("Failed = %d, want %d", r.Failed, tt.wantFailed)
			}
			if r.BlockingFailed != tt.wantBlockingFails {
				t.Errorf("BlockingFailed = %d, want %d", r.BlockingFailed, tt.wantBlockingFails)
			}
		})
	}
}

// TestDoctor_AdvisoryPerCheckLine verifies the per-check output line includes
// "(advisory)" when the result has SeverityAdvisory and StatusWarning/Error,
// and that the suffix is absent for OK results and for blocking failures.
func TestDoctor_AdvisoryPerCheckLine(t *testing.T) {
	tests := []struct {
		name       string
		check      *mockCheck
		wantLabel  string
		wantAbsent string
	}{
		{
			name:      "advisory-warning-has-suffix",
			check:     &mockCheck{name: "check-a", status: StatusWarning, severity: SeverityAdvisory, msg: "heads up"},
			wantLabel: "(advisory)",
		},
		{
			name:      "advisory-error-has-suffix",
			check:     &mockCheck{name: "check-b", status: StatusError, severity: SeverityAdvisory, msg: "note"},
			wantLabel: "(advisory)",
		},
		{
			name:       "advisory-ok-no-suffix",
			check:      &mockCheck{name: "check-c", status: StatusOK, severity: SeverityAdvisory, msg: "fine"},
			wantAbsent: "(advisory)",
		},
		{
			name:       "blocking-warning-no-suffix",
			check:      &mockCheck{name: "check-d", status: StatusWarning, severity: SeverityBlocking, msg: "bad"},
			wantAbsent: "(advisory)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Doctor{}
			d.Register(tt.check)
			var buf bytes.Buffer
			d.Run(&CheckContext{CityPath: "/tmp"}, &buf, false)
			out := buf.String()
			if tt.wantLabel != "" && !strings.Contains(out, tt.wantLabel) {
				t.Errorf("output = %q, want %q in line", out, tt.wantLabel)
			}
			if tt.wantAbsent != "" && strings.Contains(out, tt.wantAbsent) {
				t.Errorf("output = %q, must not contain %q", out, tt.wantAbsent)
			}
		})
	}
}

// TestPrintSummary_AdvisoryRenderedSeparately confirms advisory failures get
// their own component in the summary line so operators can tell at a glance
// that a doctor pass had non-blocking findings.
func TestPrintSummary_AdvisoryRenderedSeparately(t *testing.T) {
	var buf bytes.Buffer
	PrintSummary(&buf, &Report{Passed: 1, Failed: 2, BlockingFailed: 1})
	out := buf.String()
	if !strings.Contains(out, "2 failed") {
		t.Errorf("summary = %q, want '2 failed'", out)
	}
	if !strings.Contains(out, "1 advisory") {
		t.Errorf("summary = %q, want '1 advisory'", out)
	}

	buf.Reset()
	PrintSummary(&buf, &Report{Passed: 3, Failed: 1, BlockingFailed: 1})
	if got := buf.String(); strings.Contains(got, "advisory") {
		t.Errorf("summary = %q, must not include 'advisory' when all failures are blocking", got)
	}
}

// TestRunCheckTimeoutAbandonsWedgedCheck guards the per-check timeout: one
// wedged check must not stall the run — it reports as a timed-out advisory
// error and every check registered after it still executes.
func TestRunCheckTimeoutAbandonsWedgedCheck(t *testing.T) {
	d := &Doctor{CheckTimeout: 25 * time.Millisecond}
	wedged := &mockCheck{name: "wedged", status: StatusOK, block: make(chan struct{})}
	t.Cleanup(func() { close(wedged.block) }) // abandon the wedge once the run has returned
	after := &mockCheck{name: "after", status: StatusOK, msg: "ran"}
	d.Register(wedged)
	d.Register(after)

	var buf bytes.Buffer
	start := time.Now()
	report := d.Run(&CheckContext{}, &buf, false)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("run took %s; the wedged check was not abandoned", elapsed)
	}

	if len(report.Results) != 2 {
		t.Fatalf("Results = %d, want 2 (check after the wedge must still run)", len(report.Results))
	}
	got := report.Results[0]
	if !got.TimedOut || got.Status != StatusError || got.Severity != SeverityAdvisory {
		t.Fatalf("wedged result = %+v, want TimedOut StatusError SeverityAdvisory", got)
	}
	if !strings.Contains(got.Message, "timed out") {
		t.Fatalf("wedged message = %q, want a timeout explanation", got.Message)
	}
	if report.Results[1].Name != "after" || report.Results[1].Status != StatusOK {
		t.Fatalf("after result = %+v, want it to have run normally", report.Results[1])
	}
	if report.Failed != 1 || report.BlockingFailed != 0 || report.Passed != 1 {
		t.Fatalf("report = %+v, want 1 advisory failure + 1 pass", report)
	}
	if out := buf.String(); !strings.Contains(out, "wedged") || !strings.Contains(out, "after") {
		t.Fatalf("output = %q, want both check lines", out)
	}
}

// TestRunCheckTimeoutZeroIsUnbounded pins the default: CheckTimeout zero runs
// checks inline with no bound, preserving historical behavior for embedders
// that never set the field.
func TestRunCheckTimeoutZeroIsUnbounded(t *testing.T) {
	d := &Doctor{}
	check := &mockCheck{name: "unbounded", status: StatusOK}
	d.Register(check)

	var buf bytes.Buffer
	report := d.Run(&CheckContext{}, &buf, false)
	if report.Passed != 1 || len(report.Results) != 1 || report.Results[0].TimedOut {
		t.Fatalf("report = %+v, want the check to run inline and complete unbounded (never TimedOut)", report)
	}
}

// TestRunCheckTimeoutSkipsFixForTimedOutCheck: a timed-out check's failure
// state is unknown, so --fix must not attempt remediation (whose verifying
// re-run could wedge the loop the same way the check did).
func TestRunCheckTimeoutSkipsFixForTimedOutCheck(t *testing.T) {
	d := &Doctor{CheckTimeout: 25 * time.Millisecond}
	wedged := &mockCheck{name: "wedged", status: StatusError, block: make(chan struct{}), canFix: true}
	t.Cleanup(func() { close(wedged.block) }) // abandon the wedge once the run has returned
	d.Register(wedged)

	var buf bytes.Buffer
	report := d.Run(&CheckContext{}, &buf, true)
	if wedged.fixCalls != 0 {
		t.Fatalf("fixCalls = %d, want 0 (no fix for a timed-out check)", wedged.fixCalls)
	}
	if report.Fixed != 0 {
		t.Fatalf("report.Fixed = %d, want 0", report.Fixed)
	}
}

// TestRunCheckTimeoutFlushesCompletedCheckOutput: a check that completes
// within the bound and wrote to ctx.Output must have that output reach the
// real writer (the private abandonment buffer is an implementation detail).
func TestRunCheckTimeoutFlushesCompletedCheckOutput(t *testing.T) {
	d := &Doctor{CheckTimeout: time.Second}
	d.Register(&outputWritingCheck{mockCheck{name: "writer", status: StatusOK}})

	var buf bytes.Buffer
	d.Run(&CheckContext{}, &buf, false)
	if out := buf.String(); !strings.Contains(out, "incidental diagnostic") {
		t.Fatalf("output = %q, want the check's incidental ctx.Output write flushed", out)
	}
}

// outputWritingCheck writes to ctx.Output during Run, like checks that
// surface fix-time diagnostics.
type outputWritingCheck struct{ mockCheck }

func (o *outputWritingCheck) Run(ctx *CheckContext) *CheckResult {
	if ctx.Output != nil {
		fmt.Fprintln(ctx.Output, "incidental diagnostic") //nolint:errcheck // test writer
	}
	return o.mockCheck.Run(ctx)
}

// TestRunCheckTimeoutBoundsPostFixVerification guards the post-fix
// verification rerun: a check that fails fast, fixes fast, then wedges on the
// verifying re-run must not hang gc doctor --fix. Without bounding that rerun,
// the per-check timeout only covers the initial Run and this exact interaction
// re-opens the wedge the feature exists to close.
func TestRunCheckTimeoutBoundsPostFixVerification(t *testing.T) {
	d := &Doctor{CheckTimeout: 25 * time.Millisecond}
	check := &wedgeOnVerifyCheck{name: "wedge-on-verify", release: make(chan struct{})}
	t.Cleanup(func() { close(check.release) }) // release the abandoned verification goroutine
	d.Register(check)

	var buf bytes.Buffer
	start := time.Now()
	report := d.Run(&CheckContext{}, &buf, true)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("run took %s; the post-fix verification rerun was not bounded", elapsed)
	}

	// Fix ran once (the initial fast failure is fixable and not timed out),
	// proving we exercised the post-fix path rather than skipping it.
	if check.fixCalls != 1 {
		t.Fatalf("fixCalls = %d, want 1 (fix runs once for the fast initial failure)", check.fixCalls)
	}
	if len(report.Results) != 1 {
		t.Fatalf("Results = %d, want 1", len(report.Results))
	}
	got := report.Results[0]
	// The only path to TimedOut here is the verification boundedRun timing
	// out: the initial run failed fast, so it did not time out.
	if !got.TimedOut || got.Status != StatusError || got.Severity != SeverityAdvisory {
		t.Fatalf("verification result = %+v, want a timed-out advisory error", got)
	}
	if !got.FixAttempted {
		t.Fatalf("result.FixAttempted = false, want true; the fix ran but verification never confirmed it")
	}
	if got.Fixed || report.Fixed != 0 {
		t.Fatalf("result.Fixed=%v report.Fixed=%d, want the fix unconfirmed (0)", got.Fixed, report.Fixed)
	}
}

// wedgeOnVerifyCheck fails fast on its first Run, its Fix succeeds, then its
// post-fix verification Run wedges (blocks on release until the test abandons
// it). fixCalls is written only by Fix on the main goroutine before the
// verification goroutine is spawned, so the abandoned goroutine only ever reads
// it — no data race.
type wedgeOnVerifyCheck struct {
	name     string
	fixCalls int
	release  chan struct{} // closed by the test to release the abandoned verification goroutine
}

func (c *wedgeOnVerifyCheck) Name() string { return c.name }
func (c *wedgeOnVerifyCheck) Run(_ *CheckContext) *CheckResult {
	if c.fixCalls > 0 {
		<-c.release // verification wedges; abandoned on timeout, released at cleanup
		return &CheckResult{Name: c.name, Status: StatusOK}
	}
	return &CheckResult{Name: c.name, Status: StatusError, Severity: SeverityBlocking, Message: "needs fix"}
}
func (c *wedgeOnVerifyCheck) CanFix() bool { return true }
func (c *wedgeOnVerifyCheck) Fix(_ *CheckContext) error {
	c.fixCalls++
	return nil
}
func (c *wedgeOnVerifyCheck) WarmupEligible() bool { return false }

// TestRunCheckTimeoutBoundsFix guards fix execution itself: a check that fails
// fast then wedges inside Fix must not hang gc doctor --fix. The fix is
// abandoned at the per-check bound (not killed — it finishes in the
// background), the run returns promptly, and the result reports an unconfirmed
// remediation while preserving the check's original failing status.
func TestRunCheckTimeoutBoundsFix(t *testing.T) {
	d := &Doctor{CheckTimeout: 25 * time.Millisecond}
	check := &wedgeOnFixCheck{name: "wedge-on-fix", release: make(chan struct{})}
	t.Cleanup(func() { close(check.release) }) // release the abandoned fix goroutine
	d.Register(check)

	var buf bytes.Buffer
	start := time.Now()
	report := d.Run(&CheckContext{}, &buf, true)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("run took %s; the wedged fix was not bounded", elapsed)
	}

	// Fix ran once (the initial fast failure is fixable and not timed out),
	// proving we exercised the fix path rather than skipping it.
	if got := check.fixCalls.Load(); got != 1 {
		t.Fatalf("fixCalls = %d, want 1 (fix runs once for the fast initial failure)", got)
	}
	if len(report.Results) != 1 {
		t.Fatalf("Results = %d, want 1", len(report.Results))
	}
	got := report.Results[0]
	// The initial Run failed fast (not timed out); the FIX is what got
	// abandoned, so the result is an unconfirmed remediation, never Fixed.
	if got.Fixed || report.Fixed != 0 {
		t.Fatalf("result.Fixed=%v report.Fixed=%d, want the fix unconfirmed (0)", got.Fixed, report.Fixed)
	}
	if !got.FixAttempted {
		t.Fatalf("result.FixAttempted = false, want true; the fix ran but never confirmed")
	}
	if !strings.Contains(got.FixError, "timed out") {
		t.Fatalf("result.FixError = %q, want a fix-timeout explanation", got.FixError)
	}
	// The check still fails at its original severity: a wedged fix leaves the
	// problem unremediated, so it must still gate (unlike a timed-out Run,
	// which is advisory because its outcome is unknown).
	if got.Status != StatusError || got.Severity != SeverityBlocking {
		t.Fatalf("result = %+v, want the original blocking failure preserved", got)
	}
	if report.Failed != 1 || report.BlockingFailed != 1 {
		t.Fatalf("report = %+v, want the unfixed check counted as a blocking failure", report)
	}
}

// wedgeOnFixCheck fails fast on Run, then wedges inside Fix (blocks on release
// until the test abandons it) — modeling a remediation that hangs (e.g. a pack
// fix script blocked on I/O). Fix runs in the abandoned goroutine, so fixCalls
// is atomic: the goroutine writes it while the test reads it after the fix is
// abandoned.
type wedgeOnFixCheck struct {
	name     string
	fixCalls atomic.Int32
	release  chan struct{} // closed by the test to release the abandoned fix goroutine
}

func (c *wedgeOnFixCheck) Name() string { return c.name }
func (c *wedgeOnFixCheck) Run(_ *CheckContext) *CheckResult {
	return &CheckResult{Name: c.name, Status: StatusError, Severity: SeverityBlocking, Message: "needs fix"}
}
func (c *wedgeOnFixCheck) CanFix() bool { return true }
func (c *wedgeOnFixCheck) Fix(_ *CheckContext) error {
	c.fixCalls.Add(1)
	<-c.release // wedged remediation; abandoned on timeout, released at cleanup
	return nil
}
func (c *wedgeOnFixCheck) WarmupEligible() bool { return false }

// TestRunCheckTimeoutIsolatesLateOutputAndSkipsRenderExtras proves the two
// safety guards the timeout path adds: an abandoned check's late writes land
// in its private buffer (never the real writer), and RenderExtras is not
// invoked for a timed-out check. Coordination is via channels so the
// assertions are deterministic rather than timing-based.
func TestRunCheckTimeoutIsolatesLateOutputAndSkipsRenderExtras(t *testing.T) {
	d := &Doctor{CheckTimeout: 25 * time.Millisecond}
	check := &lateWritingCheck{
		name:      "late-writer",
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		wroteLate: make(chan struct{}),
	}
	d.Register(check)

	var buf bytes.Buffer
	done := make(chan *Report, 1)
	go func() { done <- d.Run(&CheckContext{}, &buf, false) }()

	// Wait until the check is running, then let the 25ms bound fire and
	// abandon it (the check stays blocked on release, so it cannot complete).
	<-check.started
	report := <-done

	if len(report.Results) != 1 || !report.Results[0].TimedOut {
		t.Fatalf("Results = %+v, want one timed-out result", report.Results)
	}

	// Release the abandoned check so it performs its late ctx.Output write,
	// then wait for that write to finish before asserting.
	outputBeforeLateWrite := buf.String()
	close(check.release)
	<-check.wroteLate

	if got := buf.String(); got != outputBeforeLateWrite {
		t.Fatalf("real writer received abandoned late output: before=%q after=%q", outputBeforeLateWrite, got)
	}
	if check.renderExtrasCalled.Load() {
		t.Fatalf("RenderExtras was invoked for a timed-out check; it must be skipped")
	}
}

// lateWritingCheck blocks inside Run until released, then writes to
// ctx.Output — modeling an abandoned check whose goroutine keeps running and
// emits output after the doctor timeout fired. It implements Renderer so the
// test can prove RenderExtras is skipped for a timed-out check.
type lateWritingCheck struct {
	name               string
	started            chan struct{}
	release            chan struct{}
	wroteLate          chan struct{}
	renderExtrasCalled atomic.Bool
}

func (c *lateWritingCheck) Name() string { return c.name }
func (c *lateWritingCheck) Run(ctx *CheckContext) *CheckResult {
	close(c.started)
	<-c.release
	// The doctor timeout has fired by now; this write must land in the
	// abandoned private buffer, never the real writer.
	if ctx.Output != nil {
		fmt.Fprintln(ctx.Output, "late abandoned output") //nolint:errcheck // test writer
	}
	close(c.wroteLate)
	return &CheckResult{Name: c.name, Status: StatusOK}
}
func (c *lateWritingCheck) CanFix() bool              { return false }
func (c *lateWritingCheck) Fix(_ *CheckContext) error { return nil }
func (c *lateWritingCheck) WarmupEligible() bool      { return false }
func (c *lateWritingCheck) RenderExtras(_ *CheckContext, _ io.Writer) {
	c.renderExtrasCalled.Store(true)
}

// TestRunCheckTimeoutIsolatesLateFixOutput proves the fix path gets the same
// output isolation as the Run path: when a wedged Fix is abandoned at the
// per-check bound, its late writes to ctx.Output must land in the private
// abandonment buffer, never the real writer. Without isolation the abandoned
// fix goroutine races the main run's writer (garbled output on a stream, slice
// corruption on a buffer-backed writer). Coordination is via channels so the
// assertion is deterministic rather than timing-based and holds under -race.
func TestRunCheckTimeoutIsolatesLateFixOutput(t *testing.T) {
	d := &Doctor{CheckTimeout: 25 * time.Millisecond}
	check := &lateWritingOnFixCheck{
		name:      "late-fix-writer",
		fixStart:  make(chan struct{}),
		release:   make(chan struct{}),
		wroteLate: make(chan struct{}),
	}
	d.Register(check)

	var buf bytes.Buffer
	done := make(chan *Report, 1)
	go func() { done <- d.Run(&CheckContext{}, &buf, true) }()

	// Wait until Fix is running, then let the 25ms bound fire and abandon it
	// (Fix stays blocked on release, so it cannot complete within the bound).
	<-check.fixStart
	report := <-done

	// The initial Run failed fast; the FIX was abandoned, so the result is an
	// unconfirmed remediation — proving we exercised the fix-timeout path.
	if len(report.Results) != 1 {
		t.Fatalf("Results = %d, want 1", len(report.Results))
	}
	if got := report.Results[0]; got.Fixed || !got.FixAttempted || !strings.Contains(got.FixError, "timed out") {
		t.Fatalf("result = %+v, want an unconfirmed fix-timeout", got)
	}

	// Release the abandoned fix so it performs its late ctx.Output write, then
	// wait for that write to finish before asserting nothing reached the real
	// writer.
	outputBeforeLateWrite := buf.String()
	close(check.release)
	<-check.wroteLate

	if got := buf.String(); got != outputBeforeLateWrite {
		t.Fatalf("real writer received abandoned late fix output: before=%q after=%q", outputBeforeLateWrite, got)
	}
}

// lateWritingOnFixCheck fails fast on Run, then blocks inside Fix until
// released and writes to ctx.Output afterward — modeling an abandoned fix
// goroutine (e.g. the dolt-drift or v2-migration fixes, which write ctx.Output)
// that keeps running and emits diagnostics after the doctor timeout fired. The
// late write must land in the fix path's private buffer, never the real writer.
type lateWritingOnFixCheck struct {
	name      string
	fixStart  chan struct{}
	release   chan struct{}
	wroteLate chan struct{}
}

func (c *lateWritingOnFixCheck) Name() string { return c.name }
func (c *lateWritingOnFixCheck) Run(_ *CheckContext) *CheckResult {
	return &CheckResult{Name: c.name, Status: StatusError, Severity: SeverityBlocking, Message: "needs fix"}
}
func (c *lateWritingOnFixCheck) CanFix() bool { return true }
func (c *lateWritingOnFixCheck) Fix(ctx *CheckContext) error {
	close(c.fixStart)
	<-c.release
	// The doctor timeout has fired by now; this write must land in the
	// abandoned private buffer, never the real writer.
	if ctx.Output != nil {
		fmt.Fprintln(ctx.Output, "late abandoned fix output") //nolint:errcheck // test writer
	}
	close(c.wroteLate)
	return nil
}
func (c *lateWritingOnFixCheck) WarmupEligible() bool { return false }
