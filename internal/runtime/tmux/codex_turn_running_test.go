package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// codexStaleWorkingFixture is the bottom of a live codex-cli 0.162.1 pane
// (BB live E2E approvals.repeated, session ble-31, a 120-row capture at the
// queued submit): the approved command ran and the turn ended, and the live
// composer at the bottom is idle, but the scrollback still holds the bottom
// pane Codex drew before the approval overlay, "• Working (5s • esc to
// interrupt)" and its composer included. Every queued delivery read that row
// as a running turn: 70 attempts, about every 16s for 19 minutes, each
// "nudge deferred: session is busy", while the rollout said idle.
const codexStaleWorkingFixture = "idle-after-approve-stale-working-scrollback-0.162.1.txt"

// codexStaticPane is a tmux executor for a Codex pane whose capture is
// always screen.
type codexStaticPane struct{ screen string }

func (f *codexStaticPane) execute(args []string) (string, error) {
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "show-environment":
		if slices.Contains(args, "GC_PROVIDER") {
			return "GC_PROVIDER=codex", nil
		}
		return "", errors.New("unknown variable")
	case "display-message":
		return "0", nil
	case "capture-pane":
		return f.screen, nil
	}
	return "", nil
}

func (f *codexStaticPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

// An idle Codex composer is not a running turn because an earlier turn's
// Working row is still in the scrollback: the Escape-led submit is neither
// refused nor held waiting for the composer.
func TestIdleCodexComposerWithAStaleWorkingRowIsNotBusy(t *testing.T) {
	tm := NewTmux()
	tm.exec = &codexStaticPane{screen: readCodexApprovalFixture(t, codexStaleWorkingFixture)}
	tm.cfg.NudgeReadyTimeout = 300 * time.Millisecond

	if err := tm.refuseInterruptingSubmitWhileBusy("lab"); err != nil {
		t.Errorf("refuseInterruptingSubmitWhileBusy = %v, want nil: the live composer is idle", err)
	}
	if err := tm.awaitIdleComposerBeforeTyping("lab", fakeAgentPane("lab"), []string{"Escape", "Enter"}); err != nil {
		t.Errorf("awaitIdleComposerBeforeTyping = %v, want nil: the live composer is idle", err)
	}
}

// The submit itself, end to end, below that scrollback: typed, submitted
// once, and recorded by Codex.
func TestCodexSubmitIsNotHeldByAStaleWorkingRowInTheScrollback(t *testing.T) {
	rows := strings.Split(readCodexApprovalFixture(t, codexStaleWorkingFixture), "\n")
	live := -1
	for i, row := range rows {
		if matchesPromptPrefix(row, codexPromptPrefix) {
			live = i
		}
	}
	pane, err := runCodexTurnNudge(t, "codex-submit-stale-working", func(p *codexTurnPane) {
		p.scrollback = rows[:live] // the fake draws its own live screen below
	})
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil", err)
	}
	recorded, aborted, events := pane.snapshot()
	if aborted != 0 || !slices.Equal(recorded, []string{codexTurnMarker}) {
		t.Fatalf("Codex recorded %q (aborted %d; events %q), want the submitted message once", recorded, aborted, events)
	}
}

// The live turn reading the claim backstops ask for (runtime.TurnRunningProvider)
// is the nudge guard's own: an idle Codex composer under an earlier turn's
// Working row runs no turn, though SnapshotIdle, which counts an indicator
// anywhere in the capture, reads that pane busy; a Working row right above
// the live composer is a running turn.
func TestTurnRunningReadsTheLiveComposer(t *testing.T) {
	tm := NewTmux()
	p := &Provider{tm: tm}
	tm.exec = &codexStaticPane{screen: readCodexApprovalFixture(t, codexStaleWorkingFixture)}
	if running, err := p.TurnRunning("lab"); err != nil || running {
		t.Errorf("TurnRunning under a stale Working row = %v, %v; want false, nil: the live composer is idle", running, err)
	}
	if idle, err := tm.SnapshotIdle("lab"); err != nil || idle {
		t.Errorf("SnapshotIdle under a stale Working row = %v, %v; want the capture scan's busy reading, false, nil", idle, err)
	}
	tm.exec = &codexStaticPane{screen: strings.Join(codexWorkingWithBackgroundTerminalPane, "\n")}
	if running, err := p.TurnRunning("lab"); err != nil || !running {
		t.Errorf("TurnRunning with a Working row above the live composer = %v, %v; want true, nil", running, err)
	}
}

func TestPaneShowsRunningTurn(t *testing.T) {
	split := func(s string) []string { return strings.Split(s, "\n") }
	idle := split(readCodexApprovalFixture(t, "idle-after-deny-0.162.1-80x24.txt"))
	for _, tc := range []struct {
		name   string
		family string
		lines  []string
		want   bool
	}{
		{"codex working row above the composer", "codex", codexWorkingWithBackgroundTerminalPane, true},
		{"codex working row, blank row, composer", "codex", []string{"• Working (1s • esc to interrupt)", "", "› Ask Codex to do anything", "", "  gpt-6.1-sol low · ~/w"}, true},
		{"codex working row with a queued follow-up under it", "codex", []string{"• Working (9s • esc to interrupt)", "  ↳ and then run the tests", "    ⌥ + ↑ edit", "", "› Ask Codex to do anything"}, true},
		{"codex idle composer", "codex", idle, false},
		{"codex idle composer, stale working row in the scrollback", "codex", split(readCodexApprovalFixture(t, codexStaleWorkingFixture)), false},
		{"codex idle composer under a Claude-spinner-shaped tool row", "codex", append([]string{"• Ran ./fetch-fixtures.sh", "  └ fetched 14 files (2s · 3.1 MB)", ""}, idle...), false},
		{"codex idle composer right under a Claude spinner row", "codex", []string{"✻ Thinking… (12s · ↓ 1.2k tokens)", "", "› Ask Codex to do anything"}, false},
		{"codex with no composer on screen", "codex", []string{"• Working (3s • esc to interrupt)"}, true},
		{"claude keeps every indicator", "claude", []string{"✻ Thinking… (12s · ↓ 1.2k tokens)", "", "❯ "}, true},
	} {
		if got := paneShowsRunningTurn(tc.lines, tc.family); got != tc.want {
			t.Errorf("%s: paneShowsRunningTurn = %v, want %v", tc.name, got, tc.want)
		}
	}
}
