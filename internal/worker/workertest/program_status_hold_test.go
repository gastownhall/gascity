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

// checkProgramStatusHoldMidTurn is WC-PSTAT-009: under on, a session whose
// pane declares the mid-turn hold takes no wait-idle, default or immediate
// nudge while its fresh record says a turn is running, and takes one once
// the record reaches an input boundary; under off and shadow, the same
// record holds nothing back, so every nudge the hold holds is one the
// session would otherwise have been delivered.
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
	nudge := func(h worker.Handle, d worker.NudgeDelivery) string {
		return holdNudgeOutcome(h.Nudge(ctx, by, worker.NudgeRequest{Text: "hello", Delivery: d}))
	}
	// outcome is d's answer on a fresh held session whose record reads st
	// under mode.
	outcome := func(mode runtime.ProgramStatusMode, st runtime.ProgramState, d worker.NudgeDelivery) (string, worker.Handle, *runtime.Fake, string, error) {
		h, fake, name, err := holdWorkerSession(t, profile)
		if err != nil {
			return "", nil, nil, "", err
		}
		fake.SetProgramPane(name, holdWorkerRow(mode, st, declared))
		return nudge(h, d), h, fake, name, nil
	}
	delivered := 0
	for _, d := range deliveries {
		// The switch off is the baseline: a delivery it does not make (a
		// family whose wait-idle nudge the session leaves to its caller)
		// is one the hold has nothing to hold.
		off, _, _, _, err := outcome(runtime.ProgramStatusOff, runtime.ProgramWorking, d)
		if err != nil {
			return Fail(profile.ID, code, err.Error())
		}
		shadow, _, _, _, err := outcome(runtime.ProgramStatusShadow, runtime.ProgramWorking, d)
		if err != nil {
			return Fail(profile.ID, code, err.Error())
		}
		if shadow != off {
			return Fail(profile.ID, code, fmt.Sprintf("shadow, working record, %s nudge: %s, want %s as off", d, shadow, off))
		}
		{
			st := runtime.ProgramWorking
			got, h, fake, name, err := outcome(runtime.ProgramStatusOn, st, d)
			if err != nil {
				return Fail(profile.ID, code, err.Error())
			}
			checked = append(checked, fmt.Sprintf("%s:%s off=%s on=%s", d, st, off, got))
			if off != "delivered" {
				if got != off {
					return Fail(profile.ID, code, fmt.Sprintf("on, %s record, %s nudge: %s, want %s as off", st, d, got, off))
				}
				continue
			}
			// A blocked record is no typing hold: a dialog the record
			// reports is the pending-interaction slice's, so typing is
			// decided as off decides it.
			if blocked, _, _, _, err := outcome(runtime.ProgramStatusOn, runtime.ProgramBlocked, d); err != nil {
				return Fail(profile.ID, code, err.Error())
			} else if blocked != off {
				return Fail(profile.ID, code, fmt.Sprintf("on, blocked record, %s nudge: %s, want %s as off", d, blocked, off))
			}
			delivered++
			if !strings.HasPrefix(got, "held:") {
				return Fail(profile.ID, code, fmt.Sprintf("on, %s record, %s nudge: %s, want it held back", st, d, got))
			}
			fake.SetProgramPane(name, holdWorkerRow(runtime.ProgramStatusOn, runtime.ProgramIdle, declared))
			if after := nudge(h, d); after != "delivered" {
				return Fail(profile.ID, code, fmt.Sprintf("on, after the %s record turned idle, %s nudge: %s, want delivered", st, d, after))
			}
		}
	}
	if delivered == 0 {
		return Fail(profile.ID, code, "the switch off delivered no nudge, so the hold held nothing")
	}
	return Pass(profile.ID, code, "a declared mid-turn hold holds every delivery off makes under a live working record, and shadow, and a blocked record, decide as off").
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

// Kills a WC-PSTAT-009 check that passes a session that types into a held
// pane: run over panes that do not declare the hold, the working record
// holds nothing back, and the check fails.
func TestProgramStatusHoldMidTurnCheckFailsWithoutTheHold(t *testing.T) {
	profile := Phase1Profiles()[0]
	r := runHoldMidTurnCheck(t, profile, false)
	if r.Status != ResultFail || !strings.Contains(r.Detail, "want it held back") {
		t.Fatalf("WC-PSTAT-009 over undeclared panes: %+v, want a failure naming the delivered nudge", r)
	}
}
