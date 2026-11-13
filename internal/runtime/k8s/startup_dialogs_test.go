package k8s

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Claude's workspace-trust dialog with the cursor on the trust row and off it.
const (
	trustDialogOnTrustRow = "Quick safety check: Is this a project you created or one you trust?\n\n" +
		"❯ Yes, I trust this folder\n  No, exit\n\nEnter to confirm · Esc to cancel"
	trustDialogOffTrustRow = "Quick safety check: Is this a project you created or one you trust?\n\n" +
		"  Yes, I trust this folder\n❯ No, exit\n\nEnter to confirm · Esc to cancel"
	readyPrompt   = "> "
	startupNudge  = "do the work"
	oldTurnPrompt = "> earlier turn\nworking on the earlier turn..."
)

// fakePane scripts the in-pod tmux pane the k8s provider drives through its
// carrier: a workspace-trust dialog with a selectable cursor, the pane's
// scrollback history kept apart from its visible screen, and an ordered log
// of every pod interaction the startup tail makes.
type fakePane struct {
	// dialogOpen is whether the trust dialog is on the visible screen.
	dialogOpen bool
	// cursorOnTrust is whether the dialog's cursor is on the trust row.
	cursorOnTrust bool
	// movesIgnored drops selection moves, as an agent that never applies them.
	movesIgnored bool
	// blankFrames is how many captures show an empty screen before the
	// dialog (or the ready prompt) renders.
	blankFrames int
	// history is scrollback left in the pane by an earlier agent.
	history string

	nudgeIntoDialog bool
	log             []string
}

func (f *fakePane) exec(_ string, cmd []string) (string, error) {
	if len(cmd) < 2 {
		return "", nil
	}
	switch {
	case cmd[0] == "sh" && len(cmd) >= 3 && strings.Contains(cmd[2], "respawn-pane"):
		f.log = append(f.log, "respawn")
	case cmd[0] != "tmux":
	case cmd[1] == "has-session":
		f.log = append(f.log, "has-session")
	case cmd[1] == "pipe-pane":
		f.log = append(f.log, "setup")
	case cmd[1] == "clear-history":
		f.log = append(f.log, "clear-history")
		f.history = ""
	case cmd[1] == "capture-pane":
		f.log = append(f.log, "capture")
		return f.screen(), nil
	case cmd[1] == "send-keys":
		f.sendKeys(cmd[2:])
	}
	return "", nil
}

// screen is what capture-pane returns: scrollback history, then the visible
// screen.
func (f *fakePane) screen() string {
	visible := readyPrompt
	switch {
	case f.blankFrames > 0:
		f.blankFrames--
		visible = ""
	case f.dialogOpen && f.cursorOnTrust:
		visible = trustDialogOnTrustRow
	case f.dialogOpen:
		visible = trustDialogOffTrustRow
	}
	if f.history == "" {
		return visible
	}
	return f.history + "\n" + visible
}

// sendKeys models send-keys args after "send-keys": "-t main" then either
// "-l <text>" (literal nudge text) or bare key names.
func (f *fakePane) sendKeys(args []string) {
	if len(args) >= 2 && args[0] == "-t" {
		args = args[2:]
	}
	if len(args) >= 1 && args[0] == "-l" {
		f.log = append(f.log, "nudge-text")
		if f.dialogOpen {
			f.nudgeIntoDialog = true
		}
		return
	}
	for _, key := range args {
		switch key {
		case "Up", "Down":
			f.log = append(f.log, "move")
			if !f.movesIgnored {
				f.cursorOnTrust = true
			}
		case "Enter":
			switch {
			case f.dialogOpen && f.cursorOnTrust:
				f.dialogOpen = false
				f.log = append(f.log, "accept")
			case f.dialogOpen:
				f.dialogOpen = false
				f.log = append(f.log, "enter-wrong-row")
			default:
				f.log = append(f.log, "submit")
			}
		}
	}
}

// index returns the position of the first event in the log, or -1.
func (f *fakePane) index(event string) int {
	for i, e := range f.log {
		if e == event {
			return i
		}
	}
	return -1
}

// lastIndex returns the position of the last event in the log, or -1.
func (f *fakePane) lastIndex(event string) int {
	for i := len(f.log) - 1; i >= 0; i-- {
		if f.log[i] == event {
			return i
		}
	}
	return -1
}

func (f *fakePane) count(event string) int {
	n := 0
	for _, e := range f.log {
		if e == event {
			n++
		}
	}
	return n
}

// newDialogProvider returns a provider whose fake ops answer every pod exec
// from pane.
func newDialogProvider(pane *fakePane) *Provider {
	fake := newFakeK8sOps()
	fake.execFunc = pane.exec
	p := newProviderWithOps(fake)
	p.postStartSettle = 0
	return p
}

func dialogStartConfig() runtime.Config {
	return runtime.Config{
		Command:      "claude --settings .gc/settings.json",
		ProcessNames: []string{"claude"},
		Nudge:        startupNudge,
		Env:          map[string]string{"GC_CITY": "/workspace"},
	}
}

// assertAcceptedBeforeNudge checks the trust dialog was confirmed on its
// trust row, after the post-launch setup, and before the post-pass liveness
// probe and the initial nudge.
func assertAcceptedBeforeNudge(t *testing.T, pane *fakePane) {
	t.Helper()
	accept := pane.index("accept")
	if accept < 0 {
		t.Fatalf("trust dialog never accepted; log = %v", pane.log)
	}
	if pane.count("enter-wrong-row") > 0 {
		t.Fatalf("Enter confirmed the wrong row; log = %v", pane.log)
	}
	if setup := pane.lastIndex("setup"); setup < 0 || setup > accept {
		t.Fatalf("post-launch setup (%d) must precede acceptance (%d); log = %v", setup, accept, pane.log)
	}
	if pane.nudgeIntoDialog {
		t.Fatalf("initial nudge typed into the open trust dialog; log = %v", pane.log)
	}
	nudge := pane.index("nudge-text")
	if nudge < accept {
		t.Fatalf("initial nudge (%d) must follow acceptance (%d); log = %v", nudge, accept, pane.log)
	}
	if pane.index("submit") < nudge {
		t.Fatalf("nudge never submitted after its text; log = %v", pane.log)
	}
}

// TestStartupDialogsStartAcceptsBeforeLivenessAndNudge covers AC-1 for each
// way acceptance is enabled: setup, then acceptance, then the post-pass
// liveness probe, then the initial nudge.
func TestStartupDialogsStartAcceptsBeforeLivenessAndNudge(t *testing.T) {
	cases := map[string]func(*runtime.Config){
		"process names": func(*runtime.Config) {},
		"permission warning": func(c *runtime.Config) {
			c.ProcessNames = nil
			c.EmitsPermissionWarning = true
		},
		"explicit true without process names": func(c *runtime.Config) {
			c.ProcessNames = nil
			c.AcceptStartupDialogs = boolPtr(true)
		},
	}
	for label, mutate := range cases {
		t.Run(label, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pane := &fakePane{dialogOpen: true, cursorOnTrust: true}
				p := newDialogProvider(pane)
				cfg := dialogStartConfig()
				mutate(&cfg)

				if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
					t.Fatalf("Start: %v", err)
				}
				assertAcceptedBeforeNudge(t, pane)
				liveness := pane.lastIndex("has-session")
				if accept := pane.index("accept"); liveness < accept {
					t.Fatalf("post-start liveness probe (%d) must follow acceptance (%d); log = %v", liveness, accept, pane.log)
				}
				if nudge := pane.index("nudge-text"); liveness > nudge {
					t.Fatalf("post-start liveness probe (%d) must precede the nudge (%d); log = %v", liveness, nudge, pane.log)
				}
			})
		})
	}
}

// TestStartupDialogsStartAcceptsWithoutNudge: a session with no initial nudge
// still gets past its dialog, so a later nudge reaches the agent.
func TestStartupDialogsStartAcceptsWithoutNudge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pane := &fakePane{dialogOpen: true, cursorOnTrust: true}
		p := newDialogProvider(pane)
		cfg := dialogStartConfig()
		cfg.Nudge = ""

		if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if pane.index("accept") < 0 {
			t.Fatalf("trust dialog never accepted; log = %v", pane.log)
		}
		if pane.count("nudge-text") != 0 {
			t.Fatalf("Start typed a nudge it was not configured with; log = %v", pane.log)
		}
	})
}

// TestStartupDialogsStartDelayedDialog: a dialog that renders after blank
// frames is still answered before the nudge.
func TestStartupDialogsStartDelayedDialog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pane := &fakePane{dialogOpen: true, cursorOnTrust: true, blankFrames: 4}
		p := newDialogProvider(pane)

		if err := p.Start(context.Background(), "gc-test-agent", dialogStartConfig()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		assertAcceptedBeforeNudge(t, pane)
	})
}

// TestStartupDialogsStartBlankPaneEnds: a pane that never draws anything
// recognizable ends the bounded pass, and Start still nudges.
func TestStartupDialogsStartBlankPaneEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pane := &fakePane{blankFrames: 1 << 30}
		p := newDialogProvider(pane)

		if err := p.Start(context.Background(), "gc-test-agent", dialogStartConfig()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if pane.count("capture") < 2 {
			t.Fatalf("the pass never polled the blank pane; log = %v", pane.log)
		}
		if pane.index("nudge-text") < 0 {
			t.Fatalf("Start never nudged after the pass ended; log = %v", pane.log)
		}
	})
}

// paneShowsReadyPrompt makes every pane capture show a ready prompt, so a
// fixture whose subject is not startup dialogs passes the dialog pass at once.
// It wraps the fake's existing behavior, so call it after setting execFunc.
func paneShowsReadyPrompt(fake *fakeK8sOps) {
	next := fake.execFunc
	fake.execFunc = func(pod string, cmd []string) (string, error) {
		if len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == "capture-pane" {
			return readyPrompt, nil
		}
		if next != nil {
			return next(pod, cmd)
		}
		key := execKey(pod, cmd)
		if err, ok := fake.execErr[key]; ok {
			return "", err
		}
		return fake.execOutput[key], nil
	}
}

// podMutations counts the createPod and deletePod calls the fake saw.
func podMutations(p *Provider) int {
	n := 0
	for _, c := range p.ops.(*fakeK8sOps).calls {
		if c.method == "createPod" || c.method == "deletePod" {
			n++
		}
	}
	return n
}

// newWarmDialogProvider returns a provider with a warm, running pod "s" whose
// pane answers from pane.
func newWarmDialogProvider(pane *fakePane) *Provider {
	p := newDialogProvider(pane)
	addRunningPod(p.ops.(*fakeK8sOps), "s", "s")
	return p
}

// TestStartupDialogsRelaunchClearsHistoryAndAcceptsDelayedDialog covers AC-2:
// the warm pane's old prompt must not end the pass before the new agent's
// delayed trust dialog renders.
func TestStartupDialogsRelaunchClearsHistoryAndAcceptsDelayedDialog(t *testing.T) {
	cases := map[string]int{
		"dialog after blank frames":   3,
		"dialog visible when cleared": 0,
	}
	for label, blank := range cases {
		t.Run(label, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pane := &fakePane{dialogOpen: true, cursorOnTrust: true, blankFrames: blank, history: oldTurnPrompt}
				p := newWarmDialogProvider(pane)

				if err := p.Relaunch(context.Background(), "s", dialogStartConfig()); err != nil {
					t.Fatalf("Relaunch: %v", err)
				}
				cleared := pane.index("clear-history")
				if cleared < 0 {
					t.Fatalf("Relaunch never cleared the warm pane's history; log = %v", pane.log)
				}
				if respawn := pane.index("respawn"); respawn > cleared {
					t.Fatalf("history cleared (%d) before the respawn (%d); log = %v", cleared, respawn, pane.log)
				}
				if capture := pane.index("capture"); capture < cleared {
					t.Fatalf("pane polled (%d) before its history was cleared (%d); log = %v", capture, cleared, pane.log)
				}
				assertAcceptedBeforeNudge(t, pane)
				if n := podMutations(p); n != 0 {
					t.Fatalf("Relaunch created or deleted %d pods; want the warm pod kept", n)
				}
			})
		})
	}
}

// TestStartupDialogsSkippedWhenNotEnabled covers AC-3: with acceptance
// explicitly off, a one-shot lifecycle (even with process hints), or only a
// nudge, Start and Relaunch read no pane, clear no history, send no dialog
// keys, and deliver the initial nudge as before.
func TestStartupDialogsSkippedWhenNotEnabled(t *testing.T) {
	configs := map[string]func(*runtime.Config){
		"explicit false": func(c *runtime.Config) {
			c.AcceptStartupDialogs = boolPtr(false)
		},
		"one-shot with hints": func(c *runtime.Config) {
			c.Lifecycle = runtime.LifecycleOneShot
			c.EmitsPermissionWarning = true
		},
		"nudge only": func(c *runtime.Config) {
			c.ProcessNames = nil
		},
	}
	ops := map[string]func(*Provider, runtime.Config) error{
		"start": func(p *Provider, cfg runtime.Config) error {
			return p.Start(context.Background(), "gc-test-agent", cfg)
		},
		"relaunch": func(p *Provider, cfg runtime.Config) error {
			addRunningPod(p.ops.(*fakeK8sOps), "gc-test-agent", "gc-test-agent")
			return p.Relaunch(context.Background(), "gc-test-agent", cfg)
		},
	}
	for opName, op := range ops {
		for cfgName, mutate := range configs {
			t.Run(opName+"/"+cfgName, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					pane := &fakePane{history: oldTurnPrompt}
					p := newDialogProvider(pane)
					cfg := dialogStartConfig()
					mutate(&cfg)

					if err := op(p, cfg); err != nil {
						t.Fatalf("%s: %v", opName, err)
					}
					for _, event := range []string{"capture", "clear-history", "move"} {
						if n := pane.count(event); n != 0 {
							t.Fatalf("dialog pass ran (%d %s events); log = %v", n, event, pane.log)
						}
					}
					nudge := pane.index("nudge-text")
					if nudge < 0 || pane.lastIndex("has-session") > nudge || pane.index("submit") < nudge {
						t.Fatalf("initial nudge not delivered as before; log = %v", pane.log)
					}
				})
			})
		}
	}
}
