package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// gm-55kp2u incident path: beads/reviewer's `gc sling --nudge` queued
// "Work slung. Check your hook." for beads/deployer, whose pane was parked on
// an AskUserQuestion. The poller passed quiescence (pane static 45 min),
// claimed, and SendLiveOnly typed the reminder + Enter, selecting option 1.
// A queued nudge must stay queued while an interaction owns the pane.
func TestGM55KP2UPollerHoldsQueuedNudgeWhileInteractionPending(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_HOME", t.TempDir())
	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("deployer", "Work slung. Check your hook.", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "deployer", Title: "Deployer", Command: "claude", WorkDir: dir, Provider: "claude", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-45 * time.Minute) // question drawn, then nothing
	fake.Activity = map[string]time.Time{info.SessionName: idleSince}
	fake.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "question", Prompt: "How do you want to proceed?"})
	fake.Calls = nil

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "deployer"},
		sessionID:   info.ID,
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: info.SessionName,
	}
	obs := worker.LiveObservation{Running: true, LastActivity: &idleSince}
	delivered, _ := tryDeliverQueuedNudgesByPoller(target, store, store, fake, 3*time.Second, obs)

	for _, call := range fake.Calls {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			t.Errorf("typed %q into a pane with a pending interaction", call.Message)
		}
	}
	if delivered {
		t.Error("delivered = true, want false while the interaction is pending")
	}
	pending, inFlight, dead, err := listQueuedNudges(dir, "deployer", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending)+len(inFlight) != 1 || len(dead) != 0 {
		t.Fatalf("queue pending=%d inFlight=%d dead=%d, want the nudge still queued (not acked, not dead-lettered)", len(pending), len(inFlight), len(dead))
	}
	// Holding for an open question is not a failed delivery: it must not burn
	// an attempt, or a question left open for an hour dead-letters the nudge.
	for _, item := range append(pending, inFlight...) {
		if item.Attempts != 0 {
			t.Errorf("Attempts = %d, want 0 while held for a pending interaction", item.Attempts)
		}
	}
}
