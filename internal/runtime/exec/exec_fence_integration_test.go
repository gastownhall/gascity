//go:build integration

package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// tmuxConformancePrefix runs ahead of mockProviderScript: each box is a
// private tmux server (an explicit -S socket) whose session "main" runs an
// agent that ignores SIGINT and echoes its input, and the exec op runs the
// command with a tmux shim on PATH that targets that server. The mock
// answers every other op, and the start and stop ops fall through to it.
const tmuxConformancePrefix = `
STATE=%q
root=%q
op="$1"
name="$2"
box="$root/$name"
sock="$box/s"
real_tmux=$(command -v tmux)
bt() { "$real_tmux" -u -S "$sock" "$@"; }
case "$op" in
  protocol) printf '%%s' '{"version":0,"capabilities":["proc.exec","input-fence"]}'; exit 0 ;;
  start)
    if [ ! -f "$STATE/$name.running" ]; then
      mkdir -p "$box/bin"
      printf '#!/bin/sh\nexec %%s -u -S %%s "$@"\n' "$real_tmux" "$sock" > "$box/bin/tmux"
      chmod +x "$box/bin/tmux"
      bt new-session -d -s main -x 120 -y 40 "trap '' INT; exec cat" || exit 1
    fi ;;
  stop) bt kill-server 2>/dev/null; rm -rf "$box" ;;
  exec)
    cmd=$(cat)
    [ -f "$STATE/$name.running" ] && [ -d "$box/bin" ] || { echo "no box $name" >&2; exit 1; }
    env -u TMUX PATH="$box/bin:$PATH" sh -c "$cmd"
    exit $? ;;
esac
`

// newTmuxConformancePack writes the conformance pack; its state and boxes
// live directly under /tmp so the tmux socket paths stay short.
func newTmuxConformancePack(t *testing.T) string {
	t.Helper()
	if !tmuxOnPath() {
		t.Skip("tmux not installed")
	}
	root, err := os.MkdirTemp("/tmp", "gcxf-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	return writeScript(t, t.TempDir(), fmt.Sprintf(tmuxConformancePrefix, state, root)+mockProviderScript(state))
}

// The full provider contract over a real tmux per box, for a pack declaring
// the exec connection: the fenced-input cases run, since the carrier guard
// makes the pack atomic.
func TestExecConformanceTmuxBoxFenced(t *testing.T) {
	script := newTmuxConformancePack(t)
	var counter int64
	runtimetest.RunProviderTests(t, func(caseT *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBacked(script),
			runtime.Config{WorkDir: caseT.TempDir()},
			fmt.Sprintf("xf-%04d", atomic.AddInt64(&counter, 1))
	})
}

// Kills an exec relaunch that lets the old incarnation's fence reach the new
// agent, over real tmux: a separable pack's warm-box respawn changes the
// fence in the launch's own command list, and the old fence then misses
// without typing.
func TestExecRelaunchRefusesTheOldFenceRealTmux(t *testing.T) {
	script, root := newTmuxBoxPack(t)
	writeAgent(t, root, "rl", "exec cat\n")
	p := NewProvider(script)
	ctx := context.Background()
	env := func(gen string) map[string]string {
		return map[string]string{runtime.RuntimeEpochEnv: gen, runtime.ContinuationEpochEnv: "1", runtime.InstanceTokenEnv: "tok-" + gen}
	}
	t.Cleanup(func() { _ = p.Stop("rl") })
	if err := p.Start(ctx, "rl", runtime.Config{Env: env("1")}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first, _ := runtime.InputFenceFromEnv(env("1"))
	if err := p.NudgeFenced(ctx, "rl", runtime.TextContent("one"), runtime.FencedNudge{Fence: first}); err != nil {
		t.Fatalf("NudgeFenced under the start's fence: %v", err)
	}
	// The welded pack's relaunch path is a new box; drive the warm-box
	// respawn the separable path issues directly.
	argv := append([]string{"tmux", "respawn-pane", "-k", "-t", execTmuxSession, "sh " + filepath.Join(root, "rl.agent")}, p.launchFenceArgs("rl", runtime.Config{Env: env("2")})...)
	if _, code, err := p.Exec(ctx, "rl", argv); err != nil || code != 0 {
		t.Fatalf("respawn: code %d, %v", code, err)
	}
	err := p.NudgeFenced(ctx, "rl", runtime.TextContent("stale"), runtime.FencedNudge{Fence: first})
	second, _ := runtime.InputFenceFromEnv(env("2"))
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != second.Digest() {
		t.Fatalf("NudgeFenced with the old fence = %v, want a miss finding the new incarnation", err)
	}
	time.Sleep(200 * time.Millisecond)
	if out, _ := p.Peek("rl", 20); strings.Contains(out, "stale") {
		t.Fatalf("the relaunched agent received the old fence's nudge: %q", out)
	}
}

// Kills a colliding start that strips the live session's fence, over real
// tmux: a second Start of a running session clears the pane's fence before
// the start op, the pack refuses because the session exists, and the fence
// goes back on the live pane, which still takes its fenced input.
func TestExecCollidingStartKeepsTheLiveFenceRealTmux(t *testing.T) {
	script := newTmuxConformancePack(t)
	p := NewProvider(script)
	ctx := context.Background()
	env := map[string]string{runtime.RuntimeEpochEnv: "1", runtime.ContinuationEpochEnv: "1", runtime.InstanceTokenEnv: "tok-1"}
	t.Cleanup(func() { _ = p.Stop("dup") })
	if err := p.Start(ctx, "dup", runtime.Config{Env: env}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	live, _ := runtime.InputFenceFromEnv(env)
	if err := p.NudgeFenced(ctx, "dup", runtime.TextContent("one"), runtime.FencedNudge{Fence: live}); err != nil {
		t.Fatalf("NudgeFenced before the collision: %v", err)
	}
	if err := p.Start(ctx, "dup", runtime.Config{Env: env}); !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("colliding Start = %v, want ErrSessionExists", err)
	}
	if !p.IsRunning("dup") {
		t.Fatal("the colliding Start tore the live box down")
	}
	if err := p.NudgeFenced(ctx, "dup", runtime.TextContent("two"), runtime.FencedNudge{Fence: live}); err != nil {
		t.Fatalf("NudgeFenced after the colliding Start = %v, want delivered to the live incarnation", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := p.Peek("dup", 20)
		if strings.Contains(out, "two") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the live agent never echoed the fenced nudge; pane: %q", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
