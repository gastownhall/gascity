package tmux

import (
	"errors"
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
