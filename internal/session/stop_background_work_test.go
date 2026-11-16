package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Codex keeps running a stopped turn's commands in background terminals. An
// explicit stop has the runtime stop them, once the interrupt boundary is
// reached.
func TestStopTurnStopsCodexBackgroundWorkAfterTheBoundary(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "codex")

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}
	boundary := callIndex(sp.Calls, "WaitForInterruptBoundary", info.SessionName)
	stop := callIndex(sp.Calls, "StopBackgroundWork", info.SessionName)
	if boundary < 0 || stop < 0 || stop < boundary {
		t.Fatalf("calls = %v, want WaitForInterruptBoundary then StopBackgroundWork", callMethods(sp.Calls))
	}
	if got := sp.CountCalls("StopBackgroundWork", info.SessionName); got != 1 {
		t.Fatalf("StopBackgroundWork calls = %d, want 1", got)
	}
}

// A replacement message must not stop background work: Codex's /stop closes
// every background terminal of the session, including an earlier turn's dev
// server.
func TestSubmitInterruptNowLeavesCodexBackgroundWorkRunning(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "codex")

	if _, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow); err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if callIndex(sp.Calls, "WaitForInterruptBoundary", info.SessionName) < 0 {
		t.Fatalf("calls = %v, want the interrupt boundary wait", callMethods(sp.Calls))
	}
	if got := sp.CountCalls("StopBackgroundWork", info.SessionName); got != 0 {
		t.Fatalf("calls = %v, want no StopBackgroundWork for an interrupt_now submit", callMethods(sp.Calls))
	}
}

// Claude keeps running a command the model ran in the background after its
// turn is stopped, and the command's completion notice starts another turn.
// An explicit stop has the runtime stop it, after the prompt Claude may have
// restored into its input box is cleared: the runtime stops the shells from
// an empty input box.
func TestStopTurnStopsClaudeBackgroundWorkAfterClearingItsInput(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "claude")

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}
	cleared := callIndex(sp.Calls, "ClearInput", info.SessionName)
	stop := callIndex(sp.Calls, "StopBackgroundWork", info.SessionName)
	if cleared < 0 || stop < 0 || stop < cleared {
		t.Fatalf("calls = %v, want ClearInput then StopBackgroundWork", callMethods(sp.Calls))
	}
	if got := sp.CountCalls("StopBackgroundWork", info.SessionName); got != 1 {
		t.Fatalf("StopBackgroundWork calls = %d, want 1", got)
	}
}

// A replacement message leaves Claude's background shells running, as it
// does Codex's background terminals.
func TestSubmitInterruptNowLeavesClaudeBackgroundWorkRunning(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "claude")

	if _, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow); err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if got := sp.CountCalls("StopBackgroundWork", info.SessionName); got != 0 {
		t.Fatalf("calls = %v, want no StopBackgroundWork for an interrupt_now submit", callMethods(sp.Calls))
	}
}

// The turn is already stopped when its background work will not stop, so
// StopTurn succeeds and logs it.
func TestStopTurnLogsCodexBackgroundWorkThatWillNotStop(t *testing.T) {
	logs := captureLog(t)
	mgr, sp, info := newStopTestSession(t, "codex")
	sp.BackgroundWorkStopErrors = map[string]error{info.SessionName: errors.New("pane unreadable")}

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn = %v, want nil: the turn was stopped", err)
	}
	if !strings.Contains(logs.String(), "pane unreadable") {
		t.Fatalf("log = %q, want the StopBackgroundWork failure logged", logs.String())
	}
}
