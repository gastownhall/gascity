//go:build integration

package acp

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestACPProtocolExitDuringPromptPublishesExited pins that an agent dying
// mid-turn reaches session-event subscribers as exited, attributed to the
// session, with no agent_idle for the turn the death failed.
func TestACPProtocolExitDuringPromptPublishesExited(t *testing.T) {
	s := startProtocolFake(t, "--exit-during-prompt")
	ch := subscribe(t, s.p)
	expectResync(t, ch)
	ref := agentRef(t, s.p, s.name)

	s.nudge(t, "die")
	expectEvent(t, ch, runtime.SessionEventAgentStateChanged, s.name)
	if ev := expectEvent(t, ch, runtime.SessionEventExited, s.name); ev.Ref != ref {
		t.Fatalf("exited Ref = %q, want %q", ev.Ref, ref)
	}
	if s.p.IsRunning(s.name) {
		t.Fatal("IsRunning = true after the exited event")
	}
}
