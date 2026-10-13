//go:build integration

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

// TestStartRecycleQuarantinesARateLimitCorpseOnRealTmux (mc-g3la1; staging
// J04). Kills a start whose recycle relaunches a seat that died on a
// provider rate-limit screen: on the seam-backed tmux provider gc runs, the
// row's own remain-on-exit corpse showing the screen is peeked, quarantined
// 30 minutes (SESS-530) and left in place, never killed or relaunched. The
// seam's Peek read a dead pane as empty, so the recycle saw no output.
func TestStartRecycleQuarantinesARateLimitCorpseOnRealTmux(t *testing.T) {
	tmuxtest.RequireTmux(t)
	cfg := sessiontmux.DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gc-rl-%d", time.Now().UnixNano()%1_000_000_000)
	sp := sessiontmux.NewSeamBackedWithConfig(cfg)
	t.Cleanup(func() { _ = sp.(interface{ TeardownServer() error }).TeardownServer() })

	recent := gatherNow.Add(-2 * time.Minute).Format(time.RFC3339)
	k := startKit(t, intentStart, "awake", "last_woke_at", recent)
	k.it.Deadline = time.Now().Add(70 * time.Second)
	env := map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok", "GC_RUNTIME_EPOCH": "3"}
	screen := `sh -c 'echo "You'"'"'ve hit your limit · resets 3am (UTC)"; echo "/rate-limit-options"; sleep 1; exit 1'`
	if err := sp.Start(context.Background(), "s-gc-1", runtime.Config{Command: screen, Env: env}); err != nil {
		t.Fatalf("start: %v", err)
	}
	timer, tick := time.NewTimer(15*time.Second), time.NewTicker(50*time.Millisecond)
	defer timer.Stop()
	defer tick.Stop()
	for sp.IsRunning("s-gc-1") {
		select {
		case <-timer.C:
			t.Fatal("the pane never died")
		case <-tick.C:
		}
	}
	observe := func() runtime.Liveness {
		l, err := sp.(runtime.FreshLivenessObserver).ObserveLivenessSince("s-gc-1", nil, time.Now())
		if err != nil {
			t.Fatalf("fresh read: %v", err)
		}
		return l
	}
	before := observe()
	if !before.Corpse || before.ObjectID == "" {
		t.Fatalf("before the recycle: %+v, want a corpse (pane_dead=1) with its session object", before)
	}
	k.p.Runtime = sp

	s := k.runKind()
	if k.meta("sleep_reason") != string(session.SleepReasonRateLimit) || k.meta("quarantined_until") == "" {
		t.Fatalf("settlement %+v reason %q quarantine %q, want the rate-limit quarantine", s, k.meta("sleep_reason"), k.meta("quarantined_until"))
	}
	if after := observe(); !after.Corpse || after.ObjectID != before.ObjectID || after.ObjectCreated != before.ObjectCreated {
		t.Fatalf("after the recycle: %+v, want the same corpse (session %s created %s, pane_dead=1) left in place, nothing killed or relaunched", after, before.ObjectID, before.ObjectCreated)
	}
}

// recycleTmuxLeaf is the seam-backed tmux provider's surface the recycle
// reads, with a hook run inside Peek, after the classification's read.
type recycleTmuxLeaf interface {
	runtime.Provider
	runtime.LivenessObserverWithError
	runtime.FreshLivenessObserver
	runtime.SessionObjectKiller
	runtime.AttachmentObserverWithError
	runtime.EnvironmentBatchProvider
}

type peekHookLeaf struct {
	recycleTmuxLeaf
	inPeek func()
}

func (l *peekHookLeaf) Peek(name string, lines int) (string, error) {
	if f := l.inPeek; f != nil {
		l.inPeek = nil
		f()
	}
	return l.recycleTmuxLeaf.Peek(name, lines)
}

// realTmux is a private seam-backed tmux server and a tmux command on it.
func realTmux(t *testing.T, tag string) (runtime.Provider, func(args ...string) string) {
	t.Helper()
	tmuxtest.RequireTmux(t)
	cfg := sessiontmux.DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gc-%s-%d", tag, time.Now().UnixNano()%1_000_000_000)
	sp := sessiontmux.NewSeamBackedWithConfig(cfg)
	t.Cleanup(func() { _ = sp.(interface{ TeardownServer() error }).TeardownServer() })
	tm := func(args ...string) string {
		out, _ := exec.Command("tmux", append([]string{"-L", cfg.SocketName}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	return sp, tm
}

// waitTmux polls ready every 50ms for up to 15s.
func waitTmux(t *testing.T, what string, ready func() bool) {
	t.Helper()
	timer, tick := time.NewTimer(15*time.Second), time.NewTicker(50*time.Millisecond)
	defer timer.Stop()
	defer tick.Stop()
	for !ready() {
		select {
		case <-timer.C:
			t.Fatalf("never %s", what)
		case <-tick.C:
		}
	}
}

// TestStartRecycleSparesAZombiesOperatorPaneOnRealTmux (the #7762 r2
// review's must-fix). Kills a recycle that kills an operator's live pane: an
// own zombie whose agent pane is gone (the operator's split is its only,
// live pane) peeks ErrTmuxAgentPaneGone, which only a corpse reads as empty;
// the recycle refuses, KillZombieObject never runs, the session object and
// the operator's pane stand, and nothing starts.
func TestStartRecycleSparesAZombiesOperatorPaneOnRealTmux(t *testing.T) {
	sp, tm := realTmux(t, "zag")
	env := map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok", "GC_RUNTIME_EPOCH": "3"}
	if err := sp.Start(context.Background(), "s-gc-1", runtime.Config{Command: "sleep 300", Env: env}); err != nil {
		t.Fatalf("start: %v", err)
	}
	agent := tm("list-panes", "-s", "-t", "=s-gc-1:", "-F", "#{pane_id}")
	tm("split-window", "-d", "-t", "=s-gc-1:", "sh -c 'echo operator work in progress; sleep 600'")
	tm("kill-pane", "-t", agent)
	waitTmux(t, "one live operator pane", func() bool {
		return tm("list-panes", "-s", "-t", "=s-gc-1:", "-F", "#{pane_dead}") == "0"
	})
	panes := func() string {
		return tm("list-panes", "-s", "-t", "=s-gc-1:", "-F", "#{session_id}/#{session_created} #{pane_id} pid=#{pane_pid} dead=#{pane_dead}")
	}
	before := panes()
	k := startKit(t, intentStart, "awake", "last_woke_at", gatherNow.Add(-2*time.Minute).Format(time.RFC3339))
	k.it.Deadline = time.Now().Add(70 * time.Second)
	k.p.Runtime = sp
	s := k.runKind()
	if after := panes(); s.Cause != causePeekError || after != before || len(k.starts()) != 0 {
		t.Fatalf("settlement %+v, starts %d; panes %q, was %q; want refused peek-error, the operator's pane and its session untouched", s, len(k.starts()), after, before)
	}
}

// TestStartRecyclePinsThePeeksObjectOnRealTmux (the #7762 r2 review). Kills
// a screen attributed to a session object that replaced the classified one
// during the peek, or a post-capture identity check answered from the
// snapshot taken before the capture: the row's own rate-limit corpse is
// killed and a new session showing a terminal error takes the name inside
// Peek; the recycle refuses peek-error and parks nothing.
func TestStartRecyclePinsThePeeksObjectOnRealTmux(t *testing.T) {
	sp, tm := realTmux(t, "pin")
	env := map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok", "GC_RUNTIME_EPOCH": "3"}
	screen := `sh -c 'echo "You'"'"'ve hit your limit · resets 3am (UTC)"; echo "/rate-limit-options"; sleep 1; exit 1'`
	if err := sp.Start(context.Background(), "s-gc-1", runtime.Config{Command: screen, Env: env}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitTmux(t, "a corpse", func() bool { return tm("list-panes", "-s", "-t", "=s-gc-1:", "-F", "#{pane_dead}") == "1" })
	object := func() string {
		return tm("display-message", "-p", "-t", "=s-gc-1:", "#{session_id}/#{session_created}")
	}
	before := object()
	leaf := &peekHookLeaf{recycleTmuxLeaf: sp.(recycleTmuxLeaf)}
	leaf.inPeek = func() {
		tm("kill-session", "-t", "=s-gc-1")
		second := time.Now().Unix()
		waitTmux(t, "a new second", func() bool { return time.Now().Unix() > second }) // a distinct session_created
		tm("new-session", "-d", "-s", "s-gc-1", "sh -c 'echo Error: model_not_found; sleep 300'")
		waitTmux(t, "the new object's screen", func() bool {
			return strings.Contains(tm("capture-pane", "-p", "-t", "=s-gc-1:"), "model_not_found")
		})
	}
	k := startKit(t, intentStart, "awake", "last_woke_at", gatherNow.Add(-2*time.Minute).Format(time.RFC3339))
	k.it.Deadline = time.Now().Add(70 * time.Second)
	k.p.Runtime = leaf
	s := k.runKind()
	if s.Cause != causePeekError || k.meta("sleep_reason") != "" || k.meta("provider_terminal_error") != "" {
		t.Fatalf("object %q replaced by %q during the peek: settlement %+v, sleep_reason %q, provider_terminal_error %q; want refused peek-error, nothing parked", before, object(), s, k.meta("sleep_reason"), k.meta("provider_terminal_error"))
	}
}
