package runtime_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a judgement that drops the pane's settle declaration from a reading
// under our nonce, or carries it onto one that is not ours.
func TestJudgeProgramStatusCarriesTheSettleDeclaration(t *testing.T) {
	base := runtime.ProgramPaneRow{
		Mode: runtime.ProgramStatusOn, CaptureGate: true, PaneID: "%3", PanePipe: true,
		Tap: "r.%3", TapLiveness: runtime.ProgramTapAlive, SettleDeclared: true,
	}
	silent := base
	valid := base
	valid.RecordForm = runtime.ProgramRecordDecoded
	valid.Record = runtime.ProgramStatus{Nonce: "r.%3", State: runtime.ProgramIdle, Reported: true}
	cleared := base
	cleared.RecordForm = runtime.ProgramRecordDecoded
	cleared.Record = runtime.ProgramStatus{Nonce: "r.%3", Reported: true}
	for name, row := range map[string]runtime.ProgramPaneRow{"silent": silent, "valid": valid, "cleared": cleared} {
		if s := runtime.JudgeProgramStatus(row); !s.SettleDeclared {
			t.Errorf("%s: reading %+v drops the pane's settle declaration", name, s)
		}
	}
	notArmed := base
	notArmed.Tap = "r.%9"
	if s := runtime.JudgeProgramStatus(notArmed); s.SettleDeclared {
		t.Errorf("a pane armed for another pane reads settle-declared: %+v", s)
	}
}

// Kills a heal that loses or invents the settle declaration: arming declares
// it exactly when the wanted capture does, a healthy capture converges to
// want in both directions (backfill and withdrawal), independent of the hold,
// and disarming drops it.
func TestFakeHealCarriesTheSettleDeclaration(t *testing.T) {
	f := runtime.NewFake()
	if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	ff := runtime.FullFake{Fake: f}
	f.SetProgramPane("s", runtime.ProgramPaneRow{Mode: runtime.ProgramStatusOn, CaptureGate: true, PaneID: "%3"})
	settling := runtime.ProgramStatusArm{Capture: true, DecideSettle: true}
	if a, err := ff.HealProgramCapture("s", settling); err != nil || a != runtime.ProgramCaptureArmed {
		t.Fatalf("heal = %q, %v", a, err)
	}
	row := func() runtime.ProgramPaneRow { r, _ := f.ProgramPane("s"); return r }
	if !row().SettleDeclared || row().HoldDeclared {
		t.Fatalf("armed row = %+v, want settle declared and no hold", row())
	}
	if a, err := ff.HealProgramCapture("s", runtime.ProgramStatusArm{Capture: true}); err != nil || a != runtime.ProgramCaptureDeclared || row().SettleDeclared {
		t.Fatalf("withdrawal = %q, %v, row=%+v; want the settle withdrawn", a, err, row())
	}
	if a, err := ff.HealProgramCapture("s", runtime.ProgramStatusArm{Capture: true}); err != nil || a != runtime.ProgramCaptureUnchanged {
		t.Fatalf("converged heal = %q, %v; want unchanged", a, err)
	}
	if a, err := ff.HealProgramCapture("s", settling); err != nil || a != runtime.ProgramCaptureDeclared || !row().SettleDeclared {
		t.Fatalf("backfill = %q, %v, row=%+v; want the settle set", a, err, row())
	}
	if a, err := ff.HealProgramCapture("s", runtime.ProgramStatusArm{}); err != nil || a != runtime.ProgramCaptureDisarmed || row().SettleDeclared {
		t.Fatalf("disarm = %q, %v, row=%+v; want the settle dropped", a, err, row())
	}
}
