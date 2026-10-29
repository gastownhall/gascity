package herdr

import (
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
