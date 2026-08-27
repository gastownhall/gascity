package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestReconcileExactWaitDependencyStartPreservesStartSelectionGates(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata map[string]string
		setup    func(*testing.T, *reconcilerTestEnv, *exactSessionStartParams)
	}{
		{
			name: "live runtime",
			setup: func(t *testing.T, env *reconcilerTestEnv, _ *exactSessionStartParams) {
				t.Helper()
				if err := env.sp.Start(t.Context(), "worker", runtime.Config{Command: "already-running"}); err != nil {
					t.Fatalf("start existing runtime: %v", err)
				}
			},
		},
		{
			name: "open circuit",
			metadata: map[string]string{
				"session_circuit_state": sessionpkg.SessionCircuitStateOpen,
			},
		},
		{
			name: "unhealthy provider",
			setup: func(t *testing.T, env *reconcilerTestEnv, params *exactSessionStartParams) {
				t.Helper()
				env.cfg.Providers = map[string]config.ProviderSpec{"provider-red": {Command: "true"}}
				env.cfg.Agents[0].Provider = "provider-red"
				env.cfg.Agents[0].StartCommand = ""
				writeHealthCache(t, params.CityPath, "provider-red", "unhealthy", nowSecs())
			},
		},
		{
			name: "start already in flight",
			metadata: map[string]string{
				"state":                     string(sessionpkg.StateCreating),
				"pending_create_claim":      "true",
				"pending_create_started_at": time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
				"last_woke_at":              time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env, admission, params, waitID := newWaitDependencyStartSelectionFixture(t, test.metadata)
			if test.setup != nil {
				test.setup(t, env, &params)
			}
			startsBefore := env.sp.CountCalls("Start", "worker")
			before := env.sessionInfo(admission.SessionID)

			owner, err := reconcileExactSessionStartWithOwner(t.Context(), admission, params)
			if err != nil {
				t.Fatalf("reconcile gated dependency start: %v", err)
			}
			if owner != exactSessionStartKeyedOwner {
				t.Fatalf("dependency start owner = %v, want keyed", owner)
			}
			if got := env.sp.CountCalls("Start", "worker"); got != startsBefore {
				t.Fatalf("provider Start calls = %d, want unchanged %d", got, startsBefore)
			}
			wait, err := sessionFrontDoor(env.store).GetWait(waitID)
			if err != nil {
				t.Fatalf("read gated dependency wait: %v", err)
			}
			if wait.State != waitStatePending || wait.ReadyOwner != "" || wait.ReadyOperation != "" {
				t.Fatalf("gated dependency wait = %+v, want unclaimed pending wait", wait)
			}
			after := env.sessionInfo(admission.SessionID)
			if after.InstanceToken != before.InstanceToken || after.LastWokeAt != before.LastWokeAt {
				t.Fatalf("gated session crossed pre-wake: before token/woke=%q/%q after=%q/%q", before.InstanceToken, before.LastWokeAt, after.InstanceToken, after.LastWokeAt)
			}
		})
	}
}

func newWaitDependencyStartSelectionFixture(t *testing.T, metadata map[string]string) (*reconcilerTestEnv, sessionStartAdmission, exactSessionStartParams, string) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.store = openSessionWaitDependencyConditionalStore(t, rollout.Require)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	dependency, err := env.store.Create(beads.Bead{Title: "dependency"})
	if err != nil {
		t.Fatalf("create dependency: %v", err)
	}
	if err := env.store.Close(dependency.ID); err != nil {
		t.Fatalf("close dependency: %v", err)
	}
	target := env.createSessionBead("worker", "worker")
	baseMetadata := map[string]string{
		"state":              string(sessionpkg.StateAsleep),
		"continuation_epoch": "epoch-a",
		"wait_hold":          "true",
		"sleep_intent":       string(sessionpkg.SleepReasonWaitHold),
		"sleep_reason":       string(sessionpkg.SleepReasonWaitHold),
	}
	for key, value := range metadata {
		baseMetadata[key] = value
	}
	env.setSessionMetadata(&target, baseMetadata)
	waitBead, err := env.store.Create(sessionWaitShadowBead(target.ID, dependency.ID))
	if err != nil {
		t.Fatalf("create dependency wait: %v", err)
	}
	if err := env.store.SetMetadata(waitBead.ID, "registered_epoch", "epoch-a"); err != nil {
		t.Fatalf("register dependency wait epoch: %v", err)
	}
	wait, waitPersisted, err := sessionFrontDoor(env.store).GetWaitPersistedResponse(waitBead.ID)
	if err != nil {
		t.Fatalf("read dependency wait revision: %v", err)
	}
	_, sessionPersisted, err := getAuthoritativeSessionStartPersistedRecord(env.store, target.ID)
	if err != nil {
		t.Fatalf("read dependency session revision: %v", err)
	}
	lease := sessionWaitDependencyStartLease{
		WaitID:               wait.ID,
		SessionID:            target.ID,
		DepIDs:               []string{dependency.ID},
		DepMode:              "all",
		RegisteredEpoch:      "epoch-a",
		WaitRevision:         waitPersisted.Revision,
		SessionRevision:      sessionPersisted.Revision,
		IndexGeneration:      1,
		ControllerGeneration: 1,
		Operation:            "dependency-operation",
	}
	params := exactSessionStartTestParams(t, env)
	params.Generation = 1
	params.RolloutMode = rollout.Require
	params.StatusWriter, _, params.StatusWriterError = beads.ResolveConditionalWriter(env.store)
	return env, sessionStartAdmission{
		SessionID:      target.ID,
		Source:         sessionStartAdmissionWaitDependency,
		WaitDependency: &lease,
	}, params, wait.ID
}
