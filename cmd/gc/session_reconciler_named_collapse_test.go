package main

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestReconcileSessionBeads_NamedSessionTransientSpecCollapseDeferred covers
// issue #3630: a namedSessionSpecs enumeration collapse during boot can drop a
// configured named session's spec for a single reconciler tick, after which it
// reappears. A running named session whose spec is merely transiently absent
// must NOT be suspend-drained on that first tick (the drain causes a fresh
// respawn that loses in-session context). The drain is deferred until
// namedSuspendConfirmTicks consecutive ticks confirm the spec is genuinely
// gone; suspend-class drains are revertible, so a 1-tick confirmation buffer is
// safe and cheap.
func TestReconcileSessionBeads_NamedSessionTransientSpecCollapseDeferred(t *testing.T) {
	env := newReconcilerTestEnv()
	// cfg has the agent template but NO [[named_session]] entry this tick —
	// modeling the transient collapse where the named spec briefly vanishes.
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "warlord", StartCommand: "true"}},
	}
	sessionName := "warlord"
	_ = env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"})
	session := env.createSessionBead(sessionName, "warlord")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "warlord",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"last_woke_at":               env.clk.Now().UTC().Format(time.RFC3339),
	})

	// Tick 1 (collapse tick): spec absent for the first time → defer, do not drain.
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("named session must not be drained on the first spec-absent tick (transient collapse #3630), got drain reason=%q", ds.reason)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("named session %q must stay running through a single-tick spec collapse", sessionName)
	}

	// Tick 2 (still absent): now confirmed across N consecutive ticks → drain proceeds.
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds == nil {
		t.Fatal("named session should be drained after namedSuspendConfirmTicks consecutive spec-absent ticks")
	}
}

// TestReconcileSessionBeads_NamedSessionSpecReappearsClearsDeferral covers the
// recovery half of issue #3630: when the spec reappears after a collapse tick,
// the confirmation counter resets so a LATER genuine removal still gets a full
// confirmation window rather than draining on its first tick.
func TestReconcileSessionBeads_NamedSessionSpecReappearsClearsDeferral(t *testing.T) {
	env := newReconcilerTestEnv()
	sessionName := config.NamedSessionRuntimeName("test-city", config.Workspace{Name: "test-city"}, "warlord")
	withSpec := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "warlord", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "warlord", Mode: "always"}},
	}
	withoutSpec := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "warlord", StartCommand: "true"}},
	}

	_ = env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"})
	session := env.createSessionBead(sessionName, "warlord")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "warlord",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"last_woke_at":               env.clk.Now().UTC().Format(time.RFC3339),
	})

	// Tick 1: collapse — spec absent → defer (counter = 1).
	env.cfg = withoutSpec
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("must defer on first spec-absent tick, got drain reason=%q", ds.reason)
	}

	// Tick 2: spec reappears → preserved, counter must reset.
	env.cfg = withSpec
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "warlord",
		ConfiguredNamedIdentity: "warlord",
		ConfiguredNamedMode:     "always",
	}
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("named session with present spec must not drain, got reason=%q", ds.reason)
	}

	// Tick 3: collapse again — because the counter reset, this is the first
	// confirming tick of a fresh window, so it must defer (not drain).
	env.cfg = withoutSpec
	delete(env.desiredState, sessionName)
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("confirmation window must reset after the spec reappears, got drain reason=%q on the first absent tick of a new collapse", ds.reason)
	}
}

// A liveness observation error must preserve bead metadata, but it must not
// hide the fact that a configured named-session spec reappeared. The spec's
// presence resets #3630's in-memory consecutive-absence window even when the
// rest of lifecycle reconciliation defers until liveness is observable again.
func TestReconcileSessionBeads_NamedSpecReappearsDuringLivenessErrorClearsDeferral(t *testing.T) {
	env := newReconcilerTestEnv()
	sessionName := config.NamedSessionRuntimeName("test-city", config.Workspace{Name: "test-city"}, "warlord")
	withSpec := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "warlord", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "warlord", Mode: "always"}},
	}
	withoutSpec := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "warlord", StartCommand: "true"}},
	}

	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start named runtime: %v", err)
	}
	session := env.createSessionBead(sessionName, "warlord")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "warlord",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"last_woke_at":               env.clk.Now().UTC().Format(time.RFC3339),
		"session_key":                "resume-warlord",
		"started_config_hash":        "config-warlord",
		"continuation_reset_pending": "",
	})
	runTick := func(sp runtime.Provider) {
		configuredNames := configuredSessionNames(env.cfg, "", env.store)
		reconcileSessionBeads(
			context.Background(), []beads.Bead{session}, env.desiredState,
			configuredNames, env.cfg, sp, env.store, nil, nil, nil, env.dt,
			nil, false, nil, "", nil, env.clk, env.rec, 0, 0,
			&env.stdout, &env.stderr, env.startOptions...,
		)
	}

	// Tick 1: the spec is absent and the runtime is observably alive, so the
	// first suspend confirmation is accrued but no drain starts.
	env.cfg = withoutSpec
	runTick(env.sp)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("first absent tick started drain: reason=%q", ds.reason)
	}

	beforeTick2, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) before liveness-error tick: %v", session.ID, err)
	}
	// Tick 2: the named spec reappears outside desired state while runtime
	// observation is unavailable. Lifecycle metadata must remain byte-stable,
	// and the in-memory consecutive-absence counter must still reset.
	env.cfg = withSpec
	runTick(&runtimeUnavailableLivenessProvider{Fake: env.sp})
	afterTick2, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) after liveness-error tick: %v", session.ID, err)
	}
	if !maps.Equal(afterTick2.Metadata, beforeTick2.Metadata) {
		t.Fatalf("metadata mutated during liveness-error deferral:\n before: %#v\n  after: %#v", beforeTick2.Metadata, afterTick2.Metadata)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("spec-present liveness-error tick started drain: reason=%q", ds.reason)
	}

	// Tick 3: absence resumes with healthy liveness. Because tick 2 broke the
	// sequence, this is confirmation 1 of a fresh window and must not drain.
	env.cfg = withoutSpec
	runTick(env.sp)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("non-consecutive spec absence drained named session: reason=%q", ds.reason)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("named runtime %q stopped after non-consecutive spec absence", sessionName)
	}
}
