package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestStopTargetsBoundedUsesWorkerBoundaryForKnownSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	info, err := mgr.Create(context.Background(), "worker", "Worker", "claude", t.TempDir(), "claude", nil, sessionpkg.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	stopped := stopTargetsBounded([]stopTarget{{
		sessionID: info.ID,
		name:      info.SessionName,
		template:  "worker",
		subject:   "worker",
		resolved:  true,
	}}, &config.City{Agents: []config.Agent{{Name: "worker"}}}, store, sp, rec, "gc", &stdout, &stderr)
	if stopped != 1 {
		t.Fatalf("stopped = %d, want 1", stopped)
	}
	if sp.IsRunning(info.SessionName) {
		t.Fatal("session should have been stopped by worker boundary")
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != sessionpkg.StateSuspended {
		t.Fatalf("state = %q, want %q", got.State, sessionpkg.StateSuspended)
	}
}

func TestInterruptTargetsBoundedStopsPoolManagedSessionsThroughWorkerBoundary(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	if err := sp.Start(context.Background(), "human-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	poolInfo, err := mgr.Create(context.Background(), "pool", "Pool", "claude", t.TempDir(), "claude", nil, sessionpkg.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	targets := []stopTarget{
		{name: "human-worker", template: "worker", resolved: true},
		{sessionID: poolInfo.ID, name: poolInfo.SessionName, template: "pool", resolved: true, poolManaged: true},
	}
	var stderr bytes.Buffer
	sent := interruptTargetsBounded(targets, nil, store, sp, &stderr)
	if sent != 1 {
		t.Fatalf("sent = %d, want 1 (only human-worker)", sent)
	}
	if sp.IsRunning(poolInfo.SessionName) {
		t.Fatal("pool-managed session should have been stopped")
	}

	got, err := mgr.Get(poolInfo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != sessionpkg.StateSuspended {
		t.Fatalf("state = %q, want %q", got.State, sessionpkg.StateSuspended)
	}
	if !sp.IsRunning("human-worker") {
		t.Fatal("human-worker should still be running after interrupt")
	}
}
