package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestReconcileSessionBeads_EvictsLiveUncommittedStartAfterStartupTimeout is
// AC-6 of gastownhall/gascity#6417: a desired row with no pending-create
// claim, no operator hold, a prompt-bearing template and an empty
// started_config_hash, whose name holds a live, unattached runtime whose
// GC_SESSION_ID is the row's, gets that runtime stopped once the startup
// timeout has passed — the reconciler logs and traces the eviction, so the
// next tick starts the row and delivers its prompt.
func TestReconcileSessionBeads_EvictsLiveUncommittedStartAfterStartupTimeout(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := "worker"
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		Prompt:       "rendered template prompt",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}
	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"session_key":                "original-key",
		// The handoff's stamp: the restart cycle that cleared the start
		// witness committed, and no start that consumed it launched the
		// occupant planted below.
		"reset_committed_at": env.clk.Now().Format(time.RFC3339),
	})
	ctx := context.Background()
	if err := env.sp.Start(ctx, sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("planting the occupant runtime: %v", err)
	}
	_ = env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID)
	_ = env.sp.SetMeta(sessionName, "GC_INSTANCE_TOKEN", "tok-noncontroller")

	tick := func() {
		b, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(session): %v", err)
		}
		cfgNames := configuredSessionNames(env.cfg, "", env.store)
		_ = reconcileSessionBeadsAtPath(
			ctx, env.city, []beads.Bead{b}, env.desiredState, cfgNames, env.cfg, env.sp, env.store,
			newDrainOps(env.sp), nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
		)
	}

	// Tick 1 sights the row live and uncommitted for the first time (the fake
	// provider reports no creation time, so the first sighting is the age
	// basis); the startup window has not passed yet, so nothing is stopped.
	tick()
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("tick 1 stopped the runtime inside the startup window\nstdout=%s\nstderr=%s", env.stdout.String(), env.stderr.String())
	}
	// Tick 2 runs 90s later: the default 60s startup timeout has passed with
	// no commit taking the name, so the promptless occupant is evicted.
	env.clk.Time = env.clk.Time.Add(90 * time.Second)
	tick()
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("tick 2 left the uncommitted live start running: nothing would ever replace it\nstdout=%s\nstderr=%s", env.stdout.String(), env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "evicted "+sessionName) {
		t.Fatalf("the eviction was not logged\nstdout=%s\nstderr=%s", env.stdout.String(), env.stderr.String())
	}
}
