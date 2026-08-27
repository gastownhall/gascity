package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func TestResolveExactSessionStartOwnershipKeepsDependenciesOnLegacy(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "database", StartCommand: "true"},
			{Name: "worker", StartCommand: "true", DependsOn: []string{"database"}},
		},
	}
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}

	owned := resolveExactSessionStartOwnership(env.sessionInfo(bead.ID), env.cfg, env.clk.Now())
	if owned {
		t.Fatal("dependency-bearing session start became keyed-owned")
	}
}

func TestResolveExactSessionStartOwnershipKeepsDuplicateNamedSessionOnLegacy(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Name: "operator", Template: "worker"}},
	}
	for _, name := range []string{"operator-old", "operator-new"} {
		bead := env.createSessionBead(name, "worker")
		env.setSessionMetadata(&bead, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "operator",
		})
		if name != "operator-new" {
			continue
		}
		if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
			t.Fatalf("request explicit wake: %v", err)
		}

		owned := resolveExactSessionStartOwnership(env.sessionInfo(bead.ID), env.cfg, env.clk.Now())
		if owned {
			t.Fatal("duplicate configured named session became keyed-owned without canonical fleet proof")
		}
	}
}

func TestResolveExactSessionStartOwnershipKeepsOutOfCapPoolCreateOnLegacy(t *testing.T) {
	env := newReconcilerTestEnv()
	maxSessions := 1
	env.cfg = &config.City{Agents: []config.Agent{{
		Name:              "worker",
		StartCommand:      "true",
		MaxActiveSessions: &maxSessions,
	}}}
	bead := env.createSessionBead("worker-2", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		poolManagedMetadataKey:      "true",
		"pool_slot":                 "2",
		"state":                     string(session.StateCreating),
		"pending_create_claim":      "true",
		"pending_create_started_at": env.clk.Now().UTC().Format(time.RFC3339),
	})

	owned := resolveExactSessionStartOwnership(env.sessionInfo(bead.ID), env.cfg, env.clk.Now())
	if owned {
		t.Fatal("out-of-cap pool pending create became keyed-owned without a capacity census")
	}
}

func TestLegacyReconcilerSkipsKeyedOwnedStartWithoutMutatingWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	env.addDesired("worker", "worker", false)
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	env.startOptions = append(env.startOptions, withLegacyStartExclusion(func(info session.Info) bool {
		return resolveExactSessionStartOwnership(info, env.cfg, env.clk.Now())
	}))

	if woken := env.reconcile([]beads.Bead{bead}); woken != 0 {
		t.Fatalf("legacy starts = %d, want 0 for keyed-owned cause", woken)
	}
	if got := env.sp.CountCalls("Start", "worker"); got != 0 {
		t.Fatalf("provider Start calls = %d, want 0", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.WakeRequest != string(session.WakeCauseExplicit) {
		t.Fatalf("wake request = %q, want keyed-owned request unchanged", got.WakeRequest)
	}
	if got.LastWokeAt != "" {
		t.Fatalf("last_woke_at = %q, want no legacy pre-wake mutation", got.LastWokeAt)
	}
	if err := reconcileExactSessionStart(context.Background(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionExplicitWake,
	}, exactSessionStartTestParams(t, env)); err != nil {
		t.Fatalf("keyed reconciliation after legacy skip: %v", err)
	}
	if got := env.sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("provider Start calls after keyed owner = %d, want 1", got)
	}
}
