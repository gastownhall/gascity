//go:build integration

package main

import (
	"context"
	"fmt"
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
