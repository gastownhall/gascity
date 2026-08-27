package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// respawnGateConfig configures one agent pinned to a named provider, so the
// ADR-0013 registry entry the fixture writes is the one the reconciler resolves
// for that agent's respawn.
func respawnGateConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "test-cmd",
			MinActiveSessions: intPtr(1),
			MaxActiveSessions: intPtr(2),
		}},
	}
}

// respawnGateDesired registers the worker as desired with a RESOLVED provider,
// which is what legacy's gate keys on (tp.ResolvedProvider.Name); an unresolved
// template makes the gate fail open and would make the assertion vacuous.
func respawnGateDesired(env *reconcilerTestEnv, provider string) {
	env.desiredState["worker"] = TemplateParams{
		Command:          "test-cmd",
		SessionName:      "worker",
		TemplateName:     "worker",
		ResolvedProvider: &config.ResolvedProvider{Name: provider},
	}
}

// respawnGateAssignedWork gives the session one in-progress assigned task, the
// awake set's reason to want it running.
func respawnGateAssignedWork(t *testing.T, env *reconcilerTestEnv, sessionID string) beads.Bead {
	t.Helper()
	work, err := env.store.Create(beads.Bead{Title: "assigned task", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	status := "in_progress"
	assignee := sessionID
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("assign work bead: %v", err)
	}
	if work, err = env.store.Get(work.ID); err != nil {
		t.Fatalf("re-read work bead: %v", err)
	}
	return work
}

// TestReconcileSessionBeads_NoRespawnWhileProviderHealthRed is the coverage
// legacy never had. TestGate_NoRespawnWhileRed (provider_health_gate_test.go)
// exercises providerHealthGate's episode bookkeeping in isolation; its
// PRODUCTION counterpart — the `continue` in the wake/sleep phase that actually
// withholds the respawn — has never been integration-tested, so nothing pinned
// that a red registry keeps a dead session asleep.
//
// This drives the real reconciler over a real on-disk provider-health.json: an
// asleep, desired session under a RED provider must not be respawned, must fire
// exactly one episode alert, and must not consume the wake budget. The green
// control proves the gate is what withheld the start rather than the fixture.
func TestReconcileSessionBeads_NoRespawnWhileProviderHealthRed(t *testing.T) {
	cityPath := t.TempDir()
	env := newReconcilerTestEnv()
	env.cfg = respawnGateConfig()
	rec := events.NewFake()
	env.rec = rec
	respawnGateDesired(env, providerHealthTestProvider)
	bead := env.createSessionBead("worker", "worker")
	// One assigned, in-progress task is the wake reason. Without it the awake
	// set has nothing to want and the row never reaches the respawn gate, which
	// would make the assertion below vacuous.
	work := respawnGateAssignedWork(t, env, bead.ID)

	writeProviderHealthFile(t, cityPath, "unhealthy")

	gate := newProviderHealthGate()
	woken := reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), cityPath,
		sessionpkg.ReconcileRowsFromBeads([]beads.Bead{bead}),
		newSessionBeadSnapshotFromReconcileRows(sessionpkg.ReconcileRowsFromBeads([]beads.Bead{bead})),
		env.desiredState, configuredSessionNames(env.cfg, cityPath, env.store), env.cfg, env.sp,
		env.store, nil, []beads.Bead{work}, nil, nil, env.dt, gate,
		map[string]int{"worker": 1}, nil, nil, false, nil, "test-city",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		env.startOptions...,
	)

	if woken != 0 {
		t.Fatalf("woken = %d while the provider registry is RED, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker was respawned while its provider is RED")
	}
	if alerts := countRecordedEvents(rec, events.ProviderHealthGateAlert); alerts != 1 {
		t.Fatalf("ProviderHealthGateAlert events = %d, want exactly 1 per red episode", alerts)
	}
	if !strings.Contains(env.stdout.String(), "Provider health gate OPEN") {
		t.Fatalf("stdout missing the escalation alert, got %q", env.stdout.String())
	}

	// Green control: the very same fixture starts once the registry flips, which
	// is what proves the red run was withheld by the gate.
	writeProviderHealthFile(t, cityPath, "healthy")
	current, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	woken = reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), cityPath,
		sessionpkg.ReconcileRowsFromBeads([]beads.Bead{current}),
		newSessionBeadSnapshotFromReconcileRows(sessionpkg.ReconcileRowsFromBeads([]beads.Bead{current})),
		env.desiredState, configuredSessionNames(env.cfg, cityPath, env.store), env.cfg, env.sp,
		env.store, nil, []beads.Bead{work}, nil, nil, env.dt, gate,
		map[string]int{"worker": 1}, nil, nil, false, nil, "test-city",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		env.startOptions...,
	)
	if woken == 0 {
		t.Fatal("woken = 0 with a GREEN registry; the red assertion above proves nothing")
	}
}

// TestExactStartRefusesRespawnWhileProviderHealthRed is the keyed half of the
// same gate: the exact-key start plan must read the sweep's published snapshot
// and decline to prepare a start while the provider is red, so the keyed lane
// carries the ADR-0013 gate rather than losing it at the WE cutover.
func TestExactStartRefusesRespawnWhileProviderHealthRed(t *testing.T) {
	cityPath := t.TempDir()
	writeProviderHealthFile(t, cityPath, "healthy")

	env := newReconcilerTestEnv()
	env.cfg = respawnGateConfig()
	bead := env.createSessionBead("worker", "worker")

	red := &providerHealthSnapshot{present: true, entries: map[string]bool{providerHealthTestProvider: false}}
	params := exactSessionStartParams{
		CityPath:       cityPath,
		Config:         env.cfg,
		Store:          env.store,
		Provider:       env.sp,
		Clock:          env.clk,
		ProviderHealth: func() *providerHealthSnapshot { return red },
	}
	plan := planSessionLifecycleStartSelection(sessionLifecycleStartShadowInput{
		Info:                 env.sessionInfo(bead.ID),
		WakeDecisionObserved: true,
		ShouldWake:           true,
		RuntimeObserved:      true,
		ObservedAt:           env.clk.Now().UTC(),
		ProviderUnavailable:  exactSessionProviderUnavailable(params, providerHealthTestProvider),
	})
	if plan.Outcome == sessionLifecycleStartSelectionPrepare {
		t.Fatal("keyed start prepared a session while its provider is RED")
	}
	if plan.Reason != sessionLifecycleStartSelectionReasonProviderUnavailable {
		t.Fatalf("plan reason = %q, want %q", plan.Reason, sessionLifecycleStartSelectionReasonProviderUnavailable)
	}
}
