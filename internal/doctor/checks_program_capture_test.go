package doctor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// runProgramCapture runs the check over st.
func runProgramCapture(t *testing.T, st ProgramCaptureState) *CheckResult {
	t.Helper()
	return NewProgramCaptureCheck(func() (ProgramCaptureState, error) { return st, nil }).Run(&CheckContext{})
}

// detailWith returns the first detail containing every needle, "" if none.
func detailWith(r *CheckResult, needles ...string) string {
	for _, d := range r.Details {
		all := true
		for _, n := range needles {
			all = all && strings.Contains(d, n)
		}
		if all {
			return d
		}
	}
	return ""
}

func reading(v runtime.ProgramStatusValidity, mutate ...func(*runtime.ProgramStatus)) runtime.ProgramStatus {
	r := runtime.ProgramStatus{Mode: runtime.ProgramStatusOn, Validity: v}
	if v == runtime.ProgramStatusValid {
		r.State, r.ReaderVerified = runtime.ProgramIdle, true
	}
	for _, m := range mutate {
		m(&r)
	}
	return r
}

func TestDoctorReportsDeclaredButSilent(t *testing.T) {
	r := runProgramCapture(t, ProgramCaptureState{
		Mode:         runtime.ProgramStatusOn,
		FloorChecked: true,
		Sessions: []ProgramCaptureSession{
			{Name: "quiet", Declared: true, Reading: reading(runtime.ProgramStatusSilent)},
			{Name: "healthy", Declared: true, Reading: reading(runtime.ProgramStatusValid)},
			{Name: "plain", Declared: false, Reading: reading(runtime.ProgramStatusNotArmed)},
		},
	})
	if r.Status != StatusWarning || r.Severity != SeverityAdvisory {
		t.Fatalf("status %v severity %v, want an advisory warning: %+v", r.Status, r.Severity, r)
	}
	if detailWith(r, "quiet", "declared but silent") == "" {
		t.Fatalf("details %q lack the declared-but-silent session", r.Details)
	}
	if detailWith(r, "healthy") != "" || detailWith(r, "plain") != "" {
		t.Fatalf("details %q name a healthy or an undeclared session", r.Details)
	}
	if detailWith(r, "quiet", "probed") != "" {
		t.Fatalf("a silent session that never probed reads probed: %q", r.Details)
	}
}

func TestDoctorReportsProbedButUnanswered(t *testing.T) {
	r := runProgramCapture(t, ProgramCaptureState{
		Mode: runtime.ProgramStatusShadow,
		Sessions: []ProgramCaptureSession{
			{Name: "gated", Declared: true, Reading: reading(runtime.ProgramStatusSilent, func(s *runtime.ProgramStatus) { s.Probed = true })},
		},
	})
	if r.Status != StatusWarning {
		t.Fatalf("status %v, want a warning: %+v", r.Status, r)
	}
	if detailWith(r, "gated", "probed but unanswered", "relay = true") == "" {
		t.Fatalf("details %q lack the probed-but-unanswered session and its fix", r.Details)
	}
	if detailWith(r, "gated", "declared but silent") != "" {
		t.Fatalf("a probed session also reads plainly silent: %q", r.Details)
	}
}

func TestDoctorReportsNotArmedAndReaderLost(t *testing.T) {
	r := runProgramCapture(t, ProgramCaptureState{
		Mode: runtime.ProgramStatusOn,
		Sessions: []ProgramCaptureSession{
			{Name: "unarmed", Declared: true, Reading: reading(runtime.ProgramStatusNotArmed)},
			{Name: "lost", Declared: true, Reading: reading(runtime.ProgramStatusReaderLost)},
			{Name: "stale", Declared: false, Reading: reading(runtime.ProgramStatusValid)},
			{Name: "broken", Declared: true, ReadErr: errors.New("tmux: boom")},
			{Name: "dead", Declared: true, Reading: reading(runtime.ProgramStatusExited)},
		},
	})
	for _, want := range [][]string{
		{"unarmed", "declared but not armed"},
		{"lost", "reader lost"},
		{"stale", "no longer declared"},
		{"broken", "tmux: boom"},
	} {
		if detailWith(r, want...) == "" {
			t.Errorf("details %q lack %q", r.Details, want)
		}
	}
	if detailWith(r, "session dead") != "" {
		t.Errorf("an exited session is a finding: %q", r.Details)
	}
	if r.Message != "4 program-status capture finding(s)" {
		t.Errorf("message %q", r.Message)
	}
}

func TestDoctorReportsCompanionMissing(t *testing.T) {
	missing := fmt.Errorf("%w next to \"/usr/local/bin/gc\" or on PATH", programstatus.ErrCompanionNotFound)
	r := runProgramCapture(t, ProgramCaptureState{Mode: runtime.ProgramStatusOn, FloorChecked: true, Floor: missing})
	if r.Status != StatusWarning || detailWith(r, "capability floor failed", "not found") == "" {
		t.Fatalf("result %+v, want the failed floor named", r)
	}
	if !strings.Contains(r.FixHint, programstatus.CompanionName) {
		t.Fatalf("fix hint %q does not name the companion", r.FixHint)
	}

	old := runProgramCapture(t, ProgramCaptureState{Mode: runtime.ProgramStatusOn, FloorChecked: true, Floor: errors.New(`tmux "3.1" is older than 3.2`)})
	if old.Status != StatusWarning || !strings.Contains(old.FixHint, "tmux 3.2") {
		t.Fatalf("old tmux result %+v, want a warning with the tmux floor hint", old)
	}
}

func TestDoctorNotesUncheckedFloorAndTapLiveness(t *testing.T) {
	r := runProgramCapture(t, ProgramCaptureState{
		Mode:         runtime.ProgramStatusOn,
		FloorChecked: true,
		Floor:        fmt.Errorf("tmux server not running: %w", runtime.ErrRuntimeUnavailable),
		Sessions: []ProgramCaptureSession{
			{Name: "unverified", Declared: true, Reading: reading(runtime.ProgramStatusValid, func(s *runtime.ProgramStatus) { s.ReaderVerified = false })},
		},
	})
	if r.Status != StatusOK {
		t.Fatalf("status %v, want OK: unchecked liveness and an unchecked floor are notes: %+v", r.Status, r)
	}
	if detailWith(r, "capability floor not checked") == "" || detailWith(r, "unverified", "tap liveness unchecked") == "" {
		t.Fatalf("details %q lack the unchecked floor and tap liveness", r.Details)
	}
	if !strings.Contains(r.Message, "1 with tap liveness unchecked") {
		t.Fatalf("message %q does not count the unchecked tap", r.Message)
	}
}

func TestDoctorProgramCaptureOffAndReadError(t *testing.T) {
	for _, mode := range []runtime.ProgramStatusMode{"", runtime.ProgramStatusOff} {
		r := runProgramCapture(t, ProgramCaptureState{Mode: mode, Sessions: []ProgramCaptureSession{{Name: "x", Declared: true, Reading: reading(runtime.ProgramStatusNotArmed)}}})
		if r.Status != StatusOK || !strings.Contains(r.Message, "off") || len(r.Details) != 0 {
			t.Fatalf("mode %q: %+v, want OK and off", mode, r)
		}
	}
	r := NewProgramCaptureCheck(func() (ProgramCaptureState, error) { return ProgramCaptureState{}, errors.New("no store") }).Run(&CheckContext{})
	if r.Status != StatusWarning || !strings.Contains(r.Message, "no store") {
		t.Fatalf("read error: %+v, want a warning naming it", r)
	}
}
