package runtime_test

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestFakeProgramStatusContract proves RUNTIME-CONTRACT-017 (PS-001…PS-014)
// in the unit lane: the FullFake judges scripted pane rows whose records the
// emitter's bytes produce through the real capture pipeline.
func TestFakeProgramStatusContract(t *testing.T) {
	runtimetest.RunProgramStatusContract(t, runtimetest.FakeProgramStatusFixtures())
}

// armedFakeRow is an armed, reporting agent-pane row for the fake.
func armedFakeRow() runtime.ProgramPaneRow {
	return runtime.ProgramPaneRow{
		Mode: runtime.ProgramStatusOn, CaptureGate: true, PaneID: "%2",
		PanePipe: true, Tap: "r1.%2", TapLiveness: runtime.ProgramTapAlive,
		RecordForm: runtime.ProgramRecordDecoded,
		Record:     runtime.ProgramStatus{Nonce: "r1.%2", State: runtime.ProgramWorking, Seq: 1, Progress: -1, Reported: true},
	}
}

// Kills a fake whose reading is not the judged row, that reads a session it
// does not run, that ignores its scripted error or brokenness, or whose
// inventory drops the reading.
func TestFullFakeProgramStatusJudgesItsRow(t *testing.T) {
	f := runtime.NewFake()
	ff := runtime.FullFake{Fake: f}
	if _, err := ff.ProgramStatus("s"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("unstarted session: %v, want ErrSessionNotFound", err)
	}
	if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if r, err := ff.ProgramStatus("s"); err != nil || r.Validity != runtime.ProgramStatusUnread || r.State != "" {
		t.Fatalf("no row: %+v, %v; want Unread", r, err)
	}
	inv, err := ff.RuntimeInventory(t.Context())
	if err != nil || inv["s"].Program != (runtime.ProgramStatus{}) {
		t.Fatalf("inventory without a row: %+v, %v; want the zero reading", inv["s"].Program, err)
	}
	f.SetProgramPane("s", armedFakeRow())
	r, err := ff.ProgramStatus("s")
	if err != nil || !r.Running() || !r.Decides() || !r.ReaderVerified {
		t.Fatalf("armed row: %+v, %v; want a verified deciding working reading", r, err)
	}
	if inv, _ := ff.RuntimeInventory(t.Context()); inv["s"].Program != r {
		t.Fatalf("inventory reading %+v, want %+v", inv["s"].Program, r)
	}
	if ps, ok := runtime.ProgramStatusFor(auto.New(ff, runtime.NewFake()), "s"); !ok || ps == nil {
		t.Fatal("ProgramStatusFor did not resolve the fake leaf behind a composite")
	} else if got, _ := ps.ProgramStatus("s"); got != r {
		t.Fatalf("through the composite: %+v, want %+v", got, r)
	}
	boom := errors.New("boom")
	f.ProgramStatusErrors = map[string]error{"s": boom}
	if _, err := ff.ProgramStatus("s"); !errors.Is(err, boom) {
		t.Fatalf("scripted error: %v", err)
	}
	broken := runtime.FullFake{Fake: runtime.NewFailFake()}
	if _, err := broken.ProgramStatus("s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("broken fake: %v, want ErrRuntimeUnavailable", err)
	}
	if _, ok := any(runtime.NewFake()).(runtime.ProgramStatusProvider); ok {
		t.Error("the plain Fake implements ProgramStatusProvider: it would move every caller test onto the program-status path")
	}
}

// Kills a fake heal that takes a foreign or not-yet-closed pipe over, arms or
// disarms a dead pane, re-arms a healthy capture, reuses a nonce, does not
// disarm, closes on disarm a pipe it cannot show its own tap holds, or drops
// the nonce of an open pipe whose tap's liveness is unknown.
func TestFullFakeHealProgramCapture(t *testing.T) {
	want := runtime.ProgramStatusArm{Capture: true}
	cases := []struct {
		name   string
		row    func() (runtime.ProgramPaneRow, bool)
		want   runtime.ProgramStatusArm
		action runtime.ProgramCaptureAction
	}{
		{"healthy", func() (runtime.ProgramPaneRow, bool) { return armedFakeRow(), true }, want, runtime.ProgramCaptureUnchanged},
		{"no row", func() (runtime.ProgramPaneRow, bool) { return runtime.ProgramPaneRow{}, false }, want, runtime.ProgramCaptureDeferred},
		{"pane dead", func() (runtime.ProgramPaneRow, bool) {
			r := armedFakeRow()
			r.PaneDead, r.PanePipe = true, false
			return r, true
		}, want, runtime.ProgramCaptureDeferred},
		{"foreign pipe", func() (runtime.ProgramPaneRow, bool) {
			r := armedFakeRow()
			r.TapLiveness = runtime.ProgramTapGone
			return r, true
		}, want, runtime.ProgramCaptureDeferred},
		{"unarmed pane with a pipe", func() (runtime.ProgramPaneRow, bool) {
			r := armedFakeRow()
			r.Tap = ""
			return r, true
		}, want, runtime.ProgramCaptureDeferred},
		{"pipe closed", func() (runtime.ProgramPaneRow, bool) {
			r := armedFakeRow()
			r.PanePipe, r.TapLiveness = false, runtime.ProgramTapGone
			return r, true
		}, want, runtime.ProgramCaptureArmed},
		{"never armed", func() (runtime.ProgramPaneRow, bool) {
			return runtime.ProgramPaneRow{Mode: runtime.ProgramStatusOn, CaptureGate: true, PaneID: "%2"}, true
		}, want, runtime.ProgramCaptureArmed},
		{"disarm", func() (runtime.ProgramPaneRow, bool) { return armedFakeRow(), true }, runtime.ProgramStatusArm{}, runtime.ProgramCaptureDisarmed},
		{"disarm beside a foreign pipe", func() (runtime.ProgramPaneRow, bool) {
			r := armedFakeRow()
			r.TapLiveness = runtime.ProgramTapGone
			return r, true
		}, runtime.ProgramStatusArm{}, runtime.ProgramCaptureDisarmed},
		{"disarm with tap liveness unknown", func() (runtime.ProgramPaneRow, bool) {
			r := armedFakeRow()
			r.TapLiveness = runtime.ProgramTapUnknown
			return r, true
		}, runtime.ProgramStatusArm{}, runtime.ProgramCaptureDeferred},
		{"disarm with no pipe and tap liveness unknown", func() (runtime.ProgramPaneRow, bool) {
			r := armedFakeRow()
			r.PanePipe, r.TapLiveness = false, runtime.ProgramTapUnknown
			return r, true
		}, runtime.ProgramStatusArm{}, runtime.ProgramCaptureDisarmed},
		{"disarm a dead pane", func() (runtime.ProgramPaneRow, bool) {
			r := armedFakeRow()
			r.PaneDead = true
			return r, true
		}, runtime.ProgramStatusArm{}, runtime.ProgramCaptureDeferred},
		{"nothing to disarm", func() (runtime.ProgramPaneRow, bool) {
			return runtime.ProgramPaneRow{Mode: runtime.ProgramStatusOn, PaneID: "%2"}, true
		}, runtime.ProgramStatusArm{}, runtime.ProgramCaptureUnchanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := runtime.NewFake()
			ff := runtime.FullFake{Fake: f}
			if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			before, ok := tc.row()
			if ok {
				f.SetProgramPane("s", before)
			}
			got, err := ff.HealProgramCapture("s", tc.want)
			if err != nil || got != tc.action {
				t.Fatalf("heal = %q, %v; want %q", got, err, tc.action)
			}
			after, _ := f.ProgramPane("s")
			r, _ := ff.ProgramStatus("s")
			switch tc.action {
			case runtime.ProgramCaptureArmed:
				if after.Tap == before.Tap || after.Tap != "heal1.%2" || r.Validity != runtime.ProgramStatusSilent {
					t.Fatalf("armed row %+v reads %q; want a new nonce naming the pane, silent", after, r.Validity)
				}
				if again, _ := ff.HealProgramCapture("s", tc.want); again != runtime.ProgramCaptureUnchanged {
					t.Fatalf("second heal = %q, want unchanged", again)
				}
			case runtime.ProgramCaptureDisarmed:
				// Only a pipe the process table shows our tap holding is
				// closed: any other may be foreign.
				pipeOpen := before.PanePipe && before.TapLiveness != runtime.ProgramTapAlive
				if r.Validity != runtime.ProgramStatusNotArmed || after.PanePipe != pipeOpen {
					t.Fatalf("disarmed row %+v reads %q; want not_armed, pipe open %v", after, r.Validity, pipeOpen)
				}
			default:
				if after != before {
					t.Fatalf("%s heal changed the row: %+v -> %+v", tc.action, before, after)
				}
			}
			calls := f.SnapshotCalls()
			var last runtime.Call
			for _, c := range calls {
				if c.Method == "HealProgramCapture" {
					last = c
					break
				}
			}
			if last.Value != string(tc.action) {
				t.Fatalf("recorded heal %+v, want value %q", last, tc.action)
			}
		})
	}
	f := runtime.NewFake()
	ff := runtime.FullFake{Fake: f}
	if _, err := ff.HealProgramCapture("gone", want); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("missing session: %v, want ErrSessionNotFound", err)
	}
	if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	f.ProgramCaptureHealErrors = map[string]error{"s": boom}
	if _, err := ff.HealProgramCapture("s", want); !errors.Is(err, boom) {
		t.Fatalf("scripted error: %v", err)
	}
	if _, err := (runtime.FullFake{Fake: runtime.NewFailFake()}).HealProgramCapture("s", want); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("broken fake: %v, want ErrRuntimeUnavailable", err)
	}
}
