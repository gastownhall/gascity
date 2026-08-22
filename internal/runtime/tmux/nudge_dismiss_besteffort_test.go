package tmux

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// sessionLimitChooserPane is the provider session-limit chooser as it renders
// on a live pane: the heading plus both option lines. It mirrors the
// runtime-package fixture, duplicated here because that fixture is unexported.
const sessionLimitChooserPane = `You've hit your session limit · resets 8:40am
❯ 1. Stop and wait for limit to reset
  2. Upgrade`

// A capture-pane (peek) failure before a nudge must not abort delivery. The
// pre-nudge dismissal is best-effort: it swallows the error, reports "no dialog
// dismissed", and sends no keys, so NudgeSession falls through to its own
// retry-wrapped send path instead of failing closed on a transient read.
func TestDismissMidSessionDialogBeforeNudge_PeekErrorIsSwallowed(t *testing.T) {
	fe := &fakeExecutor{errs: []error{errors.New("capture-pane failed")}}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	if dismissed := tm.dismissMidSessionDialogBeforeNudge("agent-pane"); dismissed {
		t.Error("dismissed = true on peek error, want false")
	}
	// The peek failed, so no dismissal keystroke may be sent: exactly the one
	// capture-pane call, no send-keys.
	if len(fe.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1 (capture-pane only, no send-keys)", len(fe.calls))
	}
	if !slices.Contains(fe.calls[0], "capture-pane") {
		t.Errorf("first call = %v, want a capture-pane invocation", fe.calls[0])
	}
}

// A send-keys failure while dismissing a matched dialog must likewise not abort
// the nudge: the helper swallows it and reports "no dialog dismissed" so the
// caller still proceeds to deliver the message. This is the resilience contract
// the sibling DismissModelSwitchModalIfPresent already honors.
func TestDismissMidSessionDialogBeforeNudge_SendErrorIsSwallowed(t *testing.T) {
	sendErr := errors.New("send-keys failed")
	fe := &fakeExecutor{
		outs: []string{sessionLimitChooserPane},
		errs: []error{nil, sendErr},
	}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	if dismissed := tm.dismissMidSessionDialogBeforeNudge("agent-pane"); dismissed {
		t.Error("dismissed = true on send error, want false")
	}
	// The chooser matched (capture-pane), then the first dismissal keystroke
	// failed; the error must be swallowed rather than propagated.
	if len(fe.calls) < 2 {
		t.Fatalf("executor calls = %d, want >=2 (capture-pane + send-keys)", len(fe.calls))
	}
	if !slices.Contains(fe.calls[0], "capture-pane") {
		t.Errorf("first call = %v, want a capture-pane invocation", fe.calls[0])
	}
	if !slices.Contains(fe.calls[1], "send-keys") {
		t.Errorf("second call = %v, want a send-keys invocation", fe.calls[1])
	}
}

// resumeDialogPane is the mid-session "Resume previous conversation?" dialog as
// it renders on a live pane. It mirrors the runtime-package fixture, duplicated
// here because that fixture is unexported. A single "Enter" dismisses it, which
// keeps these caller-level tests free of inter-key settle delays.
const resumeDialogPane = `Resume previous conversation?
❯ Resume from summary
  Resume full session as-is
Enter to confirm · Esc to cancel`

// The pre-nudge dialog check must read only the visible screen, never
// scrollback: a scrollback-inclusive capture ("-S -N") could match an
// already-dismissed dialog and inject dismissal keys into a live prompt before
// the intended nudge (PR #3427 regression). This pins the exact argv of that
// capture at the executor boundary.
func TestDismissMidSessionDialogBeforeNudge_UsesVisibleOnlyCapture(t *testing.T) {
	fe := &fakeExecutor{outs: []string{resumeDialogPane}}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	if dismissed := tm.dismissMidSessionDialogBeforeNudge("agent-pane"); !dismissed {
		t.Fatal("dismissed = false, want true for a live resume dialog")
	}
	if len(fe.calls) == 0 {
		t.Fatal("no executor calls recorded")
	}
	// Exactly `capture-pane -p -t agent-pane`, with no scrollback selector.
	// run() prepends "-u".
	gotCapture := fe.calls[0]
	wantCapture := []string{"-u", "capture-pane", "-p", "-t", "agent-pane"}
	if !slices.Equal(gotCapture, wantCapture) {
		t.Fatalf("capture call = %v, want %v (visible-only, no -S)", gotCapture, wantCapture)
	}
}

// The no-dialog path must issue the visible capture and NO dismissal keystrokes,
// so an ordinary idle or working pane is never keyed before a nudge (and the
// settle delay, which is gated on a dialog having been dismissed, is skipped).
func TestDismissMidSessionDialogBeforeNudge_NoDialogSendsNoKeys(t *testing.T) {
	fe := &fakeExecutor{outs: []string{"❯ "}}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}

	if dismissed := tm.dismissMidSessionDialogBeforeNudge("agent-pane"); dismissed {
		t.Error("dismissed = true on a plain idle prompt, want false")
	}
	if len(fe.calls) != 1 {
		t.Fatalf("executor calls = %d (%v), want 1 (visible capture-pane only, no send-keys)", len(fe.calls), fe.calls)
	}
	if !slices.Contains(fe.calls[0], "capture-pane") {
		t.Errorf("only call = %v, want a capture-pane invocation", fe.calls[0])
	}
	if slices.Contains(fe.calls[0], "-S") {
		t.Errorf("capture call = %v, must not use scrollback (-S)", fe.calls[0])
	}
}

// dialogThenSilentExecutor answers the visible-only dismissal capture
// (capture-pane with no "-S") with a live dialog and every other tmux call with
// empty output. paneBusy and the ready-prompt observers use "-S" captures, so
// they stay silent here; that lets a test assert the dialog capture and its
// dismissal keystroke precede the literal message paste without modeling the
// whole submit path.
type dialogThenSilentExecutor struct {
	calls [][]string
	pane  string
}

func (d *dialogThenSilentExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	d.calls = append(d.calls, cp)
	if slices.Contains(args, "capture-pane") && !slices.Contains(args, "-S") {
		return d.pane, nil
	}
	return "", nil
}

func (d *dialogThenSilentExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return d.execute(args)
}

// The behavior change is the WIRING: NudgeSession must peek and dismiss a
// blocking dialog BEFORE it pastes the message, or the nudge lands inside the
// dialog and is lost. A refactor that dropped the dismissal call or moved it
// after the paste would silently reintroduce that bug while the pure-matcher
// tests stayed green. This drives the real NudgeSession and asserts the
// ordering at the executor boundary (PR #3427 caller-level finding).
func TestNudgeSessionDismissesDialogBeforeLiteralPaste(t *testing.T) {
	ex := &dialogThenSilentExecutor{pane: resumeDialogPane}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	if err := tm.NudgeSession("agent-pane", "hello world"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	captureIdx, dismissEnterIdx, literalIdx := -1, -1, -1
	for i, c := range ex.calls {
		switch {
		case slices.Contains(c, "capture-pane") && !slices.Contains(c, "-S") && captureIdx == -1:
			captureIdx = i
		case slices.Contains(c, "send-keys") && slices.Contains(c, "-l") && slices.Contains(c, "hello world") && literalIdx == -1:
			literalIdx = i
		case slices.Contains(c, "send-keys") && slices.Contains(c, "Enter") && literalIdx == -1 && dismissEnterIdx == -1:
			dismissEnterIdx = i
		}
	}

	if captureIdx == -1 {
		t.Fatalf("no visible-only capture-pane (dialog check) in calls: %v", ex.calls)
	}
	if literalIdx == -1 {
		t.Fatalf(`no literal message paste (send-keys -l "hello world") in calls: %v`, ex.calls)
	}
	if dismissEnterIdx == -1 {
		t.Fatalf("no dismissal Enter keystroke before the paste in calls: %v", ex.calls)
	}
	if captureIdx >= literalIdx {
		t.Errorf("dialog capture at %d must precede the literal paste at %d", captureIdx, literalIdx)
	}
	if dismissEnterIdx >= literalIdx {
		t.Errorf("dismissal Enter at %d must precede the literal paste at %d", dismissEnterIdx, literalIdx)
	}
	if captureIdx >= dismissEnterIdx {
		t.Errorf("dialog capture at %d must precede its dismissal keystroke at %d", captureIdx, dismissEnterIdx)
	}
}
