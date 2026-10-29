package herdr

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ── startup-dialog acceptance against the fake herdr (issue #7027) ──────────
//
// herdr's equivalent of tmux's doStartSession Step 3: a freshly launched agent
// that stops on its workspace-trust dialog must be answered through the pane
// before the idle wait and first turn, or the agent stalls until a human
// attaches and every turn typed at it lands in the dialog. The fake herdr's
// trust_dialog state shows Claude's "Quick safety check" until an enter lands.

// captureStartupDialogWarnings redirects startupDialogWarningOut for one test.
func captureStartupDialogWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := startupDialogWarningOut
	startupDialogWarningOut = &buf
	t.Cleanup(func() { startupDialogWarningOut = prev })
	return &buf
}

// The trust dialog is confirmed through pane-scoped read/send-keys on the
// launch's pane, before readiness and before the first turn.
func TestStartAcceptsWorkspaceTrustDialogBeforeFirstTurn(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "trust_dialog")
	warnings := captureStartupDialogWarnings(t)

	cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, Nudge: "do work"}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	read := strings.Index(calls, "pane read %5 --source visible")
	enter := strings.Index(calls, "pane send-keys %5 enter")
	if read < 0 || enter < 0 {
		t.Fatalf("trust dialog was not read and confirmed on the launch pane:\n%s", calls)
	}
	if read > enter {
		t.Fatalf("enter was sent before the pane was read:\n%s", calls)
	}
	if wait := strings.Index(calls, "agent wait"); wait < 0 || enter > wait {
		t.Fatalf("trust dialog must be confirmed before the readiness wait:\n%s", calls)
	}
	if prompt := strings.Index(calls, "agent prompt %5 do work"); prompt < 0 || enter > prompt {
		t.Fatalf("trust dialog must be confirmed before the first turn:\n%s", calls)
	}
	if warnings.Len() != 0 {
		t.Fatalf("a confirmed dialog must not warn: %q", warnings.String())
	}
}
