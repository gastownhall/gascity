package workertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	worker "github.com/gastownhall/gascity/internal/worker"
)

func init() {
	programStatusChecks[RequirementProgramStatusHoldMidTurn] = checkProgramStatusHoldMidTurn
}

// holdWorkerRow is programStatusPhaseRow under mode whose state was entered
// now, so a hold it makes is live, its pane declaring the mid-turn hold
// when declared.
func holdWorkerRow(mode runtime.ProgramStatusMode, st runtime.ProgramState, declared bool) runtime.ProgramPaneRow {
	row := programStatusPhaseRow(mode, st)
	row.Record.Since = time.Now()
	row.HoldDeclared = declared
	return row
}

// holdWorkerSession starts a session handle of profile's family on a fresh
// fake, and returns the handle, the fake and the session's runtime name.
func holdWorkerSession(t *testing.T, profile Profile) (worker.Handle, *runtime.Fake, string, error) {
	t.Helper()
	fake := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(beads.NewMemStore()), runtime.FullFake{Fake: fake},
		sessionpkg.WithCityPath(t.TempDir()),
		sessionpkg.WithStaleKeyDetectionWaiter(func(context.Context, string) error { return nil }))
	handle, err := worker.NewSessionHandle(worker.SessionHandleConfig{Manager: manager, Session: worker.SessionSpec{
		Profile: worker.Profile(profile.ID), Provider: ProfileFamily(profile),
		Template: "probe", Title: "Probe", WorkDir: t.TempDir(),
	}})
	if err != nil {
		return nil, nil, "", fmt.Errorf("NewSessionHandle: %w", err)
	}
	ctx := context.Background()
	city, err := manager.CityDir()
	if err != nil {
		return nil, nil, "", fmt.Errorf("CityDir: %w", err)
	}
	if err := handle.Start(ctx, sessionpkg.Actor{Kind: sessionpkg.ActorOperator, City: city}); err != nil {
		return nil, nil, "", fmt.Errorf("Start: %w", err)
	}
	state, err := handle.State(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("State: %w", err)
	}
	// The fake's idle wait returns at once, so a wait-idle nudge is
	// delivered unless something holds it.
	fake.WaitForIdleErrors = map[string]error{state.SessionName: nil}
	return handle, fake, state.SessionName, nil
}

// holdNudgeOutcome is one nudge's answer: delivered, or held back (queued,
// or refused as busy or pending), or an error that is neither.
func holdNudgeOutcome(r worker.NudgeResult, err error) string {
	switch {
	case err == nil && r.Delivered:
		return "delivered"
	case err == nil:
		return "held:" + string(r.Undelivered)
	case errors.Is(err, runtime.ErrSessionBusy):
		return "held:busy"
	case errors.Is(err, runtime.ErrPendingInteraction):
		return "held:pending"
	default:
		return "error:" + err.Error()
	}
}

// checkProgramStatusHoldMidTurn is WC-PSTAT-009: a session whose pane
// declares the mid-turn hold takes every wait-idle, default and immediate
// nudge exactly as the switch off does, whatever its record says (a record
// may say working with no turn running), under shadow and on alike; and,
// under on, its runtime's fresh hold read reports a live working record as
// a typing hold, the one the starved-queue bypass is vetoed on, and a
// blocked or boundary record as none.
func checkProgramStatusHoldMidTurn(t *testing.T, profile Profile) Result {
	t.Helper()
	return runHoldMidTurnCheck(t, profile, true)
}

// runHoldMidTurnCheck is WC-PSTAT-009's check over panes that declare the
// hold exactly when declared: the check's own panes declare it, and its
// mutation test runs it over panes that do not.
func runHoldMidTurnCheck(t *testing.T, profile Profile, declared bool) Result {
	t.Helper()
	code := RequirementProgramStatusHoldMidTurn
	ctx := context.Background()
	by := sessionpkg.Actor{Kind: sessionpkg.ActorAgent}
	deliveries := []worker.NudgeDelivery{worker.NudgeDeliveryWaitIdle, worker.NudgeDeliveryDefault, worker.NudgeDeliveryImmediate}
	var checked []string
	// outcome is d's answer on a fresh session whose record reads st under
	// mode, and the runtime's typing hold on it just before.
	outcome := func(mode runtime.ProgramStatusMode, st runtime.ProgramState, d worker.NudgeDelivery) (string, bool, error) {
		h, fake, name, err := holdWorkerSession(t, profile)
		if err != nil {
			return "", false, err
		}
		fake.SetProgramPane(name, holdWorkerRow(mode, st, declared))
		_, held := runtime.ProgramRecordTypingHold(runtime.FullFake{Fake: fake}, name, time.Now())
		return holdNudgeOutcome(h.Nudge(ctx, by, worker.NudgeRequest{Text: "hello", Delivery: d})), held, nil
	}
	for _, d := range deliveries {
		off, _, err := outcome(runtime.ProgramStatusOff, runtime.ProgramWorking, d)
		if err != nil {
			return Fail(profile.ID, code, err.Error())
		}
		for _, c := range []struct {
			mode runtime.ProgramStatusMode
			st   runtime.ProgramState
			held bool
		}{
			{runtime.ProgramStatusShadow, runtime.ProgramWorking, false},
			{runtime.ProgramStatusOn, runtime.ProgramWorking, true},
			{runtime.ProgramStatusOn, runtime.ProgramBlocked, false},
			{runtime.ProgramStatusOn, runtime.ProgramIdle, false},
		} {
			got, held, err := outcome(c.mode, c.st, d)
			if err != nil {
				return Fail(profile.ID, code, err.Error())
			}
			checked = append(checked, fmt.Sprintf("%s:%s:%s off=%s got=%s held=%v", c.mode, c.st, d, off, got, held))
			if got != off {
				return Fail(profile.ID, code, fmt.Sprintf("%s, %s record, %s nudge: %s, want %s as off: the hold types nothing differently", c.mode, c.st, d, got, off))
			}
			if held != c.held {
				return Fail(profile.ID, code, fmt.Sprintf("%s, %s record: typing hold %v, want %v", c.mode, c.st, held, c.held))
			}
		}
	}
	return Pass(profile.ID, code, "every delivery is the one off makes, and the fresh hold read holds typing on a live working record only").
		WithEvidence(map[string]string{"provider": profile.Provider, "steps": strings.Join(checked, ",")})
}

// TestProgramStatusHoldMidTurnCheckPassesForEveryProfile runs WC-PSTAT-009's
// check against every profile as if it declared the hold: the row's
// behavior is the worker's over a held pane, whatever the family.
func TestProgramStatusHoldMidTurnCheckPassesForEveryProfile(t *testing.T) {
	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range profiles {
		t.Run(string(profile.ID), func(t *testing.T) {
			profile.ProgramStatus.ProgramStatusDeclaration = ProgramStatusDeclaration{Declared: true, HoldMidTurn: true}
			if r := checkProgramStatusHoldMidTurn(t, profile); r.Status != ResultPass {
				t.Fatalf("WC-PSTAT-009: %s", r.Detail)
			}
		})
	}
}

// Kills a WC-PSTAT-009 check that passes a runtime with no typing hold: run
// over panes that do not declare the hold, a working record holds nothing,
// and the check fails.
func TestProgramStatusHoldMidTurnCheckFailsWithoutTheHold(t *testing.T) {
	profile := Phase1Profiles()[0]
	r := runHoldMidTurnCheck(t, profile, false)
	if r.Status != ResultFail || !strings.Contains(r.Detail, "typing hold false, want true") {
		t.Fatalf("WC-PSTAT-009 over undeclared panes: %+v, want a failure naming the missing typing hold", r)
	}
}
