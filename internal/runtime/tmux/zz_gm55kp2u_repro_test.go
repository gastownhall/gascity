package tmux

import "testing"

// askUserQuestionPane is a live capture (Claude Code v2.1.280, fullscreen TUI)
// of an open AskUserQuestion dialog; header lines sanitized. gm-55kp2u.
const askUserQuestionPane = ` ▐▛███▛█   Claude Code v2.1.280
▝▜██████▀  Sonnet 5 · Claude Max
  ▝▝ ▝▝    /home/user/projects/gc-management/.gc/worktrees/beads/deployer
❯ Check your hook and continue the deploy.
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 ☐ be-tx30v
│ Push deploy/be-tx30v-gate to headfork and open the PR against gastownhall/beads. How do you want to proceed?
❯ 1. Run gates, then ship if clean (Recommended)
     Run gates, then ship if clean
  2. Run gates, ask again before push
     Run gates, ask again before push
  3. Keep holding
     Keep holding
  4. Type something.
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
  5. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel
`

// An open AskUserQuestion owns the pane's input: a submitted Enter selects the
// focused option. Pending must report it so guarded send paths refuse to type.
func TestGM55KP2UProviderPendingDetectsAskUserQuestionDialog(t *testing.T) {
	fe := &fakeExecutor{out: askUserQuestionPane}
	provider := &Provider{tm: &Tmux{cfg: Config{SocketName: "gm55kp2u"}, exec: fe}}
	pending, err := provider.Pending("auq")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil {
		t.Fatal("Pending = nil for an open AskUserQuestion dialog; a nudge's Enter will answer it with option 1")
	}
}

// The dialog's focused row renders as "❯ 1. …" and past prompts in scrollback
// render as "❯ …"; neither is an idle composer while the dialog is open.
func TestGM55KP2USnapshotPaneIdleRejectsAskUserQuestionDialog(t *testing.T) {
	tm := &Tmux{cfg: Config{SocketName: "gm55kp2u"}, exec: &fakeExecutor{out: askUserQuestionPane}}
	idle, err := tm.snapshotPaneIdleWithPrefix("auq", DefaultReadyPromptPrefix)
	if err != nil {
		t.Fatalf("snapshotPaneIdleWithPrefix: %v", err)
	}
	if idle {
		t.Fatal("open AskUserQuestion dialog reads as an idle prompt; WaitForIdle returns nil and Provider.Nudge submits into it")
	}
}

// Every typed-nudge path (queue poller, claim backstops, drain reminders,
// warm-bind, mail notify, sling, gc nudge) funnels into nudgeSession. It must
// not paste or press Enter while an AskUserQuestion owns the pane.
func TestGM55KP2UNudgeSessionSendsNoKeysIntoAskUserQuestionDialog(t *testing.T) {
	fe := &fakeExecutor{out: askUserQuestionPane}
	cfg := DefaultConfig()
	cfg.SocketName = "gm55kp2u"
	tm := &Tmux{cfg: cfg, exec: fe}
	_ = tm.NudgeSession("auq", "<system-reminder>\n- [sling] Work slung. Check your hook.\n</system-reminder>\n")
	for _, call := range fe.calls {
		for i, a := range call {
			if a == "send-keys" {
				t.Fatalf("sent keys into an open AskUserQuestion dialog: %q", call[i:])
			}
		}
	}
}
