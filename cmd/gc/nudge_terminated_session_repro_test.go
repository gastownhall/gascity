package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestQueuedNudgeIsDeadLetteredWhenTargetSessionCloses covers
// gastownhall/gascity#4534: a nudge legitimately queued (with a managed wake)
// to a sleeping session that is then closed must be dead-lettered by the
// session's termination, so it leaves a trace in the dead bucket instead of
// sitting pending for a session that never comes back.
func TestQueuedNudgeIsDeadLetteredWhenTargetSessionCloses(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sleepUnheld(t, store, info.ID)

	prevManaged := nudgeCityUsesManagedReconciler
	prevPoke := nudgePokeController
	nudgeCityUsesManagedReconciler = func(cityPath string) bool { return cityPath == dir }
	nudgePokeController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() {
		nudgeCityUsesManagedReconciler = prevManaged
		nudgePokeController = prevPoke
	})

	target := nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}},
		sessionID:   info.ID,
		sessionName: info.SessionName,
		identity:    "worker",
		agent:       config.Agent{Name: "worker", Provider: "claude"},
	}
	var stdout, stderr bytes.Buffer
	if code := deliverSessionNudgeWithWorker(target, store, fake, nudgeDeliveryImmediate, &stdout, &stderr); code != 0 {
		t.Fatalf("deliverSessionNudgeWithWorker = %d, want 0 (queued); stderr: %s", code, stderr.String())
	}
	pending, _, _, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget before close: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending before close = %d, want 1", len(pending))
	}

	// The session drains: its record is closed and it never comes back.
	if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget after close: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 1 {
		t.Fatalf("after closing session %s, queue pending/inFlight/dead = %d/%d/%d, want 0/0/1 (queued nudge dead-lettered by session termination)",
			info.ID, len(pending), len(inFlight), len(dead))
	}
}
