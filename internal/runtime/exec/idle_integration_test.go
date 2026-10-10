//go:build integration

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

// tmuxBoxPack is a generic tmux-in-box exec pack: each session is a box with
// its own tmux server (a private socket), running the agent in session
// "main". The exec op runs the command with a tmux shim on PATH that targets
// the box's server, so the orchestrator's in-box tmux commands reach it. The
// agent a box runs is the file <root>/<name>.agent.
const tmuxBoxPack = `
op="$1"
name="$2"
root=%q
box="$root/$name"
sock="$box/s"
real_tmux=$(command -v tmux)
export LC_ALL=C.UTF-8
bt() { "$real_tmux" -u -S "$sock" "$@"; }
case "$op" in
  protocol) printf '%%s' '{"version":0,"capabilities":["proc.exec","report-activity","report-attachment"]}' ;;
  start)
    cat > /dev/null
    mkdir -p "$box/bin"
    printf '#!/bin/sh\nexport LC_ALL=C.UTF-8\nexec %%s -u -S %%s "$@"\n' "$real_tmux" "$sock" > "$box/bin/tmux"
    chmod +x "$box/bin/tmux"
    bt new-session -d -s main -x 120 -y 40 "sh $root/$name.agent" ;;
  stop) bt kill-server 2>/dev/null; rm -rf "$box"; exit 0 ;;
  is-running) bt has-session -t main 2>/dev/null && echo true || echo false ;;
  is-attached)
    if ! bt has-session -t main 2>/dev/null; then echo false; exit 0; fi
    n=$(bt display-message -p -t main '#{session_attached}') || exit 1
    if [ "${n:-0}" -gt 0 ]; then echo true; else echo false; fi ;;
  get-last-activity)
    out=$(bt list-windows -t main -F '#{window_activity}' 2>/dev/null) || exit 0
    max=0
    for v in $out; do [ "$v" -gt "$max" ] && max=$v; done
    [ "$max" -gt 0 ] || exit 0
    date -u -d "@$max" +%%Y-%%m-%%dT%%H:%%M:%%SZ 2>/dev/null || date -u -r "$max" +%%Y-%%m-%%dT%%H:%%M:%%SZ ;;
  exec)
    cmd=$(cat)
    [ -d "$box/bin" ] || { echo "no box $name" >&2; exit 1; }
    PATH="$box/bin:$PATH" sh -c "$cmd" ;;
  *) exit 2 ;;
esac
`

// tmuxOnPath reports whether a tmux executable is on PATH.
func tmuxOnPath() bool {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if info, err := os.Stat(filepath.Join(dir, "tmux")); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// newTmuxBoxPack writes the pack and returns its script and box root. The root
// lives directly under /tmp so the tmux socket paths stay short.
func newTmuxBoxPack(t *testing.T) (script, root string) {
	t.Helper()
	if !tmuxOnPath() {
		t.Skip("tmux not installed")
	}
	root, err := os.MkdirTemp("/tmp", "gcx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return writeScript(t, t.TempDir(), fmt.Sprintf(tmuxBoxPack, root)), root
}

func writeAgent(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name+".agent"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Over a real tmux server per box, a pack declaring the idle boundary gets
// full idle sleep: WaitForIdle proves an idle prompt, times out on a busy
// pane, honors a custom ready prompt, and the activity and attachment ops
// answer from the box's tmux.
func TestTmuxBoxPackIdleBoundary(t *testing.T) {
	script, root := newTmuxBoxPack(t)
	p := NewProvider(script)
	ctx := context.Background()
	start := func(name string, cfg runtime.Config) {
		t.Helper()
		t.Cleanup(func() { _ = p.Stop(name) })
		if err := p.Start(ctx, name, cfg); err != nil {
			t.Fatalf("Start %s: %v", name, err)
		}
	}

	writeAgent(t, root, "idle", "printf 'work done\\n\\n❯ \\n'\nexec sleep 3600\n")
	writeAgent(t, root, "busy", "printf '❯ \\n· Working… (12s · ↓ 1.2k tokens)\\n'\nexec sleep 3600\n")
	writeAgent(t, root, "custom", "printf '> \\n'\nexec sleep 3600\n")
	start("idle", runtime.Config{})
	start("busy", runtime.Config{})
	start("custom", runtime.Config{ReadyPromptPrefix: "> "})

	if got := p.SleepCapability("idle"); got != runtime.SessionSleepCapabilityFull {
		t.Fatalf("SleepCapability = %q, want full", got)
	}
	if err := p.WaitForIdle(ctx, "idle", 10*time.Second); err != nil {
		out, _ := p.Peek("idle", 20)
		t.Fatalf("WaitForIdle on an idle box: %v\npane:\n%s", err, out)
	}
	if err := p.WaitForIdle(ctx, "custom", 10*time.Second); err != nil {
		t.Fatalf("WaitForIdle on a box showing its custom prompt: %v", err)
	}
	began := time.Now()
	if err := p.WaitForIdle(ctx, "busy", time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForIdle on a busy box = %v, want DeadlineExceeded", err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("WaitForIdle on a busy box took %v with a 1s timeout", took)
	}
	if err := NewSeamBacked(script).(runtime.IdleWaitProvider).WaitForIdle(ctx, "idle", 10*time.Second); err != nil {
		t.Fatalf("seam-backed WaitForIdle on an idle box: %v", err)
	}

	if attached, err := p.IsAttachedWithError("idle"); err != nil || attached {
		t.Fatalf("IsAttachedWithError on a detached box = (%v, %v), want (false, nil)", attached, err)
	}
	at, err := p.GetLastActivity("idle")
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	if at.IsZero() || time.Since(at) > 5*time.Minute || time.Until(at) > time.Minute {
		t.Fatalf("GetLastActivity = %v, want the box's recent window_activity", at)
	}

	if err := p.Stop("idle"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if attached, err := p.IsAttachedWithError("idle"); err != nil || attached {
		t.Fatalf("IsAttachedWithError on a stopped box = (%v, %v), want (false, nil)", attached, err)
	}
	if err := p.WaitForIdle(ctx, "idle", 500*time.Millisecond); err == nil || !strings.Contains(err.Error(), "not idle") {
		t.Fatalf("WaitForIdle on a stopped box = %v, want a not-idle timeout", err)
	}
}
