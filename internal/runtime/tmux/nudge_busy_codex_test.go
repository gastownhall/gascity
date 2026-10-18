package tmux

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestFencedDefaultNudgeTypesIntoABusyCodexAfterTheIdleWait pins a known gap
// the conversation lane inherits (SESSION-RUNTIME-025): a fenced default
// (boundary) nudge into a Codex pane that stays mid-turn waits only
// NudgeIdleTimeout, then types anyway, ending with Escape, which Codex reads
// as cancel. A lane that probed the pane idle and lost the race to a turn
// start therefore cancels that turn instead of waiting for its boundary.
// When the default nudge stops typing into a busy pane, this test must be
// inverted to require no input at all.
func TestFencedDefaultNudgeTypesIntoABusyCodexAfterTheIdleWait(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.env["GC_PROVIDER"] = "codex"
	m.screen = func(*fenceModel) string { return "• Working (1s • esc to interrupt)\n› " }
	p := fenceModelProvider(m)

	start := time.Now()
	err := p.NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent("after this turn"), runtime.FencedNudge{Fence: testFence})

	_, events, _ := m.snapshot()
	if !slices.Contains(events, "text:after this turn") || !slices.Contains(events, "key:Escape") {
		t.Fatalf("events = %q (err %v), want the text typed and Escape sent into the busy pane after the idle wait", events, err)
	}
	if elapsed := time.Since(start); elapsed < p.cfg.NudgeIdleTimeout {
		t.Fatalf("nudge typed after %v, before the %v idle wait ran out: it took no boundary wait", elapsed, p.cfg.NudgeIdleTimeout)
	}
}
