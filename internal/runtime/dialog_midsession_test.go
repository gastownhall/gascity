package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

const (
	claudeResumeDialogPane = `Resume previous conversation?
❯ Resume from summary
  Resume full session as-is
Enter to confirm · Esc to cancel`

	claudeSessionFeedbackPane = `How is Claude doing this session?
❯ 1: Bad    2: Fine    3: Great
  0: Dismiss`

	claudeSessionLimitPane = `You've hit your session limit · resets 8:40am
❯ 1. Stop and wait for limit to reset
  2. Upgrade`
)

func TestDismissMidSessionDialogsSendsExpectedKeys(t *testing.T) {
	withZeroDialogTimings(t)

	tests := []struct {
		name          string
		pane          string
		wantKeys      []string
		wantDismissed bool
	}{
		{
			name:          "resume dialog confirms preselected summary",
			pane:          claudeResumeDialogPane,
			wantKeys:      []string{"Enter"},
			wantDismissed: true,
		},
		{
			name:          "feedback dialog skipped",
			pane:          claudeSessionFeedbackPane,
			wantKeys:      []string{"0", "Enter"},
			wantDismissed: true,
		},
		{
			name:          "session limit chooser stops and waits",
			pane:          claudeSessionLimitPane,
			wantKeys:      []string{"1", "Enter"},
			wantDismissed: true,
		},
		{
			name:          "plain idle prompt untouched",
			pane:          "❯ ",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			name:          "mid-turn output untouched",
			pane:          "Compiling internal/runtime...\n· Thinking",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			// Scrollback mentioning resume without the dialog footer must
			// not trigger keystrokes.
			name:          "stale resume mention without footer",
			pane:          "Earlier we chose Resume from summary and it worked.\n❯ ",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			// Scrollback quoting the feedback question without the active
			// "0: Dismiss" option line must not trigger keystrokes.
			name:          "stale feedback question without dismiss option",
			pane:          "Earlier: How is Claude doing this session? We moved on.\n❯ ",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			// A narrative mention of hitting the session limit without the
			// chooser's option lines must not trigger keystrokes.
			name:          "stale session-limit mention without options",
			pane:          "Note: you may have hit your session limit earlier today.\n❯ ",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			// Even scrollback containing both the session-limit phrase and the
			// reconciler-owned "/rate-limit-options" string must not match
			// without the chooser's option lines: this path no longer keys on
			// "/rate-limit-options" at all.
			name:          "session-limit phrase plus rate-limit-options without chooser",
			pane:          "log: hit your session limit; see /rate-limit-options for details.\n❯ ",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			// The provider rate-limit / quarantine screen is owned by the
			// session reconciler, not the nudge dismisser; injecting "1" here
			// is the wrong remedy, so it must be left untouched.
			name:          "provider rate-limit screen left to reconciler",
			pane:          "You've hit your limit, Pro plan\n\n/rate-limit-options",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			// A full, previously rendered resume dialog left in the capture
			// ABOVE a later idle prompt is stale: the active bottom block is
			// the prompt, not the dialog. Firing here would inject Enter into
			// the live prompt. The active-block guard requires the dialog's
			// tail line to be the bottom-most rendered line, so this must not
			// match. (Regression for the PR #3427 scrollback false-match.)
			name:          "stale full resume dialog above idle prompt",
			pane:          claudeResumeDialogPane + "\n❯ ",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			// Same hazard for the feedback dialog: a full "How is Claude doing
			// this session?" block with its "0: Dismiss" option scrolled above
			// a live prompt must not inject "0 Enter".
			name:          "stale full feedback dialog above idle prompt",
			pane:          claudeSessionFeedbackPane + "\n❯ ",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			// Same hazard for the session-limit chooser: a full chooser block
			// above a live prompt must not inject "1 Enter".
			name:          "stale full session-limit dialog above idle prompt",
			pane:          claudeSessionLimitPane + "\n❯ ",
			wantKeys:      nil,
			wantDismissed: false,
		},
		{
			// A live chooser as tmux actually captures it: the option lines
			// followed by trailing blank padding to the pane bottom. Trailing
			// blank lines must be ignored when locating the active bottom
			// block, so this still matches and dismisses.
			name:          "active session-limit chooser with trailing blank padding",
			pane:          claudeSessionLimitPane + "\n\n",
			wantKeys:      []string{"1", "Enter"},
			wantDismissed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sent []string
			dismissed, err := DismissMidSessionDialogs(
				context.Background(),
				func() (string, error) { return tt.pane, nil },
				func(keys ...string) error {
					sent = append(sent, keys...)
					return nil
				},
			)
			if err != nil {
				t.Fatalf("DismissMidSessionDialogs: %v", err)
			}
			if dismissed != tt.wantDismissed {
				t.Errorf("dismissed = %v, want %v", dismissed, tt.wantDismissed)
			}
			if !reflect.DeepEqual(sent, tt.wantKeys) {
				t.Errorf("sent keys = %v, want %v", sent, tt.wantKeys)
			}
		})
	}
}

func TestDismissMidSessionDialogsPropagatesPeekError(t *testing.T) {
	peekErr := errors.New("capture-pane failed")
	dismissed, err := DismissMidSessionDialogs(
		context.Background(),
		func() (string, error) { return "", peekErr },
		func(...string) error { return nil },
	)
	if !errors.Is(err, peekErr) {
		t.Fatalf("err = %v, want %v", err, peekErr)
	}
	if dismissed {
		t.Error("dismissed = true on peek error, want false")
	}
}

func TestDismissMidSessionDialogsPropagatesSendKeysError(t *testing.T) {
	withZeroDialogTimings(t)

	sendErr := errors.New("send-keys failed")
	dismissed, err := DismissMidSessionDialogs(
		context.Background(),
		func() (string, error) { return claudeSessionLimitPane, nil },
		func(...string) error { return sendErr },
	)
	if !errors.Is(err, sendErr) {
		t.Fatalf("err = %v, want %v", err, sendErr)
	}
	if dismissed {
		t.Error("dismissed = true on send error, want false")
	}
}
