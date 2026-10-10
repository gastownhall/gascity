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
// the list is exhausted). Other ops log their name to opsLog.
type idlePack struct {
	script     string
	captureLog string
	opsLog     string
}

func newIdlePack(t *testing.T, handshake string, panes ...string) idlePack {
	t.Helper()
	dir := t.TempDir()
	pk := idlePack{
		captureLog: filepath.Join(dir, "captures.log"),
		opsLog:     filepath.Join(dir, "ops.log"),
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
    case "$cmd" in
      *capture-pane*)
        printf '%%s\n' "$cmd" >> %[3]q
        n=$(wc -l < %[3]q | tr -d ' ')
        f=%[4]q/pane.$n
        [ -f "$f" ] || f=%[4]q/pane.last
        [ -f "$f" ] && cat "$f"
        ;;
    esac ;;
  start) cat > /dev/null; echo start >> %[2]q ;;
  stop) echo stop >> %[2]q ;;
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

// Kills: a scan that ignores the busy footer, and a timeout that is not a
// context.DeadlineExceeded (callers classify on it).
func TestWaitForIdleBusyPaneTimesOut(t *testing.T) {
	fastIdlePoll(t)
	pk := newIdlePack(t, idleHandshake, busyClaudePane)
	err := NewProvider(pk.script).WaitForIdle(context.Background(), "s", 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForIdle on a busy pane = %v, want DeadlineExceeded", err)
	}
	if len(pk.captures(t)) == 0 {
		t.Fatal("no capture ran before the timeout")
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
// it, and the default comes back after Stop.
// Kills: ignoring Config.ReadyPromptPrefix (codex-style agents never read
// idle), and a cache that outlives the session.
func TestWaitForIdleUsesSessionReadyPrompt(t *testing.T) {
	fastIdlePoll(t)
	codex := newIdlePack(t, idleHandshake, idleCodexPane)
	p := NewProvider(codex.script)
	if err := p.Start(context.Background(), "s", runtime.Config{ReadyPromptPrefix: "› "}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle with the session's prompt on screen: %v", err)
	}
	if err := p.Stop("s"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 200*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForIdle after Stop on a codex pane = %v, want DeadlineExceeded (default prompt again)", err)
	}

	claude := newIdlePack(t, idleHandshake, idleClaudePane)
	p = NewProvider(claude.script)
	if err := p.Start(context.Background(), "s", runtime.Config{ReadyPromptPrefix: "› "}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 200*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForIdle for a codex session on a claude-only pane = %v, want DeadlineExceeded", err)
	}
	if err := p.Stop("s"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle after Stop on a claude pane: %v", err)
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
