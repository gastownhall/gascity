package doctor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// mockCheck is a configurable Check for testing the runner.
type mockCheck struct {
	name   string
	status CheckStatus
	msg    string
	canFix bool
	fixErr error
	fixed  bool // set by Fix
}

func (m *mockCheck) Name() string { return m.name }
func (m *mockCheck) Run(_ *CheckContext) *CheckResult {
	st := m.status
	if m.fixed {
		st = StatusOK
	}
	return &CheckResult{
		Name:    m.name,
		Status:  st,
		Message: m.msg,
	}
}
func (m *mockCheck) CanFix() bool { return m.canFix }
func (m *mockCheck) Fix(_ *CheckContext) error {
	if m.fixErr != nil {
		return m.fixErr
	}
	m.fixed = true
	return nil
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
