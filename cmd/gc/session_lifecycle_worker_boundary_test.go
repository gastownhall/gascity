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
	mgr := newSessionManagerWithConfig(t.TempDir(), store, sp, nil) // the create's start leases in its city
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: sessionpkg.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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
	mgr := newSessionManagerWithConfig(t.TempDir(), store, sp, nil) // the create's start leases in its city
	if err := sp.Start(context.Background(), "human-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	poolInfo, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{Template: "pool", Title: "Pool", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: sessionpkg.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
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

// TestInterruptTargetsBoundedFollowsRuntimeTurnInterruptMode pins the
// graceful interrupt pass of gc stop and restart to the runtime's
// turn-interrupt mode: a protocol runtime (t3bridge) gets one Interrupt for
// a codex session instead of an Escape it drops, and a runtime-only target
// on a runtime that cannot interrupt the turn is sent nothing and not
// counted as interrupted.
func TestInterruptTargetsBoundedFollowsRuntimeTurnInterruptMode(t *testing.T) {
	t.Run("protocol", func(t *testing.T) {
		sp := runtime.NewFake()
		sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		store := beads.NewMemStore()
		mgr := newSessionManagerWithConfig(t.TempDir(), store, sp, nil) // the create's start leases in its city
		info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		sp.Calls = nil
		var stderr bytes.Buffer
		sent := interruptTargetsBounded([]stopTarget{{sessionID: info.ID, name: info.SessionName, template: "worker", resolved: true}}, nil, store, sp, &stderr)
		if sent != 1 {
			t.Fatalf("sent = %d, want 1; stderr: %s", sent, stderr.String())
		}
		var interrupts int
		for _, call := range sp.Calls {
			if call.Method == "SendKeys" {
				t.Fatalf("calls = %#v, want no SendKeys on a protocol runtime", sp.Calls)
			}
			if call.Method == "Interrupt" && call.Name == info.SessionName {
				interrupts++
			}
		}
		if interrupts != 1 {
			t.Fatalf("Interrupt calls = %d, want 1; calls = %#v", interrupts, sp.Calls)
		}
		if !sp.IsRunning(info.SessionName) {
			t.Fatal("the interrupt pass stopped the session; want only its turn interrupted")
		}
	})
	t.Run("unsupported runtime-only target", func(t *testing.T) {
		sp := runtime.NewFake()
		sp.TurnInterruptModeValue = runtime.TurnInterruptUnsupported
		if err := sp.Start(context.Background(), "legacy-worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		sp.Calls = nil
		var stderr bytes.Buffer
		sent := interruptTargetsBounded([]stopTarget{{name: "legacy-worker", template: "worker", resolved: true}}, nil, beads.NewMemStore(), sp, &stderr)
		if sent != 0 {
			t.Fatalf("sent = %d, want 0; stderr: %s", sent, stderr.String())
		}
		for _, call := range sp.Calls {
			if call.Method == "Interrupt" || call.Method == "SendKeys" {
				t.Fatalf("calls = %#v, want nothing sent", sp.Calls)
			}
		}
	})
}
