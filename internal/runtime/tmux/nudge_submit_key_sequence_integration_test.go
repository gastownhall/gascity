//go:build integration

package tmux

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSendNudgeSubmitSequenceSendsEachKeyInOrder is a live-tmux integration
// test proving sendNudgeSubmitSequence actually emits every key in the
// declared sequence, not just the last one. cat -v echoes control bytes as
// visible caret notation (Escape -> "^["), so a multi-key sequence leaves a
// distinguishing mark in the pane a single-Enter sequence would not.
func TestSendNudgeSubmitSequenceSendsEachKeyInOrder(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	tm := testTmux()
	sessionName := "gt-test-submit-seq-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, os.TempDir(), "cat -v", nil); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

	if err := tm.sendNudgeSubmitSequence(sessionName, []string{"Escape", "Enter"}); err != nil {
		t.Fatalf("sendNudgeSubmitSequence: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "^[") {
		t.Fatalf("CapturePaneAll missing Escape for a declared [Escape, Enter] sequence:\n%s", out)
	}
}

// TestNudgeSessionUsesDeclaredSequenceForProviderFamily proves NudgeSession
// itself (not just the low-level primitive) resolves and sends a registered
// family's declared sequence — the actual wiring a future ra-oudpha
// finding-3 fix would depend on. Registers a throwaway "testfam" family
// pointing at [Escape, Enter] so this doesn't depend on (or change) any
// real provider's shipped behavior.
func TestNudgeSessionUsesDeclaredSequenceForProviderFamily(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	origSeq := nudgeSubmitKeySequences
	nudgeSubmitKeySequences = map[string][]string{"testfam": {"Escape", "Enter"}}
	defer func() { nudgeSubmitKeySequences = origSeq }()

	// Step 3 of NudgeSession (unrelated to this patch) also sends a
	// pre-submit Escape for any family NOT in this skip list. "testfam" is
	// unregistered there, so without also skipping it here, that pre-existing
	// step would inject its own "^[" and this test would pass regardless of
	// whether the new declarative submit sequence (step 5) is wired up —
	// skip it so the observed Escape can only come from step 5.
	origSkip := providersSkippingEscapeBeforeEnter
	providersSkippingEscapeBeforeEnter = append(append([]string(nil), origSkip...), "testfam")
	defer func() { providersSkippingEscapeBeforeEnter = origSkip }()

	tm := testTmux()
	sessionName := "gt-test-nudge-testfam-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, os.TempDir(), "cat -v", map[string]string{
		"GC_PROVIDER": "testfam",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

	if err := tm.NudgeSession(sessionName, "hello"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "^[") {
		t.Fatalf("CapturePaneAll missing Escape for testfam's declared [Escape, Enter] submit sequence:\n%s", out)
	}
}
