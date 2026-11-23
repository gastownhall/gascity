package tmux

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestFencedDefaultNudgeDefersABusyCodexAfterTheIdleWait pins the close of a
// gap the conversation lane inherited (SESSION-RUNTIME-025): a fenced default
// (boundary) nudge into a Codex pane that stays mid-turn waits
// NudgeIdleTimeout, then types nothing, because its submit ends with Escape,
// which Codex reads as cancel, and defers with runtime.ErrSessionBusy. A lane
// that probed the pane idle and lost the race to a turn start therefore no
// longer cancels that turn. With no input sent, the call's duration shows the
// idle wait.
func TestFencedDefaultNudgeDefersABusyCodexAfterTheIdleWait(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.env["GC_PROVIDER"] = "codex"
	m.screen = func(*fenceModel) string { return "• Working (1s • esc to interrupt)\n› " }
	p := fenceModelProvider(m)

	start := time.Now()
	err := p.NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent("after this turn"), runtime.FencedNudge{Fence: testFence})
	waited := time.Since(start)

	if !errors.Is(err, runtime.ErrSessionBusy) {
		t.Fatalf("NudgeFenced = %v, want runtime.ErrSessionBusy", err)
	}
	if _, events, _ := m.snapshot(); len(events) != 0 {
		t.Fatalf("events = %q, want no input sent into the busy pane", events)
	}
	if waited < p.cfg.NudgeIdleTimeout {
		t.Fatalf("deferred after %v, before the %v idle wait ran out: it took no boundary wait", waited, p.cfg.NudgeIdleTimeout)
	}
}
