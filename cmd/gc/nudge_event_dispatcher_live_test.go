package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
)

// TestNudgeEventDispatcherLiveHerdr drives the dispatcher against a real herdr
// binary: the provider's own event stream, herdr's own agent registry, and a
// real status report are what wake it, not a fake. What it asserts is the chain
// this change adds — an idle status report for a registered pane arrives as an
// attributed event and becomes a delivery pass for THAT session, while the
// sidecar poller class stays retired (its directory empty).
//
// It deliberately stops short of asserting the text reaches the pane, because a
// fixture cannot get there on herdr 0.8.0. The pane runs a bare shell, so the
// agent herdr has registered for it is never an "active named agent": `agent
// prompt` is refused with agent_not_ready, and the paste fallback is refused in
// turn, on purpose, because a registered pane may have an agent mid-turn
// (client.go targetHasNoNamedAgent). Only a real agent TUI completes delivery,
// which is the live work cycle's job rather than a unit fixture's. The
// dispatcher's own delivery mechanics are covered against a fake, where the
// closed loop can be driven end to end.
func TestNudgeEventDispatcherLiveHerdr(t *testing.T) {
	herdrtest.RequireLive(t)
	t.Setenv("GC_BEADS", "file")

	// Unique per run: herdr persists session state (agent names included)
	// across server restarts, so a fixed name collides with a prior run's
	// leftovers.
	herdrSession := fmt.Sprintf("gctest-nudge-dispatch-%d", time.Now().UnixNano())
	cityPath := t.TempDir()
	p := herdr.New(herdrSession, t.TempDir(), cityPath, 0, 0)
	_ = p.TeardownServer() // clear any leftover server from a crashed prior run
	t.Cleanup(func() { _ = p.TeardownServer() })
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}

	// Start the runtime directly on the provider: a bare interactive sh pane.
	// herdr 0.8.0 registers no agent for a pane started from a raw command, so
	// the first status report below is what puts this pane in the registry the
	// event stream reads — without it every frame for the pane arrives
	// unattributed and the dispatcher drops it by design. The session bead
	// exists only for target resolution.
	const agentName = "nudge-live-a"
	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	if err := p.Start(startCtx, agentName, runtime.Config{WorkDir: cityPath, Command: "/bin/sh"}); err != nil {
		t.Fatalf("provider Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(agentName) })

	store := openNudgeBeadStore(cityPath)
	if store.Store == nil {
		t.Fatal("opening city bead store")
	}
	if _, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": agentName,
			"alias":        "worker",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("creating session bead: %v", err)
	}

	herdrtest.Poll(t, fmt.Sprintf("agent %q registering with herdr", agentName), 15*time.Second, func() (bool, string) {
		return p.IsRunning(agentName), "not running yet"
	})

	ctx, cancel := context.WithCancel(context.Background())
	d := newNudgeEventDispatcher(ctx, cityPath, testWriter(t), "live", testNudgeDispatchStores(cityPath))
	seen := newPasses()
	d.observePasses(seen.record)
	d.update(p, &config.City{}, true)
	defer func() {
		cancel()
		select {
		case <-d.workerDone:
		case <-time.After(5 * time.Second):
			t.Log("dispatcher worker did not stop within 5s")
		}
	}()
	if !d.streaming() {
		t.Fatal("dispatcher not streaming against live herdr")
	}
	// The subscription's leading resync runs a full pass against an empty
	// queue; drain it so the passes asserted below are the ones this test
	// causes.
	seen.next(t, "the subscription's leading resync pass")

	// The agent is BUSY when the nudge is queued — the wait-idle contract. This
	// report also creates the pane's agent registration, under the gc session
	// name, which is what lets the stream attribute later frames.
	pane := func() string { return herdrLivePaneID(p, agentName) }
	herdrtest.ReportAgent(t, herdrSession, agentName, "working", pane)
	const nudgeText = "wait satisfied: live-dispatch proceed"
	if err := enqueueQueuedNudge(cityPath, newQueuedNudge("worker", nudgeText, time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// The idle transition is the event under test: herdr reports it, the
	// provider translates it into the semantic idle kind, and the dispatcher
	// must turn it into a pass for this session and no other.
	herdrtest.ReportAgent(t, herdrSession, agentName, "idle", pane)
	idleAt := time.Now()
	for {
		filter := seen.next(t, "a delivery pass caused by the idle report")
		if filter == agentName {
			t.Logf("idle report reached the dispatcher as a pass for %q after %.1fs", agentName, time.Since(idleAt).Seconds())
			break
		}
		// A resync from herdr's own resubscribe cycle runs a full pass; keep
		// reading until the attributed one arrives (or next fails the test).
		if filter != "" {
			t.Fatalf("pass ran for session %q, want %q: the idle event was attributed to the wrong session", filter, agentName)
		}
	}

	// The queue still holds the item, because delivery cannot complete against
	// a shell pane (see this test's doc comment) — but it must still be there
	// rather than silently dropped or acked.
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending)+len(state.InFlight) == 0 {
		t.Fatalf("queue is empty: a delivery that cannot have reached the pane must not ack; state=%+v", state)
	}

	// The sidecar poller class must not have been touched: the whole point of
	// the event path is that it replaces those processes.
	pollersDir := filepath.Join(cityPath, ".gc", "nudges", "pollers")
	if entries, err := os.ReadDir(pollersDir); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("sidecar poller artifacts present, want none: %v", names)
	}
}
