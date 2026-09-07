package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestManagerSuspendRemainsDormantAcrossControllerTicks(t *testing.T) {
	for _, demand := range []string{"manual", "legacy-manual", "assigned", "pinned", "minimum-floor"} {
		for _, wake := range []string{"wake-request", "manager-start"} {
			t.Run(demand+"/"+wake, func(t *testing.T) {
				testManagerSuspendDormantTicks(t, demand, wake)
			})
		}
	}
}

func testManagerSuspendDormantTicks(t *testing.T, demand, wake string) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.clk.Time = time.Now().UTC()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	b := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&b, map[string]string{
		"state": "awake", "session_origin": "manual", "session_key": "retained-conversation",
		"started_config_hash": "configured-launch", "last_woke_at": env.clk.Now().Add(-time.Minute).Format(time.RFC3339),
	})
	var work []beads.Bead
	if demand == "assigned" {
		task, err := env.store.Create(beads.Bead{Title: "assigned task", Type: "task", Status: "in_progress", Assignee: b.ID})
		if err != nil {
			t.Fatal(err)
		}
		work = []beads.Bead{task}
	}
	if demand == "pinned" {
		env.setSessionMetadata(&b, map[string]string{"pin_awake": "true"})
	}
	if demand == "minimum-floor" {
		floor := 1
		env.cfg.Agents[0].MinActiveSessions = &floor
	}
	runTick := func() {
		current, err := env.store.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		reconcileSessionBeads(context.Background(), []beads.Bead{current}, env.desiredState, configuredSessionNames(env.cfg, "", env.store), env.cfg, env.sp, env.store, nil, work, nil, env.dt, map[string]int{"worker": 1}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...)
	}
	if demand == "legacy-manual" {
		// An older manager acknowledged suspension without the durable hold fields.
		env.setSessionMetadata(&b, map[string]string{"state": "suspended", "sleep_reason": "user-hold", "last_woke_at": ""})
		if err := env.sp.Stop("worker"); err != nil {
			t.Fatal(err)
		}
	}
	mgr := sessionpkg.NewManagerWithOptions(env.store, env.sp, sessionpkg.WithStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter))
	if err := mgr.Suspend(b.ID); err != nil {
		t.Fatal(err)
	}
	starts := env.sp.CountCalls("Start", "worker")
	for tick := range 2 {
		runTick()
		got := env.sessionInfo(b.ID)
		if env.sp.IsRunning("worker") || env.sp.CountCalls("Start", "worker") != starts {
			t.Fatalf("controller tick %d restarted suspended manual session", tick+1)
		}
		if got.SessionKey != "retained-conversation" || got.SleepReason != "user-hold" {
			t.Fatalf("controller tick %d lost suspension/continuity: key=%q reason=%q", tick+1, got.SessionKey, got.SleepReason)
		}
		env.clk.Advance(time.Minute)
	}
	if wake == "wake-request" {
		if _, err := sessionFrontDoor(env.store).WakeSession(b.ID, env.clk.Now(), sessionpkg.WakeOpts{}); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := mgr.Start(context.Background(), b.ID, "test-cmd --resume retained-conversation", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	runTick()
	got := env.sessionInfo(b.ID)
	if !env.sp.IsRunning("worker") || env.sp.CountCalls("Start", "worker") != starts+1 || got.SessionKey != "retained-conversation" || got.HeldUntil != "" || got.SleepIntent != "" {
		t.Fatalf("explicit wake did not release only the hold and resume: running=%v key=%q held=%q intent=%q", env.sp.IsRunning("worker"), got.SessionKey, got.HeldUntil, got.SleepIntent)
	}
}
