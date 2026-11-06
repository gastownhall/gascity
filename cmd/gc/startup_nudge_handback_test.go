package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Kills a start whose startup nudge its program held back behind a dialog
// (design §10.2 row A20, program_status = "on") and that drops the nudge, or
// queues one the leaf never handed back: the handed-back nudge is queued for
// the session, once, and a start that handed nothing back queues nothing.
func TestStartupNudgeHandbackIsQueuedForTheSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	info := session.Info{ID: "gc-41", SessionNameMetadata: "worker-session", Template: "worker", AgentName: "worker"}
	target := resolveNudgeTargetFromSessionInfo(dir, &config.City{}, info)
	fake := runtime.NewFake()
	sp := runtime.FullFake{Fake: fake}
	now := time.Now()

	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", now); err != nil {
		t.Fatalf("no handback: %v", err)
	}
	if pending, _, _, err := listQueuedNudgesForTarget(dir, target, now); err != nil || len(pending) != 0 {
		t.Fatalf("no handback: pending %d (%v); want nothing queued", len(pending), err)
	}

	fake.SetStartupNudgeHandback("worker-session", "start working")
	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", now); err != nil {
		t.Fatalf("handback: %v", err)
	}
	pending, _, _, err := listQueuedNudgesForTarget(dir, target, now)
	if err != nil || len(pending) != 1 {
		t.Fatalf("handback: pending %d (%v); want the startup nudge queued", len(pending), err)
	}
	if got := pending[0]; got.Message != "start working" || got.Source != startupNudgeHandbackSource || got.SessionID != info.ID {
		t.Fatalf("queued %+v; want the startup nudge for %s from %q", got, info.ID, startupNudgeHandbackSource)
	}
	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", now); err != nil {
		t.Fatalf("second take: %v", err)
	}
	if pending, _, _, _ := listQueuedNudgesForTarget(dir, target, now); len(pending) != 1 {
		t.Fatalf("second take: pending %d; want the handback queued once", len(pending))
	}
}
