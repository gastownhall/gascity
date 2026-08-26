//go:build integration

package tmux

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNudgeSessionClearsPendingInputBeforePaste is the regression test for
// ra-3x46cy finding 2: NudgeSession must clear any pending (undelivered)
// input already sitting in the pane before pasting a new message, mirroring
// SendKeysReplace's leading C-u. Pre-fix, a leftover draft from an earlier
// stalled nudge (e.g. one whose submit Enter was lost, ga-bwm) would
// concatenate with the new paste into one undeliverable, multi-fragment line
// instead of being replaced.
func TestNudgeSessionClearsPendingInputBeforePaste(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName := fmt.Sprintf("gt-test-nudge-clear-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	// cat -v echoes each newline-terminated line back to the pane (control
	// characters rendered visibly), so the echoed line reveals exactly what
	// was submitted. GC_PROVIDER=opencode skips the Escape-before-Enter step
	// and is not submit-verify-eligible (fast, single-attempt fallback path),
	// keeping this test's assertions focused on the clear-before-paste step.
	if err := tm.NewSessionWithCommandAndEnv(sessionName, os.TempDir(), "cat -v", map[string]string{
		"GC_PROVIDER": "opencode",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

	// Simulate a leftover, undelivered draft: text typed into the pane but
	// never terminated with Enter, as an earlier stalled nudge would leave.
	if _, err := tm.run("send-keys", "-t", sessionName, "-l", "leftover-draft"); err != nil {
		t.Fatalf("send-keys leftover draft: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := tm.NudgeSession(sessionName, "fresh-message"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if strings.Contains(out, "leftover-draftfresh-message") {
		t.Fatalf("pending input was not cleared before paste; stacked draft merged with the new nudge:\n%s", out)
	}
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "fresh-message" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected an echoed line exactly matching %q (clean submit, leftover cleared), got:\n%s", "fresh-message", out)
	}
}
