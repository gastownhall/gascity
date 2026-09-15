package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// newUndesiredPoolSeat builds a live pool-managed seat that has no entry in
// the desired set — the shape a slot shrink or a surplus seat produces — and
// stamps held_until if hold is non-zero.
func newUndesiredPoolSeat(t *testing.T, hold time.Duration) (*reconcilerTestEnv, beads.Bead, string) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	_ = env.sp.Start(context.Background(), "worker", runtime.Config{})
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	meta := map[string]string{"pool_managed": "true"}
	heldUntil := ""
	if hold != 0 {
		heldUntil = env.clk.Now().Add(hold).UTC().Format(time.RFC3339)
		meta["held_until"] = heldUntil
	}
	env.setSessionMetadata(&session, meta)
	return env, session, heldUntil
}

func reconcileUndesiredSeat(t *testing.T, env *reconcilerTestEnv, id string) {
	t.Helper()
	got, err := env.store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	env.reconcile([]beads.Bead{got})
}

// A pool-managed seat that fell out of the desired set (slot shrink, surplus
// after a demand recount) but carries a live heartbeat hold must not be
// drained as orphaned: the hold is the seat's keep-alive contract, the timer
// arm already honors it (#3994), and this is the arm the field incident went
// through (gastownhall/gascity#6173). No drain is ever begun, so the second
// tick past defaultDrainTimeout only shows the decision is stable; the
// in-flight and acked-drain tests below cover a drain that already exists.
func TestReconcileSessionBeads_HeldPoolSeatNotDesiredIsNotDrained(t *testing.T) {
	env, session, heldUntil := newUndesiredPoolSeat(t, 45*time.Minute)

	reconcileUndesiredSeat(t, env, session.ID)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("held pool seat drained on the first tick: reason=%q", ds.reason)
	}

	env.clk.Time = env.clk.Now().Add(defaultDrainTimeout + time.Minute)
	reconcileUndesiredSeat(t, env, session.ID)
	if !env.sp.IsRunning("worker") {
		t.Fatal("held pool seat was stopped past defaultDrainTimeout")
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("held pool seat left with a pending drain: reason=%q", ds.reason)
	}
	if strings.Contains(env.stdout.String(), "Draining session 'worker': orphaned") {
		t.Fatalf("orphan drain logged for a held seat:\n%s", env.stdout.String())
	}
	if got := env.sessionInfo(session.ID); got.HeldUntil != heldUntil {
		t.Errorf("held_until = %q, want preserved %q", got.HeldUntil, heldUntil)
	}
}

// Control: the same seat with no hold still drains as orphaned, so the gate
// changes nothing for an ordinary surplus seat.
func TestReconcileSessionBeads_UnheldPoolSeatNotDesiredStillDrains(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, 0)
	reconcileUndesiredSeat(t, env, session.ID)
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("expected the orphan drain to begin; stdout=%q", env.stdout.String())
	}
	if ds.reason != "orphaned" {
		t.Fatalf("drain reason = %q, want orphaned", ds.reason)
	}
}

// An expired hold protects nothing: the seat drains as orphaned once
// held_until is in the past.
func TestReconcileSessionBeads_ExpiredHoldPoolSeatNotDesiredDrains(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, -time.Minute)
	reconcileUndesiredSeat(t, env, session.ID)
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "orphaned" {
		t.Fatalf("expected an orphan drain for an expired hold, got %+v", ds)
	}
}

// A suspend hold is the intentional-park shape (sleep_intent="user-hold" with
// held_until, written by gc session suspend) and must keep draining when the
// seat is not desired; only a heartbeat hold — held_until with no
// sleep_intent — is a keep-alive.
func TestReconcileSessionBeads_SuspendedPoolSeatNotDesiredStillDrains(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, 45*time.Minute)
	env.setSessionMetadata(&session, map[string]string{"sleep_intent": "user-hold", "state": "suspended"})
	reconcileUndesiredSeat(t, env, session.ID)
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "orphaned" {
		t.Fatalf("expected the orphan drain for a suspend-parked seat, got %+v", ds)
	}
}

// The hold can arrive one tick late (the seat renewed its heartbeat after the
// orphan drain began): the in-flight drain is canceled through the hold lens,
// since the plain cancel refuses "orphaned". The tracker entry is what the
// drain scan would have force-stopped at the deadline, so its removal is the
// assertion; the tick past the original deadline shows nothing re-enters.
func TestReconcileSessionBeads_HeartbeatHoldCancelsInFlightOrphanDrain(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, 0)
	reconcileUndesiredSeat(t, env, session.ID)
	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != "orphaned" {
		t.Fatalf("expected the orphan drain to begin first, got %+v", ds)
	}
	deadline := ds.deadline

	heldUntil := env.clk.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{"held_until": heldUntil})
	reconcileUndesiredSeat(t, env, session.ID)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("in-flight orphan drain not canceled by the hold: %+v", *ds)
	}

	env.clk.Time = deadline.Add(time.Minute)
	reconcileUndesiredSeat(t, env, session.ID)
	if !env.sp.IsRunning("worker") {
		t.Fatal("held pool seat was stopped past the drain deadline it had already entered")
	}
	if strings.Contains(env.stdout.String(), "Draining session 'worker': orphaned\nDraining session 'worker': orphaned") {
		t.Fatalf("orphan drain re-entered after the hold:\n%s", env.stdout.String())
	}
}

// The hold can also arrive after the reconciler published its own drain ack:
// the drain-ack branch runs before the orphan arm, so it must honor the hold
// too — cancel the tracked drain, clear the reconciler's ack, keep the seat.
func TestReconcileSessionBeads_HeartbeatHoldCancelsAckedOrphanDrain(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, 45*time.Minute)
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(defaultDrainTimeout),
		reason:     "orphaned",
		generation: 1,
		ackSet:     true,
	})
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", &drainState{
		reason:     "orphaned",
		generation: 1,
		ackSet:     true,
	}); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}

	reconcileSessionBeadsAtPath(
		context.Background(), "", []beads.Bead{got}, nil, nil, env.cfg, env.sp, env.store,
		newDrainOps(env.sp), nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	if !env.sp.IsRunning("worker") {
		t.Fatal("acked orphan drain stopped a heartbeat-held pool seat")
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared", ack)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("tracked drain survived the hold: %+v", *ds)
	}
	if !strings.Contains(env.stdout.String(), "Canceled drain-acked session 'worker' (heartbeat hold)") {
		t.Fatalf("expected the heartbeat-hold cancel line, got:\n%s", env.stdout.String())
	}
}

// A hold that expires while the seat is still surplus stops protecting it:
// healExpiredTimersInfo clears the stale held_until at the top of the tick and
// the seat drains as orphaned — the transition most likely to regress.
func TestReconcileSessionBeads_HeldPoolSeatDrainsOnceHoldExpires(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, 10*time.Minute)
	reconcileUndesiredSeat(t, env, session.ID)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("live hold did not keep the seat: %+v", *ds)
	}

	env.clk.Time = env.clk.Now().Add(11 * time.Minute)
	reconcileUndesiredSeat(t, env, session.ID)
	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != "orphaned" {
		t.Fatalf("expected the orphan drain once the hold expired, got %+v", ds)
	}
	if got := env.sessionInfo(session.ID); got.HeldUntil != "" {
		t.Errorf("held_until = %q, want cleared once expired", got.HeldUntil)
	}
}

func reconcileUndesiredSeatWithDrainOps(t *testing.T, env *reconcilerTestEnv, id string, storeQueryPartial bool) {
	t.Helper()
	got, err := env.store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	reconcileSessionBeadsAtPath(
		context.Background(), "", []beads.Bead{got}, nil, nil, env.cfg, env.sp, env.store,
		newDrainOps(env.sp), nil, nil, nil, env.dt, map[string]int{}, storeQueryPartial, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)
}

// An AGENT drain-ack outranks the seat's own earlier keep-alive: the agent has
// agreed to stop, so the hold must not cancel the drain behind that ack.
func TestReconcileSessionBeads_HeartbeatHoldYieldsToAgentDrainAck(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, 45*time.Minute)
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(defaultDrainTimeout),
		reason:     "orphaned",
		generation: 1,
	})
	for key, value := range map[string]string{
		reconcilerDrainAckSourceKey: drainAckSourceAgentValue,
		"GC_DRAIN_ACK":              "1",
	} {
		if err := env.sp.SetMeta("worker", key, value); err != nil {
			t.Fatalf("SetMeta(%s): %v", key, err)
		}
	}

	reconcileUndesiredSeatWithDrainOps(t, env, session.ID, false)

	out := env.stdout.String()
	if strings.Contains(out, "(heartbeat hold)") || strings.Contains(out, "heartbeat hold active") {
		t.Fatalf("hold overrode an agent drain-ack:\n%s", out)
	}
	if source, _ := env.sp.GetMeta("worker", reconcilerDrainAckSourceKey); source == "" && env.sp.IsRunning("worker") {
		t.Fatal("agent ack was cleared while the seat kept running")
	}
}

// The hold needs no work query, so it is honored on a partial-store tick too:
// the not-desired arm defers everything else, but a deferred tick would still
// let the drain scan advance an existing orphan drain to a forced stop.
func TestReconcileSessionBeads_HeartbeatHoldHonoredOnPartialStoreTick(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, 45*time.Minute)
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(defaultDrainTimeout),
		reason:     "orphaned",
		generation: 1,
	})

	reconcileUndesiredSeatWithDrainOps(t, env, session.ID, true)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("in-flight orphan drain survived a partial-store tick under a live hold: %+v", *ds)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("held pool seat was stopped on a partial-store tick")
	}
	if !strings.Contains(env.stdout.String(), "heartbeat hold active") {
		t.Fatalf("expected the hold to decide before the partial-store deferral:\n%s", env.stdout.String())
	}
}

// Same on the drain-ack branch: a reconciler-published ack under a live hold
// is canceled even when the store query was partial.
func TestReconcileSessionBeads_HeartbeatHoldCancelsAckedOrphanDrainOnPartialStoreTick(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, 45*time.Minute)
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(defaultDrainTimeout),
		reason:     "orphaned",
		generation: 1,
		ackSet:     true,
	})
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", &drainState{reason: "orphaned", generation: 1, ackSet: true}); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
	}

	reconcileUndesiredSeatWithDrainOps(t, env, session.ID, true)

	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared on a partial-store tick under a live hold", ack)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("tracked drain survived: %+v", *ds)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("held pool seat was stopped")
	}
}

// The Phase 2 drain scan is the last place a lingering orphan drain can
// force-stop a held seat: any reconciler path that skipped the arm-level
// checks still reaches the deadline here. A live heartbeat hold on a pool seat
// cancels the drain in the scan itself; an agent ack still wins.
func TestAdvanceSessionDrains_HeartbeatHeldPoolSeatSurvivesOrphanDeadline(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()
	_ = sp.Start(context.Background(), "worker", runtime.Config{})
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"template":     "worker",
			"provider":     "claude",
			"work_dir":     t.TempDir(),
			"generation":   "3",
			"state":        "active",
			"pool_managed": "true",
			"held_until":   now.Add(45 * time.Minute).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-60 * time.Second),
		deadline:   now.Add(-10 * time.Second),
		reason:     "orphaned",
		generation: 3,
	})
	lookup := infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	})

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, lookup, map[string]wakeEvaluation{}, &config.City{}, clk, nil)

	if !sp.IsRunning("worker") {
		t.Fatal("heartbeat-held pool seat was force-stopped by the drain scan past an orphan deadline")
	}
	if dt.get(b.ID) != nil {
		t.Fatal("orphan drain should have been canceled by the hold in the drain scan")
	}

	// Same seat, but the agent acked the drain: the hold yields and the
	// deadline force-stop proceeds as before.
	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-60 * time.Second),
		deadline:   now.Add(-10 * time.Second),
		reason:     "orphaned",
		generation: 3,
	})
	for key, value := range map[string]string{reconcilerDrainAckSourceKey: drainAckSourceAgentValue, "GC_DRAIN_ACK": "1"} {
		if err := sp.SetMeta("worker", key, value); err != nil {
			t.Fatalf("SetMeta(%s): %v", key, err)
		}
	}
	advanceSessionDrainsWithSessionsTraced(dt, sp, store, lookup, map[string]wakeEvaluation{}, &config.City{}, clk, nil)
	if sp.IsRunning("worker") {
		t.Fatal("agent-acked orphan drain must still force-stop past its deadline despite the hold")
	}
}

// provenanceReadErrorProvider makes one metadata key unreadable, standing in
// for a provider whose ack-source read fails while the ack itself is set.
type provenanceReadErrorProvider struct {
	runtime.Provider
	failKey string
}

func (p provenanceReadErrorProvider) GetMeta(name, key string) (string, error) {
	if key == p.failKey {
		return "", errors.New("provenance unreadable")
	}
	return p.Provider.GetMeta(name, key)
}

// Provenance that cannot be established is not permission: an ack whose
// source read fails is left alone by the hold on the drain-ack branch, and
// the drain scan still force-stops it past its deadline.
func TestReconcileSessionBeads_HeartbeatHoldLeavesAckWhenProvenanceUnreadable(t *testing.T) {
	env, session, _ := newUndesiredPoolSeat(t, 45*time.Minute)
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(defaultDrainTimeout),
		reason:     "orphaned",
		generation: 1,
	})
	for key, value := range map[string]string{reconcilerDrainAckSourceKey: drainAckSourceAgentValue, "GC_DRAIN_ACK": "1"} {
		if err := env.sp.SetMeta("worker", key, value); err != nil {
			t.Fatalf("SetMeta(%s): %v", key, err)
		}
	}
	sp := provenanceReadErrorProvider{Provider: env.sp, failKey: reconcilerDrainAckSourceKey}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reconcileSessionBeadsAtPath(
		context.Background(), "", []beads.Bead{got}, nil, nil, env.cfg, sp, env.store,
		newDrainOps(sp), nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)
	if strings.Contains(env.stdout.String(), "(heartbeat hold)") {
		t.Fatalf("hold consumed an ack whose provenance was unreadable:\n%s", env.stdout.String())
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack == "" && env.sp.IsRunning("worker") {
		t.Fatal("ack cleared while the seat kept running")
	}

	// Drain scan, same seat, deadline passed: the hold yields, the stop proceeds.
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now().Add(-defaultDrainTimeout - time.Minute),
		deadline:   env.clk.Now().Add(-time.Minute),
		reason:     "orphaned",
		generation: 1,
	})
	advanceSessionDrainsWithSessionsTraced(env.dt, sp, env.store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		b, _ := env.store.Get(id)
		return &b
	}), map[string]wakeEvaluation{}, env.cfg, env.clk, nil)
	if env.sp.IsRunning("worker") {
		t.Fatal("drain scan kept a seat whose ack provenance was unreadable")
	}
}
