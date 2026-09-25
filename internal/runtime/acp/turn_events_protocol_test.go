//go:build integration

package acp

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// turnProtocolSessionID is the GC_SESSION_ID the turn-event protocol cases
// start the fake with.
const turnProtocolSessionID = "gc-turn-proto"

// startTurnProtocolFake starts fakeacp with GC_SESSION_ID set and opens a
// turn-event stream before any turn runs.
func startTurnProtocolFake(t *testing.T, args ...string) (*protocolSession, <-chan runtime.TurnEvent) {
	t.Helper()
	s := startProtocolFakeEnv(t, map[string]string{"GC_SESSION_ID": turnProtocolSessionID}, args...)
	return s, subscribeTurns(t, s.p)
}

func TestACPProtocolTurnEventsEndTurnWithUsage(t *testing.T) {
	s, ch := startTurnProtocolFake(t, "--usage", "12,34")
	s.nudge(t, "hello")
	completed := expectTurnPair(t, ch, s.name, turnProtocolSessionID)
	if completed.Status != runtime.TurnStatusCompleted || completed.StopReason != "end_turn" {
		t.Fatalf("completed = %+v, want completed/end_turn", completed)
	}
	want := runtime.TurnUsage{InputTokens: 12, OutputTokens: 34, TotalTokens: 46}
	if completed.Usage == nil || *completed.Usage != want {
		t.Fatalf("usage = %+v, want %+v", completed.Usage, want)
	}
}

func TestACPProtocolTurnEventsInterruptCancels(t *testing.T) {
	s, ch := startTurnProtocolFake(t, "--sigint", "cancel", "--turn-delay", "1h")
	s.nudge(t, "long turn")
	// Interrupt only after the fake has logged the prompt, so the cancel
	// cannot race the prompt's arrival.
	waitForMethod(t, s, "session/prompt")
	if err := s.p.Interrupt(s.name); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	completed := expectTurnPair(t, ch, s.name, turnProtocolSessionID)
	if completed.Status != runtime.TurnStatusCancelled || completed.StopReason != acpWireCancelled || completed.Error != "" {
		t.Fatalf("completed = %+v, want cancelled", completed)
	}
}

func TestACPProtocolTurnEventsPromptErrorFails(t *testing.T) {
	s, ch := startTurnProtocolFake(t, "--prompt-error", "model unavailable")
	s.nudge(t, "hello")
	completed := expectTurnPair(t, ch, s.name, turnProtocolSessionID)
	if completed.Status != runtime.TurnStatusFailed || completed.Error != "model unavailable" || completed.StopReason != "" {
		t.Fatalf("completed = %+v, want failed with the agent's error", completed)
	}
}

func TestACPProtocolTurnEventsExitDuringPromptFails(t *testing.T) {
	s, ch := startTurnProtocolFake(t, "--exit-during-prompt")
	s.nudge(t, "die")
	completed := expectTurnPair(t, ch, s.name, turnProtocolSessionID)
	if completed.Status != runtime.TurnStatusFailed || completed.Error != turnFailureConnClosed {
		t.Fatalf("completed = %+v, want failed with %q", completed, turnFailureConnClosed)
	}
}
