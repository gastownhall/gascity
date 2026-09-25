package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
)

// TestNudgeEventDispatcherLiveHerdr proves the PR's end-to-end path against a
// real herdr binary: a queued wait-idle nudge for a busy agent is delivered by
// the event dispatcher within seconds of the agent's idle transition, through
// the provider's closed-loop paste+submit delivery, with the sidecar pollers
// directory staying empty throughout. Production timing knobs (3s quiescence)
// are kept so the observed latency is the deployed one. Skipped when herdr is
// unavailable or in -short mode.
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

	// Start the runtime directly on the provider: a bare interactive sh pane
	// (it echoes pastes, which the closed-loop delivery's screen-diff
	// verification needs, and herdr keeps bare-sh agents registered — unlike
	// adopted codex TUIs, which it drops). The session bead exists only for
	// target resolution.
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

	registerDeadline := time.Now().Add(15 * time.Second)
	for !p.IsRunning(agentName) {
		if time.Now().After(registerDeadline) {
			t.Fatalf("agent %q never registered with herdr", agentName)
		}
		time.Sleep(200 * time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := newNudgeEventDispatcher(ctx, cityPath, testWriter(t), "live")
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

	// The agent is BUSY when the nudge is queued — the wait-idle contract.
	herdrtest.ReportAgent(t, herdrSession, agentName, "working", func() string { return herdrLivePaneID(p, agentName) })
	const nudgeText = "wait satisfied: live-dispatch proceed"
	if err := enqueueQueuedNudge(cityPath, newQueuedNudge("worker", nudgeText, time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// A real agent leaves idle when it takes the submitted turn — the
	// closed-loop delivery confirms submission by observing exactly that.
	// The reported status of this fake agent never changes on its own, so
	// mimic the turn-take: once the pasted text shows on the pane, flip the
	// reported status to working. Without this, the closed loop correctly
	// refuses to ack (typed-but-unsubmitted protection).
	turnTaken := make(chan struct{})
	go func() {
		defer close(turnTaken)
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			if out, err := p.Peek(agentName, 0); err == nil && screenContains(out, "live-dispatch proceed") {
				herdrtest.ReportAgentBestEffort(herdrSession, agentName, "working", func() string { return herdrLivePaneID(p, agentName) })
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()

	// Give the busy state a beat, then the idle transition must trigger
	// delivery: event → fresh-stamp attempt → aged-stamp retry → verified
	// paste+submit.
	time.Sleep(1 * time.Second)
	herdrtest.ReportAgent(t, herdrSession, agentName, "idle", func() string { return herdrLivePaneID(p, agentName) })
	idleAt := time.Now()

	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := nudgequeue.LoadState(cityPath)
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		if len(state.Pending) == 0 && len(state.InFlight) == 0 {
			t.Logf("queued nudge delivered %.1fs after the idle transition", time.Since(idleAt).Seconds())
			break
		}
		if time.Now().After(deadline) {
			screen, _ := p.Peek(agentName, 0)
			t.Fatalf("queued nudge not delivered within 30s of idle transition; state=%+v\nscreen:\n%s", state, screen)
		}
		time.Sleep(200 * time.Millisecond)
	}
	<-turnTaken

	// The delivery must have gone through the pane (paste visible), and the
	// sidecar poller class must not have been touched.
	if out, err := p.Peek(agentName, 0); err != nil {
		t.Logf("Peek: %v (screen assertion skipped)", err)
	} else if !screenContains(out, "live-dispatch proceed") {
		t.Errorf("pane screen does not show the delivered nudge text:\n%s", out)
	}
	pollersDir := filepath.Join(cityPath, ".gc", "nudges", "pollers")
	if entries, err := os.ReadDir(pollersDir); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("sidecar poller artifacts present, want none: %v", names)
	}
}

// screenContains reports whether needle appears in a rendered pane screen,
// tolerating the hard line wraps and row padding the render introduces by
// comparing with all whitespace collapsed.
func screenContains(screen, needle string) bool {
	compact := func(s string) string { return strings.Join(strings.Fields(s), "") }
	return strings.Contains(compact(screen), compact(needle))
}
