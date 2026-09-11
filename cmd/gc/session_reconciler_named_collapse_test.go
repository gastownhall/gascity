package main

import (
	"context"
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
