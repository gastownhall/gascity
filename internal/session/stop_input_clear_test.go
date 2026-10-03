package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func newStopTestSession(t *testing.T, provider string) (*Manager, *runtime.Fake, Info) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: provider, WorkDir: t.TempDir(), Provider: provider, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return mgr, sp, info
}

func callIndex(calls []runtime.Call, method, name string) int {
	for i, call := range calls {
		if call.Method == method && call.Name == name {
			return i
		}
	}
	return -1
}

// callMethods lists the calls as Method(Message), for failure messages.
func callMethods(calls []runtime.Call) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		if call.Message != "" {
			out = append(out, call.Method+"("+call.Message+")")
			continue
		}
		out = append(out, call.Method)
	}
	return out
}

// A stop that lands before Claude's first response chunk makes Claude Code put
// the interrupted prompt back into the input box, sometimes after the stop has
// settled. StopTurn must leave the input box empty, or the next submit is
// appended to the old prompt and both go out as one message.
func TestStopTurnClearsClaudeInputAfterIdle(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "claude")

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}

	calls := sp.SnapshotCalls()
	interrupt := callIndex(calls, "Interrupt", info.SessionName)
	wait := callIndex(calls, "WaitForIdle", info.SessionName)
	clearAt := callIndex(calls, "ClearInput", info.SessionName)
	if interrupt < 0 || wait <= interrupt || clearAt <= wait {
		t.Fatalf("calls = %v, want Interrupt -> WaitForIdle -> ClearInput", callMethods(calls))
	}
	window, err := time.ParseDuration(calls[clearAt].Value)
	if err != nil || window <= 0 {
		t.Fatalf("ClearInput restore window = %q, want a positive window for Claude to restore the prompt", calls[clearAt].Value)
	}
	if n := sp.CountCalls("SendKeys", info.SessionName); n != 0 {
		t.Fatalf("calls = %v, want no raw SendKeys from StopTurn", callMethods(calls))
	}
}

// A restored draft that cannot be cleared is reported, not swallowed: the
// caller's next message would be merged into it.
func TestStopTurnReportsClaudeInputThatWillNotClear(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "claude")
	sp.ClearInputErrors[info.SessionName] = errors.New("input box still holds \"d1.0-A: write\"")

	err := mgr.StopTurn(info.ID)
	if err == nil || !strings.Contains(err.Error(), "d1.0-A: write") {
		t.Fatalf("StopTurn error = %v, want the ClearInput failure", err)
	}
}

// Runtimes without a verified clear keep the old StopTurn behavior.
func TestStopTurnToleratesRuntimeWithoutInputClear(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "claude")
	sp.ClearInputErrors[info.SessionName] = runtime.ErrInteractionUnsupported

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}
}

// Only Claude restores an interrupted prompt into its input box; other
// providers' stops are unchanged.
func TestStopTurnDoesNotClearCodexInput(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "codex")

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}
	if n := sp.CountCalls("ClearInput", info.SessionName); n != 0 {
		t.Fatalf("calls = %v, want no ClearInput for codex", callMethods(sp.SnapshotCalls()))
	}
}

// interrupt_now clears the interrupted prompt with the same verified clear
// before the replacement message goes in.
func TestSubmitInterruptNowClearsWholeClaudeInput(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "claude")

	if _, err := mgr.Submit(context.Background(), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow); err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}

	calls := sp.SnapshotCalls()
	wait := callIndex(calls, "WaitForIdle", info.SessionName)
	clearAt := callIndex(calls, "ClearInput", info.SessionName)
	nudge := callIndex(calls, "NudgeNow", info.SessionName)
	if wait < 0 || clearAt <= wait || nudge <= clearAt {
		t.Fatalf("calls = %v, want WaitForIdle -> ClearInput -> NudgeNow", callMethods(calls))
	}
	for _, call := range calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "C-u" {
			t.Fatalf("calls = %v, want the verified ClearInput instead of a single Ctrl-U", callMethods(calls))
		}
	}
}
