package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
)

// TestNudgeEventDispatcherLiveHerdr proves the PR's end-to-end path against a
// real herdr binary: a queued wait-idle nudge for a busy agent is delivered by
// the event dispatcher within seconds of the agent's idle transition, through
// the provider's closed-loop paste+submit delivery, with the sidecar pollers
// directory staying empty throughout. Production timing knobs (3s quiescence)
// are kept so the observed latency is the deployed one. Skipped when herdr is
// unavailable or in -short mode.
func TestNudgeEventDispatcherLiveHerdr(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr test in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
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
	herdrLiveReportAgent(t, herdrSession, agentName, "working")
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
				herdrLiveBestEffortReport(herdrSession, agentName, "working")
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()

	// Give the busy state a beat, then the idle transition must trigger
	// delivery: event → fresh-stamp attempt → aged-stamp retry → verified
	// paste+submit.
	time.Sleep(1 * time.Second)
	herdrLiveReportAgent(t, herdrSession, agentName, "idle")
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

// herdrLivePaneID resolves the pane id for an agent name via the herdr CLI's
// JSON envelope output. Returns "" when the agent is not (yet) listed.
func herdrLivePaneID(t *testing.T, herdrSession, agentName string) string {
	t.Helper()
	out, err := exec.Command("herdr", "--session", herdrSession, "agent", "list").CombinedOutput()
	if err != nil {
		t.Fatalf("herdr agent list: %v: %s", err, out)
	}
	var envelope struct {
		Result struct {
			Agents []struct {
				Name   string `json:"name"`
				PaneID string `json:"pane_id"`
			} `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("parsing herdr agent list output: %v: %s", err, out)
	}
	for _, a := range envelope.Result.Agents {
		if a.Name == agentName {
			return a.PaneID
		}
	}
	return ""
}

// herdrLiveReportAgent forces an agent status via herdr's report-agent API,
// generating a real pane.agent_status_changed event on the stream. The pane
// id is re-resolved per attempt with retries: right after an agent start the
// provider closes the placement's stray shell pane, and the listed pane id
// can go stale for a beat.
func herdrLiveReportAgent(t *testing.T, herdrSession, agentName, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr string
	for time.Now().Before(deadline) {
		paneID := herdrLivePaneID(t, herdrSession, agentName)
		if paneID != "" {
			out, err := exec.Command("herdr", "--session", herdrSession, "pane", "report-agent", paneID,
				"--source", "gctest", "--agent", "gctest", "--state", state).CombinedOutput()
			if err == nil {
				return
			}
			lastErr = fmt.Sprintf("pane report-agent %s %s: %v: %s", paneID, state, err, out)
		} else {
			lastErr = fmt.Sprintf("agent %q not in herdr agent list", agentName)
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("forcing agent status %q failed: %s", state, lastErr)
}

// herdrLiveBestEffortReport is herdrLiveReportAgent without test-fatal
// semantics, safe to call from helper goroutines.
func herdrLiveBestEffortReport(herdrSession, agentName, state string) {
	out, err := exec.Command("herdr", "--session", herdrSession, "agent", "list").CombinedOutput()
	if err != nil {
		return
	}
	var envelope struct {
		Result struct {
			Agents []struct {
				Name   string `json:"name"`
				PaneID string `json:"pane_id"`
			} `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		return
	}
	for _, a := range envelope.Result.Agents {
		if a.Name == agentName {
			_ = exec.Command("herdr", "--session", herdrSession, "pane", "report-agent", a.PaneID,
				"--source", "gctest", "--agent", "gctest", "--state", state).Run()
			return
		}
	}
}
