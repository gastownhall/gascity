package runtime

import (
	"context"
	"reflect"
	"strings"
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
			name:    "gemini trust dialog",
			content: "Do you trust the files in this folder?\n1. Trust folder",
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
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			if len(sent) == 0 {
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

func TestAcceptStartupDialogsAcceptsGeminiTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			if len(sent) == 0 {
				return "Do you trust the files in this folder?\n● 1. Trust folder (city)\n  2. Trust parent folder\n  3. Don't trust", nil
			}
			return "Type your message or @path/to/file", nil
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

func TestAcceptStartupDialogsSkipsCodexUpdateDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(lines int) (string, error) {
			if lines < 100 {
				return "loading...", nil
			}
			return "✨ Update available! 0.124.0 -> 0.125.0\n" +
				"› 1. Update now (runs `bun install -g @openai/codex`)\n" +
				"  2. Skip\n" +
				"  3. Skip until next version\n" +
				"Press enter to continue", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsSkipsUpdateThenHandlesTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	staleUpdateReturned := false
	err := AcceptStartupDialogs(
		context.Background(),
		func(lines int) (string, error) {
			if lines < 100 {
				return "loading...", nil
			}
			switch {
			case len(sent) < 2:
				return codexUpdateDialogFixture(), nil
			case !staleUpdateReturned:
				staleUpdateReturned = true
				return codexUpdateDialogFixture(), nil
			case len(sent) == 2:
				return "Do you trust the contents of this directory?", nil
			default:
				return "› Implement {feature}", nil
			}
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsFromStreamSkipsCodexUpdateDialog(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- codexUpdateDialogFixture()
	snapshots <- "› Implement {feature}"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
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

func TestAcceptStartupDialogsAcceptsCustomAPIKeyDialog(t *testing.T) {
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
			return "Detected a custom API key in your environment\nDo you want to use this API key?\n1. Yes\n2. No (recommended)", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Up", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Up Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamAcceptsTrustDialog(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- "Do you trust the contents of this directory?"
	snapshots <- "user@host $"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent keys = %v, want [Enter]", sent)
	}
}

func TestAcceptWorkspaceTrustDialogFromStreamPreservesEarlierSnapshots(t *testing.T) {
	stream := &replayableSnapshotStream{update: make(chan struct{})}
	stream.publish("Do you trust the contents of this directory?")
	stream.publish("user@host $")
	stream.finish()

	var sent []string
	_, err := acceptWorkspaceTrustDialogFromStream(
		context.Background(),
		time.Second,
		newReplayableSnapshotCursorFromStream(stream),
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("acceptWorkspaceTrustDialogFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Enter"}) {
		t.Fatalf("sent keys = %v, want [Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamPrefersLaterDialogOverEarlierPrompt(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- "user@host $"
	snapshots <- "Bypass Permissions mode"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamWaitsBrieflyForDelayedDialogAfterPrompt(t *testing.T) {
	oldGrace := startupDialogStreamReadyGrace
	startupDialogStreamReadyGrace = 75 * time.Millisecond
	t.Cleanup(func() {
		startupDialogStreamReadyGrace = oldGrace
	})

	var sent []string
	snapshots := make(chan string, 1)
	snapshots <- "user@host $"
	go func() {
		time.Sleep(20 * time.Millisecond)
		snapshots <- "Bypass Permissions mode"
		close(snapshots)
	}()

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptBypassPermissionsWarningFromStreamSendsKeysSeparately(t *testing.T) {
	oldDelay := bypassDialogConfirmDelay
	bypassDialogConfirmDelay = 10 * time.Millisecond
	t.Cleanup(func() {
		bypassDialogConfirmDelay = oldDelay
	})

	stream := &replayableSnapshotStream{update: make(chan struct{})}
	stream.publish("Bypass Permissions mode")
	stream.finish()

	var calls []string
	var callTimes []time.Time
	_, err := acceptBypassPermissionsWarningFromStream(
		context.Background(),
		time.Second,
		newReplayableSnapshotCursorFromStream(stream),
		func(keys ...string) error {
			calls = append(calls, strings.Join(keys, ","))
			callTimes = append(callTimes, time.Now())
			return nil
		},
	)
	if err != nil {
		t.Fatalf("acceptBypassPermissionsWarningFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"Down", "Enter"}) {
		t.Fatalf("calls = %v, want [Down Enter]", calls)
	}
	if len(callTimes) != 2 || callTimes[1].Sub(callTimes[0]) < 10*time.Millisecond {
		t.Fatalf("callTimes gap = %v, want >= 10ms", callTimes[1].Sub(callTimes[0]))
	}
}

func TestAcceptStartupDialogsFromStreamReplaysBypassDialogAcrossPhases(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 1)
	snapshots <- "Bypass Permissions mode"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamReplaysCustomAPIKeyDialogAcrossPhases(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 1)
	snapshots <- "Detected a custom API key in your environment\nDo you want to use this API key?"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Up", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Up Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamReplaysRateLimitDialogAcrossPhases(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 1)
	snapshots <- "Usage limit reached"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if !reflect.DeepEqual(sent, []string{"Down", "Enter"}) {
		t.Fatalf("sent keys = %v, want [Down Enter]", sent)
	}
}

func TestAcceptStartupDialogsFromStreamTimesOutDespiteContinuousIrrelevantSnapshots(t *testing.T) {
	stream := &replayableSnapshotStream{update: make(chan struct{})}
	donePublishing := make(chan struct{})
	go func() {
		defer close(donePublishing)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 50; i++ {
			stream.publish("still booting")
			<-ticker.C
		}
		stream.finish()
	}()

	start := time.Now()
	_, err := acceptWorkspaceTrustDialogFromStream(
		context.Background(),
		30*time.Millisecond,
		newReplayableSnapshotCursorFromStream(stream),
		func(_ ...string) error { return nil },
	)
	if err != nil {
		t.Fatalf("acceptWorkspaceTrustDialogFromStream() error = %v, want nil timeout exit", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("acceptWorkspaceTrustDialogFromStream() took %s, want timeout-bounded exit", elapsed)
	}
	<-donePublishing
}

func TestAcceptStartupDialogsFromStreamWithStatusReturnsFalseAfterIrrelevantSnapshots(t *testing.T) {
	observed, err := AcceptStartupDialogsFromStreamWithStatus(
		context.Background(),
		30*time.Millisecond,
		func() <-chan string {
			snapshots := make(chan string, 1)
			snapshots <- "starting up"
			close(snapshots)
			return snapshots
		}(),
		func(_ ...string) error { return nil },
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStreamWithStatus() error = %v", err)
	}
	if observed {
		t.Fatal("AcceptStartupDialogsFromStreamWithStatus() observed = true, want false")
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
		{name: "claude nbsp prompt", content: "❯\u00a0", want: true},
		{name: "codex prompt", content: "›", want: true},
		{name: "codex prompt with nbsp", content: "›\u00a0", want: true},
		{name: "codex prompt with placeholder", content: "› Improve documentation in @filename", want: true},
		{name: "claude prompt with text", content: "❯ run tests", want: true},
		{name: "codex numbered menu row", content: "› 1. Update now (runs `bun install -g @openai/codex`)", want: false},
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

func codexUpdateDialogFixture() string {
	return "✨ Update available! 0.124.0 -> 0.125.0\n" +
		"› 1. Update now (runs `bun install -g @openai/codex`)\n" +
		"  2. Skip\n" +
		"  3. Skip until next version\n" +
		"Press enter to continue"
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

func TestExitsEarlyOnClaudeNBSPPrompt(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			return "❯\u00a0", nil
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
		t.Fatalf("sent keys = %v, want none (NBSP prompt detected)", sent)
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

func TestContainsCustomAPIKeyDialog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "custom api key prompt",
			content: "Detected a custom API key in your environment\nDo you want to use this API key?",
			want:    true,
		},
		{
			name:    "question only",
			content: "Do you want to use this API key?",
			want:    true,
		},
		{
			name:    "normal output",
			content: "Starting Claude Code...",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsCustomAPIKeyDialog(tt.content); got != tt.want {
				t.Fatalf("containsCustomAPIKeyDialog(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}
