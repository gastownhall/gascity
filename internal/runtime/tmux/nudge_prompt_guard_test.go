package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// A permission prompt can appear at any moment of a running turn, including
// after NudgeNow's first check and while the nudge waits for its lock, clears
// the input box, pastes, and sends Enter. Text typed into the prompt is read
// as menu input (Enter picks the highlighted "Yes"), so the nudge re-checks
// under the lock, before the paste and again before Enter (#2892).

func promptGuardPane(t *testing.T) *fakeClaudePane {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "claude-approval", "edit-80x24.txt"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	pane := newFakeClaudePane("")
	pane.prompt = string(data)
	return pane
}

func nudgeNowText(t *testing.T, pane *fakeClaudePane, text string) error {
	t.Helper()
	p := &Provider{tm: newFakeClaudeTmux(pane)}
	return p.NudgeNow(pane.sessionName, []runtime.ContentBlock{{Type: "text", Text: text}})
}

// The prompt appears after NudgeNow's first check passed: the first capture
// under the nudge lock shows it. Nothing may reach the prompt.
func TestNudgeNowRefusesPromptThatAppearsUnderTheNudgeLock(t *testing.T) {
	pane := promptGuardPane(t)
	pane.promptOnCapture = 2 // capture 1 is NudgeNow's own check

	err := nudgeNowText(t, pane, "1")
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeNow = %v, want runtime.ErrPendingInteraction", err)
	}
	if len(pane.promptKeys) != 0 {
		t.Fatalf("the permission prompt received %q, want nothing", pane.promptKeys)
	}
}

// The prompt appears while the message is being pasted: the paste already
// sits in the input box, but Enter would now answer the prompt, so it must
// not be sent.
func TestNudgeNowRefusesEnterWhenPromptAppearsAfterThePaste(t *testing.T) {
	pane := promptGuardPane(t)
	pane.promptAfterPaste = true

	err := nudgeNowText(t, pane, "run the tests")
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeNow = %v, want runtime.ErrPendingInteraction", err)
	}
	if len(pane.promptKeys) != 0 {
		t.Fatalf("the permission prompt received %q, want nothing (no Enter)", pane.promptKeys)
	}
	if len(pane.submitted) != 0 {
		t.Fatalf("submitted %q, want nothing", pane.submitted)
	}
}

// The look before Enter cannot read the pane, and a prompt came up with the
// paste: a look that cannot rule a prompt out refuses the nudge like a prompt
// on screen does, so no Enter may be sent.
func TestNudgeNowSendsNoEnterWhenTheLookBeforeItCannotReadThePane(t *testing.T) {
	pane := promptGuardPane(t)
	pane.promptAfterPaste = true
	pane.captureErr = errors.New("capture-pane: timed out")
	pane.captureErrAfterPaste = true

	err := nudgeNowText(t, pane, "run the tests")
	if !errors.Is(err, pane.captureErr) {
		t.Fatalf("NudgeNow = %v, want the failed capture", err)
	}
	if len(pane.promptKeys) != 0 {
		t.Fatalf("the permission prompt received %q, want nothing (no Enter)", pane.promptKeys)
	}
}

// A session gone by the look under the nudge lock stays the best-effort no-op
// a nudge to a gone session is: nothing typed, no error.
func TestNudgeNowTypesNothingIntoASessionGoneByTheLookUnderTheLock(t *testing.T) {
	pane := promptGuardPane(t)
	pane.prompt = ""
	pane.captureErr = ErrSessionNotFound
	pane.captureErrOnCapture = 2 // capture 1 is NudgeNow's own check

	if err := nudgeNowText(t, pane, "run the tests"); err != nil {
		t.Fatalf("NudgeNow = %v, want nil for a session that is gone", err)
	}
	if len(pane.keys) != 0 || pane.draft != "" || len(pane.submitted) != 0 {
		t.Fatalf("keys %q, draft %q, submitted %q, want nothing typed", pane.keys, pane.draft, pane.submitted)
	}
}

// Without a prompt the guarded nudge still delivers.
func TestNudgeNowDeliversWhenNoPromptAppears(t *testing.T) {
	pane := promptGuardPane(t)
	pane.prompt = ""

	if err := nudgeNowText(t, pane, "run the tests"); err != nil {
		t.Fatalf("NudgeNow: %v", err)
	}
	if len(pane.submitted) != 1 || pane.submitted[0] != "run the tests" {
		t.Fatalf("submitted %q, want the message", pane.submitted)
	}
}

// A prompt that shows up before a re-send stops the re-sends at once: the
// confirm loop must not keep retrying Enter while a prompt is on screen.
func TestSubmitEnterAndConfirmStopsAtPendingPrompt(t *testing.T) {
	calls := 0
	send := func() error {
		calls++
		if calls > 1 {
			return runtime.ErrPendingInteraction
		}
		return nil
	}
	notBusy := func() (bool, error) { return false, nil }

	confirmed, err := submitEnterAndConfirm(send, func() {}, notBusy, func(time.Duration) {})
	if confirmed || !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("submitEnterAndConfirm = (%v, %v), want (false, ErrPendingInteraction)", confirmed, err)
	}
	if calls != 2 {
		t.Fatalf("submit attempts = %d, want 2 (the first send, then the refused re-send)", calls)
	}
}

// A look that cannot read the pane before a re-send stops the re-sends at once,
// as a prompt on screen does.
func TestSubmitEnterAndConfirmStopsWhenALookCannotReadThePane(t *testing.T) {
	calls := 0
	send := func() error {
		calls++
		if calls > 1 {
			return fmt.Errorf("%w: capture-pane: timed out", errPromptLookFailed)
		}
		return nil
	}
	notBusy := func() (bool, error) { return false, nil }

	confirmed, err := submitEnterAndConfirm(send, func() {}, notBusy, func(time.Duration) {})
	if confirmed || !errors.Is(err, errPromptLookFailed) {
		t.Fatalf("submitEnterAndConfirm = (%v, %v), want (false, errPromptLookFailed)", confirmed, err)
	}
	if calls != 2 {
		t.Fatalf("submit attempts = %d, want 2 (the first send, then the refused re-send)", calls)
	}
}

// A look that refuses only a re-send, after the first submit went out, makes
// the nudge report the submit unconfirmed rather than failed: that submit may
// already have delivered the message, and a caller that retries a failure
// would send it twice.
func TestNudgeSessionReportsARefusedReSendUnconfirmed(t *testing.T) {
	pane := newFakeClaudePane("")
	tm := newFakeClaudeTmux(pane)
	looks := 0
	look := func(string) error {
		looks++
		if looks > 3 { // under the lock, before the paste, before the first submit
			return fmt.Errorf("%w: capture-pane: timed out", errPromptLookFailed)
		}
		return nil
	}
	// The text never reaches the input box, so Enter submits nothing, the pane
	// never turns busy, and the submit is re-sent.
	dropText := func(string, string, time.Duration) error { return nil }

	err := tm.nudgeSession(pane.sessionName, "run the tests", dropText,
		func(string) bool { return false }, func(string) []string { return []string{"Enter"} }, look)
	if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("nudgeSession = %v, want ErrNudgeSubmitUnconfirmed", err)
	}
	if n := pane.keyCount("Enter"); n != 1 {
		t.Fatalf("sent Enter %d times, want 1 (the re-send refused)", n)
	}
}

// A permission question hard-wrapped onto two rows is still a prompt: the
// nudge refuses it instead of pasting and pressing Enter on it.
func TestNudgeNowRefusesWrappedPermissionQuestion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "claude-approval", "edit-wrapped-question-2.1.289-80x30.txt"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	pane := newFakeClaudePane("")
	pane.prompt = string(data)
	pane.promptOnCapture = 1

	err = nudgeNowText(t, pane, "run the tests")
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeNow = %v, want runtime.ErrPendingInteraction", err)
	}
	if len(pane.promptKeys) != 0 {
		t.Fatalf("the permission prompt received %q, want nothing", pane.promptKeys)
	}
}

// A permission prompt does not turn idle on its own, so the wait-idle Nudge
// refuses it before the idle wait rather than after it: the queued-nudge
// dispatcher must not stall for the whole idle timeout on every pass over a
// session that waits on a prompt.
func TestNudgeRefusesPendingPromptWithoutWaitingForIdle(t *testing.T) {
	pane := promptGuardPane(t)
	pane.promptOnCapture = 1
	tm := newFakeClaudeTmux(pane)
	tm.cfg.NudgeIdleTimeout = 4 * time.Second
	p := &Provider{tm: tm}

	start := time.Now()
	err := p.Nudge(pane.sessionName, []runtime.ContentBlock{{Type: "text", Text: "run the tests"}})
	elapsed := time.Since(start)

	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("Nudge = %v, want runtime.ErrPendingInteraction", err)
	}
	if elapsed >= tm.cfg.NudgeIdleTimeout/2 {
		t.Fatalf("Nudge refused after %v, want well under the %v idle timeout", elapsed, tm.cfg.NudgeIdleTimeout)
	}
	if len(pane.promptKeys) != 0 {
		t.Fatalf("the permission prompt received %q, want nothing", pane.promptKeys)
	}
}
