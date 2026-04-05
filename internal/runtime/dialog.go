package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"
)

var (
	dialogPollInterval       = 500 * time.Millisecond
	dialogPollTimeout        = 8 * time.Second
	startupDialogAcceptDelay = 500 * time.Millisecond
	bypassDialogConfirmDelay = 200 * time.Millisecond
)

// AcceptStartupDialogs dismisses startup dialogs that can block automated
// sessions. Handles (in order):
//  1. Workspace trust dialog (Claude "Quick safety check", Codex "Do you trust the contents of this directory?")
//  2. Bypass permissions warning ("Bypass Permissions mode") — requires Down+Enter
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
	if err := acceptWorkspaceTrustDialog(ctx, peek, sendKeys); err != nil {
		return fmt.Errorf("workspace trust dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptBypassPermissionsWarning(ctx, peek, sendKeys); err != nil {
		return fmt.Errorf("bypass permissions warning: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dismissRateLimitDialog(ctx, peek, sendKeys); err != nil {
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
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(dialogPollTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(30)
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

func containsWorkspaceTrustDialog(content string) bool {
	return strings.Contains(content, "trust this folder") ||
		strings.Contains(content, "Quick safety check") ||
		strings.Contains(content, "Do you trust the contents of this directory?")
}

// acceptBypassPermissionsWarning dismisses the Claude Code bypass permissions
// warning. When Claude starts with --dangerously-skip-permissions, it shows a
// warning requiring Down arrow to select "Yes, I accept" and then Enter.
func acceptBypassPermissionsWarning(
	ctx context.Context,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(dialogPollTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(30)
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

// dismissRateLimitDialog detects rate limit / usage limit dialogs (e.g.,
// Gemini's "Usage limit reached") and selects "Stop" to let the session
// exit cleanly. The reconciler treats the exit as a startup failure and
// retries later when the rate limit resets.
func dismissRateLimitDialog(
	ctx context.Context,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(dialogPollTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(30)
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
		trimmed := strings.TrimRight(line, " \t")
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
