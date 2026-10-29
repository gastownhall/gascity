package herdr

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ── startup-dialog acceptance in start() (herdr's tmux Step 3, #7027) ────────
//
// The stateful fake herdr in panebinding_provider_test.go drives these: state
// file "trust_dialog" makes `pane read` show Claude's workspace-trust dialog
// until `pane send-keys … enter` confirms it (see the pane_read /
// pane_send-keys verbs added there), "pane_read_fails" makes the read error,
// and "trust_stuck" parks the cursor on "No, exit" and ignores movement keys.
// All of them assert on calls.log, so they pin exactly which herdr verbs the
// dialog step does — and does not — issue.

// dialogEnabledConfig returns a config whose managed hints turn the startup
// dialog step on, with a first turn to deliver (Nudge).
func dialogEnabledConfig() runtime.Config {
	return runtime.Config{
		Command:      "claude",
		ProcessNames: []string{"claude"},
		Nudge:        "do work",
	}
}

// AC-1: with acceptance enabled and the pane showing Claude's trust dialog
// (cursor on the trust row), start() answers it through the pane — both the
// read and the confirm land before the idle readiness wait and the first-turn
// prompt, and Start still succeeds.
func TestStartAcceptsWorkspaceTrustDialogBeforeFirstTurn(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "trust_dialog")

	if err := p.Start(context.Background(), "gastown__witness", dialogEnabledConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lines := strings.Split(fakeCalls(t, state), "\n")
	readIdx := logIndex(lines, "pane read %5 --source visible")
	enterIdx := logIndex(lines, "pane send-keys %5 enter")
	waitIdx := logIndex(lines, "agent wait")
	promptIdx := logIndex(lines, "agent prompt %5 do work")
	if readIdx < 0 || enterIdx < 0 {
		t.Fatalf("trust dialog was not answered through the pane; log:\n%s", strings.Join(lines, "\n"))
	}
	if waitIdx < 0 || promptIdx < 0 {
		t.Fatalf("startup tail (idle wait, first turn) missing; log:\n%s", strings.Join(lines, "\n"))
	}
	if readIdx > waitIdx || enterIdx > waitIdx || enterIdx > promptIdx {
		t.Fatalf("dialog acceptance must precede the readiness wait and the first turn; log:\n%s", strings.Join(lines, "\n"))
	}
}

// AC-2: with acceptance off — no managed hints, or an explicit false — the
// dialog step issues no pane reads and no send-keys, leaving the herdr call
// sequence unchanged. Regression guard for the gate.
func TestStartSkipsStartupDialogsWhenNotEnabled(t *testing.T) {
	disabled := false
	cases := []struct {
		name string
		cfg  runtime.Config
	}{
		{"no managed hints", runtime.Config{Command: "claude", Nudge: "do work"}},
		{"accept explicitly false", runtime.Config{
			Command:              "claude",
			ProcessNames:         []string{"claude"},
			AcceptStartupDialogs: &disabled,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			setState(t, state, "trust_dialog")
			if runtime.ShouldAcceptStartupDialogs(tc.cfg) {
				t.Fatal("test setup: dialog acceptance must be off")
			}

			if err := p.Start(context.Background(), "gastown__witness", tc.cfg); err != nil {
				t.Fatalf("Start: %v", err)
			}
			calls := fakeCalls(t, state)
			if strings.Contains(calls, "pane read") || strings.Contains(calls, "pane send-keys") {
				t.Fatalf("dialog step ran with acceptance disabled; log:\n%s", calls)
			}
		})
	}
}

// AC-3: a session with no startup text and no session_setup still gets its
// trust dialog answered — the step is outside the provisioning tail's gate,
// because a session parked on a dialog would swallow any later out-of-band
// nudge. No agent wait, no agent prompt.
func TestStartAcceptsStartupDialogsWithoutStartupTurn(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "trust_dialog")

	cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane send-keys %5 enter") {
		t.Fatalf("trust dialog was not answered for a session with no startup text:\n%s", calls)
	}
	if strings.Contains(calls, "agent wait") || strings.Contains(calls, "agent prompt") {
		t.Fatalf("a session with no startup text must not wait or prompt:\n%s", calls)
	}
}

// captureStartupDialogWarnings swaps startupDialogWarningOut for a buffer for
// the duration of the test and returns it.
func captureStartupDialogWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := startupDialogWarningOut
	startupDialogWarningOut = &buf
	t.Cleanup(func() { startupDialogWarningOut = old })
	return &buf
}

// AC-4: a dialog step that fails — pane read errors, or the trust cursor never
// reaches the trust row (ErrWorkspaceTrustUnconfirmed) — is non-fatal: Start
// returns nil, the tail still runs, exactly one warning line goes to
// startupDialogWarningOut, and Enter is never sent from an unconfirmed frame.
func TestStartStartupDialogFailureIsNonFatal(t *testing.T) {
	cases := []struct{ name, flag string }{
		{"pane read fails", "pane_read_fails"},
		{"trust cursor never reaches the trust row", "trust_stuck"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			setState(t, state, "trust_dialog")
			setState(t, state, tc.flag)
			warnings := captureStartupDialogWarnings(t)

			if err := p.Start(context.Background(), "gastown__witness", dialogEnabledConfig()); err != nil {
				t.Fatalf("Start: %v (a dialog step failure must not fail Start)", err)
			}
			calls := fakeCalls(t, state)
			if !strings.Contains(calls, "agent wait") || !strings.Contains(calls, "agent prompt %5 do work") {
				t.Fatalf("startup tail did not proceed after the dialog step failed:\n%s", calls)
			}
			if strings.Contains(calls, "pane send-keys %5 enter") {
				t.Fatalf("Enter was sent from an unconfirmed trust frame:\n%s", calls)
			}
			warnLines := strings.Split(strings.TrimRight(warnings.String(), "\n"), "\n")
			if len(warnLines) != 1 || !strings.HasPrefix(warnLines[0], `warning: session "gastown__witness": `) {
				t.Fatalf("startup-dialog warnings = %q; want exactly one warning line for gastown__witness", warnings.String())
			}
		})
	}
}
