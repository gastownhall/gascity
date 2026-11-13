package k8s

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	execerr "k8s.io/client-go/util/exec"

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

// earlierAgentRecord is a startupDialogsUnresolvedKey record left on a warm
// pod's session by an earlier agent.
const earlierAgentRecord = "2026-01-01T00:00:00Z initial nudge withheld: earlier agent"

var (
	// errTmuxNoServer is how a tmux command exits in the pod once the agent
	// has exited and taken its session, and so the tmux server, with it.
	errTmuxNoServer = execerr.CodeExitError{Err: errors.New("no server running on /tmp/tmux-0/default"), Code: 1}
	// errExecBlip is a pod exec that failed in the apiserver, not in the pod.
	errExecBlip = errors.New("error dialing backend: EOF")
)

// externalImportsModal is Claude's "Allow external CLAUDE.md file imports?"
// modal listing path. Its pre-selected option allows the imports.
func externalImportsModal(path string) string {
	return "Allow external CLAUDE.md file imports?\n" +
		"This project's CLAUDE.md imports files outside the current working directory. " +
		"Never allow this for third-party repositories.\n\n" +
		"External imports:\n  " + path + "\n\n" +
		"❯ 1. Yes, allow external imports\n  2. No, disable external imports\n\n" +
		"Enter to confirm · Esc to cancel"
}

// fakePane scripts the in-pod tmux pane the k8s provider drives through its
// carrier: a workspace-trust dialog with a selectable cursor, Claude's
// external-imports modal behind it, the pane's scrollback history kept apart
// from its visible screen, the session's tmux environment, and an ordered log
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
	// ready is the visible screen once no dialog is open: the agent's output
	// above its prompt. Empty means readyPrompt alone.
	ready string
	// history is scrollback left in the pane by an earlier agent.
	history string

	// imports is a file the project's CLAUDE.md imports from outside the work
	// dir. When set, accepting the trust dialog opens the external-imports
	// modal listing it, and any Enter that reaches the modal allows it.
	imports     string
	importsOpen bool
	// gitCommonDir is what `git rev-parse --git-common-dir` prints in the
	// pod; empty means the work dir is in no repository (git exits 128).
	gitCommonDir string
	// gitDirs is the -C directory of every git query run in the pod.
	gitDirs []string

	// dieAfterProbe is the has-session probe after which the agent exits and
	// takes its tmux session with it, so tmux commands exit 1 from then on.
	// Zero keeps the session alive.
	dieAfterProbe int
	probes        int
	dead          bool

	// env is the session's tmux environment (SetMeta, RemoveMeta).
	env map[string]string

	// failOn makes the named tmux events fail with a transport error.
	failOn map[string]error
	// captureBlips fails that many captures with errExecBlip first.
	captureBlips int
	// screenBlips fails that many captures of the visible screen alone, the
	// read after the pass, with errExecBlip first.
	screenBlips int
	// cancelOn names the event whose first occurrence calls cancel.
	cancelOn string
	cancel   context.CancelFunc

	nudgeIntoDialog bool
	log             []string
}

// record appends event to the log and fires cancel on the cancelOn event.
func (f *fakePane) record(event string) {
	f.log = append(f.log, event)
	if f.cancel != nil && event == f.cancelOn {
		f.cancel()
		f.cancel = nil
	}
}

func (f *fakePane) exec(_ string, cmd []string) (string, error) {
	if len(cmd) < 2 {
		return "", nil
	}
	switch {
	case cmd[0] == "git":
		return f.gitQuery(cmd)
	case cmd[0] == "sh" && len(cmd) >= 3 && strings.Contains(cmd[2], "respawn-pane"):
		f.record("respawn")
	case cmd[0] != "tmux":
	case cmd[1] == "has-session":
		f.record("has-session")
		return "", f.probe()
	case cmd[1] == "pipe-pane":
		f.record("setup")
	case cmd[1] == "clear-history":
		f.record("clear-history")
		if err := f.failOn["clear-history"]; err != nil {
			return "", err
		}
		f.history = ""
	case cmd[1] == "capture-pane":
		return f.capture(cmd)
	case cmd[1] == "send-keys":
		if f.dead {
			return "", errTmuxNoServer
		}
		f.sendKeys(cmd[2:])
	case cmd[1] == "set-environment":
		f.setEnvironment(cmd[2:])
	}
	return "", nil
}

// gitQuery answers `git -C <dir> rev-parse --git-common-dir` run in the pod.
func (f *fakePane) gitQuery(cmd []string) (string, error) {
	if len(cmd) >= 3 && cmd[1] == "-C" {
		f.gitDirs = append(f.gitDirs, cmd[2])
	}
	if f.gitCommonDir == "" {
		return "", execerr.CodeExitError{Err: errors.New("fatal: not a git repository"), Code: 128}
	}
	return f.gitCommonDir + "\n", nil
}

// probe answers has-session: alive until the session dies, then tmux's
// "no server running" exit.
func (f *fakePane) probe() error {
	if f.dead {
		return errTmuxNoServer
	}
	f.probes++
	if f.dieAfterProbe > 0 && f.probes >= f.dieAfterProbe {
		f.dead = true
	}
	return nil
}

// capture answers capture-pane: scrollback history then the visible screen
// when cmd names a start line (-S), the visible screen alone otherwise.
func (f *fakePane) capture(cmd []string) (string, error) {
	f.record("capture")
	screenOnly := !slices.Contains(cmd, "-S")
	switch {
	case f.dead:
		return "", errTmuxNoServer
	case f.captureBlips > 0:
		f.captureBlips--
		return "", errExecBlip
	case screenOnly && f.screenBlips > 0:
		f.screenBlips--
		return "", errExecBlip
	}
	if err := f.failOn["capture"]; err != nil {
		return "", err
	}
	if screenOnly {
		return f.visible(), nil
	}
	if f.history == "" {
		return f.visible(), nil
	}
	return f.history + "\n" + f.visible(), nil
}

// visible is the pane's visible screen.
func (f *fakePane) visible() string {
	switch {
	case f.blankFrames > 0:
		f.blankFrames--
		return ""
	case f.dialogOpen && f.cursorOnTrust:
		return trustDialogOnTrustRow
	case f.dialogOpen:
		return trustDialogOffTrustRow
	case f.importsOpen:
		return externalImportsModal(f.imports)
	case f.ready != "":
		return f.ready
	}
	return readyPrompt
}

// setEnvironment applies set-environment args after "set-environment":
// "-t main" then "KEY VALUE" or "-u KEY".
func (f *fakePane) setEnvironment(args []string) {
	if len(args) >= 2 && args[0] == "-t" {
		args = args[2:]
	}
	if len(args) != 2 {
		return
	}
	if f.env == nil {
		f.env = map[string]string{}
	}
	if args[0] == "-u" {
		delete(f.env, args[1])
		return
	}
	f.env[args[0]] = args[1]
}

// sendKeys models send-keys args after "send-keys": "-t main" then either
// "-l <text>" (literal nudge text) or bare key names.
func (f *fakePane) sendKeys(args []string) {
	if len(args) >= 2 && args[0] == "-t" {
		args = args[2:]
	}
	if len(args) >= 1 && args[0] == "-l" {
		f.record("nudge-text")
		if f.dialogOpen || f.importsOpen {
			f.nudgeIntoDialog = true
		}
		return
	}
	for _, key := range args {
		switch key {
		case "Up", "Down":
			f.record("move")
			if !f.movesIgnored {
				f.cursorOnTrust = true
			}
		case "Enter":
			switch {
			case f.dialogOpen && f.cursorOnTrust:
				f.dialogOpen = false
				f.importsOpen = f.imports != ""
				f.record("accept")
			case f.dialogOpen:
				f.dialogOpen = false
				f.record("enter-wrong-row")
			case f.importsOpen:
				f.importsOpen = false
				f.record("imports-allowed")
			default:
				f.record("submit")
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

// TestStartupDialogsCancellation covers AC-4: a ctx that ends while the
// startup-dialog pass (or Relaunch's history clearing) runs fails the
// operation with the ctx error and skips the post-pass liveness probe and the
// nudge. Start removes the pod it created; Relaunch keeps the warm pod.
func TestStartupDialogsCancellation(t *testing.T) {
	cases := []struct {
		name     string
		relaunch bool
		cancelOn string
		deadline bool
	}{
		{name: "start canceled while polling", cancelOn: "capture"},
		{name: "start deadline while polling", deadline: true},
		{name: "relaunch canceled while polling", relaunch: true, cancelOn: "capture"},
		{name: "relaunch deadline while polling", relaunch: true, deadline: true},
		{name: "relaunch canceled while clearing history", relaunch: true, cancelOn: "clear-history"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// A blank pane keeps the pass polling until the ctx ends.
				pane := &fakePane{blankFrames: 1 << 30, cancelOn: tc.cancelOn}
				p := newDialogProvider(pane)
				fake := p.ops.(*fakeK8sOps)

				var ctx context.Context
				var cancel context.CancelFunc
				want := context.Canceled
				if tc.deadline {
					ctx, cancel = context.WithTimeout(context.Background(), runtime.StartupDialogTimeout()/2)
					want = context.DeadlineExceeded
				} else {
					ctx, cancel = context.WithCancel(context.Background())
					pane.cancel = cancel
				}
				defer cancel()

				var err error
				if tc.relaunch {
					addRunningPod(fake, "gc-test-agent", "gc-test-agent")
					err = p.Relaunch(ctx, "gc-test-agent", dialogStartConfig())
				} else {
					err = p.Start(ctx, "gc-test-agent", dialogStartConfig())
				}
				if !errors.Is(err, want) {
					t.Fatalf("error = %v, want %v", err, want)
				}

				passStart := pane.index("clear-history")
				if passStart < 0 {
					passStart = pane.index("capture")
				}
				if passStart < 0 {
					t.Fatalf("the startup-dialog pass never ran; log = %v", pane.log)
				}
				if pane.lastIndex("has-session") > passStart {
					t.Fatalf("liveness probed after the canceled pass; log = %v", pane.log)
				}
				if pane.count("nudge-text") != 0 || pane.count("submit") != 0 {
					t.Fatalf("nudge sent after the canceled pass; log = %v", pane.log)
				}
				_, podLeft := fake.pods["gc-test-agent"]
				if tc.relaunch {
					if !podLeft || podMutations(p) != 0 {
						t.Fatalf("Relaunch did not keep the warm pod (left=%v, mutations=%d)", podLeft, podMutations(p))
					}
				} else if podLeft {
					t.Fatal("Start left the pod it created after the canceled pass")
				}
			})
		})
	}
}

// TestStartupDialogsTransportFailuresAreBestEffort: with a live ctx, a pane
// capture or history-clear failure is reported on stderr, naming the session,
// and the operation still succeeds and probes liveness. A failed history
// clear leaves the pane readable, so the initial nudge goes. A pane that
// cannot be read at all cannot show that the pass left no dialog on it, so
// the nudge is withheld, and the failed pass and the withheld nudge are
// recorded on the session.
func TestStartupDialogsTransportFailuresAreBestEffort(t *testing.T) {
	transportErr := errors.New("error dialing backend: connection refused")
	cases := []struct {
		name      string
		relaunch  bool
		failOn    string
		wantNudge bool
	}{
		{name: "start capture fails", failOn: "capture"},
		{name: "relaunch capture fails", relaunch: true, failOn: "capture"},
		{name: "relaunch history clear fails", relaunch: true, failOn: "clear-history", wantNudge: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pane := &fakePane{failOn: map[string]error{tc.failOn: transportErr}}
				p := newDialogProvider(pane)
				var stderr strings.Builder
				p.stderr = &stderr

				var err error
				if tc.relaunch {
					addRunningPod(p.ops.(*fakeK8sOps), "gc-test-agent", "gc-test-agent")
					err = p.Relaunch(context.Background(), "gc-test-agent", dialogStartConfig())
				} else {
					err = p.Start(context.Background(), "gc-test-agent", dialogStartConfig())
				}
				if err != nil {
					t.Fatalf("a best-effort %s failure failed the operation: %v", tc.failOn, err)
				}
				if got := stderr.String(); !strings.Contains(got, `"gc-test-agent"`) || !strings.Contains(got, transportErr.Error()) {
					t.Fatalf("stderr = %q, want a warning naming the session and the failure", got)
				}
				probe := pane.lastIndex("has-session")
				if probe < pane.index(tc.failOn) {
					t.Fatalf("liveness not probed after the failed %s; log = %v", tc.failOn, pane.log)
				}
				nudge := pane.index("nudge-text")
				if !tc.wantNudge {
					if nudge >= 0 {
						t.Fatalf("nudge typed into a screen that could not be read; log = %v", pane.log)
					}
					record := pane.env[startupDialogsUnresolvedKey]
					if !strings.Contains(record, "withheld") || !strings.Contains(record, "unreadable") || !strings.Contains(record, transportErr.Error()) {
						t.Fatalf("session record %s = %q, want the failed pass and the withheld nudge recorded", startupDialogsUnresolvedKey, record)
					}
					return
				}
				if nudge < 0 || probe > nudge {
					t.Fatalf("liveness then nudge did not follow the failed %s; log = %v", tc.failOn, pane.log)
				}
			})
		})
	}
}

// TestStartupDialogsTrustSelection covers AC-5: a trust dialog whose cursor
// starts off the trust row is confirmed only once the shared helper sees the
// cursor on that row. When moves never get it there, the session stays up, a
// warning names it, and the initial nudge is withheld so its Enter cannot
// answer the dialog.
func TestStartupDialogsTrustSelection(t *testing.T) {
	ops := map[string]func(*Provider) error{
		"start": func(p *Provider) error {
			return p.Start(context.Background(), "gc-test-agent", dialogStartConfig())
		},
		"relaunch": func(p *Provider) error {
			addRunningPod(p.ops.(*fakeK8sOps), "gc-test-agent", "gc-test-agent")
			return p.Relaunch(context.Background(), "gc-test-agent", dialogStartConfig())
		},
	}
	for opName, op := range ops {
		t.Run(opName+"/moves reach the trust row", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pane := &fakePane{dialogOpen: true}
				p := newDialogProvider(pane)

				if err := op(p); err != nil {
					t.Fatalf("%s: %v", opName, err)
				}
				if move, accept := pane.index("move"), pane.index("accept"); move < 0 || move > accept {
					t.Fatalf("Enter not preceded by a selection move; log = %v", pane.log)
				}
				assertAcceptedBeforeNudge(t, pane)
			})
		})
		t.Run(opName+"/moves never reach the trust row", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pane := &fakePane{dialogOpen: true, movesIgnored: true}
				p := newDialogProvider(pane)
				var stderr strings.Builder
				p.stderr = &stderr

				if err := op(p); err != nil {
					t.Fatalf("%s failed a live session left on an unconfirmed dialog: %v", opName, err)
				}
				if pane.count("move") == 0 {
					t.Fatalf("the helper never tried to move the selection; log = %v", pane.log)
				}
				for _, event := range []string{"accept", "enter-wrong-row", "nudge-text", "submit"} {
					if n := pane.count(event); n != 0 {
						t.Fatalf("%d %s events with the trust row unconfirmed; log = %v", n, event, pane.log)
					}
				}
				if _, alive := p.ops.(*fakeK8sOps).pods["gc-test-agent"]; !alive {
					t.Fatal("the pod was removed; the live session must stay running")
				}
				got := stderr.String()
				if !strings.Contains(got, `"gc-test-agent"`) || !strings.Contains(got, "withheld") {
					t.Fatalf("stderr = %q, want a warning naming the session and the withheld nudge", got)
				}
				if record := pane.env[startupDialogsUnresolvedKey]; !strings.Contains(record, "withheld") {
					t.Fatalf("session record %s = %q, want the withheld nudge recorded", startupDialogsUnresolvedKey, record)
				}
			})
		})
	}
}

// dialogOps runs Start, or Relaunch into a warm pod, for gc-test-agent.
var dialogOps = map[string]func(context.Context, *Provider, runtime.Config) error{
	"start": func(ctx context.Context, p *Provider, cfg runtime.Config) error {
		return p.Start(ctx, "gc-test-agent", cfg)
	},
	"relaunch": func(ctx context.Context, p *Provider, cfg runtime.Config) error {
		addRunningPod(p.ops.(*fakeK8sOps), "gc-test-agent", "gc-test-agent")
		return p.Relaunch(ctx, "gc-test-agent", cfg)
	},
}

// worktreeDialogConfig runs the agent in a linked worktree of the rig's
// repository, which the pod names /workspace/rig/ga-1-fix.
func worktreeDialogConfig() runtime.Config {
	cfg := dialogStartConfig()
	cfg.WorkDir = "/workspace/rig/ga-1-fix"
	return cfg
}

// TestStartupDialogsExternalImportsInPodRepositoryAccepted: after the trust
// dialog, Claude asks to allow the CLAUDE.md imports from outside the work
// dir. Imports inside the repository the work dir is in are first-party, and
// that repository is resolved in the pod, where the import paths the modal
// lists live; the pass allows them before the initial nudge.
func TestStartupDialogsExternalImportsInPodRepositoryAccepted(t *testing.T) {
	for opName, op := range dialogOps {
		t.Run(opName, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pane := &fakePane{
					dialogOpen: true, cursorOnTrust: true,
					imports: "/workspace/rig/AGENTS.md", gitCommonDir: "/workspace/rig/.git",
				}
				p := newDialogProvider(pane)
				p.prebaked = true // no host files to stage for the rig work dir

				if err := op(context.Background(), p, worktreeDialogConfig()); err != nil {
					t.Fatalf("%s: %v", opName, err)
				}
				if want := []string{"/workspace/rig/ga-1-fix"}; !slices.Equal(pane.gitDirs, want) {
					t.Fatalf("trust root resolved in %q, want the pod work dir %q", pane.gitDirs, want)
				}
				assertAcceptedBeforeNudge(t, pane)
				if allowed, nudge := pane.index("imports-allowed"), pane.index("nudge-text"); allowed < 0 || allowed > nudge {
					t.Fatalf("first-party imports not allowed before the nudge; log = %v", pane.log)
				}
				if record, ok := pane.env[startupDialogsUnresolvedKey]; ok {
					t.Fatalf("clean pass left session record %s = %q", startupDialogsUnresolvedKey, record)
				}
			})
		})
	}
}

// TestStartupDialogsExternalImportsLeftForHumanWithholdNudge: imports that
// are not inside the work dir's repository in the pod, or a work dir in no
// repository there, are left for a human. The modal's pre-selected option
// allows the imports, so the initial nudge is withheld: its text would land
// in the modal and its Enter would allow them. The session stays up, and the
// withheld nudge is reported on stderr and recorded on the session.
func TestStartupDialogsExternalImportsLeftForHumanWithholdNudge(t *testing.T) {
	cases := map[string]string{
		"import outside the repository":        "/workspace/rig/.git",
		"work dir in no repository in the pod": "",
	}
	for opName, op := range dialogOps {
		for caseName, gitCommonDir := range cases {
			t.Run(opName+"/"+caseName, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					pane := &fakePane{
						dialogOpen: true, cursorOnTrust: true,
						imports: "/workspace/AGENTS.md", gitCommonDir: gitCommonDir,
					}
					p := newDialogProvider(pane)
					p.prebaked = true
					var stderr strings.Builder
					p.stderr = &stderr

					if err := op(context.Background(), p, worktreeDialogConfig()); err != nil {
						t.Fatalf("%s failed a live session left on the imports modal: %v", opName, err)
					}
					if pane.index("accept") < 0 {
						t.Fatalf("trust dialog never accepted; log = %v", pane.log)
					}
					for _, event := range []string{"imports-allowed", "nudge-text", "submit"} {
						if n := pane.count(event); n != 0 {
							t.Fatalf("%d %s events with the imports modal left for a human; log = %v", n, event, pane.log)
						}
					}
					if !pane.importsOpen {
						t.Fatal("the imports modal was answered; it must stay for a human")
					}
					if _, alive := p.ops.(*fakeK8sOps).pods["gc-test-agent"]; !alive {
						t.Fatal("the pod was removed; the live session must stay running")
					}
					if got := stderr.String(); !strings.Contains(got, `"gc-test-agent"`) || !strings.Contains(got, "withheld") {
						t.Fatalf("stderr = %q, want a warning naming the session and the withheld nudge", got)
					}
					if record := pane.env[startupDialogsUnresolvedKey]; !strings.Contains(record, "withheld") {
						t.Fatalf("session record %s = %q, want the withheld nudge recorded", startupDialogsUnresolvedKey, record)
					}
				})
			})
		}
	}
}

// TestStartupDialogsDeadSessionReportedWithinSettle: an agent that exits at
// once (a stale resume key) takes its tmux session with it, so reading the
// pane exits 1 in the pod. The pass stops at that first read instead of
// polling an empty pane for its whole budget, so the post-start liveness
// probe reports the death within the settle, also when the deadline is
// shorter than the settle plus that budget, and the session layer can still
// retry without the stale key.
func TestStartupDialogsDeadSessionReportedWithinSettle(t *testing.T) {
	const settle = 3 * time.Second // the production settle
	for opName, op := range dialogOps {
		for _, deadline := range []bool{false, true} {
			name := opName + "/no deadline"
			if deadline {
				name = opName + "/deadline under settle plus the dialog budget"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// The first probe (Start's wait for tmux, Relaunch's warm
					// check) finds the session; the agent then exits.
					pane := &fakePane{dieAfterProbe: 1}
					p := newDialogProvider(pane)
					p.postStartSettle = settle
					ctx := context.Background()
					if deadline {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, settle+runtime.StartupDialogTimeout()/2)
						defer cancel()
					}

					began := time.Now()
					err := op(ctx, p, dialogStartConfig())
					if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
						t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
					}
					if took := time.Since(began); took > settle {
						t.Fatalf("death reported after %v, want within the %v settle", took, settle)
					}
					if n := pane.count("capture"); n != 1 {
						t.Fatalf("pass read the dead pane %d times, want 1; log = %v", n, pane.log)
					}
					if n := pane.count("nudge-text"); n != 0 {
						t.Fatalf("nudge typed into a dead session; log = %v", pane.log)
					}
				})
			})
		}
	}
}

// TestStartupDialogsTransportBlipRetried: one failed pane read, which the
// apiserver exec path produces independently of the pod, does not end the
// pass on its first frame: the pass runs once more and still answers the
// trust dialog before the initial nudge.
func TestStartupDialogsTransportBlipRetried(t *testing.T) {
	for opName, op := range dialogOps {
		t.Run(opName, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pane := &fakePane{dialogOpen: true, cursorOnTrust: true, captureBlips: 1}
				p := newDialogProvider(pane)

				if err := op(context.Background(), p, dialogStartConfig()); err != nil {
					t.Fatalf("%s: %v", opName, err)
				}
				assertAcceptedBeforeNudge(t, pane)
				if record, ok := pane.env[startupDialogsUnresolvedKey]; ok {
					t.Fatalf("recovered pass left session record %s = %q", startupDialogsUnresolvedKey, record)
				}
			})
		})
	}
}

// TestStartupDialogsCleanPassClearsRecord: a warm pod keeps its tmux
// environment across a relaunch, so a clean pass removes the record an
// earlier agent left, and the record always describes the current agent.
func TestStartupDialogsCleanPassClearsRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pane := &fakePane{
			dialogOpen: true, cursorOnTrust: true,
			env: map[string]string{startupDialogsUnresolvedKey: earlierAgentRecord},
		}
		p := newDialogProvider(pane)

		if err := dialogOps["relaunch"](context.Background(), p, dialogStartConfig()); err != nil {
			t.Fatalf("Relaunch: %v", err)
		}
		assertAcceptedBeforeNudge(t, pane)
		if record, ok := pane.env[startupDialogsUnresolvedKey]; ok {
			t.Fatalf("earlier agent's record %q survived a clean pass", record)
		}
	})
}

// TestStartupDialogsReadyScreenQuotingDialogWordingGetsNudge: once the trust
// dialog is answered, an agent whose screen shows dialog wording above its
// prompt (a resumed transcript with a rate-limit error, a usage-limit notice,
// trust wording, or the imports modal itself) is waiting for input, not on a
// dialog. It gets its initial nudge, exactly once, and the clean pass clears
// the record.
func TestStartupDialogsReadyScreenQuotingDialogWordingGetsNudge(t *testing.T) {
	screens := map[string]string{
		"rate-limit error":   "⏺ Bash(gh pr view 9140)\n  ⎿  gh: API rate limit exceeded for installation ID 1\n\n" + readyPrompt,
		"usage-limit notice": "⏺ Resuming.\n  ⎿  You've hit your limit · resets 3pm (UTC)\n\n" + readyPrompt,
		"trust wording":      "⏺ The setup guide says to trust this folder before the first run.\n\n" + readyPrompt,
		"imports modal":      externalImportsModal("/workspace/AGENTS.md") + "\n\n" + readyPrompt,
	}
	for opName, op := range dialogOps {
		for screenName, screen := range screens {
			t.Run(opName+"/"+screenName, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					pane := &fakePane{
						dialogOpen: true, cursorOnTrust: true, ready: screen,
						env: map[string]string{startupDialogsUnresolvedKey: earlierAgentRecord},
					}
					p := newDialogProvider(pane)

					if err := op(context.Background(), p, dialogStartConfig()); err != nil {
						t.Fatalf("%s: %v", opName, err)
					}
					accept, nudge := pane.index("accept"), pane.index("nudge-text")
					if accept < 0 || nudge < accept {
						t.Fatalf("initial nudge (%d) not typed after the trust dialog was accepted (%d); log = %v", nudge, accept, pane.log)
					}
					if n := pane.count("nudge-text"); n != 1 {
						t.Fatalf("initial nudge typed %d times, want once; log = %v", n, pane.log)
					}
					if pane.lastIndex("submit") < nudge {
						t.Fatalf("nudge never submitted after its text; log = %v", pane.log)
					}
					if record, ok := pane.env[startupDialogsUnresolvedKey]; ok {
						t.Fatalf("clean pass left session record %s = %q", startupDialogsUnresolvedKey, record)
					}
				})
			})
		}
	}
}

// TestStartupDialogsScreenReadAfterPass: the read of the visible screen after
// the pass decides the nudge, over the same apiserver exec path as the pass's
// own reads. One failed read is read again. A screen still unreadable cannot
// show that the pass left no dialog on it, so the nudge is withheld rather
// than typed blind, where its Enter would allow the imports of a modal left
// for a human, and the record says why rather than being cleared as after a
// clean pass.
func TestStartupDialogsScreenReadAfterPass(t *testing.T) {
	cases := []struct {
		name string
		// imports outside the repository leave the imports modal for a human.
		imports     string
		screenBlips int
		wantNudge   bool
		wantRecord  string
	}{
		{name: "imports modal, one failed read", imports: "/workspace/AGENTS.md", screenBlips: 1, wantRecord: "still on screen"},
		{name: "imports modal, screen unreadable", imports: "/workspace/AGENTS.md", screenBlips: 2, wantRecord: "unreadable"},
		{name: "ready, one failed read", screenBlips: 1, wantNudge: true},
		{name: "ready, screen unreadable", screenBlips: 2, wantRecord: "unreadable"},
	}
	for opName, op := range dialogOps {
		for _, tc := range cases {
			t.Run(opName+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					pane := &fakePane{
						dialogOpen: true, cursorOnTrust: true,
						imports: tc.imports, gitCommonDir: "/workspace/rig/.git",
						screenBlips: tc.screenBlips,
						env:         map[string]string{startupDialogsUnresolvedKey: earlierAgentRecord},
					}
					p := newDialogProvider(pane)
					p.prebaked = true

					if err := op(context.Background(), p, worktreeDialogConfig()); err != nil {
						t.Fatalf("%s: %v", opName, err)
					}
					if pane.index("accept") < 0 {
						t.Fatalf("trust dialog never accepted; log = %v", pane.log)
					}
					if n := pane.count("imports-allowed"); n != 0 {
						t.Fatalf("imports allowed %d times with the modal left for a human; log = %v", n, pane.log)
					}
					record, recorded := pane.env[startupDialogsUnresolvedKey]
					nudges := pane.count("nudge-text")
					if tc.wantNudge {
						if nudges != 1 || recorded {
							t.Fatalf("nudges = %d, record %q (present %v); want one nudge and the record cleared after the read was retried; log = %v",
								nudges, record, recorded, pane.log)
						}
						return
					}
					if nudges != 0 {
						t.Fatalf("initial nudge typed %d times; want it withheld; log = %v", nudges, pane.log)
					}
					if !strings.Contains(record, "withheld") || !strings.Contains(record, tc.wantRecord) {
						t.Fatalf("session record %s = %q, want the withheld nudge recorded with %q", startupDialogsUnresolvedKey, record, tc.wantRecord)
					}
				})
			})
		}
	}
}
