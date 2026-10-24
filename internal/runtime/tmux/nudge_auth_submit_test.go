package tmux

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
)

// authSubmitPane consumes Enter without ever drawing a busy row, like a turn
// that fails on auth immediately. The shared Claude pane still owns composer
// rendering, literal input, clearing, and session/provider discovery.
type authSubmitPane struct {
	*fakeClaudePane
	keepDraft      bool
	confirm        bool
	captureErr     error
	submits        int
	failedCaptures int
}

func (p *authSubmitPane) execute(args []string) (string, error) {
	submit := slices.Contains(args, "send-keys") && slices.Contains(args, "Enter") && !slices.Contains(args, "-l")
	if submit {
		p.submits++
		if p.keepDraft {
			return "", nil
		}
	}
	// Once the last submit reaches tmux, plain captures fail. Styled input
	// captures still work, so the caller can recover the composer verdict.
	if p.captureErr != nil && p.submits >= submitEnterMaxSends && slices.Contains(args, "capture-pane") && !slices.Contains(args, "-e") {
		p.failedCaptures++
		return "", p.captureErr
	}
	out, err := p.fakeClaudePane.execute(args)
	if submit && !p.confirm {
		p.mu.Lock()
		p.busy = false
		p.mu.Unlock()
	}
	return out, err
}

func (p *authSubmitPane) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}

func TestNudgeSessionAuthFailureAndFallbackVerdicts(t *testing.T) {
	const banner = "● Login expired · Please run /login"
	for _, tc := range []struct {
		name       string
		banner     string
		keepDraft  bool
		captureErr bool
		confirm    bool
		want       error
	}{
		{name: "auth overrides drained composer", banner: banner, want: ErrNudgeSubmitAuthFailed},
		{name: "auth overrides staged composer", banner: banner, keepDraft: true, want: ErrNudgeSubmitAuthFailed},
		{name: "drained without auth", want: ErrNudgeSubmitDeliveredUnobserved},
		{name: "staged without auth", keepDraft: true, want: ErrNudgeSubmitUnconfirmed},
		{name: "failed auth capture keeps drained verdict", banner: banner, captureErr: true, want: ErrNudgeSubmitDeliveredUnobserved},
		{name: "failed auth capture keeps staged verdict", banner: banner, keepDraft: true, captureErr: true, want: ErrNudgeSubmitUnconfirmed},
		{name: "confirmed turn ignores historical auth banner", banner: banner, confirm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pane := &authSubmitPane{fakeClaudePane: newFakeClaudePane(""), keepDraft: tc.keepDraft, confirm: tc.confirm}
				pane.sessionName = t.Name()
				t.Cleanup(func() { sessionNudgeLocks.Delete(pane.sessionName) })
				pane.header = []string{tc.banner}
				if tc.captureErr {
					pane.captureErr = errors.New("capture-pane: transient read failure")
				}
				tm := newFakeClaudeTmux(pane.fakeClaudePane)
				tm.exec = pane

				err := tm.NudgeSession(pane.sessionName, "auth-submit-probe")

				if !errors.Is(err, tc.want) {
					t.Fatalf("NudgeSession = %v, want %v", err, tc.want)
				}
				if errors.Is(tc.want, ErrNudgeSubmitAuthFailed) && errors.Is(err, ErrNudgeSubmitDeliveredUnobserved) {
					t.Fatalf("auth failure was also reported as delivery: %v", err)
				}
				if tc.captureErr && pane.failedCaptures == 0 {
					t.Fatal("capture failure was never exercised")
				}
				if pane.submits == 0 {
					t.Fatal("NudgeSession never sent Enter")
				}
				_, draft := pane.state()
				if got := draft != ""; got != tc.keepDraft {
					t.Fatalf("composer holds %q, want staged=%t", draft, tc.keepDraft)
				}
			})
		})
	}
}
