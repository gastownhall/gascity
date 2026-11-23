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

// TestReconcileSessionBeads_NamedSessionTransientSpecCollapseDeferred covers
// issue #3630: a namedSessionSpecs enumeration collapse during boot can drop a
// configured named session's spec for a single reconciler tick, after which it
// reappears. A running named session whose spec is merely transiently absent
// must NOT be suspend-drained on that first tick (the drain causes a fresh
// respawn that loses in-session context). The drain is deferred until
// namedSuspendConfirmTicks consecutive ticks confirm the spec is genuinely
// gone; suspend-class drains are revertible, so a 1-tick confirmation buffer is
// safe and cheap.
//
// The collapse keeps the [[named_session]] entry and drops its backing
// [[agent]] for the tick, so the row stays a configured name and the drain is
// suspend-class ("suspended"), not "orphaned" — the revertible class the
// confirmation buffer exists for. The entry is on_demand so the row carries no
// standing config wake of its own; the drain's lifecycle is observed in
// isolation. Once the drain begins it must converge monotonically: it stays
// tracked on the same generation while the spec is absent, stops the runtime
// and completes once its deadline passes, and no later tick begins or cancels
// it again.
func TestReconcileSessionBeads_NamedSessionTransientSpecCollapseDeferred(t *testing.T) {
	env := newReconcilerTestEnv(t)
	// cfg keeps the [[named_session]] entry but its backing [[agent]] is
	// absent this tick — the transient collapse that makes the spec
	// unresolvable while the row stays a configured name.
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{Template: "warlord", Mode: "on_demand"}},
	}
	sessionName := "warlord"
	_ = env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"})
	session := env.createSessionBead(sessionName, "warlord")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "warlord",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		// Past the wake grace: the undesired arm spares a runtime the wake family
		// started moments ago (ga-qgtb3). The subject here is the #3630
		// spec-collapse confirmation window, which sits behind that grace.
		"last_woke_at": env.clk.Now().Add(-wakeUndesiredGrace - time.Minute).UTC().Format(time.RFC3339),
	})

	// Tick 1 (collapse tick): spec absent for the first time → defer, do not drain.
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("named session must not be drained on the first spec-absent tick (transient collapse #3630), got drain reason=%q", ds.reason)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("named session %q must stay running through a single-tick spec collapse", sessionName)
	}

	// Tick 2 (still absent): now confirmed across N consecutive ticks → drain
	// proceeds, as a suspend-class drain (the agent stays configured, so the
	// spec collapse reads as a suspend, not an orphan).
	env.reconcile([]beads.Bead{session})
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("named session should be drained after namedSuspendConfirmTicks consecutive spec-absent ticks\nstdout:\n%s\nstderr:\n%s", env.stdout.String(), env.stderr.String())
	}
	if ds.reason != "suspended" {
		t.Fatalf("spec collapse of a configured named session must drain as \"suspended\", got reason=%q", ds.reason)
	}
	generation := ds.generation

	// Tick 3 (still absent, drain in flight): the tracked drain must persist —
	// neither re-begun (a second begin would reset its confirmation premise)
	// nor canceled (nothing released the suspend) — while the runtime stays up
	// and the drain's ack arms against it.
	env.reconcile([]beads.Bead{session})
	ds = env.dt.get(session.ID)
	if ds == nil || ds.reason != "suspended" || ds.generation != generation {
		t.Fatalf("the in-flight suspend drain must stay tracked on the same generation, got %+v", ds)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("named session %q must stay running while the suspend drain is in flight", sessionName)
	}

	// Past the drain deadline the drain converges: the timeout stop ends the
	// runtime and the drain completes — no begin/cancel loop may restart it.
	env.clk.Time = env.clk.Time.Add(defaultDrainTimeout + time.Minute)
	env.reconcile([]beads.Bead{session})
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("named session %q must be stopped once the suspend drain's deadline passes", sessionName)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("the suspend drain must complete once the runtime stopped, still tracked: %+v", *ds)
	}

	// A further tick must not resurrect the drain against the stopped row: the
	// spec is still absent, but the drain has already run to completion.
	env.clk.Time = env.clk.Time.Add(time.Minute)
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("no drain may begin again after the suspend drain completed, got %+v", *ds)
	}
	if got := strings.Count(env.stdout.String(), "Draining session 'warlord': suspended"); got != 1 {
		t.Fatalf("the suspend drain must begin exactly once, got %d begin lines\nstdout:\n%s", got, env.stdout.String())
	}
	if strings.Contains(env.stdout.String(), "Canceled") {
		t.Fatalf("nothing may cancel the spec-collapse suspend drain\nstdout:\n%s", env.stdout.String())
	}
}

// TestReconcileSessionBeads_AlwaysNamedSpecCollapseSuspendDrainConverges pins
// the always-mode half of the #3630 collapse. The [[named_session]] entry is
// always-mode, so the row still carries its standing config wake while its
// backing [[agent]] is transiently missing — the exact row shape a
// demand-keyed resume cancel would falsely "recover", re-beginning the drain
// each tick and canceling it again the same tick, so the row never converges.
// Nothing resumed anything here: the drain's premise (the confirmed spec
// absence) holds on every tick, so the drain must converge monotonically —
// stay tracked "suspended" on the same generation, stop the runtime and
// complete once its deadline passes, with exactly one begin and no cancels.
func TestReconcileSessionBeads_AlwaysNamedSpecCollapseSuspendDrainConverges(t *testing.T) {
	env := newReconcilerTestEnv(t)
	// cfg keeps the always-mode [[named_session]] entry but its backing
	// [[agent]] is absent this tick — the transient collapse that makes the
	// spec unresolvable while the row stays a configured name with a standing
	// config wake.
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{Template: "warlord", Mode: "always"}},
	}
	sessionName := "warlord"
	_ = env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"})
	session := env.createSessionBead(sessionName, "warlord")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "warlord",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		// Past the wake grace, so the #3630 confirmation window is what defers.
		"last_woke_at": env.clk.Now().Add(-wakeUndesiredGrace - time.Minute).UTC().Format(time.RFC3339),
	})

	// Tick 1 (collapse tick): spec absent for the first time → defer, do not drain.
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("named session must not be drained on the first spec-absent tick (transient collapse #3630), got drain reason=%q", ds.reason)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("named session %q must stay running through a single-tick spec collapse", sessionName)
	}

	// Tick 2 (still absent): confirmed → the suspend drain begins and must
	// STAY begun: the row's standing config wake is not a resume.
	env.reconcile([]beads.Bead{session})
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("named session should be drained after namedSuspendConfirmTicks consecutive spec-absent ticks\nstdout:\n%s\nstderr:\n%s", env.stdout.String(), env.stderr.String())
	}
	if ds.reason != "suspended" {
		t.Fatalf("spec collapse of a configured named session must drain as \"suspended\", got reason=%q", ds.reason)
	}
	generation := ds.generation

	// Tick 3 (still absent, drain in flight): the tracked drain must persist —
	// a standing config wake is not a resume, so no release may void it.
	env.reconcile([]beads.Bead{session})
	ds = env.dt.get(session.ID)
	if ds == nil || ds.reason != "suspended" || ds.generation != generation {
		t.Fatalf("the in-flight suspend drain must stay tracked on the same generation, got %+v", ds)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("named session %q must stay running while the suspend drain is in flight", sessionName)
	}

	// Past the drain deadline the drain converges: the timeout stop ends the
	// runtime and the drain completes.
	env.clk.Time = env.clk.Time.Add(defaultDrainTimeout + time.Minute)
	env.reconcile([]beads.Bead{session})
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("named session %q must be stopped once the suspend drain's deadline passes", sessionName)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("the suspend drain must complete once the runtime stopped, still tracked: %+v", *ds)
	}

	// A further tick must not resurrect the drain against the stopped row, and
	// across the whole lifecycle the drain began exactly once and was never
	// canceled: a begin/cancel loop would keep the armed drain alive against
	// every future replacement of the row.
	env.clk.Time = env.clk.Time.Add(time.Minute)
	env.reconcile([]beads.Bead{session})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("no drain may begin again after the suspend drain completed, got %+v", *ds)
	}
	if got := strings.Count(env.stdout.String(), "Draining session 'warlord': suspended"); got != 1 {
		t.Fatalf("the suspend drain must begin exactly once, got %d begin lines\nstdout:\n%s", got, env.stdout.String())
	}
	if strings.Contains(env.stdout.String(), "Canceled") {
		t.Fatalf("nothing may cancel the spec-collapse suspend drain\nstdout:\n%s", env.stdout.String())
	}
}

// TestReconcileSessionBeads_NamedSessionSpecReappearsClearsDeferral covers the
// recovery half of issue #3630: when the spec reappears after a collapse tick,
// the confirmation counter resets so a LATER genuine removal still gets a full
// confirmation window rather than draining on its first tick.
func TestReconcileSessionBeads_NamedSessionSpecReappearsClearsDeferral(t *testing.T) {
	env := newReconcilerTestEnv(t)
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
		// Past the INC-003 wake grace, so the #3630 window is what defers.
		"last_woke_at": env.clk.Now().Add(-wakeUndesiredGrace - time.Minute).UTC().Format(time.RFC3339),
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
