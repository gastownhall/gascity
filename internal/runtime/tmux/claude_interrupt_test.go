package tmux

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Interrupting a Claude turn that is waiting on a permission prompt must
// cancel the prompt with Escape, the key it offers. Ctrl-C leaves the prompt
// up, and no numbered option may be chosen to stop a turn.
func TestClaudeInterruptCancelsPermissionPromptWithEscape(t *testing.T) {
	for _, fixture := range []string{"bash-manual-80x24.txt", "edit-80x24.txt", "bash-automode-option.txt"} {
		t.Run(fixture, func(t *testing.T) {
			pane := newFakeClaudePane("")
			pane.prompt = readApprovalFixture(t, fixture)
			pane.promptOnCapture = 1

			if err := (&Provider{tm: newFakeClaudeTmux(pane)}).Interrupt(pane.sessionName); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			if !slices.Equal(pane.promptKeys, []string{"Escape"}) {
				t.Fatalf("permission prompt received %q, want exactly Escape", pane.promptKeys)
			}
		})
	}
}

// A turn with no prompt on screen is still interrupted with Ctrl-C.
func TestClaudeInterruptWithoutPromptSendsCtrlC(t *testing.T) {
	pane := newFakeClaudePane("")
	pane.busy = true

	if err := (&Provider{tm: newFakeClaudeTmux(pane)}).Interrupt(pane.sessionName); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if !slices.Equal(pane.keys, []string{"C-c"}) {
		t.Fatalf("keys = %q, want exactly C-c", pane.keys)
	}
}

// A pane that cannot be read falls back to the Ctrl-C interrupt and logs
// why it could not look for a prompt first.
func TestClaudeInterruptFallsBackToCtrlCWhenThePaneCannotBeRead(t *testing.T) {
	pane := newFakeClaudePane("")
	pane.busy = true
	pane.captureErr = errors.New("capture-pane: timed out")
	pane.captureErrOnCapture = 1
	var logged []string
	p := &Provider{tm: newFakeClaudeTmux(pane), logf: func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}}

	if err := p.Interrupt(pane.sessionName); err != nil {
		t.Fatalf("Interrupt = %v, want the Ctrl-C fallback to succeed", err)
	}
	if !slices.Equal(pane.keys, []string{"C-c"}) {
		t.Fatalf("keys = %q, want exactly C-c", pane.keys)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "capture-pane: timed out") {
		t.Fatalf("logged %q, want one warning naming the capture error", logged)
	}
}

// Interrupt stays best-effort for a session that is gone.
func TestClaudeInterruptOfAGoneSessionIsANoOp(t *testing.T) {
	for _, gone := range []error{ErrSessionNotFound, ErrNoServer} {
		pane := newFakeClaudePane("")
		pane.captureErr = gone
		pane.captureErrOnCapture = 1

		if err := (&Provider{tm: newFakeClaudeTmux(pane)}).Interrupt(pane.sessionName); err != nil {
			t.Fatalf("Interrupt of a gone session (%v) = %v, want nil", gone, err)
		}
		if len(pane.keys) != 0 {
			t.Fatalf("keys = %q, want none", pane.keys)
		}
	}
}
