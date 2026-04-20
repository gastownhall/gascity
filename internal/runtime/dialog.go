package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	dialogPollInterval       = 500 * time.Millisecond
	dialogPollTimeout        = 8 * time.Second
	startupDialogAcceptDelay = 500 * time.Millisecond
	bypassDialogConfirmDelay = 200 * time.Millisecond
	startupDialogPeekLines   = 120
)

// StartupDialogTimeout returns the current timeout budget used by the shared
// startup dialog helpers. Tests override the backing variable directly.
func StartupDialogTimeout() time.Duration {
	return dialogPollTimeout
}

// AcceptStartupDialogs dismisses startup dialogs that can block automated
// sessions. Handles (in order):
//  1. Workspace trust dialog (Claude "Quick safety check", Codex "Do you trust the contents of this directory?")
//  2. Bypass permissions warning ("Bypass Permissions mode") — requires Down+Enter
//  3. Claude custom API key confirmation — requires Up+Enter to select "Yes"
//
// The peek function should return the last N lines of the session's terminal output.
// The sendKeys function should send bare tmux-style keystrokes (e.g., "Enter", "Down").
//
// Idempotent: safe to call on sessions without dialogs.
func AcceptStartupDialogs(
	ctx context.Context,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	return AcceptStartupDialogsWithTimeout(ctx, dialogPollTimeout, peek, sendKeys)
}

// AcceptStartupDialogsFromStream dismisses known startup dialogs using an
// event stream of full-screen snapshots instead of repeated peeks.
func AcceptStartupDialogsFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots <-chan string,
	sendKeys func(keys ...string) error,
) error {
	stream := newReplayableSnapshotStream(snapshots)
	if err := acceptWorkspaceTrustDialogFromStream(ctx, timeout, stream, sendKeys); err != nil {
		return fmt.Errorf("workspace trust dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptBypassPermissionsWarningFromStream(ctx, timeout, stream, sendKeys); err != nil {
		return fmt.Errorf("bypass permissions warning: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptCustomAPIKeyDialogFromStream(ctx, timeout, stream, sendKeys); err != nil {
		return fmt.Errorf("custom API key dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dismissRateLimitDialogFromStream(ctx, timeout, stream, sendKeys); err != nil {
		return fmt.Errorf("rate limit dialog: %w", err)
	}
	return nil
}

// AcceptStartupDialogsWithTimeout dismisses known startup dialogs using the
// provided timeout budget for each dialog class.
func AcceptStartupDialogsWithTimeout(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	if err := acceptWorkspaceTrustDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("workspace trust dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptBypassPermissionsWarning(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("bypass permissions warning: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptCustomAPIKeyDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("custom API key dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dismissRateLimitDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("rate limit dialog: %w", err)
	}
	return nil
}

// acceptWorkspaceTrustDialog dismisses workspace trust dialogs for supported
// agents. Claude shows "Quick safety check"; Codex shows
// "Do you trust the contents of this directory?". In both cases the safe
// continue option is pre-selected, so Enter accepts.
func acceptWorkspaceTrustDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsWorkspaceTrustDialog(content) {
			if err := sendKeys("Enter"); err != nil {
				return err
			}
			sleep(ctx, startupDialogAcceptDelay)
			return nil
		}

		if containsPromptIndicator(content) {
			return nil
		}

		// Check if a bypass dialog appeared instead — let the next phase handle it.
		if strings.Contains(content, "Bypass Permissions mode") {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func acceptWorkspaceTrustDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotStream,
	sendKeys func(keys ...string) error,
) error {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsWorkspaceTrustDialog,
		matchKeys:   []string{"Enter"},
		matchDelay:  startupDialogAcceptDelay,
		ready:       containsPromptIndicator,
		readyOrNext: func(content string) bool { return strings.Contains(content, "Bypass Permissions mode") },
	})
}

func containsWorkspaceTrustDialog(content string) bool {
	return strings.Contains(content, "trust this folder") ||
		strings.Contains(content, "Quick safety check") ||
		strings.Contains(content, "Do you trust the contents of this directory?") ||
		strings.Contains(content, "Do you trust the files in this folder?")
}

// acceptBypassPermissionsWarning dismisses the Claude Code bypass permissions
// warning. When Claude starts with --dangerously-skip-permissions, it shows a
// warning requiring Down arrow to select "Yes, I accept" and then Enter.
func acceptBypassPermissionsWarning(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if strings.Contains(content, "Bypass Permissions mode") {
			if err := sendKeys("Down"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func acceptBypassPermissionsWarningFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotStream,
	sendKeys func(keys ...string) error,
) error {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:      func(content string) bool { return strings.Contains(content, "Bypass Permissions mode") },
		matchKeys:  []string{"Down", "Enter"},
		matchDelay: bypassDialogConfirmDelay,
		ready:      containsPromptIndicator,
	})
}

// acceptCustomAPIKeyDialog dismisses Claude's API-key confirmation prompt.
// In headless CI, Claude detects the injected ANTHROPIC_API_KEY and asks if it
// should use it. The menu defaults to "No (recommended)", so press Up then
// Enter to choose "Yes" and proceed with the configured provider.
func acceptCustomAPIKeyDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsCustomAPIKeyDialog(content) {
			if err := sendKeys("Up"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) || containsRateLimitDialog(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func acceptCustomAPIKeyDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotStream,
	sendKeys func(keys ...string) error,
) error {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:      containsCustomAPIKeyDialog,
		matchKeys:  []string{"Up", "Enter"},
		matchDelay: bypassDialogConfirmDelay,
		ready:      containsPromptIndicator,
		readyOrNext: func(content string) bool {
			return containsRateLimitDialog(content)
		},
	})
}

func containsCustomAPIKeyDialog(content string) bool {
	return strings.Contains(content, "Detected a custom API key in your environment") ||
		strings.Contains(content, "Do you want to use this API key?")
}

// dismissRateLimitDialog detects rate limit / usage limit dialogs (e.g.,
// Gemini's "Usage limit reached") and selects "Stop" to let the session
// exit cleanly. The reconciler treats the exit as a startup failure and
// retries later when the rate limit resets.
func dismissRateLimitDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsRateLimitDialog(content) {
			// Select "Stop" (option 2). The menu has "Keep trying" selected
			// by default, so press Down then Enter.
			if err := sendKeys("Down"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func dismissRateLimitDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotStream,
	sendKeys func(keys ...string) error,
) error {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:      containsRateLimitDialog,
		matchKeys:  []string{"Down", "Enter"},
		matchDelay: bypassDialogConfirmDelay,
		ready:      containsPromptIndicator,
	})
}

type streamDialogSpec struct {
	match       func(string) bool
	ready       func(string) bool
	readyOrNext func(string) bool
	matchKeys   []string
	matchDelay  time.Duration
}

type replayableSnapshotStream struct {
	mu      sync.Mutex
	history []string
	closed  bool
	update  chan struct{}
}

func newReplayableSnapshotStream(src <-chan string) *replayableSnapshotStream {
	stream := &replayableSnapshotStream{update: make(chan struct{})}
	go func() {
		for content := range src {
			stream.publish(content)
		}
		stream.finish()
	}()
	return stream
}

func (s *replayableSnapshotStream) publish(content string) {
	s.mu.Lock()
	s.history = append(s.history, content)
	update := s.update
	s.update = make(chan struct{})
	s.mu.Unlock()
	close(update)
}

func (s *replayableSnapshotStream) finish() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	update := s.update
	s.mu.Unlock()
	close(update)
}

func (s *replayableSnapshotStream) historyFrom(start int) ([]string, bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if start < 0 {
		start = 0
	}
	if start > len(s.history) {
		start = len(s.history)
	}
	snapshots := append([]string(nil), s.history[start:]...)
	return snapshots, s.closed, s.update
}

func acceptDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotStream,
	sendKeys func(keys ...string) error,
	spec streamDialogSpec,
) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	next := 0

	for {
		history, closed, updated := snapshots.historyFrom(next)
		if len(history) > 0 {
			next += len(history)
			for _, content := range history {
				if spec.match != nil && spec.match(content) {
					if err := sendKeys(spec.matchKeys...); err != nil {
						return err
					}
					sleep(ctx, spec.matchDelay)
					return nil
				}
				if spec.ready != nil && spec.ready(content) {
					return nil
				}
				if spec.readyOrNext != nil && spec.readyOrNext(content) {
					return nil
				}
			}
			continue
		}
		if closed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-updated:
		}
	}
}

func containsRateLimitDialog(content string) bool {
	return strings.Contains(content, "Usage limit reached") ||
		strings.Contains(content, "rate limit") ||
		strings.Contains(content, "Rate limit")
}

// containsPromptIndicator checks whether any line in the content ends with
// a common shell or REPL prompt suffix, indicating the session is ready
// and no dialog is present.
func containsPromptIndicator(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.ReplaceAll(line, "\u00a0", " ")
		trimmed = strings.TrimRight(trimmed, " \t")
		if trimmed == "" {
			continue
		}
		for _, suffix := range []string{">", "$", "%", "#", "\u276f"} {
			if strings.HasSuffix(trimmed, suffix) {
				return true
			}
		}
	}
	return false
}

// sleep waits for the given duration or until ctx is canceled.
func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
