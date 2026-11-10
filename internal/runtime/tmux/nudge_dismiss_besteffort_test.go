package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
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

	if dismissed := tm.dismissMidSessionDialogBeforeNudge(context.Background(), fakeAgentPane("agent-pane")); dismissed {
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

	if dismissed := tm.dismissMidSessionDialogBeforeNudge(context.Background(), fakeAgentPane("agent-pane")); dismissed {
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

	if dismissed := tm.dismissMidSessionDialogBeforeNudge(context.Background(), fakeAgentPane("agent-pane")); !dismissed {
		t.Fatal("dismissed = false, want true for a live resume dialog")
	}
	if len(fe.calls) == 0 {
		t.Fatal("no executor calls recorded")
	}
	// Exactly `capture-pane -p -t <agent pane>`, with no scrollback selector.
	// run() prepends "-u".
	gotCapture := fe.calls[0]
	wantCapture := []string{"-u", "capture-pane", "-p", "-t", fakeAgentPane("agent-pane")}
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

	if dismissed := tm.dismissMidSessionDialogBeforeNudge(context.Background(), fakeAgentPane("agent-pane")); dismissed {
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
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
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

// chooserOverClaudePane is a Claude pane parked on the session-limit chooser:
// its captures show chooser until the "1" "Enter" that picks "Stop and wait"
// reach it, and the composer underneath (the fakeClaudePane) from then on.
// The session publishes the ready prompt "❯ ", which reads back trimmed, as
// the real executor returns tmux output.
type chooserOverClaudePane struct {
	*fakeClaudePane

	mu      sync.Mutex
	chooser string
	keys    []string // every key or text that reached the chooser
	peeks   int      // visible-screen captures (no -S) of the chooser
	// onFirstPeek, when set, runs as the chooser's first visible-screen
	// capture (a dismissal's peek) is taken, before that capture returns.
	onFirstPeek func()
}

func (p *chooserOverClaudePane) execute(args []string) (string, error) {
	switch tmuxSubcommand(args) {
	case "show-environment":
		if args[len(args)-1] == ReadyPromptEnvKey {
			return strings.TrimSpace(ReadyPromptEnvKey + "=" + DefaultReadyPromptPrefix), nil
		}
	case "capture-pane":
		p.mu.Lock()
		screen := p.chooser
		peek := screen != "" && !slices.Contains(args, "-S")
		if peek {
			p.peeks++
		}
		first := peek && p.peeks == 1
		hook := p.onFirstPeek
		p.mu.Unlock()
		if first && hook != nil {
			hook()
		}
		if screen != "" {
			return screen, nil
		}
	case "send-keys":
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.chooser != "" {
			p.keys = append(p.keys, args[len(args)-1])
			if slices.Equal(p.keys, []string{"1", "Enter"}) {
				p.chooser = ""
			}
			return "", nil
		}
	}
	return p.fakeClaudePane.execute(args)
}

func (p *chooserOverClaudePane) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}

// chooserKeys returns what reached the chooser, in order.
func (p *chooserOverClaudePane) chooserKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.keys)
}

// A nudge to a Claude session parked on the session-limit chooser clears the
// chooser before its idle wait: a numbered menu never reads idle, so a nudge
// that waited first would hold every queued delivery for the whole
// NudgeIdleTimeout before the dismissal ahead of its text cleared it (#3427:
// the nudge path must not stall behind the chooser). The message then lands
// at the composer.
// Kills: a Provider.nudge that waits for idle before dismissing.
func TestNudgeDismissesSessionLimitChooserBeforeIdleWait(t *testing.T) {
	const session, message = "claude-session-limit-chooser", "check deploy status"
	// The bubble creates this session's nudge lock channel; drop it so
	// nothing outside the bubble ever touches it.
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	synctest.Test(t, func(t *testing.T) {
		claude := newFakeClaudePane("")
		claude.header = []string{"● Done."}
		claude.sessionName = session
		pane := &chooserOverClaudePane{fakeClaudePane: claude, chooser: claudeSessionLimitMenu}
		tm := NewTmux()
		tm.exec = pane

		start := time.Now()
		if err := (&Provider{tm: tm}).Nudge(session, runtime.TextContent(message)); err != nil {
			t.Fatalf("Nudge: %v", err)
		}
		if took, wait := time.Since(start), tm.cfg.NudgeIdleTimeout; took >= wait {
			t.Fatalf("Nudge took %s, want under its %s idle wait: the chooser must be cleared before the wait", took, wait)
		}
		if keys := pane.chooserKeys(); !slices.Equal(keys, []string{"1", "Enter"}) {
			t.Fatalf("keys sent to the chooser = %q, want [1 Enter] (Stop and wait)", keys)
		}
		if submitted, _ := claude.state(); !slices.Equal(submitted, []string{message}) {
			t.Fatalf("submitted = %q, want [%q]: the nudge belongs at the composer", submitted, message)
		}
	})
}

// Two nudges that reach a chooser-parked session together clear it once:
// the dismissal ahead of the idle wait holds the nudge lock through its
// settle, so the second nudge peeks only after the chooser is gone. Without
// the lock both see the chooser and both send "1" "Enter": the keys
// interleave in the chooser, or the second pair reaches the composer once
// the first has closed it, where Enter submits the "1" as a turn.
// Kills: a dismissal ahead of the idle wait that skips the nudge lock.
func TestNudgeConcurrentNudgesDismissChooserOnce(t *testing.T) {
	const session = "claude-session-limit-chooser-twice"
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	synctest.Test(t, func(t *testing.T) {
		claude := newFakeClaudePane("")
		claude.header = []string{"● Done."}
		claude.sessionName = session
		pane := &chooserOverClaudePane{fakeClaudePane: claude, chooser: claudeSessionLimitMenu}
		tm := NewTmux()
		tm.exec = pane
		p := &Provider{tm: tm}

		second := make(chan error, 1)
		// The second nudge starts while the first holds the chooser's first
		// peek, and runs until it blocks for good (on the nudge lock, or in a
		// sleep once it has dismissed the chooser itself).
		pane.onFirstPeek = func() {
			go func() { second <- p.Nudge(session, runtime.TextContent("second")) }()
			synctest.Wait()
		}
		firstErr := p.Nudge(session, runtime.TextContent("first"))
		secondErr := <-second
		if keys := pane.chooserKeys(); !slices.Equal(keys, []string{"1", "Enter"}) {
			t.Errorf("keys sent to the chooser = %q, want [1 Enter]", keys)
		}
		if n := claude.keyCount("1"); n != 0 {
			t.Errorf("%d dismissal key(s) %q reached the composer, want none: the chooser was already gone", n, "1")
		}
		// Whichever message is submitted second finds the first one's turn
		// running (this composer stays busy), so that nudge is delivered but
		// unobserved; both reach the composer.
		for name, err := range map[string]error{"first": firstErr, "second": secondErr} {
			if err != nil && !errors.Is(err, ErrNudgeSubmitDeliveredUnobserved) {
				t.Errorf("%s Nudge: %v", name, err)
			}
		}
		if submitted, _ := claude.state(); len(submitted) != 2 {
			t.Errorf("submitted = %q, want both nudges at the composer", submitted)
		}
	})
}
