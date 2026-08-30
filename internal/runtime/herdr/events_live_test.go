package herdr

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// waitForEvent drains the stream until an event matches, tolerating
// interleaved noise (resyncs from resubscribe cycles, stray-pane events,
// unrelated status flaps).
func waitForEvent(t *testing.T, ch <-chan runtime.SessionEvent, timeout time.Duration, match func(runtime.SessionEvent) bool) runtime.SessionEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("event channel closed while waiting")
			}
			if match(ev) {
				return ev
			}
			t.Logf("  (skipping %s session=%q status=%q ref=%q)", ev.Kind, ev.Session, ev.AgentStatus, ev.Ref)
		case <-deadline:
			t.Fatalf("timed out after %v waiting for matching event", timeout)
		}
	}
}

// TestSessionEventsLive drives SubscribeSessionEvents against a real herdr
// binary in an isolated session: leading resync, forced agent-status change
// (herdr pane report-agent), dynamic resubscribe for an agent started after
// the stream came up, natural process exit, pane close via Stop, and stream
// re-attach across a full server bounce. Skipped when herdr is unavailable or
// in -short mode.
func TestSessionEventsLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr test in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}

	const session = "gctest-events-live"
	p := New(session, t.TempDir(), t.TempDir(), 0, 0)
	_ = p.c.stopServer() // clear any leftover server from a crashed prior run
	t.Cleanup(func() { _ = p.TeardownServer() })
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// evt-a exists before the stream: its pane rides the initial filter set.
	cfgA := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 120"}
	if err := p.Start(ctx, "evt-a", cfgA); err != nil {
		t.Fatalf("Start evt-a: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop("evt-a") })

	ch, err := p.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	waitForEvent(t, ch, 10*time.Second, func(ev runtime.SessionEvent) bool {
		return ev.Kind == runtime.SessionEventResync
	})

	// Forced status change on evt-a's pane must arrive attributed.
	a, ok, err := p.c.getAgent(ctx, "evt-a")
	if err != nil || !ok {
		t.Fatalf("getAgent evt-a: ok=%v err=%v", ok, err)
	}
	report := func(pane, state string) {
		t.Helper()
		out, err := exec.Command("herdr", "--session", session, "pane", "report-agent", pane,
			"--source", "gctest", "--agent", "gctest", "--state", state).CombinedOutput()
		if err != nil {
			t.Fatalf("pane report-agent %s %s: %v: %s", pane, state, err, out)
		}
	}
	report(a.PaneID, "working")
	ev := waitForEvent(t, ch, 10*time.Second, func(ev runtime.SessionEvent) bool {
		return ev.Kind == runtime.SessionEventAgentStatus && ev.Session == "evt-a"
	})
	if ev.AgentStatus != "working" {
		t.Errorf("evt-a status event = %q, want working", ev.AgentStatus)
	}

	// evt-b starts while the stream is live: pane_created → debounced re-list
	// → resubscribe; its status events must then flow, attributed.
	cfgB := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 120"}
	if err := p.Start(ctx, "evt-b", cfgB); err != nil {
		t.Fatalf("Start evt-b: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop("evt-b") })
	b, ok, err := p.c.getAgent(ctx, "evt-b")
	if err != nil || !ok {
		t.Fatalf("getAgent evt-b: ok=%v err=%v", ok, err)
	}
	// The resubscribe cycle emits a fresh resync; wait for it so the report
	// below races nothing.
	waitForEvent(t, ch, 15*time.Second, func(ev runtime.SessionEvent) bool {
		return ev.Kind == runtime.SessionEventResync
	})
	report(b.PaneID, "blocked")
	ev = waitForEvent(t, ch, 10*time.Second, func(ev runtime.SessionEvent) bool {
		return ev.Kind == runtime.SessionEventAgentStatus && ev.Session == "evt-b"
	})
	if ev.AgentStatus != "blocked" {
		t.Errorf("evt-b status event = %q, want blocked", ev.AgentStatus)
	}

	// evt-c exits on its own: pane_exited must arrive attributed (the
	// merge-only pane map keeps the mapping even if herdr reaps the agent
	// record first).
	cfgC := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 3"}
	if err := p.Start(ctx, "evt-c", cfgC); err != nil {
		t.Fatalf("Start evt-c: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop("evt-c") })
	waitForEvent(t, ch, 20*time.Second, func(ev runtime.SessionEvent) bool {
		return ev.Kind == runtime.SessionEventExited && ev.Session == "evt-c"
	})

	// Stop closes the pane: pane_closed must arrive attributed.
	if err := p.Stop("evt-b"); err != nil {
		t.Fatalf("Stop evt-b: %v", err)
	}
	waitForEvent(t, ch, 10*time.Second, func(ev runtime.SessionEvent) bool {
		return ev.Kind == runtime.SessionEventClosed && ev.Session == "evt-b"
	})

	// Full server bounce: the stream must re-attach and lead with a resync.
	if err := p.TeardownServer(); err != nil {
		t.Fatalf("TeardownServer: %v", err)
	}
	time.Sleep(1 * time.Second) // let the drop register before the server returns
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer (bounce): %v", err)
	}
	waitForEvent(t, ch, 20*time.Second, func(ev runtime.SessionEvent) bool {
		return ev.Kind == runtime.SessionEventResync
	})

	cancel()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed within 3s of ctx cancel")
		}
	}
}
