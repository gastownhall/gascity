//go:build integration

package tmux

import (
	"errors"
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

// TestCodexBusyNudgeDoesNotSendEscape proves a running codex turn is queued
// with Enter only. cat -v renders Escape as "^[". The fixture prints the
// busy footer codex shows, then echoes keys, so an Escape-then-Enter submit
// would leave that mark.
func TestCodexBusyNudgeDoesNotSendEscape(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	cfg.NudgeReadyTimeout = 3 * time.Second
	tm := NewTmuxWithConfig(cfg)
	sessionName := fmt.Sprintf("gc-codex-busy-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, os.TempDir(), `sh -c 'printf "esc to interrupt\n"; exec cat -v'`, map[string]string{
		"GC_PROVIDER": "codex",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	waitForPaneText(t, tm, sessionName, "esc to interrupt")

	if err := tm.NudgeSession(sessionName, "queue-this-nudge"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	waitForPaneText(t, tm, sessionName, "queue-this-nudge")

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if strings.Contains(out, "^[") {
		t.Fatalf("busy codex nudge sent Escape (that cancels the turn):\n%s", out)
	}
}

// TestCodexIdleNudgeSendsEscapeThenEnter proves the idle path still sends the
// #4706 sequence. The fixture has no busy footer, so the nudge must not take
// the Enter-only queue path.
func TestCodexIdleNudgeSendsEscapeThenEnter(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	cfg.NudgeReadyTimeout = 3 * time.Second
	tm := NewTmuxWithConfig(cfg)
	sessionName := fmt.Sprintf("gc-codex-idle-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, os.TempDir(), "cat -v", map[string]string{
		"GC_PROVIDER": "codex",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	err := tm.NudgeSession(sessionName, "idle-codex-nudge")
	if err != nil && !errors.Is(err, ErrNudgeSubmitUnconfirmed) && !errors.Is(err, ErrNudgeSubmitDeliveredUnobserved) {
		t.Fatalf("NudgeSession: %v", err)
	}
	waitForPaneText(t, tm, sessionName, "idle-codex-nudge")

	out, capErr := tm.CapturePaneAll(sessionName)
	if capErr != nil {
		t.Fatalf("CapturePaneAll: %v", capErr)
	}
	if !strings.Contains(out, "^[") {
		t.Fatalf("idle codex nudge did not send Escape (paste-swallow sequence missing):\n%s", out)
	}
}

// waitForPaneText polls the pane until it shows want, failing after 10s with
// the last capture.
func waitForPaneText(t *testing.T, tm *Tmux, name, want string) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		if pane, err := tm.CapturePane(name, promptObservationLines); err == nil {
			if last = pane; strings.Contains(pane, want) {
				return
			}
		}
		select {
		case <-timer.C:
			t.Fatalf("pane %q never showed %q; last capture:\n%s", name, want, last)
		case <-ticker.C:
		}
	}
}
