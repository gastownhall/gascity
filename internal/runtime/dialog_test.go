package runtime

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func withZeroDialogTimings(t *testing.T) {
	t.Helper()
	oldPollInterval := dialogPollInterval
	oldPollTimeout := dialogPollTimeout
	oldAcceptDelay := startupDialogAcceptDelay
	oldConfirmDelay := bypassDialogConfirmDelay
	dialogPollInterval = 0
	dialogPollTimeout = 0
	startupDialogAcceptDelay = 0
	bypassDialogConfirmDelay = 0
	t.Cleanup(func() {
		dialogPollInterval = oldPollInterval
		dialogPollTimeout = oldPollTimeout
		startupDialogAcceptDelay = oldAcceptDelay
		bypassDialogConfirmDelay = oldConfirmDelay
	})
}

func TestContainsWorkspaceTrustDialog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "claude quick safety check",
			content: "Quick safety check\nYes, I trust this folder",
			want:    true,
		},
		{
			name:    "claude trust this folder",
			content: "Do you trust this folder?",
			want:    true,
		},
		{
			name:    "codex trust dialog",
			content: "> Do you trust the contents of this directory?",
			want:    true,
		},
		{
			name:    "normal prompt text",
			content: "> waiting for input",
			want:    false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containsWorkspaceTrustDialog(tt.content); got != tt.want {
				t.Fatalf("containsWorkspaceTrustDialog(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestAcceptStartupDialogsAcceptsCodexTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	// Override timeout to allow at least one poll iteration.
	dialogPollTimeout = time.Second

	var sent []string
	peekCall := 0
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			peekCall++
			if peekCall == 1 {
				return "Do you trust the contents of this directory?", nil
			}
			return "user@host $", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent keys = %v, want [Enter]", sent)
	}
}

func TestAcceptStartupDialogsPeeksDeepEnoughForLateTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(lines int) (string, error) {
			if lines < 100 {
				return "› Implement {feature}", nil
			}
			return "Do you trust the contents of this directory?\n› Implement {feature}", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent keys = %v, want [Enter]", sent)
	}
}

func TestAcceptStartupDialogsAcceptsBypassPermissionsWarning(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	call := 0
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			call++
			if call <= 2 {
				// First two peeks: no trust dialog, no bypass. Then bypass appears.
				return "normal startup output", nil
			}
			return "Bypass Permissions mode", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestContainsPromptIndicator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "dollar prompt", content: "user@host $", want: true},
		{name: "hash prompt", content: "root@host #", want: true},
		{name: "percent prompt", content: "zsh %", want: true},
		{name: "angle prompt", content: "claude >", want: true},
		{name: "powerline prompt", content: "dir \u276f", want: true},
		{name: "empty content", content: "", want: false},
		{name: "no prompt", content: "loading...", want: false},
		{name: "blank lines only", content: "\n\n", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containsPromptIndicator(tt.content); got != tt.want {
				t.Fatalf("containsPromptIndicator(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestExitsEarlyOnPrompt(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			return "user@host $", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent keys = %v, want none (prompt detected)", sent)
	}
}

func TestPollsUntilDialogAppears(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var peekCount atomic.Int32
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			n := peekCount.Add(1)
			if n < 3 {
				return "starting up...", nil
			}
			return "Quick safety check\ntrust this folder", nil
		},
		func(...string) error {
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if got := peekCount.Load(); got < 3 {
		t.Fatalf("peekCount = %d, want >= 3 (polled until dialog appeared)", got)
	}
}

func TestRespectsContextCancellation(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollInterval = 50 * time.Millisecond
	dialogPollTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := AcceptStartupDialogs(
		ctx,
		func(_ int) (string, error) {
			return "loading...", nil
		},
		func(...string) error {
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
}

func TestAcceptStartupDialogsDismissesRateLimitDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	call := 0
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			call++
			if call <= 2 {
				return "normal startup output", nil
			}
			return "Usage limit reached for gemini-3-flash-preview.\n1. Keep trying\n2. Stop", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	// Should select "Stop" (Down + Enter).
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestContainsRateLimitDialog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "gemini usage limit", content: "Usage limit reached for gemini-3-flash-preview.", want: true},
		{name: "generic rate limit", content: "rate limit exceeded", want: true},
		{name: "Rate limit caps", content: "Rate limit: try again later", want: true},
		{name: "normal output", content: "Hello world", want: false},
		{name: "empty", content: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsRateLimitDialog(tt.content); got != tt.want {
				t.Errorf("containsRateLimitDialog(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}
