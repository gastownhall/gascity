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
	// When a startup stream emits only irrelevant snapshots and then goes quiet,
	// fall back instead of waiting the full dialog timeout.
	startupDialogStreamIdleGrace = 100 * time.Millisecond
	// Give streamed startup snapshots a short chance to surface a follow-on
	// dialog after an initial shell prompt appears.
	startupDialogStreamReadyGrace = 100 * time.Millisecond
)

// StartupDialogTimeout returns the current timeout budget used by the shared
// startup dialog helpers. Tests override the backing variable directly.
func StartupDialogTimeout() time.Duration {
	return dialogPollTimeout
}

// AcceptStartupDialogs dismisses startup dialogs that can block automated
// sessions. Handles (in order):
//  1. Codex update dialog ("Update available") — requires Down+Enter to skip
//  2. Workspace trust dialog (Claude "Quick safety check", Codex "Do you trust the contents of this directory?")
//  3. Bypass permissions warning ("Bypass Permissions mode") — requires Down+Enter
//  4. Claude custom API key confirmation — requires Up+Enter to select "Yes"
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
	_, err := AcceptStartupDialogsFromStreamWithStatus(ctx, timeout, snapshots, sendKeys)
	return err
}

// AcceptStartupDialogsFromStreamWithStatus dismisses known startup dialogs
// using an event stream of full-screen snapshots instead of repeated peeks
// and reports whether the stream observed readiness or a known dialog state.
func AcceptStartupDialogsFromStreamWithStatus(
	ctx context.Context,
	timeout time.Duration,
	snapshots <-chan string,
	sendKeys func(keys ...string) error,
) (bool, error) {
	stream := newReplayableSnapshotCursor(snapshots)
	observed := false
	handledDialog := false
	trackingSendKeys := func(keys ...string) error {
		handledDialog = true
		return sendKeys(keys...)
	}

	phaseObserved, err := acceptCodexUpdateDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("codex update dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptWorkspaceTrustDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("workspace trust dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptBypassPermissionsWarningFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("bypass permissions warning: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptCustomAPIKeyDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("custom API key dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = dismissRateLimitDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("rate limit dialog: %w", err)
	}
	observed = observed || phaseObserved
	if handledDialog {
		promptObserved, err := acceptDialogFromStream(ctx, startupDialogStreamReadyGrace, stream, nil, streamDialogSpec{
			ready: containsPromptIndicator,
		})
		if err != nil {
			return observed, fmt.Errorf("startup readiness: %w", err)
		}
		if !promptObserved {
			return false, nil
		}
		observed = true
	}
	return observed, nil
}

// AcceptStartupDialogsWithTimeout dismisses known startup dialogs using the
// provided timeout budget for each dialog class.
func AcceptStartupDialogsWithTimeout(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	if err := acceptCodexUpdateDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("codex update dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
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

// acceptCodexUpdateDialog skips Codex's interactive update prompt. The default
// selection is "Update now", so automated sessions must move down to "Skip".
func acceptCodexUpdateDialog(
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

		if containsCodexUpdateDialog(content) {
			if err := sendKeys("Down"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) ||
			containsWorkspaceTrustDialog(content) ||
			strings.Contains(content, "Bypass Permissions mode") ||
			containsCustomAPIKeyDialog(content) ||
			containsRateLimitDialog(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func containsCodexUpdateDialog(content string) bool {
	return strings.Contains(content, "Update available!") &&
		strings.Contains(content, "Skip until next version") &&
		strings.Contains(content, "Press enter to continue")
}

func acceptCodexUpdateDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsCodexUpdateDialog,
		matchKeys:   []string{"Down", "Enter"},
		matchDelay:  bypassDialogConfirmDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostUpdateStartupDialog,
	})
}

func containsPostUpdateStartupDialog(content string) bool {
	return containsWorkspaceTrustDialog(content) ||
		strings.Contains(content, "Bypass Permissions mode") ||
		containsCustomAPIKeyDialog(content) ||
		containsRateLimitDialog(content)
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
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsWorkspaceTrustDialog,
		matchKeys:   []string{"Enter"},
		matchDelay:  startupDialogAcceptDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostTrustStartupDialog,
	})
}

func containsWorkspaceTrustDialog(content string) bool {
	return strings.Contains(content, "trust this folder") ||
		strings.Contains(content, "Quick safety check") ||
		strings.Contains(content, "Do you trust the contents of this directory?") ||
		strings.Contains(content, "Do you trust the files in this folder?")
}

func containsPostTrustStartupDialog(content string) bool {
	return strings.Contains(content, "Bypass Permissions mode") ||
		containsCustomAPIKeyDialog(content) ||
		containsRateLimitDialog(content)
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
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       func(content string) bool { return strings.Contains(content, "Bypass Permissions mode") },
		matchKeys:   []string{"Down", "Enter"},
		matchDelay:  bypassDialogConfirmDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostBypassStartupDialog,
	})
}

func containsPostBypassStartupDialog(content string) bool {
	return containsCustomAPIKeyDialog(content) || containsRateLimitDialog(content)
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
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsCustomAPIKeyDialog,
		matchKeys:   []string{"Up", "Enter"},
		matchDelay:  bypassDialogConfirmDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsRateLimitDialog,
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
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
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

type replayableSnapshotCursor struct {
	stream *replayableSnapshotStream
	next   int
	carry  []string
}

func newReplayableSnapshotCursor(src <-chan string) *replayableSnapshotCursor {
	return newReplayableSnapshotCursorFromStream(newReplayableSnapshotStream(src))
}

func newReplayableSnapshotCursorFromStream(stream *replayableSnapshotStream) *replayableSnapshotCursor {
	return &replayableSnapshotCursor{stream: stream}
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

func (c *replayableSnapshotCursor) nextBatch() ([]string, bool, <-chan struct{}) {
	batch := append([]string(nil), c.carry...)
	c.carry = nil
	history, closed, updated := c.stream.historyFrom(c.next)
	c.next += len(history)
	if len(history) > 0 {
		batch = append(batch, history...)
	}
	return batch, closed, updated
}

func (c *replayableSnapshotCursor) replay(history []string) {
	if len(history) == 0 {
		return
	}
	c.carry = append(append([]string(nil), history...), c.carry...)
}

func acceptDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
	spec streamDialogSpec,
) (bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var (
		readySeen     bool
		latestReady   string
		readyTimer    *time.Timer
		readyDeadline <-chan time.Time
		idleTimer     *time.Timer
		idleDeadline  <-chan time.Time
	)
	stopTimer := func(timer *time.Timer) {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	resetIdleTimer := func() {
		if startupDialogStreamIdleGrace <= 0 {
			return
		}
		if idleTimer == nil {
			idleTimer = time.NewTimer(startupDialogStreamIdleGrace)
			idleDeadline = idleTimer.C
			return
		}
		stopTimer(idleTimer)
		idleTimer.Reset(startupDialogStreamIdleGrace)
		idleDeadline = idleTimer.C
	}
	defer stopTimer(readyTimer)
	defer stopTimer(idleTimer)

	for {
		history, closed, updated := snapshots.nextBatch()
		if len(history) > 0 {
			for idx, content := range history {
				if spec.match != nil && spec.match(content) {
					snapshots.replay(history[idx+1:])
					return true, sendDialogKeys(ctx, sendKeys, spec.matchKeys, spec.matchDelay)
				}
				if spec.readyOrNext != nil && spec.readyOrNext(content) {
					snapshots.replay(history[idx:])
					return true, nil
				}
				if spec.ready != nil && spec.ready(content) {
					latestReady = content
					if !readySeen {
						readySeen = true
						stopTimer(idleTimer)
						idleDeadline = nil
						if startupDialogStreamReadyGrace <= 0 {
							snapshots.replay([]string{latestReady})
							return true, nil
						}
						readyTimer = time.NewTimer(startupDialogStreamReadyGrace)
						readyDeadline = readyTimer.C
					}
				}
			}
			if !readySeen {
				resetIdleTimer()
			}
		}
		if closed {
			if readySeen {
				snapshots.replay([]string{latestReady})
			}
			return readySeen, nil
		}
		if readySeen {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-timer.C:
				snapshots.replay([]string{latestReady})
				return true, nil
			case <-readyDeadline:
				snapshots.replay([]string{latestReady})
				return true, nil
			case <-updated:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, nil
		case <-idleDeadline:
			return false, nil
		case <-updated:
		}
	}
}

func sendDialogKeys(
	ctx context.Context,
	sendKeys func(keys ...string) error,
	keys []string,
	delay time.Duration,
) error {
	if len(keys) == 0 {
		return nil
	}
	if len(keys) == 1 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sendKeys(keys[0]); err != nil {
			return err
		}
		sleep(ctx, delay)
		return ctx.Err()
	}
	for i, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sendKeys(key); err != nil {
			return err
		}
		if i < len(keys)-1 {
			sleep(ctx, delay)
		}
	}
	return nil
}

func containsRateLimitDialog(content string) bool {
	return strings.Contains(content, "Usage limit reached") ||
		strings.Contains(content, "rate limit") ||
		strings.Contains(content, "Rate limit")
}

// containsPromptIndicator checks whether any line in the content looks like a
// common shell or agent prompt, indicating the session is ready and no dialog is
// present. Full-screen agent UIs often render placeholder input after the prompt
// glyph, so Claude/Codex prompts are accepted as prefixes too.
func containsPromptIndicator(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.ReplaceAll(line, "\u00a0", " ")
		trimmed = strings.TrimRight(trimmed, " \t")
		if trimmed == "" {
			continue
		}
		for _, prefix := range []string{"\u276f", "\u203a"} {
			rest, ok := strings.CutPrefix(trimmed, prefix+" ")
			if trimmed == prefix || (ok && !isNumberedMenuRow(rest)) {
				return true
			}
		}
		for _, suffix := range []string{">", "$", "%", "#", "\u276f", "\u203a"} {
			if strings.HasSuffix(trimmed, suffix) {
				return true
			}
		}
	}
	return false
}

func isNumberedMenuRow(content string) bool {
	digits := 0
	for digits < len(content) && content[digits] >= '0' && content[digits] <= '9' {
		digits++
	}
	return digits > 0 && digits < len(content) && content[digits] == '.'
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
