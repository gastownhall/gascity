package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ── startup-dialog acceptance against the fake herdr (issue #7027) ──────────
//
// herdr's equivalent of tmux's launchOrchestration dialog passes: a freshly
// launched agent that stops on its workspace-trust dialog must be answered
// through the pane before the idle wait and first turn, or the agent stalls
// until a human attaches and every turn typed at it lands in the dialog. The
// fake herdr's trust_dialog state shows Claude's "Quick safety check" until an
// enter lands, and models herdr ≥0.8.2 reading that dialog as blocked: `agent
// start` exits agent_not_ready with the agent still registered, `agent wait`
// holds unless it names blocked, and `agent prompt` is refused with
// agent_blocked. dialog_reads_idle has herdr read the same dialog as idle
// instead (Codex's hook review is one such dialog), so `agent start` succeeds
// over it.

// startDeadline stands in for the deadline the reconciler gives Start, the
// [session] startup_timeout. That defaults to the readiness wait's own 60s
// bound, part of which Start has spent before the wait begins, so a wait that
// holds while herdr reads the agent blocked outlasts it. Tests that leave a
// blocked agent in front of the readiness wait start under this shorter
// deadline, so such a hold fails them instead of hanging them.
const startDeadline = startupNudgeIdleTimeout / 3

// startDeadlineCtx returns a ctx that ends after startDeadline.
func startDeadlineCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), startDeadline)
	t.Cleanup(cancel)
	return ctx
}

// captureStartupDialogWarnings redirects startupDialogWarningOut for one test.
func captureStartupDialogWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := startupDialogWarningOut
	startupDialogWarningOut = &buf
	t.Cleanup(func() { startupDialogWarningOut = prev })
	return &buf
}

// startedBlocked reports whether the fake's `agent start` took its herdr
// ≥0.8.2 branch: the launch exited agent_not_ready on a blocked agent.
func startedBlocked(t *testing.T, state string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(state, "started_blocked"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// The trust dialog is confirmed through pane-scoped read/send-keys on the
// launch's pane, before readiness and before the first turn, whether herdr
// reads the dialog as blocked (its launch then exits agent_not_ready) or as
// idle.
func TestStartAcceptsWorkspaceTrustDialogBeforeFirstTurn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		states  []string
		blocked bool // whether herdr reports the launch blocked
	}{
		{"herdr reads the dialog as blocked", []string{"trust_dialog"}, true},
		{"herdr reads the dialog as idle", []string{"trust_dialog", "dialog_reads_idle"}, false},
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
				t.Fatalf("Start: %v", err)
			}
			if got := startedBlocked(t, state); got != tc.blocked {
				t.Fatalf("test setup: agent start reported the launch blocked = %v, want %v", got, tc.blocked)
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
			if v, _ := p.GetMeta("gastown__worker", metaStartupUnconfirmed); v != "" {
				t.Fatalf("a delivered first turn recorded an unconfirmed marker: %q", v)
			}
		})
	}
}

// With acceptance disabled the dialog step issues nothing. The blocked launch
// is still live: the readiness wait ends on herdr reading it blocked, its first
// turn goes to herdr, which refuses it while the dialog is up, and the strand
// is recorded within Start's deadline.
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

			if err := p.Start(startDeadlineCtx(t), "gastown__worker", tc.cfg); err != nil {
				t.Fatalf("Start: %v", err)
			}
			calls := fakeCalls(t, state)
			if strings.Contains(calls, "pane read") || strings.Contains(calls, "pane send-keys") {
				t.Fatalf("disabled dialog acceptance still touched the pane:\n%s", calls)
			}
			if !strings.Contains(calls, "agent prompt %5 do work") {
				t.Fatalf("first turn was not attempted:\n%s", calls)
			}
			v, _ := p.GetMeta("gastown__worker", metaStartupUnconfirmed)
			if !strings.Contains(v, "agent_blocked") || !strings.Contains(v, "idle_wait="+string(idleWaitBlocked)) {
				t.Fatalf("the first turn herdr refused was not recorded with the blocked readiness verdict: %q", v)
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
	warnings := captureStartupDialogWarnings(t)

	cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !startedBlocked(t, state) {
		t.Fatal("test setup: agent start did not report the launch blocked")
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane send-keys %5 enter") {
		t.Fatalf("trust dialog was not confirmed for a session without a startup turn:\n%s", calls)
	}
	if strings.Contains(calls, "agent wait") || strings.Contains(calls, "agent prompt") {
		t.Fatalf("a session without a startup turn must not run the provisioning tail:\n%s", calls)
	}
	if warnings.Len() != 0 {
		t.Fatalf("a confirmed dialog must not warn: %q", warnings.String())
	}
}

// A dialog the agent draws after its first prompt is answered by the pass
// after the idle wait, before the first turn: the first pass saw only the
// prompt.
func TestStartAcceptsLateStartupDialogBeforeFirstTurn(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "late_trust_dialog")
	warnings := captureStartupDialogWarnings(t)

	cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, Nudge: "do work"}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	wait := strings.Index(calls, "agent wait")
	if read := strings.Index(calls, "pane read %5"); read < 0 || wait < 0 || read > wait {
		t.Fatalf("test setup: the first dialog pass did not read the pane before the idle wait:\n%s", calls)
	}
	enter := strings.Index(calls, "pane send-keys %5 enter")
	if enter < wait {
		t.Fatalf("the late trust dialog was not confirmed after the idle wait:\n%s", calls)
	}
	if prompt := strings.Index(calls, "agent prompt %5 do work"); prompt < 0 || enter > prompt {
		t.Fatalf("the late trust dialog must be confirmed before the first turn:\n%s", calls)
	}
	if warnings.Len() != 0 {
		t.Fatalf("a confirmed dialog must not warn: %q", warnings.String())
	}
}

// A failed dialog step is best-effort: Start still succeeds, warns exactly
// once (for the pass after the idle wait), and never confirms an unconfirmed
// trust frame. The first turn still goes out after failed pane reads, but not
// over a trust dialog left unconfirmed, where its submit Enter would answer
// the dialog: that turn is withheld and recorded. The stuck dialog keeps herdr
// reading the agent blocked, so all of it has to happen within Start's
// deadline rather than after the readiness wait's bound.
func TestStartStartupDialogFailureIsNonFatal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		states   []string
		want     string // the cause the warning carries
		wantTurn bool   // whether the first turn is still delivered
	}{
		{"pane read fails", []string{"pane_read_fails"}, "pane not found", true},
		{"trust cursor stuck", []string{"trust_dialog", "trust_stuck"}, runtime.ErrWorkspaceTrustUnconfirmed.Error(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			for _, s := range tc.states {
				setState(t, state, s)
			}
			warnings := captureStartupDialogWarnings(t)

			cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, Nudge: "do work"}
			if err := p.Start(startDeadlineCtx(t), "gastown__worker", cfg); err != nil {
				t.Fatalf("Start must not fail on a dialog-step error: %v", err)
			}
			calls := fakeCalls(t, state)
			if !strings.Contains(calls, "agent wait") {
				t.Fatalf("Start did not proceed to the idle wait:\n%s", calls)
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
			marker, _ := p.GetMeta("gastown__worker", metaStartupUnconfirmed)
			if tc.wantTurn {
				if !strings.Contains(calls, "agent prompt %5 do work") {
					t.Fatalf("first turn was not delivered:\n%s", calls)
				}
				if marker != "" {
					t.Fatalf("a delivered first turn recorded an unconfirmed marker: %q", marker)
				}
			} else {
				if strings.Contains(calls, "agent prompt") {
					t.Fatalf("first turn was typed at an unconfirmed trust dialog:\n%s", calls)
				}
				if !strings.Contains(marker, "first turn withheld") || !strings.Contains(marker, tc.want) {
					t.Fatalf("the withheld first turn was not recorded with its cause: %q", marker)
				}
				if !strings.Contains(marker, "idle_wait="+string(idleWaitBlocked)) {
					t.Fatalf("the withheld first turn does not record that herdr read the agent blocked: %q", marker)
				}
			}
		})
	}
}

// onVerbInFlight arms the FIFO a fake herdr verb signals through (its name
// under state) and runs fn once that verb is in flight. The fake then holds
// the verb until Start's ctx kills it.
func onVerbInFlight(t *testing.T, state, fifo string, fn func()) {
	t.Helper()
	path := filepath.Join(state, fifo)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		f, err := os.Open(path) // returns once the fake opens the FIFO to signal
		if err != nil {
			return
		}
		defer f.Close() //nolint:errcheck // read-only FIFO
		var b [1]byte
		if _, err := f.Read(b[:]); err == nil {
			fn()
		}
	}()
	t.Cleanup(func() {
		// Release the reader above if Start never reached the verb.
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
}

// expiringCtx is a context whose deadline passes exactly when expire is
// called, so a test can end it inside a chosen startup step without racing a
// timer. tmux's startup tests use the same shape.
type expiringCtx struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newExpiringCtx() *expiringCtx {
	return &expiringCtx{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringCtx) expire() { c.once.Do(func() { close(c.done) }) }

func (c *expiringCtx) Done() <-chan struct{} { return c.done }

func (c *expiringCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// Canceling Start's ctx while a dialog pass runs fails Start with the
// cancellation rather than a warning, and no first turn goes out.
func TestStartReturnsContextErrorWhenCanceledDuringStartupDialogs(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	warnings := captureStartupDialogWarnings(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onVerbInFlight(t, state, "pane_read_entered", cancel)

	cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, Nudge: "do work"}
	err := p.Start(ctx, "gastown__worker", cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want context.Canceled", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane read %5") {
		t.Fatalf("test setup: Start was not canceled inside a dialog pass:\n%s", calls)
	}
	if strings.Contains(calls, "agent prompt") {
		t.Fatalf("a canceled Start still delivered the first turn:\n%s", calls)
	}
	if warnings.Len() != 0 {
		t.Fatalf("a cancellation must fail Start, not warn: %q", warnings.String())
	}
}

// Start's deadline passing while its readiness wait holds on a boot that never
// idles does not fail Start while the agent is still up, mirroring tmux's
// ignoreDeadlineIfSessionAlive: the session is live, and failing Start would
// only respawn it. Start returns nil and records the first turn it could not
// deliver. A canceled ctx, or a deadline once the agent is gone, still fails
// Start.
func TestStartSurvivesReadinessWaitDeadlineOnlyWithALiveAgent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancel    bool  // the ctx is canceled rather than past its deadline
		agentGone bool  // herdr no longer lists the agent when the ctx ends
		want      error // nil: Start succeeds and records the undelivered turn
	}{
		{name: "deadline with the agent alive"},
		{name: "deadline with the agent gone", agentGone: true, want: context.DeadlineExceeded},
		{name: "canceled", cancel: true, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			setState(t, state, "ready_prompt")
			warnings := captureStartupDialogWarnings(t)
			var ctx context.Context
			var end func()
			if tc.cancel {
				cctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				ctx, end = cctx, cancel
			} else {
				ectx := newExpiringCtx()
				ctx, end = ectx, ectx.expire
			}
			onVerbInFlight(t, state, "agent_wait_entered", func() {
				if tc.agentGone {
					_ = os.Remove(filepath.Join(state, "registered"))
				}
				end()
			})

			cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, Nudge: "do work"}
			err := p.Start(ctx, "gastown__worker", cfg)
			calls := fakeCalls(t, state)
			if !strings.Contains(calls, "agent wait") {
				t.Fatalf("test setup: Start's ctx did not end inside the readiness wait:\n%s", calls)
			}
			if strings.Contains(calls, "agent prompt") {
				t.Fatalf("Start delivered the first turn after its ctx ended:\n%s", calls)
			}
			if warnings.Len() != 0 {
				t.Fatalf("an ended ctx is not a startup-dialog warning: %q", warnings.String())
			}
			marker, _ := p.GetMeta("gastown__worker", metaStartupUnconfirmed)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("Start = %v, want %v", err, tc.want)
				}
				if marker != "" {
					t.Fatalf("a failed Start recorded an undelivered first turn: %q", marker)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start = %v, want nil: the deadline passed on a live agent", err)
			}
			for _, want := range []string{"first turn not delivered", context.DeadlineExceeded.Error(), "idle_wait=" + string(idleWaitError)} {
				if !strings.Contains(marker, want) {
					t.Fatalf("the undelivered first turn's record %q lacks %q", marker, want)
				}
			}
		})
	}
}

// A startup-dialog answer can end the agent: the rate-limit step selects Stop
// on purpose, and the bypass step sends a blind Down then Enter, so a dropped
// Down confirms "No, exit". herdr's `agent start` types the agent's command
// into the pane's shell, which outlives the agent. Start fails as tmux's Step 5
// does, instead of going on to type the first turn into that shell.
func TestStartFailsWhenAStartupDialogAnswerEndsTheAgent(t *testing.T) {
	const (
		rateLimit = "Usage limit reached\n\n❯ 1. Keep trying\n  2. Stop\n"
		bypass    = "WARNING: Claude Code running in Bypass Permissions mode\n\n❯ 1. No, exit\n  2. Yes, I accept\n"
	)
	for _, tc := range []struct {
		name  string
		frame string // the dialog whose answer ends the agent
		nudge string
	}{
		{"rate-limit dialog answered Stop", rateLimit, "do work"},
		{"bypass warning confirmed No, exit after a dropped Down", bypass, "do work"},
		{"no first turn", rateLimit, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			if err := os.WriteFile(filepath.Join(state, "exit_dialog"), []byte(tc.frame), 0o644); err != nil {
				t.Fatal(err)
			}

			cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, Nudge: tc.nudge}
			err := p.Start(startDeadlineCtx(t), "gastown__worker", cfg)
			if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
				t.Fatalf("Start = %v, want ErrSessionDiedDuringStartup", err)
			}
			calls := fakeCalls(t, state)
			if !strings.Contains(calls, "pane send-keys %5 enter") {
				t.Fatalf("test setup: the dialog was not answered:\n%s", calls)
			}
			for _, verb := range []string{"agent prompt", "pane run"} {
				if strings.Contains(calls, verb) {
					t.Fatalf("Start went on to the first turn (%s) after the dialog answer ended the agent:\n%s", verb, calls)
				}
			}
		})
	}
}

// agent_not_ready counts as a launch only while herdr's registry holds an
// agent on the launch pane. Without one Start fails as before: there is no
// agent to answer, and the first turn would otherwise be typed into a bare
// shell.
func TestStartBlockedLaunchWithoutRegisteredAgentFails(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "start_not_ready_unregistered")

	cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, Nudge: "do work"}
	err := p.Start(context.Background(), "gastown__worker", cfg)
	if err == nil || herdrCodeAnyShape(err) != "agent_not_ready" {
		t.Fatalf("Start = %v, want the agent_not_ready launch failure", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "agent get %5") {
		t.Fatalf("the blocked launch was not checked against herdr's registry:\n%s", calls)
	}
	for _, verb := range []string{"pane read", "agent prompt", "pane run"} {
		if strings.Contains(calls, verb) {
			t.Fatalf("an unconfirmed launch still ran %q:\n%s", verb, calls)
		}
	}
}

// A raw launch (a command herdr cannot start as an agent kind) gets the same
// dialog pass on its pane before its first turn, which it receives by paste.
func TestStartRawLaunchAcceptsStartupDialogsBeforeFirstTurn(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "trust_dialog")
	warnings := captureStartupDialogWarnings(t)

	cfg := runtime.Config{Command: "claude --model=opus", ProcessNames: []string{"claude"}, Nudge: "do work"}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	launch := strings.Index(calls, "pane run %5 exec /bin/sh -c")
	if launch < 0 || strings.Contains(calls, "agent start") {
		t.Fatalf("test setup: the command did not take the raw launch path:\n%s", calls)
	}
	enter := strings.Index(calls, "pane send-keys %5 enter")
	if read := strings.Index(calls, "pane read %5"); read < launch || enter < read {
		t.Fatalf("trust dialog was not read and confirmed on the raw launch's pane:\n%s", calls)
	}
	if turn := strings.Index(calls, "pane run %5 do work"); turn < 0 || enter > turn {
		t.Fatalf("trust dialog must be confirmed before the pasted first turn:\n%s", calls)
	}
	if warnings.Len() != 0 {
		t.Fatalf("a confirmed dialog must not warn: %q", warnings.String())
	}
}

// An adopted holder is a live session whose winning Start already answered
// its dialogs; re-running the matcher could answer an in-session dialog.
func TestStartAdoptedHolderSkipsStartupDialogs(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "name_taken")
	setState(t, state, "trust_dialog")

	cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, Nudge: "do work"}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "agent start") || strings.Contains(calls, "agent prompt") {
		t.Fatalf("test setup: Start did not adopt the live holder:\n%s", calls)
	}
	if strings.Contains(calls, "pane read") || strings.Contains(calls, "pane send-keys") {
		t.Fatalf("adopted holder was put through the dialog step:\n%s", calls)
	}
}

// Start's readiness wait names blocked, so herdr ends it as soon as it reads
// the agent blocked on a dialog, and that verdict is reported apart from idle.
// WaitForIdle's wait still names only idle and done.
func TestStartupReadinessWaitEndsWhenHerdrReadsTheAgentBlocked(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	setState(t, state, "trust_dialog")
	ctx := startDeadlineCtx(t)

	if got := p.waitForStartupReadiness(ctx, "gastown__worker", startupNudgeIdleTimeout); got != idleWaitBlocked {
		t.Fatalf("readiness wait on a blocked agent = %q, want %q", got, idleWaitBlocked)
	}
	setState(t, state, "trusted")
	if got := p.waitForStartupReadiness(ctx, "gastown__worker", startupNudgeIdleTimeout); got != idleWaitReached {
		t.Fatalf("readiness wait on an idle agent = %q, want %q", got, idleWaitReached)
	}
	if got := p.waitForIdleOutcome(ctx, "gastown__worker", startupNudgeIdleTimeout); got != idleWaitReached {
		t.Fatalf("idle wait on an idle agent = %q, want %q", got, idleWaitReached)
	}
	calls := strings.Split(strings.TrimRight(fakeCalls(t, state), "\n"), "\n")
	want := []string{
		"agent wait gastown__worker --until idle --until done --until blocked --timeout 60000",
		"agent wait gastown__worker --until idle --until done --until blocked --timeout 60000",
		"agent wait gastown__worker --until idle --until done --timeout 60000",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("agent wait calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// A successful wait is classified by the status herdr matched. A reply that
// does not decode is herdr answering something unexpected, not the agent
// reaching idle.
func TestMatchedStatusOutcome(t *testing.T) {
	for _, tc := range []struct {
		res  string
		want idleWaitOutcome
	}{
		{`{"agent":{"name":"w","agent_status":"idle"}}`, idleWaitReached},
		{`{"agent":{"name":"w","agent_status":"done"}}`, idleWaitReached},
		{`{"agent":{"name":"w","agent_status":"blocked"}}`, idleWaitBlocked},
		{`{"agent":"w"`, idleWaitError},
		{``, idleWaitError},
	} {
		if got := matchedStatusOutcome(json.RawMessage(tc.res)); got != tc.want {
			t.Errorf("matchedStatusOutcome(%q) = %q, want %q", tc.res, got, tc.want)
		}
	}
}
