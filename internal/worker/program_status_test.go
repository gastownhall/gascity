package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// programRow is an armed agent-pane row reporting st (with kind, for
// blocked) under mode, its tap alive: a valid reading unless mode is off.
func programRow(mode runtime.ProgramStatusMode, st runtime.ProgramState, kind runtime.ProgramBlockKind) runtime.ProgramPaneRow {
	return runtime.ProgramPaneRow{
		Mode: mode, CaptureGate: true, PaneID: "%1",
		PanePipe: true, Tap: "n.%1", TapLiveness: runtime.ProgramTapAlive,
		RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{
			Nonce: "n.%1", State: st, Kind: kind, App: "probe",
			Seq: 3, Progress: -1, TurnStarts: 1, Reported: true,
		},
	}
}

// Kills a ProgramPhase that decides from a shadow or invalid reading, maps a
// state to the wrong phase, or treats error as a terminal failure.
func TestProgramPhaseDecidesOnlyUnderOn(t *testing.T) {
	cases := []struct {
		st   runtime.ProgramState
		want Phase
	}{
		{runtime.ProgramWorking, PhaseBusy},
		{runtime.ProgramBlocked, PhaseBlocked},
		{runtime.ProgramIdle, PhaseReady},
		{runtime.ProgramDone, PhaseReady},
		{runtime.ProgramError, PhaseReady},
	}
	for _, tc := range cases {
		on := runtime.JudgeProgramStatus(programRow(runtime.ProgramStatusOn, tc.st, ""))
		if got, ok := ProgramPhase(on); !ok || got != tc.want {
			t.Errorf("on %s: ProgramPhase = %q, %v; want %q", tc.st, got, ok, tc.want)
		}
		shadow := runtime.JudgeProgramStatus(programRow(runtime.ProgramStatusShadow, tc.st, ""))
		if !shadow.Valid() {
			t.Fatalf("shadow %s reading %+v is not valid", tc.st, shadow)
		}
		if got, ok := ProgramPhase(shadow); ok {
			t.Errorf("shadow %s: ProgramPhase = %q, decided; want undecided", tc.st, got)
		}
	}
	silent := programRow(runtime.ProgramStatusOn, runtime.ProgramWorking, "")
	silent.RecordForm, silent.Record = runtime.ProgramRecordAbsent, runtime.ProgramStatus{}
	for name, ps := range map[string]runtime.ProgramStatus{
		"unread": {},
		"silent": runtime.JudgeProgramStatus(silent),
	} {
		if got, ok := ProgramPhase(ps); ok {
			t.Errorf("%s: ProgramPhase = %q, decided; want undecided", name, got)
		}
	}
}

// startedProgramStatusHandle is a started session handle on a FullFake (which
// takes program-status readings), and its runtime session name.
func startedProgramStatusHandle(t *testing.T) (*SessionHandle, *runtime.Fake, string) {
	t.Helper()
	fake := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(beads.NewMemStore(), runtime.FullFake{Fake: fake}, sessionpkg.WithCityPath(t.TempDir()))
	handle, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Session: SessionSpec{
		Profile: ProfileCodexTmuxCLI, Template: "probe", Title: "Probe", WorkDir: t.TempDir(),
	}})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	state, err := handle.State(context.Background())
	if err != nil || state.SessionName == "" {
		t.Fatalf("State after start: %+v, %v", state, err)
	}
	return handle, fake, state.SessionName
}

// Kills a SessionHandle.State that drops the reading, lets a shadow reading
// move the phase, ignores an on reading, or lets a record outrank a
// parser-detected pending interaction.
func TestSessionHandleStateProgramFollowsRecordOnlyUnderOn(t *testing.T) {
	handle, fake, name := startedProgramStatusHandle(t)
	ctx := context.Background()

	base, err := handle.State(ctx)
	if err != nil || base.Phase != PhaseReady || base.Program.Validity != runtime.ProgramStatusUnread || base.Program.State != "" {
		t.Fatalf("no record: %+v, %v; want ready with an Unread reading", base, err)
	}

	fake.SetProgramPane(name, programRow(runtime.ProgramStatusShadow, runtime.ProgramWorking, ""))
	shadow, err := handle.State(ctx)
	if err != nil || shadow.Phase != base.Phase || !shadow.Program.Valid() || shadow.Program.State != runtime.ProgramWorking {
		t.Fatalf("shadow working: %+v, %v; want phase %s with the valid reading carried", shadow, err, base.Phase)
	}

	for st, want := range map[runtime.ProgramState]Phase{
		runtime.ProgramWorking: PhaseBusy,
		runtime.ProgramBlocked: PhaseBlocked,
		runtime.ProgramDone:    PhaseReady,
		runtime.ProgramError:   PhaseReady,
	} {
		fake.SetProgramPane(name, programRow(runtime.ProgramStatusOn, st, ""))
		got, err := handle.State(ctx)
		if err != nil || got.Phase != want || got.Program.State != st {
			t.Fatalf("on %s: %+v, %v; want phase %s", st, got, err, want)
		}
		if got.Pending != nil {
			t.Fatalf("on %s: pending %+v; a record yields no pending interaction in this slice", st, got.Pending)
		}
	}

	fake.SetProgramPane(name, programRow(runtime.ProgramStatusOn, runtime.ProgramWorking, ""))
	fake.SetPendingInteraction(name, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval"})
	got, err := handle.State(ctx)
	if err != nil || got.Phase != PhaseBlocked || got.Pending == nil || got.Pending.RequestID != "req-1" {
		t.Fatalf("parser pending under a working record: %+v, %v; want blocked on req-1", got, err)
	}
}

// Kills a RuntimeHandle.State that reads no program status or decides from a
// shadow reading.
func TestRuntimeHandleStateProgramFollowsRecordOnlyUnderOn(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "rt", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: runtime.FullFake{Fake: fake}, SessionName: "rt", ProviderName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	fake.SetProgramPane("rt", programRow(runtime.ProgramStatusShadow, runtime.ProgramBlocked, runtime.ProgramBlockQuestion))
	got, err := handle.State(context.Background())
	if err != nil || got.Phase != PhaseReady || !got.Program.Blocked() || got.Program.Kind != runtime.ProgramBlockQuestion {
		t.Fatalf("shadow blocked: %+v, %v; want ready carrying the blocked reading", got, err)
	}
	fake.SetProgramPane("rt", programRow(runtime.ProgramStatusOn, runtime.ProgramWorking, ""))
	if got, err := handle.State(context.Background()); err != nil || got.Phase != PhaseBusy {
		t.Fatalf("on working: %+v, %v; want busy", got, err)
	}
}

// Kills a ProgramStatusOf that leaks a read error as a reading with state.
func TestProgramStatusOfReadsUnknownWhenUnreadable(t *testing.T) {
	fake := runtime.NewFake()
	if got := ProgramStatusOf(runtime.FullFake{Fake: fake}, "absent"); got != (runtime.ProgramStatus{}) {
		t.Fatalf("missing session: %+v, want Unread", got)
	}
	if got := ProgramStatusOf(fake, "absent"); got != (runtime.ProgramStatus{}) {
		t.Fatalf("runtime without the capability: %+v, want Unread", got)
	}
}
