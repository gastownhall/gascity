package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
)

// openFleetBreaker trips the env's guard with three failed starts across three
// templates; the env clock stays inside the backoff, so no probe is due.
func (e *fleetReconcileEnv) openFleetBreaker(t *testing.T) {
	t.Helper()
	e.primeTwoFailures(t)
	e.clk.Advance(time.Second)
	failFleetStart(t, e.guard, "other-3", "gamma")
	if got := fleetState(e.guard); got != resilience.StateOpen {
		t.Fatalf("premise: breaker = %v, want open", got)
	}
}

// reconcileTraced runs one reconcile tick with the pool demand derived from the
// desired state, the way env.reconcile does, recording into trace.
func (e *fleetReconcileEnv) reconcileTraced(trace *sessionReconcilerTraceCycle, sessions ...beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	reconcileSessionBeadsTraced(
		context.Background(), "", sessions, e.desiredState, configuredSessionNames(e.cfg, "", e.store),
		e.cfg, e.sp, e.store, nil, nil, nil, nil, e.dt, poolDesired, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr, trace,
		e.startOptions...,
	)
}

// startCallsFor counts the Start calls the env's provider saw for name.
func (e *fleetReconcileEnv) startCallsFor(name string) int {
	return e.sp.CountCalls("Start", name)
}

// A due automatic wake that the open fleet breaker defers at the reconciler's
// gate neither reaches the provider nor spends the session's wake budget: the
// durable wake_attempts counter is untouched, so the deferral cannot walk a
// healthy session toward quarantine while the fleet is held.
func TestReconcile_FleetGateDefersAWakeWithoutSpendingTheWakeBudget(t *testing.T) {
	// Premise: with the breaker closed the same session is woken.
	control := newFleetReconcileEnv(t)
	control.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	control.addDesired("worker", "worker", false)
	cs := control.createSessionBead("worker", "worker")
	control.markSessionCreating(&cs)
	control.reconcile([]beads.Bead{cs})
	if control.startCallsFor("worker") == 0 {
		t.Fatal("premise: a closed breaker did not wake the asleep worker, so the gate test below proves nothing")
	}

	env := newFleetReconcileEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)
	env.setSessionMetadata(&session, map[string]string{"wake_attempts": "1"})
	env.openFleetBreaker(t)

	trace := newPoolDesiredStateTestTrace("worker", fleetTraceTemplate)
	env.reconcileTraced(trace, session)

	if n := env.startCallsFor("worker"); n != 0 {
		t.Errorf("provider Start calls = %d, want 0: the gate must defer before the executor", n)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["wake_attempts"] != "1" {
		t.Errorf("wake_attempts = %q, want 1 unchanged: a deferral must not spend the wake budget", got.Metadata["wake_attempts"])
	}
	if env.sp.IsRunning("worker") {
		t.Error("the deferred session was started")
	}
	recs := fleetTraceRecords(trace)
	if len(recs) != 1 {
		t.Fatalf("fleet breaker trace records = %d, want 1 for the tick", len(recs))
	}
	if got := recs[0].Fields["deferred"]; got != 1 {
		t.Errorf("trace deferred = %#v, want 1: the gate's deferral counted for the tick record", got)
	}
}

// trackedLiveWorker is a running worker the guard is confirming, with the
// reconciler's agent config in place.
func (e *fleetReconcileEnv) trackedLiveWorker(t *testing.T) beads.Bead {
	t.Helper()
	e.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	e.addRunningWorkerDesiredWithNewConfig()
	session := e.createSessionBead("worker", "worker")
	e.setSessionMetadata(&session, map[string]string{
		"state":               "active",
		"last_woke_at":        e.clk.Now().UTC().Format(time.RFC3339),
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	ticket, ok := e.guard.Admit(session.ID, "worker")
	if !ok {
		t.Fatal("Admit(worker) denied, want admitted")
	}
	ticket.Resolve(fleetStartOK)
	if n := e.tracked(); n != 1 {
		t.Fatalf("premise: guard confirms %d start(s), want 1", n)
	}
	return session
}

// A config-drift drain is the reconciler's own doing: the guard stops
// confirming the drained session at once, so its runtime ending is never read
// as a start that failed.
func TestReconcile_FleetGuardForgetsASessionOnConfigDriftDrain(t *testing.T) {
	env := newFleetReconcileEnv(t)
	session := env.trackedLiveWorker(t)

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != "config-drift" {
		t.Fatalf("premise: no config-drift drain began (drain = %+v, stderr = %s)", ds, env.stderr.String())
	}
	if n := env.tracked(); n != 0 {
		t.Errorf("guard still confirms %d start(s) of a session the reconciler began draining for config drift, want none", n)
	}
}

// A drain-acked stop is likewise the reconciler's own doing.
func TestReconcile_FleetGuardForgetsASessionOnDrainAckStop(t *testing.T) {
	env := newFleetReconcileEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	sp := newBlockingStopProvider()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-sp.releaseStop:
		default:
			close(sp.releaseStop)
		}
	})
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{"pending_create_claim": "true"})
	if err := sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	ticket, ok := env.guard.Admit(session.ID, "worker")
	if !ok {
		t.Fatal("Admit(worker) denied, want admitted")
	}
	ticket.Resolve(fleetStartOK)
	if n := env.tracked(); n != 1 {
		t.Fatalf("premise: guard confirms %d start(s), want 1", n)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	reconcileSessionBeadsAtPath(
		context.Background(), env.city, []beads.Bead{session}, env.desiredState,
		map[string]bool{"worker": true}, env.cfg, sp, env.store, dops, nil, nil, nil,
		env.dt, nil, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		append(env.startOptions, withAsyncDrainAckStopTracker(env.stops))...,
	)

	select {
	case <-sp.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("premise: the drain-ack stop did not start")
	}
	if n := env.tracked(); n != 0 {
		t.Errorf("guard still confirms %d start(s) of a session the reconciler is stopping on a drain ack, want none", n)
	}
}

// fleetTraceTemplate is the template key the fleet breaker's per-tick record
// is filed under.
const fleetTraceTemplate = "fleet_start_breaker"

func fleetTraceRecords(trace *sessionReconcilerTraceCycle) []SessionReconcilerTraceRecord {
	var out []SessionReconcilerTraceRecord
	for _, r := range trace.records {
		if r.SiteCode == TraceSiteFleetStartBreaker {
			out = append(out, r)
		}
	}
	return out
}

// The per-tick fleet record is written on every tick while an episode is under
// way (open, half-open, or starts being deferred) and never while the fleet is
// healthy and idle. A tick that carries no record is how an operator reads
// "healthy", so the quiet tick matters as much as the loud one.
func TestFleetGuard_RecordTickWritesWhileAnEpisodeIsUnderWay(t *testing.T) {
	record := func(g *fleetStartGuard) []SessionReconcilerTraceRecord {
		trace := newPoolDesiredStateTestTrace(fleetTraceTemplate)
		g.recordTick(trace, nil, nil)
		return fleetTraceRecords(trace)
	}

	t.Run("closed and idle writes nothing", func(t *testing.T) {
		g, _ := newFleetTestGuard(t)
		if recs := record(g); len(recs) != 0 {
			t.Fatalf("records = %d, want none for a healthy idle fleet", len(recs))
		}
	})
	t.Run("open writes a record every tick", func(t *testing.T) {
		g, clk := newFleetTestGuard(t)
		tripFleet(t, g, clk)
		for tick := 1; tick <= 2; tick++ {
			recs := record(g)
			if len(recs) != 1 {
				t.Fatalf("tick %d: records = %d, want 1 while the breaker is open", tick, len(recs))
			}
			if recs[0].OutcomeCode != TraceOutcomeOpen || recs[0].Fields["state"] != "open" {
				t.Errorf("tick %d: outcome = %q state = %#v, want open", tick, recs[0].OutcomeCode, recs[0].Fields["state"])
			}
			if recs[0].Fields["reason"] != fleetReasonFailures {
				t.Errorf("tick %d: reason = %#v, want %q", tick, recs[0].Fields["reason"], fleetReasonFailures)
			}
		}
	})
	t.Run("closed but deferring writes a record once", func(t *testing.T) {
		// The breaker closed mid-tick after the gate had deferred a start: the
		// tick still records what it deferred, and the next one is quiet.
		g, _ := newFleetTestGuard(t)
		g.NoteDeferred()
		g.NoteDeferred()
		recs := record(g)
		if len(recs) != 1 {
			t.Fatalf("records = %d, want 1 for a tick that deferred starts", len(recs))
		}
		if got := recs[0].Fields["deferred"]; got != 2 {
			t.Errorf("deferred = %#v, want 2", got)
		}
		if recs := record(g); len(recs) != 0 {
			t.Errorf("next tick records = %d, want none: the deferred count resets each tick", len(recs))
		}
	})
}

// The executor's admission refusal is the last place an open breaker stops a
// start: the candidate never reaches the provider, and the trace says why, so a
// "the fleet is held" tick reads the same whether the reconciler gate or the
// executor turned the start away.
func TestExecutePlannedStarts_OpenFleetBreakerTracesTheDeferral(t *testing.T) {
	r := newFleetProbeRun(t, "w5")
	tripFleet(t, r.g, r.e.clk)

	r.tick(t)

	if got := r.attempts(); got != 0 {
		t.Fatalf("provider Start calls = %d, want 0 while the breaker is open (calls: %v)", got, r.startCounts())
	}
	recs := r.e.records(TraceSiteLifecycleStartRun)
	var deferred []SessionReconcilerTraceRecord
	for _, rec := range recs {
		if rec.OutcomeCode == TraceOutcomeDeferredByFleetStartBreaker {
			deferred = append(deferred, rec)
		}
	}
	if len(deferred) != len(r.cands) {
		t.Fatalf("deferred-by-fleet-breaker trace records = %d, want %d (one per refused candidate)", len(deferred), len(r.cands))
	}
	for _, rec := range deferred {
		if rec.ReasonCode != TraceReasonFleetStartBreakerOpen {
			t.Errorf("record reason = %q, want %q", rec.ReasonCode, TraceReasonFleetStartBreakerOpen)
		}
		if rec.Fields["state"] != "open" {
			t.Errorf("record state = %#v, want open", rec.Fields["state"])
		}
	}
}
