package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// heldDrainAckFixture is a live session an operator suspended (a user hold)
// whose controller drain has begun on that row and been acknowledged: the
// next tick's drain-ack handler marks it stop-pending. It returns the row as
// the drain read it.
func heldDrainAckFixture(t *testing.T, tracked bool) (*reconcilerTestEnv, beads.Bead, beads.Bead, *fakeDrainOps) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{"held_until": "2099-01-01T00:00:00Z", "sleep_intent": "user-hold"})
	blocker, err := env.store.Create(beads.Bead{Title: "blocker", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	work, err := env.store.Create(beads.Bead{Title: "blocked assigned work", Type: "task", Status: "open", Assignee: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	// An operator's suspend drains with a reason no wake cancels: only the
	// drain's basis can void it.
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", &drainState{reason: "suspended", generation: 1, ackSet: true}); err != nil {
		t.Fatal(err)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatal(err)
	}
	if tracked {
		// The deadline is ahead: only the mark acts on the drain this tick.
		beginDrainForTest(t, env.store, env.dt, session.ID, "suspended", env.clk.Now().Add(-time.Minute), env.clk.Now().Add(time.Minute)).ackSet = true
	}
	return env, mustGetBead(t, env.store, session.ID), work, dops
}

func reconcileDrainAckTick(env *reconcilerTestEnv, snapshot, work beads.Bead, dops drainOps) {
	reconcileSessionBeadsAtPath(context.Background(), "", []beads.Bead{snapshot}, nil, nil, env.cfg, env.sp, env.store, dops,
		[]beads.Bead{work}, nil, nil, env.dt, map[string]int{}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr)
}

// operatorResume is `gc session wake`: it consumes the operator's hold.
func operatorResume(t *testing.T, env *reconcilerTestEnv, id string) {
	t.Helper()
	if _, err := sessionFrontDoor(env.store).WakeSession(id, env.clk.Now(), sessionpkg.WakeOpts{}); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}
}

func assertNotStopPending(t *testing.T, env *reconcilerTestEnv, id string) {
	t.Helper()
	if isDrainAckStopPendingInfo(env.sessionInfo(id)) {
		t.Fatalf("row %v marked stop-pending over the consumed hold", mustGetBead(t, env.store, id).Metadata)
	}
	if !env.sp.IsRunning("worker") || env.sp.CountCalls("Stop", "worker") != 0 {
		t.Fatalf("running %v, stops %d; want the resumed runtime left up", env.sp.IsRunning("worker"), env.sp.CountCalls("Stop", "worker"))
	}
}

// TestDrainAckControlCaseMarksStopPending is the fixture unmoved: the tick
// marks the held row stop-pending, so the refusals below are the fence's.
func TestDrainAckControlCaseMarksStopPending(t *testing.T) {
	env, snapshot, work, dops := heldDrainAckFixture(t, true)
	reconcileDrainAckTick(env, snapshot, work, dops)
	if !isDrainAckStopPendingInfo(env.sessionInfo(snapshot.ID)) {
		t.Fatalf("row %v, stderr %q; want stop-pending", mustGetBead(t, env.store, snapshot.ID).Metadata, env.stderr.String())
	}
}

// TestDrainAckStaleSnapshotOverAConsumedHoldIsRefused is SR3-1: the tick
// decided on a snapshot read before an operator's resume consumed the hold.
// The stop-pending mark must not land over the resumed row, whether the
// drain is the controller's (its basis) or the agent's own ack (the tick's
// snapshot).
func TestDrainAckStaleSnapshotOverAConsumedHoldIsRefused(t *testing.T) {
	for _, tracked := range []bool{true, false} {
		env, snapshot, work, dops := heldDrainAckFixture(t, tracked)
		operatorResume(t, env, snapshot.ID)
		reconcileDrainAckTick(env, snapshot, work, dops)
		assertNotStopPending(t, env, snapshot.ID)
	}
}

// TestDrainAckFollowUpTickOverAConsumedHoldIsRefused is the two-tick race
// (SR2-1): the follow-up tick reads the resumed row, but the drain's ack was
// made under the hold. The mark executes against the drain's basis, so it
// refuses, and the void drain's ack and tracker entry are cleared.
func TestDrainAckFollowUpTickOverAConsumedHoldIsRefused(t *testing.T) {
	env, snapshot, work, dops := heldDrainAckFixture(t, true)
	operatorResume(t, env, snapshot.ID)
	reconcileDrainAckTick(env, mustGetBead(t, env.store, snapshot.ID), work, dops)
	assertNotStopPending(t, env, snapshot.ID)
	if env.dt.get(snapshot.ID) != nil {
		t.Fatal("the void drain is still tracked")
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack == "1" {
		t.Fatal("the void drain's ack is still set")
	}
}

// idleRespawnDrainAcked drives an idle-respawn drain through the reconciler's
// ticks to its acknowledgement: tick 1 probes, tick 2 records the attempt,
// begins the drain and acks it.
func idleRespawnDrainAcked(t *testing.T) (*reconcilerTestEnv, beads.Bead, beads.Bead, drainOps) {
	t.Helper()
	env, session, work := newIdleRespawnReconcilerTest(t, "open", 2*time.Minute)
	dops := newDrainOps(env.sp)
	idleGate := make(chan struct{})
	env.sp.WaitForIdleErrors["worker"] = nil
	env.sp.WaitForIdleGates["worker"] = idleGate
	reconcileIdleRespawnTestTickWithDrainOps(t, env, session, work, true, dops)
	close(idleGate)
	waitForIdleProbeReady(t, env.dt, session.ID)
	reconcileIdleRespawnTestTickWithDrainOps(t, env, mustGetBead(t, env.store, session.ID), work, true, dops)
	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != idleRespawnDrainReason {
		t.Fatalf("idle-respawn drain = %+v; stderr %q", ds, env.stderr.String())
	}
	if ack, err := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); err != nil || ack != "1" {
		t.Fatalf("drain acknowledgement = %q, %v; want 1", ack, err)
	}
	// The attempt the drain recorded is in its basis: the row as stored.
	if got := ds.basis.Info().IdleRespawnAttempts; got != "1" {
		t.Fatalf("drain basis idle_respawn_attempts = %q, want the recorded attempt", got)
	}
	env.clk.Advance(10 * time.Second)
	return env, session, work, dops
}

// TestIdleRespawnDrainCompletes: the idle-respawn drain's own attempt write
// does not void its basis; the acked drain is marked stop-pending.
func TestIdleRespawnDrainCompletes(t *testing.T) {
	env, session, work, dops := idleRespawnDrainAcked(t)
	reconcileIdleRespawnTestTickWithDrainOps(t, env, mustGetBead(t, env.store, session.ID), work, true, dops)
	if !isDrainAckStopPendingInfo(env.sessionInfo(session.ID)) {
		t.Fatalf("row %v; stderr %q; want stop-pending", mustGetBead(t, env.store, session.ID).Metadata, env.stderr.String())
	}
}

// TestDrainDetachMidDrainDoesNotStallIt: a user detaching while the drain
// runs (reconcileDetachedAtInfo's marker) is not a fact the drain rests on.
func TestDrainDetachMidDrainDoesNotStallIt(t *testing.T) {
	env, session, work, dops := idleRespawnDrainAcked(t)
	if err := sessionFrontDoor(env.store).SetMarker(session.ID, "detached_at", env.clk.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	reconcileIdleRespawnTestTickWithDrainOps(t, env, mustGetBead(t, env.store, session.ID), work, true, dops)
	if !isDrainAckStopPendingInfo(env.sessionInfo(session.ID)) {
		t.Fatalf("row %v; stderr %q; want stop-pending", mustGetBead(t, env.store, session.ID).Metadata, env.stderr.String())
	}
}

// TestBeginIdleRespawnDrainIfIdleDecidesOnItsRow: the attempt is recorded
// only on the row the drain decided on; a row that moved records nothing and
// begins no drain.
func TestBeginIdleRespawnDrainIfIdleDecidesOnItsRow(t *testing.T) {
	clk := &clock.Fake{Time: time.Now().UTC()}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "w", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	info := sessionpkg.Info{ID: "s1", SessionNameMetadata: "w", Generation: "1", DetachedAt: clk.Now().Add(-2 * time.Minute).Format(time.RFC3339)}
	policy := resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Effective: "60s", Capability: runtime.SessionSleepCapabilityFull, Duration: time.Minute}
	eval := wakeEvaluation{Reason: "assigned-work", Reasons: []WakeReason{WakeWork}, Policy: policy, AssignedWorkBeadID: "work-1"}
	moved := info
	moved.Generation = "2"
	sessFront := idleRespawnUnitStore(t, moved)
	dt := newDrainTracker()
	probe := dt.startIdleProbe(info.ID)
	dt.finishIdleProbe(info.ID, probe, true, clk.Now().Add(-time.Second))
	began, patch, err := beginIdleRespawnDrainIfIdle(info, eval, dt, sp, sessFront, clk)
	if err != nil || began || patch != nil || dt.get(info.ID) != nil {
		t.Fatalf("begin over a moved row = %v, %v, %v (drain %+v); want nothing", began, patch, err, dt.get(info.ID))
	}
	if got, _ := sessFront.Get(info.ID); got.IdleRespawnAttempts != "" {
		t.Fatalf("idle_respawn_attempts = %q on the moved row, want unrecorded", got.IdleRespawnAttempts)
	}
}

// TestDrainTimeoutOverAConsumedHoldIsRefused is the two-tick race on the
// drain's timeout kill: the tick reads the resumed row, but the kill executes
// against the drain's basis, so it refuses and the void drain is dropped.
func TestDrainTimeoutOverAConsumedHoldIsRefused(t *testing.T) {
	env, snapshot, _, _ := heldDrainAckFixture(t, true)
	operatorResume(t, env, snapshot.ID)
	env.clk.Advance(2 * time.Minute) // past the drain's deadline
	advanceSessionDrainsWithSessionsTraced("", env.dt, env.sp, env.store, func(id string) (sessionpkg.Info, bool) {
		return env.sessionInfo(id), id == snapshot.ID
	}, map[string]wakeEvaluation{}, env.cfg, env.clk, nil)
	assertNotStopPending(t, env, snapshot.ID)
	if env.dt.get(snapshot.ID) != nil {
		t.Fatal("the void drain is still tracked")
	}
}

// TestResetStallEvictionDecidesOnItsRow: the eviction rests on the same
// continuation reset still pending, not on the incarnation: a row whose reset
// moved keeps its runtime; one that moved only otherwise is still evicted.
func TestResetStallEvictionDecidesOnItsRow(t *testing.T) {
	for _, c := range []struct {
		move  map[string]string
		evict bool
	}{
		{map[string]string{sessionpkg.ResetCommittedAtKey: "2026-03-08T11:59:30Z"}, false},
		{map[string]string{"continuation_reset_pending": ""}, false},
		{map[string]string{"generation": "2", "sleep_intent": "user-hold"}, true},
	} {
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}, Session: config.SessionConfig{StartupTimeout: "60s"}}
		if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "x"}); err != nil {
			t.Fatal(err)
		}
		session := env.createSessionBead("worker", "worker")
		env.setSessionMetadata(&session, map[string]string{
			"continuation_reset_pending":   "true",
			sessionpkg.ResetCommittedAtKey: env.clk.Now().Add(-75 * time.Second).UTC().Format(time.RFC3339),
		})
		decided := sessionInfoFromBead(mustGetBead(t, env.store, session.ID))
		env.setSessionMetadata(&session, c.move)
		recordResetStallIfDue("", env.store, env.sp, env.cfg, decided, "worker", "worker", true, false, time.Minute, env.clk.Now().UTC(), env.dt, nil, &env.stderr, nil)
		if evicted := env.sp.CountCalls("Stop", "worker") > 0; evicted != c.evict {
			t.Errorf("moved %v: evicted %v, want %v (stderr %q)", c.move, evicted, c.evict, env.stderr.String())
		}
	}
}

// TestIdleDrainBasisIsTheStoredRow: the idle drain marks the row
// idle-stop-pending before it begins, and its basis is that row as stored, so
// its stop-pending mark and timeout kill are not refused by its own write.
func TestIdleDrainBasisIsTheStoredRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"}, Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{"last_woke_at": ts, "detached_at": ts})
	env.sp.WaitForIdleErrors["worker"] = nil
	idleGate := make(chan struct{})
	env.sp.WaitForIdleGates["worker"] = idleGate
	tick := func(b beads.Bead) {
		reconcileSessionBeads(context.Background(), []beads.Bead{b}, env.desiredState, configuredSessionNames(env.cfg, "", env.store), env.cfg, env.sp,
			env.store, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr)
	}
	tick(session)
	close(idleGate)
	waitForIdleProbeReady(t, env.dt, session.ID)
	tick(mustGetBead(t, env.store, session.ID))
	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != "idle" {
		t.Fatalf("idle drain = %+v, want begun", ds)
	}
	if holds, err := sessionFrontDoor(env.store).Holds(ds.basis); err != nil || !holds {
		t.Fatalf("the idle drain's basis (sleep_intent %q) does not hold for its own row %v: %v", ds.basis.Info().SleepIntent, mustGetBead(t, env.store, session.ID).Metadata, err)
	}
}
