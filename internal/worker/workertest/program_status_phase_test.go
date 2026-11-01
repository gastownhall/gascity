package workertest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	worker "github.com/gastownhall/gascity/internal/worker"
	workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"
)

// programStatusPhaseRow is an armed agent-pane row of a capture that read st
// under mode, its reader attached: a valid reading.
func programStatusPhaseRow(mode runtime.ProgramStatusMode, st runtime.ProgramState) runtime.ProgramPaneRow {
	kind := runtime.ProgramBlockUnspecified
	if st == runtime.ProgramBlocked {
		kind = runtime.ProgramBlockPermission
	}
	return runtime.ProgramPaneRow{
		Mode: mode, CaptureGate: true, PaneID: "%1",
		PanePipe: true, Tap: "wc.%1", TapLiveness: runtime.ProgramTapAlive,
		RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{
			Nonce: "wc.%1", State: st, Kind: kind,
			Seq: 2, Progress: -1, TurnStarts: 1, Reported: true,
		},
	}
}

// checkProgramStatusPhase is WC-PSTAT-002: a started session of profile's
// family reads worker phase from a valid record under on (working Busy,
// blocked Blocked, idle/done/error Ready) and keeps the phase it reads with no
// record under shadow, the reading carried on State either way. A profile
// with no canonical worker identity (omp, grok) is unsupported.
func checkProgramStatusPhase(t *testing.T, profile Profile) Result {
	t.Helper()
	code := RequirementProgramStatusPhase
	// The check runs a worker session of the profile, which only a profile
	// with a canonical worker identity has.
	if _, ok := workerbuiltin.CanonicalProfileIdentity(profile.Provider); !ok {
		return Unsupported(profile.ID, code, "profile has no canonical worker profile identity, so no worker session can run it").
			WithEvidence(map[string]string{"provider": profile.Provider})
	}
	fake := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(beads.NewMemStore(), runtime.FullFake{Fake: fake},
		sessionpkg.WithCityPath(t.TempDir()),
		sessionpkg.WithStaleKeyDetectionWaiter(func(context.Context, string) error { return nil }))
	handle, err := worker.NewSessionHandle(worker.SessionHandleConfig{Manager: manager, Session: worker.SessionSpec{
		Profile: worker.Profile(profile.ID), Template: "probe", Title: "Probe", WorkDir: t.TempDir(),
	}})
	if err != nil {
		return Fail(profile.ID, code, fmt.Sprintf("NewSessionHandle: %v", err))
	}
	ctx := context.Background()
	city, err := manager.CityDir()
	if err != nil {
		return Fail(profile.ID, code, fmt.Sprintf("CityDir: %v", err))
	}
	if err := handle.Start(ctx, sessionpkg.Actor{Kind: sessionpkg.ActorOperator, City: city}); err != nil {
		return Fail(profile.ID, code, fmt.Sprintf("Start: %v", err))
	}
	base, err := handle.State(ctx)
	if err != nil {
		return Fail(profile.ID, code, fmt.Sprintf("State with no record: %v", err))
	}

	type step struct {
		mode runtime.ProgramStatusMode
		st   runtime.ProgramState
		want worker.Phase
	}
	steps := []step{
		{runtime.ProgramStatusOn, runtime.ProgramWorking, worker.PhaseBusy},
		{runtime.ProgramStatusOn, runtime.ProgramBlocked, worker.PhaseBlocked},
		{runtime.ProgramStatusOn, runtime.ProgramIdle, worker.PhaseReady},
		{runtime.ProgramStatusOn, runtime.ProgramDone, worker.PhaseReady},
		{runtime.ProgramStatusOn, runtime.ProgramError, worker.PhaseReady},
	}
	for _, st := range []runtime.ProgramState{runtime.ProgramWorking, runtime.ProgramBlocked, runtime.ProgramIdle, runtime.ProgramDone, runtime.ProgramError} {
		steps = append(steps, step{runtime.ProgramStatusShadow, st, base.Phase})
	}
	var checked []string
	for _, s := range steps {
		fake.SetProgramPane(base.SessionName, programStatusPhaseRow(s.mode, s.st))
		got, err := handle.State(ctx)
		if err != nil {
			return Fail(profile.ID, code, fmt.Sprintf("%s %s: State: %v", s.mode, s.st, err))
		}
		if got.Phase != s.want {
			return Fail(profile.ID, code, fmt.Sprintf("%s %s: phase %s, want %s", s.mode, s.st, got.Phase, s.want))
		}
		if !got.Program.Valid() || got.Program.State != s.st || got.Program.Mode != s.mode {
			return Fail(profile.ID, code, fmt.Sprintf("%s %s: State.Program %+v does not carry the valid reading", s.mode, s.st, got.Program))
		}
		checked = append(checked, fmt.Sprintf("%s:%s=%s", s.mode, s.st, got.Phase))
	}
	return Pass(profile.ID, code, "worker phase follows a valid record under on and is unchanged under shadow").
		WithEvidence(map[string]string{"provider": profile.Provider, "baseline_phase": string(base.Phase), "steps": strings.Join(checked, ",")})
}

// TestProgramStatusPhaseCheckPassesForEveryProfile runs WC-PSTAT-002's check
// against every profile as if it declared program status, so the row's
// behavior is proved before any family's declaration lands (ga-f4ovjm.18,
// ga-f4ovjm.38), when TestProgramStatusConformance starts running it.
func TestProgramStatusPhaseCheckPassesForEveryProfile(t *testing.T) {
	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range profiles {
		t.Run(string(profile.ID), func(t *testing.T) {
			profile.ProgramStatus.ProgramStatusDeclaration = ProgramStatusDeclaration{Declared: true}
			if r := checkProgramStatusPhase(t, profile); r.Status != ResultPass {
				t.Fatalf("WC-PSTAT-002: %s", r.Detail)
			}
		})
	}
}

// Kills a WC-PSTAT-002 check that opens a session for a profile with no
// canonical worker identity (a family that declares program status without
// phase-1 transcript fixtures, such as omp or grok): no worker session can
// run it, so the row is unsupported for it, never a failure.
func TestProgramStatusPhaseCheckUnsupportedWithoutWorkerIdentity(t *testing.T) {
	profile := Profile{
		ID:       "no-identity/tmux-cli",
		Provider: "no-identity/tmux-cli",
		ProgramStatus: ProgramStatusExpectation{
			ProgramStatusDeclaration: ProgramStatusDeclaration{Declared: true, App: "agent"},
		},
	}
	r := checkProgramStatusPhase(t, profile)
	if r.Status != ResultUnsupported || !strings.Contains(r.Detail, "canonical worker profile identity") {
		t.Fatalf("WC-PSTAT-002 without a worker identity: %+v, want unsupported naming the missing identity", r)
	}
	if r.Evidence["provider"] != profile.Provider {
		t.Fatalf("evidence %v does not name provider %q", r.Evidence, profile.Provider)
	}
}
