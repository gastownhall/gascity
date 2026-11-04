package runtime_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

// holdRow is armedRow under mode, declaring the mid-turn hold, with a root
// record in st that entered st at since.
func holdRow(mode runtime.ProgramStatusMode, st runtime.ProgramState, since time.Time) runtime.ProgramPaneRow {
	row := armedRow()
	row.Mode, row.HoldDeclared = mode, true
	row.Record.State, row.Record.Since = st, since
	return row
}

// heldFake is a FullFake running session "s" whose agent pane reads row
// fresh and declares the mid-turn hold.
func heldFake(t *testing.T, row runtime.ProgramPaneRow) (*runtime.Fake, runtime.FullFake) {
	t.Helper()
	f := runtime.NewFake()
	if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	f.SetProgramPane("s", row)
	return f, runtime.FullFake{Fake: f}
}

// Kills a hold that lets anything but a valid, deciding, live working or
// blocked record (or a working child) hold, outlives its bound, ignores the
// pane's activity, or reads a state entered after the caller's clock as a
// skewed record.
func TestProgramHolds(t *testing.T) {
	now := time.Now()
	reading := func(mode runtime.ProgramStatusMode, st runtime.ProgramState, since time.Time) runtime.ProgramStatus {
		s := valid(st, 2, 1)
		s.Mode, s.Since = mode, since
		return s
	}
	child := reading(runtime.ProgramStatusOn, runtime.ProgramIdle, now.Add(-time.Second))
	child.WorkingChildren = 1
	expired := now.Add(-runtime.ProgramStatusHoldLimit - time.Second)
	cases := []struct {
		name string
		r    runtime.ProgramHoldReading
		want bool
		// early moves the caller's clock before now: the reading entered
		// its state after the clock the caller passes.
		early time.Duration
	}{
		{"working", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusOn, runtime.ProgramWorking, now.Add(-time.Second))}, true, 0},
		{"blocked", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusOn, runtime.ProgramBlocked, now.Add(-time.Second))}, true, 0},
		{"working child at a boundary", runtime.ProgramHoldReading{Status: child}, true, 0},
		{"idle", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusOn, runtime.ProgramIdle, now.Add(-time.Second))}, false, 0},
		{"done", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusOn, runtime.ProgramDone, now.Add(-time.Second))}, false, 0},
		{"error", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusOn, runtime.ProgramError, now.Add(-time.Second))}, false, 0},
		{"shadow reading decides nothing", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusShadow, runtime.ProgramWorking, now.Add(-time.Second))}, false, 0},
		{"not valid", runtime.ProgramHoldReading{Status: runtime.ProgramStatus{Mode: runtime.ProgramStatusOn, Validity: runtime.ProgramStatusReaderLost}}, false, 0},
		{"past the bound", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusOn, runtime.ProgramWorking, expired)}, false, 0},
		{"past the bound, kept live by the pane's activity", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusOn, runtime.ProgramWorking, expired), Activity: now.Add(-time.Minute)}, true, 0},
		{"past the bound, the pane's activity as stale", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusOn, runtime.ProgramWorking, expired), Activity: expired}, false, 0},
		{"entered after the caller's clock", runtime.ProgramHoldReading{Status: reading(runtime.ProgramStatusOn, runtime.ProgramWorking, now)}, true, time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runtime.ProgramHolds(c.r, now.Add(-c.early)); got != c.want {
				t.Fatalf("ProgramHolds(%+v) = %v, want %v", c.r, got, c.want)
			}
		})
	}
}

// Kills a hold decision that trusts the cached reading: a snapshot that
// still says working while the pane's record already says idle delivers,
// and a snapshot that says idle while the record says working holds.
func TestProgramRecordHoldReadsFresh(t *testing.T) {
	now := time.Now()
	f, ff := heldFake(t, holdRow(runtime.ProgramStatusOn, runtime.ProgramIdle, now.Add(-time.Second)))
	f.SetProgramCachedPane("s", holdRow(runtime.ProgramStatusOn, runtime.ProgramWorking, now.Add(-time.Second)))
	if cached, _ := ff.ProgramStatus("s"); !cached.Running() {
		t.Fatalf("fixture: the cached reading %+v is not working", cached)
	}
	if r, held := runtime.ProgramRecordHold(ff, "s", now); held || r.Status.State != runtime.ProgramIdle {
		t.Fatalf("cache working, fresh idle: held=%v reading %+v, want delivered on the fresh idle", held, r.Status)
	}
	f.SetProgramPane("s", holdRow(runtime.ProgramStatusOn, runtime.ProgramWorking, now.Add(-time.Second)))
	f.SetProgramCachedPane("s", holdRow(runtime.ProgramStatusOn, runtime.ProgramIdle, now.Add(-time.Second)))
	if _, held := runtime.ProgramRecordHold(auto.New(ff, runtime.NewFake()), "s", now); !held {
		t.Fatal("cache idle, fresh working, behind a composite: not held")
	}
	if methodCalls(f, "ProgramStatus") != 1 {
		t.Fatalf("the hold read the cached reading %d times, want only the fixture's own read", methodCalls(f, "ProgramStatus")-1)
	}
}

// Kills a hold on a pane that does not declare it, under a switch that is
// not on, on a failed read, or on a leaf without the capability, and one
// whose reading is read when the switch is not on.
func TestProgramRecordHoldNeedsDeclarationAndSwitchOn(t *testing.T) {
	now := time.Now()
	working := holdRow(runtime.ProgramStatusOn, runtime.ProgramWorking, now.Add(-time.Second))

	f, ff := heldFake(t, working)
	f.SetProgramHold("s", false)
	if _, held := runtime.ProgramRecordHold(ff, "s", now); held {
		t.Error("an undeclared pane held")
	}

	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusShadow, runtime.ProgramStatusOff} {
		row := working
		row.Mode = mode
		_, ff := heldFake(t, row)
		if r, ok := ff.ProgramStatusHoldRead("s"); ok {
			t.Errorf("switch %s: a hold read %+v was taken", mode, r)
		}
		if _, held := runtime.ProgramRecordHold(ff, "s", now); held {
			t.Errorf("switch %s held", mode)
		}
	}

	f, ff = heldFake(t, working)
	f.ProgramStatusErrors = map[string]error{"s": errors.New("boom")}
	if _, held := runtime.ProgramRecordHold(ff, "s", now); held {
		t.Error("a failed read held")
	}
	if _, held := runtime.ProgramRecordHold(runtime.NewFake(), "s", now); held {
		t.Error("a leaf without the capability held")
	}
	if _, held := runtime.ProgramRecordHold(nil, "s", now); held {
		t.Error("a nil provider held")
	}
	if _, held := runtime.ProgramRecordHold(ff, "", now); held {
		t.Error("an empty session name held")
	}
}

// Kills a hold read that drops the pane's activity: the bound is judged on
// max(Since, activity), so a long turn that keeps drawing stays held.
func TestProgramRecordHoldUsesThePanesActivity(t *testing.T) {
	now := time.Now()
	f, ff := heldFake(t, holdRow(runtime.ProgramStatusOn, runtime.ProgramWorking, now.Add(-runtime.ProgramStatusHoldLimit-time.Minute)))
	if _, held := runtime.ProgramRecordHold(ff, "s", now); held {
		t.Fatal("a working record past its bound with no activity held")
	}
	f.SetActivity("s", now.Add(-time.Minute))
	r, held := runtime.ProgramRecordHold(ff, "s", now)
	if !held || !r.Activity.Equal(now.Add(-time.Minute)) {
		t.Fatalf("a working record past its bound, drawing a minute ago: held=%v activity %v, want held on that activity", held, r.Activity)
	}
}

// Kills a refusal that does not defer: it is a busy refusal, never a
// pending-interaction one.
func TestProgramHoldRefusal(t *testing.T) {
	err := runtime.ProgramHoldRefusal("s")
	if !errors.Is(err, runtime.ErrSessionBusy) || errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("ProgramHoldRefusal = %v, want a busy refusal only", err)
	}
}

// Kills a typing hold wider than a running turn: a blocked record or a
// working child is the quiet decisions' hold (ProgramHolds), never the
// leaf's, and the typing hold keeps the bound.
func TestProgramTypingHoldsOnlyARunningTurn(t *testing.T) {
	now := time.Now()
	at := func(st runtime.ProgramState, since time.Time, children int) runtime.ProgramHoldReading {
		s := valid(st, 2, 1)
		s.Since, s.WorkingChildren = since, children
		return runtime.ProgramHoldReading{Status: s}
	}
	for _, c := range []struct {
		name          string
		r             runtime.ProgramHoldReading
		typing, quiet bool
	}{
		{"working", at(runtime.ProgramWorking, now, 0), true, true},
		{"blocked", at(runtime.ProgramBlocked, now, 0), false, true},
		{"working child", at(runtime.ProgramIdle, now, 1), false, true},
		{"idle", at(runtime.ProgramIdle, now, 0), false, false},
		{"working past its bound", at(runtime.ProgramWorking, now.Add(-runtime.ProgramStatusHoldLimit-time.Second), 0), false, false},
	} {
		if got := runtime.ProgramTypingHolds(c.r, now); got != c.typing {
			t.Errorf("%s: ProgramTypingHolds = %v, want %v", c.name, got, c.typing)
		}
		if got := runtime.ProgramHolds(c.r, now); got != c.quiet {
			t.Errorf("%s: ProgramHolds = %v, want %v", c.name, got, c.quiet)
		}
	}
}

// Kills a fake whose typing ignores the hold, or holds where the tmux leaf
// would not: a declared pane's live working record refuses every nudge as
// busy, and an idle record, an undeclared pane or the switch at shadow
// deliver.
func TestFakeNudgeHonorsTheProgramHold(t *testing.T) {
	now := time.Now()
	f, ff := heldFake(t, holdRow(runtime.ProgramStatusOn, runtime.ProgramWorking, now.Add(-time.Second)))
	content := []runtime.ContentBlock{{Type: "text", Text: "hi"}}
	for name, nudge := range map[string]func() error{
		"Nudge":      func() error { return ff.Nudge("s", content) },
		"NudgeNow":   func() error { return ff.NudgeNow("s", content) },
		"NudgeKeyed": func() error { _, err := ff.NudgeKeyed("s", content, "k"); return err },
	} {
		if err := nudge(); !errors.Is(err, runtime.ErrSessionBusy) {
			t.Errorf("%s under a live working record: %v, want ErrSessionBusy", name, err)
		}
	}
	f.SetProgramPane("s", holdRow(runtime.ProgramStatusOn, runtime.ProgramIdle, now.Add(-time.Second)))
	if err := ff.Nudge("s", content); err != nil {
		t.Errorf("Nudge under an idle record: %v", err)
	}
	f.SetProgramPane("s", holdRow(runtime.ProgramStatusOn, runtime.ProgramBlocked, now.Add(-time.Second)))
	if err := ff.Nudge("s", content); err != nil {
		t.Errorf("Nudge under a blocked record: %v, want delivered (a dialog is not the leaf hold's)", err)
	}
	f.SetProgramPane("s", holdRow(runtime.ProgramStatusShadow, runtime.ProgramWorking, now.Add(-time.Second)))
	if err := ff.Nudge("s", content); err != nil {
		t.Errorf("Nudge under shadow: %v", err)
	}
	f.SetProgramPane("s", holdRow(runtime.ProgramStatusOn, runtime.ProgramWorking, now.Add(-time.Second)))
	f.SetProgramHold("s", false)
	if err := ff.Nudge("s", content); err != nil {
		t.Errorf("Nudge to an undeclared pane: %v", err)
	}
}

// Kills a heal that loses or invents the hold declaration: arming declares
// it exactly when the wanted capture does, a healthy capture converges to
// want, and disarming drops it.
func TestFakeHealCarriesTheHoldDeclaration(t *testing.T) {
	f := runtime.NewFake()
	if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	ff := runtime.FullFake{Fake: f}
	f.SetProgramPane("s", runtime.ProgramPaneRow{Mode: runtime.ProgramStatusOn, CaptureGate: true, PaneID: "%3"})
	if a, err := ff.HealProgramCapture("s", runtime.ProgramStatusArm{Capture: true, HoldMidTurn: true}); err != nil || a != runtime.ProgramCaptureArmed {
		t.Fatalf("heal = %q, %v", a, err)
	}
	declared := func() bool { row, _ := f.ProgramPane("s"); return row.HoldDeclared }
	if !declared() {
		t.Fatal("a pane armed declaring the hold does not declare it")
	}
	if a, err := ff.HealProgramCapture("s", runtime.ProgramStatusArm{Capture: true}); err != nil || a != runtime.ProgramCaptureDeclared || declared() {
		t.Fatalf("withdrawal = %q, %v, declared=%v; want the declaration withdrawn", a, err, declared())
	}
	if a, err := ff.HealProgramCapture("s", runtime.ProgramStatusArm{Capture: true}); err != nil || a != runtime.ProgramCaptureUnchanged {
		t.Fatalf("converged heal = %q, %v; want unchanged", a, err)
	}
	if a, err := ff.HealProgramCapture("s", runtime.ProgramStatusArm{Capture: true, HoldMidTurn: true}); err != nil || a != runtime.ProgramCaptureDeclared || !declared() {
		t.Fatalf("backfill = %q, %v, declared=%v; want the declaration set", a, err, declared())
	}
	if a, err := ff.HealProgramCapture("s", runtime.ProgramStatusArm{}); err != nil || a != runtime.ProgramCaptureDisarmed {
		t.Fatalf("disarm = %q, %v", a, err)
	}
	if declared() {
		t.Fatal("a disarmed pane still declares the hold")
	}
}
