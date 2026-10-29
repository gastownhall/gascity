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

// With acceptance disabled the dialog step issues nothing, so the herdr call
// sequence is the one Start issued before the step existed.
func TestStartSkipsStartupDialogsWhenNotEnabled(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name string
		cfg  runtime.Config
	}{
		{"no hints", runtime.Config{Command: "claude", Nudge: "do work"}},
		{"explicitly off", runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, AcceptStartupDialogs: &off, Nudge: "do work"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.ShouldAcceptStartupDialogs(tc.cfg) {
				t.Fatal("test setup: ShouldAcceptStartupDialogs(cfg) must be false")
			}
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			setState(t, state, "trust_dialog")

			if err := p.Start(context.Background(), "gastown__worker", tc.cfg); err != nil {
				t.Fatalf("Start: %v", err)
			}
			calls := fakeCalls(t, state)
			if strings.Contains(calls, "pane read") || strings.Contains(calls, "pane send-keys") {
				t.Fatalf("disabled dialog acceptance still touched the pane:\n%s", calls)
			}
			if !strings.Contains(calls, "agent prompt %5 do work") {
				t.Fatalf("first turn was not delivered:\n%s", calls)
			}
		})
	}
}

// A session with no startup text and no session_setup skips the provisioning
// tail, but its trust dialog must still be answered: otherwise a later nudge
// lands on the live dialog.
func TestStartAcceptsStartupDialogsWithoutStartupTurn(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "trust_dialog")
	captureStartupDialogWarnings(t)

	cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane send-keys %5 enter") {
		t.Fatalf("trust dialog was not confirmed for a session without a startup turn:\n%s", calls)
	}
	if strings.Contains(calls, "agent wait") || strings.Contains(calls, "agent prompt") {
		t.Fatalf("a session without a startup turn must not run the provisioning tail:\n%s", calls)
	}
}

// A failed dialog step is best-effort: Start still succeeds and delivers the
// first turn, warns exactly once, and never confirms an unconfirmed trust frame.
func TestStartStartupDialogFailureIsNonFatal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		states []string
		want   string
	}{
		{"pane read fails", []string{"pane_read_fails"}, "pane not found"},
		{"trust cursor stuck", []string{"trust_dialog", "trust_stuck"}, runtime.ErrWorkspaceTrustUnconfirmed.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			for _, s := range tc.states {
				setState(t, state, s)
			}
			warnings := captureStartupDialogWarnings(t)

			cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, Nudge: "do work"}
			if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
				t.Fatalf("Start must not fail on a dialog-step error: %v", err)
			}
			calls := fakeCalls(t, state)
			if !strings.Contains(calls, "agent wait") || !strings.Contains(calls, "agent prompt %5 do work") {
				t.Fatalf("Start did not proceed to the idle wait and first turn:\n%s", calls)
			}
			if strings.Contains(calls, "pane send-keys %5 enter") {
				t.Fatalf("enter was sent from an unconfirmed trust frame:\n%s", calls)
			}
			lines := strings.Split(strings.TrimRight(warnings.String(), "\n"), "\n")
			if len(lines) != 1 || !strings.HasPrefix(lines[0], `warning: session "gastown__worker": `) {
				t.Fatalf("want exactly one session warning line, got %q", warnings.String())
			}
			if !strings.Contains(lines[0], tc.want) {
				t.Fatalf("warning %q does not carry the cause %q", lines[0], tc.want)
			}
		})
	}
}
