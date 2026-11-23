package tmux

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// A nudge must never abort the turn it is meant to inform. Provider.Nudge
// waits for the pane to go idle and, when the wait runs out, used to send
// anyway; Codex's submit sequence leads with Escape, which Codex reads as
// "interrupt the running turn" (factory-lifecycle oct14b: turn_aborted one
// second into a claim nudge, the nudge then taken as the next prompt). A
// Codex pane still busy when the wait ends gets no keys at all, and the
// caller is told to try later.
func TestNudgeDefersInsteadOfInterruptingABusyCodexTurn(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.busyCaptures = 1 << 30 // the Working row stays up for the whole test
	tm := NewTmux()
	tm.exec = pane
	tm.cfg.NudgeIdleTimeout = 500 * time.Millisecond
	p := &Provider{tm: tm}

	err := p.Nudge("lab", runtime.TextContent("Run gc hook --claim --drain-ack --json now"))

	if !errors.Is(err, runtime.ErrSessionBusy) {
		t.Fatalf("Nudge = %v, want runtime.ErrSessionBusy", err)
	}
	if got := pane.typedOrPressed(); len(got) != 0 {
		t.Fatalf("the busy Codex pane received %q, want nothing typed or pressed", got)
	}
}

// Only a submit sequence that carries an interrupt key is held back. Enter
// alone (Claude and every family without a table entry) does not abort a
// running turn, so those keep the send-after-timeout behavior.
func TestSubmitSequenceInterruptsOnlyForEscapeLedSequences(t *testing.T) {
	for family, want := range map[string]bool{
		"codex":  true,
		"claude": false,
		"gemini": false,
		"":       false,
	} {
		keys := nudgeSubmitKeySequenceForFamily(family)
		if got := submitSequenceInterrupts(keys); got != want {
			t.Errorf("family %q submit %q: interrupts = %v, want %v", family, strings.Join(keys, " "), got, want)
		}
	}
	if !slices.Equal(nudgeSubmitKeySequenceForFamily("codex"), []string{"Escape", "Enter"}) {
		t.Fatalf("codex submit sequence = %q; revisit which families defer", nudgeSubmitKeySequenceForFamily("codex"))
	}
}
