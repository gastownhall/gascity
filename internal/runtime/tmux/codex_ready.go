package tmux

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

var codexLoadingField = regexp.MustCompile(`(?mi)^\s*[│┃]?\s*(model|directory):\s*loading\b`)

// Codex renders its composer while loading and uses the same glyph for menu
// selection. Neither is evidence that a resumed process can consume input.
func codexPaneReady(pane string) bool {
	lines := strings.Split(pane, "\n")
	if codexLoadingField.MatchString(pane) || paneContainsBusyIndicator(lines) || strings.Contains(pane, "Hooks need review") {
		return false
	}
	for _, line := range lines {
		if !matchesPromptPrefix(line, "› ") {
			continue
		}
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "›"))
		if len(text) > 1 && text[0] >= '0' && text[0] <= '9' && (text[1] == '.' || text[1] == ')') {
			continue
		}
		return true
	}
	return false
}

func (t *Tmux) waitForCodexIdle(ctx context.Context, session string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	consecutive := 0
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A resumed transcript can contain old idle prompts. Observe only the
		// current viewport, never the preceding scrollback or transcript history.
		pane, err := t.CapturePane(session, 0)
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer) {
			return err
		}
		if err == nil && codexPaneReady(pane) {
			consecutive++
		} else {
			consecutive = 0
		}
		if consecutive >= 2 {
			return nil
		}
		if err := waitForIdlePoll(ctx); err != nil {
			return err
		}
	}
	return ErrIdleTimeout
}
