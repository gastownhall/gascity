package herdr

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestActivityLive drives GetLastActivity against a real herdr binary in an
// isolated session: synchronous cold seed, forced status transitions (herdr
// pane report-agent) stamping and freezing, working reading as continuously
// active, the unknown-status revision leg (quiet pane ages, real output
// re-stamps), and removal dropping to zero. Opt-in live tier: see
// requireLiveHerdr.
func TestActivityLive(t *testing.T) {
	requireLiveHerdr(t)

	shrinkActivityKnobs(t)

	const session = "gctest-activity-live"
	p := New(session, t.TempDir(), t.TempDir(), 0, 0)
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

	// herdr does not list, and therefore cannot name, a pane until something
	// reports an agent onto it (`pane report-agent`/`pane report-agent-session`).
	// A bare integration-less pane sits in `pane list` with no name field at
	// all, so name-keyed queries (agent.list/agent.get, and this tracker) have
	// nothing to seed from until the first report. Resolve the pane id via
	// `pane list` (name-independent) and report once to establish identity —
	// this mirrors what any real herdr-integrated agent does on attach.
	paneID := firstPaneID(t, session)
	report := func(state string) {
		t.Helper()
		out, err := exec.Command("herdr", "--session", session, "pane", "report-agent", paneID,
			"--source", "gctest", "--agent", "act-a", "--state", state).CombinedOutput()
		if err != nil {
			t.Fatalf("pane report-agent %s: %v: %s", state, err, out)
		}
	}
	report("idle")

	// Cold seed is synchronous: the very first call after identity is
	// established observes the live agent.
	first := lastActivity(t, p, "act-a")
	if first.IsZero() {
		t.Fatal("cold seed returned zero for a live, reported agent")
	}
	t.Logf("seeded: %v (age %v)", first, time.Since(first))

	a, ok, err := p.c.getAgent(ctx, "act-a")
	if err != nil || !ok {
		t.Fatalf("getAgent act-a: ok=%v err=%v", ok, err)
	}
	t.Logf("natural agent_status of a reported-idle pane: %q (revision %d)", a.AgentStatus, a.Revision)

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
	// pane reported into the "unknown" state (herdr never names/lists a pane
	// nothing has reported an agent onto, so this leg needs its own report
	// call, same as act-a above — an unreported pane never appears here at
	// all). A quiet cat must age. Whether real output re-stamps depends on
	// the environment — herdr 0.7.3 moves a pane's revision only while a
	// client renders it, so on a HEADLESS server (gc's normal mode, verified
	// live: a pane printing every 300ms holds revision 0) output is invisible
	// to the tracker and the stamp must keep aging; with a client attached
	// the revision moves and the stamp must refresh. The leg asserts
	// whichever contract matches the observed revision behavior.
	if err := p.Start(ctx, "act-b", liveActivityCfg(t)); err != nil {
		t.Fatalf("Start act-b: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop("act-b") })
	bPaneID := firstPaneID(t, session, paneID)
	out, err := exec.Command("herdr", "--session", session, "pane", "report-agent", bPaneID,
		"--source", "gctest", "--agent", "act-b", "--state", agentStatusUnknown).CombinedOutput()
	if err != nil {
		t.Fatalf("pane report-agent unknown: %v: %s", err, out)
	}
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
	pokeOut, err := exec.Command("herdr", "--session", session, "pane", "run", b.PaneID, "revision-poke").CombinedOutput()
	if err != nil {
		t.Fatalf("pane run: %v: %s", err, pokeOut)
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

// firstPaneID returns a pane id from `herdr pane list` not present in
// exclude, for a session whose panes have no agent reported onto them yet
// (an unreported pane carries no name, so `pane list` — not agent.list — is
// the only name-independent way to find it).
func firstPaneID(t *testing.T, session string, exclude ...string) string {
	t.Helper()
	skip := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		skip[id] = true
	}
	out, err := exec.Command("herdr", "--session", session, "pane", "list").CombinedOutput()
	if err != nil {
		t.Fatalf("pane list: %v: %s", err, out)
	}
	var resp struct {
		Result struct {
			Panes []struct {
				PaneID string `json:"pane_id"`
			} `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("pane list: unmarshal: %v: %s", err, out)
	}
	for _, pane := range resp.Result.Panes {
		if !skip[pane.PaneID] {
			return pane.PaneID
		}
	}
	t.Fatalf("pane list: no unexcluded pane found: %s", out)
	return ""
}
