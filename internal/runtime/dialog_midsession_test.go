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
			name:          "resume dialog selects summary",
			pane:          claudeResumeDialogPane,
			wantKeys:      []string{"1", "Enter"},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sent []string
			dismissed, err := DismissMidSessionDialogs(
				context.Background(),
				func(int) (string, error) { return tt.pane, nil },
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
		func(int) (string, error) { return "", peekErr },
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
		func(int) (string, error) { return claudeSessionLimitPane, nil },
		func(...string) error { return sendErr },
	)
	if !errors.Is(err, sendErr) {
		t.Fatalf("err = %v, want %v", err, sendErr)
	}
	if dismissed {
		t.Error("dismissed = true on send error, want false")
	}
}
