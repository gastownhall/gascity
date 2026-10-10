package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// idleHandshake declares the three capabilities that give an exec pack an
// idle boundary.
const idleHandshake = `{"version":0,"capabilities":["proc.exec","report-activity","report-attachment"]}`

// Pane fixtures, as `tmux capture-pane -p` prints them.
const (
	idleClaudePane = "previous output\n\n❯ \n\n  ⏵⏵ bypass permissions on\n"
	busyClaudePane = "previous output\n\n❯ \n· Boogieing… (2m 28s · ↓ 10.9k tokens)\n"
	idleCodexPane  = "previous output\n\n› \n"
)

// fastIdlePoll shortens the idle poll interval for the test.
func fastIdlePoll(t *testing.T) {
	t.Helper()
	old := execIdlePollInterval
	execIdlePollInterval = time.Millisecond
	t.Cleanup(func() { execIdlePollInterval = old })
}

// idlePack is an exec pack script whose `exec` op logs each capture-pane
// command and answers the nth capture with panes[n-1] (the last pane once
// the list is exhausted). It keeps the in-box tmux GC_READY_PROMPT_PREFIX in
// a file (set by set-environment, read by show-environment, removed by stop);
// show-environment fails while the env.fail file exists. Every in-box tmux
// command is logged to tmuxLog. Other ops log their name to opsLog.
type idlePack struct {
	dir        string
	script     string
	captureLog string
	opsLog     string
	tmuxLog    string
}

func newIdlePack(t *testing.T, handshake string, panes ...string) idlePack {
	t.Helper()
	dir := t.TempDir()
	pk := idlePack{
		dir:        dir,
		captureLog: filepath.Join(dir, "captures.log"),
		opsLog:     filepath.Join(dir, "ops.log"),
		tmuxLog:    filepath.Join(dir, "tmux.log"),
	}
	for i, pane := range panes {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("pane.%d", i+1)), []byte(pane), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(panes) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "pane.last"), []byte(panes[len(panes)-1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	protocol := "exit 2"
	if handshake != "" {
		protocol = fmt.Sprintf("printf '%%s' '%s'", handshake)
	}
	pk.script = writeScript(t, dir, fmt.Sprintf(`
op="$1"
case "$op" in
  protocol) %s ;;
  exec)
    cmd=$(cat)
    echo exec >> %[2]q
    printf '%%s\n' "$cmd" >> %[4]q/tmux.log
    case "$cmd" in
      *set-environment*)
        echo set-env >> %[2]q
        eval "set -- $cmd"
        for v; do :; done
        printf '%%s' "$v" > %[4]q/env ;;
      *show-environment*)
        echo show-env >> %[2]q
        [ -f %[4]q/env.fail ] && exit 1
        echo "HOME=/root"
        echo "-TERM"
        [ -f %[4]q/env ] && printf 'GC_READY_PROMPT_PREFIX=%%s\n' "$(cat %[4]q/env)"
        exit 0 ;;
      *capture-pane*)
        printf '%%s\n' "$cmd" >> %[3]q
        n=$(wc -l < %[3]q | tr -d ' ')
        f=%[4]q/pane.$n
        [ -f "$f" ] || f=%[4]q/pane.last
        [ -f "$f" ] && cat "$f"
        ;;
    esac ;;
  start) cat > /dev/null; echo start >> %[2]q ;;
  stop) echo stop >> %[2]q; rm -f %[4]q/env ;;
  is-attached) echo is-attached >> %[2]q; echo false ;;
  *) exit 2 ;;
esac
`, protocol, pk.opsLog, pk.captureLog, dir))
	return pk
}

func (pk idlePack) captures(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(pk.captureLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// env returns the published GC_READY_PROMPT_PREFIX, and whether it is set.
func (pk idlePack) env(t *testing.T) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(pk.dir, "env"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

// tmuxCommands returns every in-box tmux command line the exec op ran.
func (pk idlePack) tmuxCommands(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(pk.tmuxLog)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (pk idlePack) ops(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(pk.opsLog)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Only a pack declaring report-activity, report-attachment AND proc.exec has
// an idle boundary, so only it gets full; everything else keeps the timed-only
// floor, never disabled, through the raw and the seam-backed provider alike.
// Kills: the hard-coded timed_only, full without proc.exec (no way to read
// the pane), full from activity+attachment alone, and a disabled floor.
func TestSleepCapabilityDerivedFromHandshake(t *testing.T) {
	tests := []struct {
		name      string
		handshake string
		want      runtime.SessionSleepCapability
	}{
		{"no handshake", "", runtime.SessionSleepCapabilityTimedOnly},
		{"malformed handshake", "{not json", runtime.SessionSleepCapabilityTimedOnly},
		{"activity only", `{"version":0,"capabilities":["report-activity"]}`, runtime.SessionSleepCapabilityTimedOnly},
		{"activity and attachment", `{"version":0,"capabilities":["report-activity","report-attachment"]}`, runtime.SessionSleepCapabilityTimedOnly},
		{"exec and activity", `{"version":0,"capabilities":["proc.exec","report-activity"]}`, runtime.SessionSleepCapabilityTimedOnly},
		{"exec and attachment", `{"version":0,"capabilities":["proc.exec","report-attachment"]}`, runtime.SessionSleepCapabilityTimedOnly},
		{"all three", idleHandshake, runtime.SessionSleepCapabilityFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pk := newIdlePack(t, tt.handshake)
			if got := NewProvider(pk.script).SleepCapability("s"); got != tt.want {
				t.Errorf("raw SleepCapability = %q, want %q", got, tt.want)
			}
			seam, ok := NewSeamBacked(pk.script).(runtime.SleepCapabilityProvider)
			if !ok {
				t.Fatal("seam-backed provider lost SleepCapabilityProvider")
			}
			if got := seam.SleepCapability("s"); got != tt.want {
				t.Errorf("seam-backed SleepCapability = %q, want %q", got, tt.want)
			}
		})
	}
}

// Kills: a WaitForIdle that probes a pack without an idle boundary (which
// would read a pane gc cannot interpret) or reports success for it.
func TestWaitForIdleUnsupportedWithoutDeclaration(t *testing.T) {
	pk := newIdlePack(t, `{"version":0,"capabilities":["report-activity","report-attachment"]}`, idleClaudePane)
	err := NewProvider(pk.script).WaitForIdle(context.Background(), "s", time.Second)
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("WaitForIdle = %v, want ErrInteractionUnsupported", err)
	}
	if ops := pk.ops(t); ops != "" {
		t.Fatalf("ops run = %q, want none", ops)
	}
}

// Kills: a capture of the wrong target or depth, and a single-observation
// success.
func TestWaitForIdleIdlePaneReturnsNil(t *testing.T) {
	fastIdlePoll(t)
	pk := newIdlePack(t, idleHandshake, idleClaudePane)
	if err := NewProvider(pk.script).WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle on an idle pane: %v", err)
	}
	got := pk.captures(t)
	if len(got) < 2 {
		t.Fatalf("captures = %d, want at least 2", len(got))
	}
	const wantArgv = `'tmux' 'capture-pane' '-t' 'main' '-p' '-S' '-120'`
	for _, c := range got {
		if c != wantArgv {
			t.Fatalf("capture command = %q, want %q", c, wantArgv)
		}
	}
}

// Kills: a timeout that is not a context.DeadlineExceeded (callers classify
// on it).
func TestWaitForIdleBusyPaneTimesOut(t *testing.T) {
	fastIdlePoll(t)
	pk := newIdlePack(t, idleHandshake, busyClaudePane)
	err := NewProvider(pk.script).WaitForIdle(context.Background(), "s", 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForIdle on a busy pane = %v, want DeadlineExceeded", err)
	}
}

// Kills: a scan that ignores the busy footer (the two busy panes would count
// as idle and the wait would end after two captures).
func TestWaitForIdleBusyPaneIsNotIdle(t *testing.T) {
	fastIdlePoll(t)
	pk := newIdlePack(t, idleHandshake, busyClaudePane, busyClaudePane, idleClaudePane, idleClaudePane)
	if err := NewProvider(pk.script).WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
	if got := len(pk.captures(t)); got != 4 {
		t.Fatalf("captures = %d, want exactly 4 (busy, busy, idle, idle)", got)
	}
}

// Kills: counting idle observations that are not consecutive.
func TestWaitForIdleNeedsTwoConsecutiveIdleObservations(t *testing.T) {
	fastIdlePoll(t)
	pk := newIdlePack(t, idleHandshake, idleClaudePane, busyClaudePane, idleClaudePane, idleClaudePane)
	if err := NewProvider(pk.script).WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
	if got := len(pk.captures(t)); got != 4 {
		t.Fatalf("captures = %d, want exactly 4 (idle, busy, idle, idle)", got)
	}
}

// The session's own ready prompt is scanned for while this provider knows
// it, and the default comes back after Stop. Each phase is told apart by how
// many captures the wait needs, not by a timeout, so no assertion races a
// subprocess against a short deadline.
// Kills: ignoring Config.ReadyPromptPrefix (codex-style agents never read
// idle), and a cache or published prompt that outlives the session.
func TestWaitForIdleUsesSessionReadyPrompt(t *testing.T) {
	fastIdlePoll(t)

	// Codex prompt: idle after two captures. After Stop the default prompt
	// is back, so the two further codex panes do not count and the wait
	// ends on the two claude panes after them.
	pk := newIdlePack(t, idleHandshake, idleCodexPane, idleCodexPane, idleCodexPane, idleCodexPane, idleClaudePane, idleClaudePane)
	p := NewProvider(pk.script)
	if err := p.Start(context.Background(), "s", runtime.Config{ReadyPromptPrefix: "› "}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle with the session's prompt on screen: %v", err)
	}
	if got := len(pk.captures(t)); got != 2 {
		t.Fatalf("captures with the codex prompt = %d, want 2", got)
	}
	if err := p.Stop("s"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle after Stop: %v", err)
	}
	if got := len(pk.captures(t)); got != 6 {
		t.Fatalf("captures after Stop = %d, want 6 (codex panes no longer idle)", got)
	}

	// A codex session does not read idle on claude panes.
	pk = newIdlePack(t, idleHandshake, idleClaudePane, idleClaudePane, idleCodexPane, idleCodexPane)
	p = NewProvider(pk.script)
	if err := p.Start(context.Background(), "s", runtime.Config{ReadyPromptPrefix: "› "}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
	if got := len(pk.captures(t)); got != 4 {
		t.Fatalf("captures for a codex session = %d, want 4 (claude panes not idle)", got)
	}
}

// A provider instance that did not start the session (a restarted
// orchestrator, a CLI process) recovers the session's ready prompt from the
// in-box tmux environment that Start published, and remembers it.
// Kills: a prefix kept only in the starting instance's memory (the other
// instance would scan for the default prompt and never read a codex session
// idle, which turns interrupt_now into a hard restart), a Start that does not
// publish, and a read on every wait.
func TestWaitForIdleSecondInstanceRecoversReadyPrompt(t *testing.T) {
	fastIdlePoll(t)
	pk := newIdlePack(t, idleHandshake, idleCodexPane)
	if err := NewProvider(pk.script).Start(context.Background(), "s", runtime.Config{ReadyPromptPrefix: "› "}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.Contains(pk.ops(t), "set-env\n") {
		t.Fatalf("Start did not publish the ready prompt; ops = %q", pk.ops(t))
	}
	other := NewProvider(pk.script)
	for i := range 2 {
		if err := other.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
			t.Fatalf("second instance WaitForIdle #%d on a codex pane: %v", i+1, err)
		}
	}
	if got := strings.Count(pk.ops(t), "show-env\n"); got != 1 {
		t.Fatalf("show-environment reads = %d, want 1 (remembered after the first)", got)
	}
}

// An absent variable (never published: a failed publish at Start, or a
// session started before publishing existed) and a failed read both fall back
// to the default prompt without remembering it, so the next wait reads again
// and picks up a prefix published later.
// Kills: an absent variable remembered as the default forever (a codex
// session whose publish failed would never read idle in this instance), and a
// failed read cached forever.
func TestWaitForIdleReadyPromptReadFallback(t *testing.T) {
	fastIdlePoll(t)

	absent := newIdlePack(t, idleHandshake, idleClaudePane, idleClaudePane, idleCodexPane)
	p := NewProvider(absent.script)
	if err := p.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle with no published prompt on a claude pane: %v", err)
	}
	// The prefix is published after the first wait; the next wait reads it.
	if err := os.WriteFile(filepath.Join(absent.dir, "env"), []byte("› "), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle after the prompt was published, on a codex pane: %v", err)
	}
	if got := strings.Count(absent.ops(t), "show-env\n"); got != 2 {
		t.Fatalf("show-environment reads with the variable absent at first = %d, want 2 (absent not remembered)", got)
	}

	failing := newIdlePack(t, idleHandshake, idleClaudePane)
	if err := os.WriteFile(filepath.Join(failing.dir, "env.fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p = NewProvider(failing.script)
	for i := range 2 {
		if err := p.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
			t.Fatalf("WaitForIdle #%d with a failing read on a claude pane: %v", i+1, err)
		}
	}
	if got := strings.Count(failing.ops(t), "show-env\n"); got != 2 {
		t.Fatalf("show-environment reads with a failing read = %d, want 2 (not remembered)", got)
	}
}

// Both in-box tmux environment commands pass -u, as local tmux does on every
// command: without a UTF-8 locale in the exec environment (an image that sets
// no LANG) tmux prints the non-ASCII prompts of every builtin agent ("❯ ",
// "› ", "→ ") as "_ ". The integration test proves the behavior against a real
// tmux; this pins the argv.
// Kills: a read or publish without -u.
func TestReadyPromptTmuxCommandsForceUTF8(t *testing.T) {
	fastIdlePoll(t)
	pk := newIdlePack(t, idleHandshake, idleCodexPane)
	if err := NewProvider(pk.script).Start(context.Background(), "s", runtime.Config{ReadyPromptPrefix: "› "}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := NewProvider(pk.script).WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
	cmds := pk.tmuxCommands(t)
	for _, want := range []string{"'tmux' '-u' 'set-environment' ", "'tmux' '-u' 'show-environment' "} {
		if !strings.Contains(cmds, want) {
			t.Fatalf("in-box tmux commands lack %q:\n%s", want, cmds)
		}
	}
}

// A blank prefix publishes the default prompt explicitly (so a relaunch with
// the default prompt overwrites a stale one, and an absent variable always
// means "never published"); a pack without an idle boundary gets no extra exec
// op at Start.
// Kills: a blank prefix that unsets the variable (indistinguishable from a
// failed publish).
func TestStartPublishesReadyPromptOnlyWithIdleBoundary(t *testing.T) {
	pk := newIdlePack(t, idleHandshake, idleClaudePane)
	if err := os.WriteFile(filepath.Join(pk.dir, "env"), []byte("› "), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewProvider(pk.script).Start(context.Background(), "s", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, ok := pk.env(t); !ok || got != "❯ " {
		t.Fatalf("published prompt after Start with a blank prefix = (%q, %v), want (\"❯ \", true)", got, ok)
	}

	// The separable launch publishes once the agent's tmux session exists.
	pk = newIdlePack(t, `{"version":0,"capabilities":["proc.exec","proc.provision","report-activity","report-attachment"]}`, idleClaudePane)
	if err := NewProvider(pk.script).launchAgent(context.Background(), "s", runtime.Config{ReadyPromptPrefix: "› "}); err != nil {
		t.Fatalf("launchAgent: %v", err)
	}
	if !strings.Contains(pk.ops(t), "set-env\n") {
		t.Fatalf("launchAgent did not publish the ready prompt; ops = %q", pk.ops(t))
	}

	pk = newIdlePack(t, `{"version":0,"capabilities":["proc.exec","report-activity"]}`, idleClaudePane)
	if err := NewProvider(pk.script).Start(context.Background(), "s", runtime.Config{ReadyPromptPrefix: "› "}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if strings.Contains(pk.ops(t), "set-env") {
		t.Fatalf("Start published without an idle boundary; ops = %q", pk.ops(t))
	}
}

// Kills: a WaitForIdle that ignores its context.
func TestWaitForIdleHonorsContext(t *testing.T) {
	fastIdlePoll(t)
	pk := newIdlePack(t, idleHandshake, busyClaudePane)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewProvider(pk.script).WaitForIdle(ctx, "s", 10*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForIdle with a canceled context = %v, want context.Canceled", err)
	}
	if got := pk.captures(t); len(got) != 0 {
		t.Fatalf("captures = %d after cancellation, want none", len(got))
	}
}

// Kills: an attachment probe that runs for a pack that never declared it,
// reads an op failure as "not attached" (a destructive gate would then
// proceed), or misparses the answer.
func TestIsAttachedWithError(t *testing.T) {
	tests := []struct {
		name         string
		handshake    string
		isAttached   string
		wantAttached bool
		wantErr      error
		wantOp       bool
	}{
		{"undeclared", `{"version":0,"capabilities":["report-activity"]}`, `echo true`, false, nil, false},
		{"attachment only", `{"version":0,"capabilities":["report-attachment"]}`, `echo true`, true, nil, true},
		{"attachment only op error", `{"version":0,"capabilities":["report-attachment"]}`, `exit 1`, false, runtime.ErrRuntimeUnavailable, true},
		{"attached", idleHandshake, `echo true`, true, nil, true},
		{"attached with whitespace", idleHandshake, `printf ' true \n'`, true, nil, true},
		{"not attached", idleHandshake, `echo false`, false, nil, true},
		{"op error", idleHandshake, `echo boom >&2; exit 1`, false, runtime.ErrRuntimeUnavailable, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			opsLog := filepath.Join(dir, "ops.log")
			script := writeScript(t, dir, fmt.Sprintf(`
case "$1" in
  protocol) printf '%%s' '%s' ;;
  is-attached) echo is-attached >> %q; %s ;;
  *) exit 2 ;;
esac
`, tt.handshake, opsLog, tt.isAttached))
			p := NewProvider(script)
			attached, err := p.IsAttachedWithError("s")
			if attached != tt.wantAttached {
				t.Errorf("attached = %v, want %v", attached, tt.wantAttached)
			}
			switch {
			case tt.wantErr == nil && err != nil:
				t.Errorf("err = %v, want nil", err)
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			_, statErr := os.Stat(opsLog)
			if ran := statErr == nil; ran != tt.wantOp {
				t.Errorf("is-attached op ran = %v, want %v", ran, tt.wantOp)
			}
			if got := p.IsAttached("s"); got != (tt.wantAttached && tt.wantErr == nil) {
				t.Errorf("IsAttached = %v, want %v", got, tt.wantAttached && tt.wantErr == nil)
			}
		})
	}
}

// The production seam-backed provider forwards the idle wait and the
// error-bearing attachment probe to the raw provider.
// Kills: a missing pass-through (runtime.IsAttachedWithError would fall back
// to the seam's IsAttached and lose the error; launchIdleProbes would find
// no IdleWaitProvider and never drain).
func TestSeamBackedForwardsIdleWaitAndAttachProbe(t *testing.T) {
	fastIdlePoll(t)
	pk := newIdlePack(t, idleHandshake, idleClaudePane)
	seam := NewSeamBacked(pk.script)
	wp, ok := seam.(runtime.IdleWaitProvider)
	if !ok {
		t.Fatal("seam-backed provider does not implement runtime.IdleWaitProvider")
	}
	if err := wp.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("seam-backed WaitForIdle: %v", err)
	}
	if _, ok := seam.(runtime.AttachmentObserverWithError); !ok {
		t.Fatal("seam-backed provider does not implement runtime.AttachmentObserverWithError")
	}

	dir := t.TempDir()
	failing := writeScript(t, dir, fmt.Sprintf(`
case "$1" in
  protocol) printf '%%s' '%s' ;;
  is-attached) echo boom >&2; exit 1 ;;
  *) exit 2 ;;
esac
`, idleHandshake))
	if _, err := runtime.IsAttachedWithError(NewSeamBacked(failing), "s"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("seam-backed IsAttachedWithError on a failing op = %v, want ErrRuntimeUnavailable", err)
	}
}
