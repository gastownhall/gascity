package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
)

// ga-8n8fja: Claude Code draws the hint "Press up to edit queued messages"
// with the input cursor on its first character. That cell is reverse video and
// the rest of the hint is dim. tmux's capture-pane -e serializes the cell as
// "\x1b[7mP" and the dim run after it as "\x1b[0;2m..." (checked on a private
// tmux 3.4 socket), the way it emits an idle composer's cursor ("❯ \x1b[7m ",
// captured from a live Claude pane). Dropping only the dim run keeps the "P",
// so the input box reads as holding a one-letter draft; a soak's ops failed
// with exactly that text: `input box still holds "P" after 2 Ctrl-U Ctrl-K`.

// claudeQueuedHintUnderCursor is Claude's input box row while it holds a
// message queued behind a running turn, the cursor drawn on the hint.
const claudeQueuedHintUnderCursor = "❯ \x1b[7mP\x1b[0;2mress up to edit queued messages"

// claudeLongTurn and claudeLongTurnFooter frame a Claude pane minutes into a
// turn, as the soak's review sessions were.
var (
	// claudeInputRule is the full-width rule Claude draws above and below its
	// input box.
	claudeInputRule      = strings.Repeat("─", 80)
	claudeLongTurn       = []string{"✻ Working… (16m 1s · ↓ 88.8k tokens · deep in thought with max effort)", "  ⎿  Tip: Use /btw to ask a quick side question", ""}
	claudeLongTurnFooter = []string{claudeInputRule, "  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt"}
)

// queuedBehindTurnExecutor answers as a Claude pane running a turn: its
// capture is before until the first submit Enter, then after.
type queuedBehindTurnExecutor struct {
	*agentPaneExecutor
	before, after []string
}

func (e *queuedBehindTurnExecutor) execute(args []string) (string, error) {
	if !slices.Contains(args, "capture-pane") {
		return e.agentPaneExecutor.execute(args)
	}
	e.calls = append(e.calls, slices.Clone(args))
	lines := e.before
	if e.submitEnters() > 0 {
		lines = e.after
	}
	out := strings.Join(lines, "\n")
	if !slices.Contains(args, "-e") {
		out = sgrSequence.ReplaceAllString(out, "")
	}
	return out, nil
}

func (e *queuedBehindTurnExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// clearPresses counts the Ctrl-U Ctrl-K presses the pane received.
func (e *queuedBehindTurnExecutor) clearPresses() int {
	n := 0
	for _, k := range e.sentKeys() {
		if k == "C-k" {
			n++
		}
	}
	return n
}

// TestNudgeSessionClaudeQueuedBehindTurnUnderCursor: a nudge typed into a
// Claude turn that was running before it arrived, and stayed running, is
// queued by Claude, which then shows its hint with the cursor on it. That is
// the Enter reaching the pane, not a draft left in the input box: the nudge
// must not report the submit unconfirmed, which makes every retrying caller
// type the same message into the same turn again.
func TestNudgeSessionClaudeQueuedBehindTurnUnderCursor(t *testing.T) {
	const session, message = "busy-claude-queued-cursor", "blocker gc-1 closed — routed dependent gc-2 may be ready to claim"
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	synctest.Test(t, func(t *testing.T) {
		ex := &queuedBehindTurnExecutor{
			agentPaneExecutor: &agentPaneExecutor{session: session, provider: "claude", attached: "0"},
			before:            slices.Concat(claudeLongTurn, []string{claudeInputRule, "❯ \x1b[7m "}, claudeLongTurnFooter),
			after:             slices.Concat(claudeLongTurn, []string{"❯ " + message, claudeInputRule, claudeQueuedHintUnderCursor}, claudeLongTurnFooter),
		}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}
		err := tm.NudgeSession(session, message)
		if !errors.Is(err, ErrNudgeSubmitDeliveredUnobserved) {
			t.Fatalf("NudgeSession = %v, want ErrNudgeSubmitDeliveredUnobserved: Claude queued the message", err)
		}
		if enters := ex.submitEnters(); enters != 1 {
			t.Fatalf("submit Enter sends = %d, want 1 (no submit into the running turn)", enters)
		}
	})
}

// TestNudgeSessionClearsPastClaudeQueuedHintUnderCursor: while an earlier
// message is still queued, Claude's input box shows only the hint. Clearing
// the input box before the paste must read it empty and press nothing, not
// fail on a "P" that no Ctrl-U can delete.
func TestNudgeSessionClearsPastClaudeQueuedHintUnderCursor(t *testing.T) {
	const session, message = "busy-claude-hint-clear", "blocker gc-1 closed — routed dependent gc-2 may be ready to claim"
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	synctest.Test(t, func(t *testing.T) {
		queued := slices.Concat(claudeLongTurn, []string{"❯ an earlier message", claudeInputRule, claudeQueuedHintUnderCursor}, claudeLongTurnFooter)
		ex := &queuedBehindTurnExecutor{
			agentPaneExecutor: &agentPaneExecutor{session: session, provider: "claude", attached: "0"},
			before:            queued,
			after:             queued,
		}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}
		err := tm.NudgeSession(session, message)
		if err != nil && strings.Contains(err.Error(), "input box still holds") {
			t.Fatalf("NudgeSession = %v; the input box held only Claude's hint", err)
		}
		if presses := ex.clearPresses(); presses != 0 {
			t.Fatalf("Ctrl-U Ctrl-K presses = %d, want 0 on an input box holding only the hint", presses)
		}
	})
}

// TestReadClaudeInputCursorOnPlaceholder: the reverse-video cursor cell that
// starts a dim placeholder is part of the placeholder; a cursor cell anywhere
// in typed text is part of the text.
func TestReadClaudeInputCursorOnPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  string
		want string
	}{
		{"queued hint under the cursor", claudeQueuedHintUnderCursor, ""},
		{"start-up placeholder under the cursor", "❯ \x1b[7mT\x1b[0;2mry \"write a test for <filepath>\"", ""},
		{"queued hint drawn dim without a cursor", "❯ \x1b[2mPress up to edit queued messages\x1b[0m", ""},
		{"empty input box, cursor drawn", "❯ \x1b[7m ", ""},
		{"typed P, cursor after it", "❯ \x1b[0mP\x1b[7m ", "P"},
		{"typed draft, cursor on its first letter", "❯ \x1b[7mP\x1b[0mress on", "Press on"},
		{"typed draft, cursor on its last letter", "❯ Press o\x1b[7mn\x1b[0m", "Press on"},
		{"cursor on a multi-byte placeholder letter", "❯ \x1b[7mÉ\x1b[0;2mcrivez ici", ""},
		{"cursor cleared by SGR 27 into the dim placeholder", "❯ \x1b[7mP\x1b[27;2mress up to edit queued messages", ""},
		{"highlighted typed text before a dim suggestion", "❯ \x1b[7mab\x1b[0;2m suggestion", "ab"},
		{"typed text, then the cursor cell on the dim suggestion", "❯ fix \x1b[7mt\x1b[0;2mhe tests", "fix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{claudeInputRule, tc.row, claudeInputRule}
			for i := range lines {
				lines[i] = stripDimText(lines[i])
			}
			in, ok := readClaudeInput(lines)
			if !ok {
				t.Fatalf("readClaudeInput(%q) found no input box", lines)
			}
			if got := in.text(); got != tc.want {
				t.Fatalf("input box reads %q, want %q", got, tc.want)
			}
		})
	}
}
