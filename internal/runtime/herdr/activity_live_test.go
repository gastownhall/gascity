package herdr

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestActivityLive drives GetLastActivity against a real herdr binary in an
// isolated session: synchronous cold seed, forced status transitions (herdr
// pane report-agent) stamping and freezing, working reading as continuously
// active, the unknown-status revision leg (quiet pane ages, real output
// re-stamps), and removal dropping to zero. Skipped when herdr is unavailable
// or in -short mode.
func TestActivityLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr test in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}

	shrinkActivityKnobs(t)

	const session = "gctest-activity-live"
	p := New(session, t.TempDir(), t.TempDir(), 0)
	_ = p.c.stopServer() // clear any leftover server from a crashed prior run
	t.Cleanup(func() { _ = p.TeardownServer() })
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}
	t.Cleanup(p.act.stop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// act-a idles in cat: no agent integration, no output until fed input.
	if err := p.Start(ctx, "act-a", liveActivityCfg(t)); err != nil {
		t.Fatalf("Start act-a: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop("act-a") })

	// Cold seed is synchronous: the very first call observes the live agent.
	first := lastActivity(t, p, "act-a")
	if first.IsZero() {
		t.Fatal("cold seed returned zero for a live agent")
	}
	t.Logf("seeded: %v (age %v)", first, time.Since(first))

	a, ok, err := p.c.getAgent(ctx, "act-a")
	if err != nil || !ok {
		t.Fatalf("getAgent act-a: ok=%v err=%v", ok, err)
	}
	t.Logf("natural agent_status of an undetected pane: %q (revision %d)", a.AgentStatus, a.Revision)

	report := func(state string) {
		t.Helper()
		out, err := exec.Command("herdr", "--session", session, "pane", "report-agent", a.PaneID,
			"--source", "gctest", "--agent", "gctest", "--state", state).CombinedOutput()
		if err != nil {
			t.Fatalf("pane report-agent %s: %v: %s", state, err, out)
		}
	}

	// working → continuously active: successive reads advance and stay ~now.
	report("working")
	waitActivity(t, p, "act-a", 5*time.Second, func(got time.Time) bool {
		return !got.IsZero() && time.Since(got) < 100*time.Millisecond
	})
	w1 := lastActivity(t, p, "act-a")
	time.Sleep(30 * time.Millisecond)
	w2 := lastActivity(t, p, "act-a")
	if !w2.After(w1) {
		t.Fatalf("working must read continuously active: %v then %v", w1, w2)
	}

	// working → idle: the stamp freezes at the observed transition and ages.
	report("idle")
	frozen := waitActivity(t, p, "act-a", 5*time.Second, func(got time.Time) bool {
		return !got.IsZero() && time.Since(got) > 150*time.Millisecond
	})
	time.Sleep(100 * time.Millisecond)
	if again := lastActivity(t, p, "act-a"); !again.Equal(frozen) {
		t.Fatalf("idle stamp must freeze: %v then %v", frozen, again)
	}
	t.Logf("idle stamp frozen at %v", frozen)

	// Unknown-status revision leg: reported states stick, so use a second
	// undetected pane. A quiet cat must age. Whether real output re-stamps
	// depends on the environment — herdr 0.7.3 moves a pane's revision only
	// while a client renders it, so on a HEADLESS server (gc's normal mode,
	// verified live: a pane printing every 300ms holds revision 0) output is
	// invisible to the tracker and the stamp must keep aging; with a client
	// attached the revision moves and the stamp must refresh. The leg asserts
	// whichever contract matches the observed revision behavior.
	if strings.EqualFold(strings.TrimSpace(a.AgentStatus), agentStatusUnknown) {
		if err := p.Start(ctx, "act-b", liveActivityCfg(t)); err != nil {
			t.Fatalf("Start act-b: %v", err)
		}
		t.Cleanup(func() { _ = p.Stop("act-b") })
		waitActivity(t, p, "act-b", 5*time.Second, func(got time.Time) bool {
			return !got.IsZero()
		})
		// Let any startup output settle so the aging window is clean.
		time.Sleep(300 * time.Millisecond)
		aged := lastActivity(t, p, "act-b")
		time.Sleep(150 * time.Millisecond)
		if got := lastActivity(t, p, "act-b"); !got.Equal(aged) {
			t.Fatalf("quiet unknown-status pane must age, not re-stamp: %v then %v", aged, got)
		}
		b, ok, err := p.c.getAgent(ctx, "act-b")
		if err != nil || !ok {
			t.Fatalf("getAgent act-b: ok=%v err=%v", ok, err)
		}
		out, err := exec.Command("herdr", "--session", session, "pane", "run", b.PaneID, "revision-poke").CombinedOutput()
		if err != nil {
			t.Fatalf("pane run: %v: %s", err, out)
		}
		time.Sleep(500 * time.Millisecond) // several shrunk poll intervals
		after, ok, err := p.c.getAgent(ctx, "act-b")
		if err != nil || !ok {
			t.Fatalf("getAgent act-b after poke: ok=%v err=%v", ok, err)
		}
		if after.Revision != b.Revision {
			bumped := waitActivity(t, p, "act-b", 5*time.Second, func(got time.Time) bool {
				return got.After(aged)
			})
			t.Logf("revision leg (rendered: %d→%d): output re-stamped %v", b.Revision, after.Revision, bumped)
		} else {
			if got := lastActivity(t, p, "act-b"); !got.Equal(aged) {
				t.Fatalf("revision held at %d but the stamp moved: %v then %v", after.Revision, aged, got)
			}
			t.Logf("revision leg (headless: revision held at %d): output invisible, stamp kept aging as designed", after.Revision)
		}
	} else {
		t.Logf("skipping revision leg: undetected pane reports %q, not %q", a.AgentStatus, agentStatusUnknown)
	}

	// Stop → the agent leaves agent.list → activity drops to zero (unknown).
	if err := p.Stop("act-a"); err != nil {
		t.Fatalf("Stop act-a: %v", err)
	}
	waitActivity(t, p, "act-a", 10*time.Second, func(got time.Time) bool {
		return got.IsZero()
	})
}

// liveActivityCfg is the live-test agent config: cat idles forever with no
// output until fed input, which makes both the aging and the re-stamp legs
// deterministic.
func liveActivityCfg(t *testing.T) runtime.Config {
	t.Helper()
	return runtime.Config{WorkDir: t.TempDir(), Command: "cat"}
}
