package main

import (
	"context"
	"errors"
	"io"
	"strings"
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
	return drainAckFixture(t, tracked, true)
}

// drainAckFixture is heldDrainAckFixture, the row held or not.
func drainAckFixture(t *testing.T, tracked, held bool) (*reconcilerTestEnv, beads.Bead, beads.Bead, *fakeDrainOps) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	if held {
		env.setSessionMetadata(&session, map[string]string{"held_until": "2099-01-01T00:00:00Z", "sleep_intent": "user-hold"})
	}
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
	reconcileSessionBeadsAtPath(context.Background(), env.city, []beads.Bead{snapshot}, nil, nil, env.cfg, env.sp, env.store, dops,
		[]beads.Bead{work}, nil, nil, env.dt, map[string]int{}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...)
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
// controller still tracks its drain (decided on the drain's basis) or lost
// the tracker entry, a controller restart (decided on the tick's snapshot).
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
// against the drain's basis, so it refuses and the void drain is dropped with
// its reconciler-owned ack; a following tick stops nothing. An agent's own
// ack outlives the void drain.
func TestDrainTimeoutOverAConsumedHoldIsRefused(t *testing.T) {
	for _, agent := range []bool{false, true} {
		env, snapshot, work, _ := heldDrainAckFixture(t, true)
		if agent {
			agentAck(t, env)
		}
		operatorResume(t, env, snapshot.ID)
		env.clk.Advance(2 * time.Minute) // past the drain's deadline
		advanceSessionDrainsWithSessionsTraced(env.city, env.dt, env.sp, env.store, func(id string) (sessionpkg.Info, bool) {
			return env.sessionInfo(id), id == snapshot.ID
		}, map[string]wakeEvaluation{}, env.cfg, env.clk, nil)
		assertNotStopPending(t, env, snapshot.ID)
		if env.dt.get(snapshot.ID) != nil {
			t.Fatalf("agent %v: the void drain is still tracked", agent)
		}
		ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK")
		if agent {
			if ack != "1" {
				t.Fatal("the void drain erased the agent's own ack")
			}
			continue
		}
		if ack == "1" {
			t.Fatal("the void drain's ack is still set")
		}
		// The next tick reads the provider's ack through the production drain ops.
		reconcileDrainAckTick(env, mustGetBead(t, env.store, snapshot.ID), work, newDrainOps(env.sp))
		assertNotStopPending(t, env, snapshot.ID)
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
		env := newReconcilerTestEnv(t)
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
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"}, Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{"last_woke_at": ts, "detached_at": ts})
	env.sp.WaitForIdleErrors["worker"] = nil
	idleGate := make(chan struct{})
	env.sp.WaitForIdleGates["worker"] = idleGate
	tick := func(b beads.Bead) {
		reconcileSessionBeadsAtPath(context.Background(), env.city, []beads.Bead{b}, env.desiredState, configuredSessionNames(env.cfg, "", env.store), env.cfg, env.sp,
			env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...)
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

// TestDrainAckOverANewIncarnationIsRefused: a new incarnation started (its
// pre-wake commit) between the tick's snapshot and the stop-pending mark is
// not the row the drain decided on, whether the decision is the drain's basis
// or the tick's snapshot: the mark does not land and nothing is stopped.
func TestDrainAckOverANewIncarnationIsRefused(t *testing.T) {
	for _, tracked := range []bool{true, false} {
		env, snapshot, work, dops := drainAckFixture(t, tracked, false)
		reconcileDrainAckTick(env, snapshot, work, dops) // the control: unmoved, it marks
		if !isDrainAckStopPendingInfo(env.sessionInfo(snapshot.ID)) {
			t.Fatalf("tracked %v: the unmoved row was not marked (test premise); stderr %q", tracked, env.stderr.String())
		}

		env, snapshot, work, dops = drainAckFixture(t, tracked, false)
		newIncarnation := sessionpkg.PreWakePatch(sessionpkg.PreWakePatchInput{Generation: 2, InstanceToken: "tok-2", Now: env.clk.Now()})
		if err := sessionFrontDoor(env.store).ApplyPatch(snapshot.ID, newIncarnation); err != nil {
			t.Fatal(err)
		}
		reconcileDrainAckTick(env, snapshot, work, dops)
		if isDrainAckStopPendingInfo(env.sessionInfo(snapshot.ID)) || env.sp.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("tracked %v: row %v, stops %d; want the new incarnation unmarked and unstopped", tracked, mustGetBead(t, env.store, snapshot.ID).Metadata, env.sp.CountCalls("Stop", "worker"))
		}
	}
}

// agentAck turns the fixture's ack into the agent's own: `gc runtime
// drain-ack` through the production drain ops, which overwrite the source.
func agentAck(t *testing.T, env *reconcilerTestEnv) drainOps {
	t.Helper()
	dops := newDrainOps(env.sp)
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatal(err)
	}
	return dops
}

// heartbeat is `gc runtime heartbeat`: the agent's held_until.
func heartbeat(t *testing.T, env *reconcilerTestEnv) {
	t.Helper()
	var stderr strings.Builder
	if rc := doRuntimeHeartbeat(env.store, time.Hour, "worker", "worker", false, io.Discard, &stderr); rc != 0 {
		t.Fatalf("heartbeat rc %d: %s", rc, stderr.String())
	}
}

func assertStopPending(t *testing.T, env *reconcilerTestEnv, id string) {
	t.Helper()
	if !isDrainAckStopPendingInfo(env.sessionInfo(id)) {
		t.Fatalf("row %v; stderr %q; want stop-pending", mustGetBead(t, env.store, id).Metadata, env.stderr.String())
	}
}

// TestAgentDrainAckIsDecidedOnTheCurrentRow: an agent's own ack over a drain
// the controller still tracks is the agent's decision, made on the row as it
// is now. A heartbeat or a wake request that moved the row since the
// controller's drain began neither voids nor erases it: the row is marked.
func TestAgentDrainAckIsDecidedOnTheCurrentRow(t *testing.T) {
	for name, move := range map[string]func(t *testing.T, env *reconcilerTestEnv, id string){
		"heartbeat": func(t *testing.T, env *reconcilerTestEnv, _ string) { heartbeat(t, env) },
		"wake request": func(t *testing.T, env *reconcilerTestEnv, id string) {
			if err := sessionFrontDoor(env.store).ApplyPatch(id, sessionpkg.MetadataPatch{"wake_request": "explicit"}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			env, snapshot, work, _ := drainAckFixture(t, true, false)
			dops := agentAck(t, env)
			move(t, env, snapshot.ID)
			reconcileDrainAckTick(env, mustGetBead(t, env.store, snapshot.ID), work, dops)
			assertStopPending(t, env, snapshot.ID)
			source, _ := env.sp.GetMeta("worker", reconcilerDrainAckSourceKey)
			if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "1" || source != drainAckSourceAgentValue {
				t.Fatalf("ack %q source %q; want the agent's ack kept", ack, source)
			}
		})
	}
}

// TestDrainMidDrainHeartbeatAndSuspend: the controller's drain rests on the
// operator's intent, not on held_until. A heartbeat mid-drain (held_until
// alone, #3994) leaves the drain to stop the row; an operator's suspend
// mid-drain voids it.
func TestDrainMidDrainHeartbeatAndSuspend(t *testing.T) {
	env, snapshot, work, dops := drainAckFixture(t, true, false)
	heartbeat(t, env)
	reconcileDrainAckTick(env, mustGetBead(t, env.store, snapshot.ID), work, dops)
	assertStopPending(t, env, snapshot.ID)

	env, snapshot, work, dops = drainAckFixture(t, true, false)
	if err := sessionFrontDoor(env.store).OperatorSuspend(snapshot.ID, env.clk.Now()); err != nil {
		t.Fatal(err)
	}
	reconcileDrainAckTick(env, mustGetBead(t, env.store, snapshot.ID), work, dops)
	if isDrainAckStopPendingInfo(env.sessionInfo(snapshot.ID)) || env.dt.get(snapshot.ID) != nil {
		t.Fatalf("row %v, drain %+v; want the suspend to void the drain", mustGetBead(t, env.store, snapshot.ID).Metadata, env.dt.get(snapshot.ID))
	}
}

// TestIdleRespawnOverAnIdleStopPendingIntentKeepsItsBasis: a respawn that
// begins while the row still carries the idle drain's idle-stop-pending
// intent clears it in its own write, so the tick's later clear does not
// void the respawn's basis.
func TestIdleRespawnOverAnIdleStopPendingIntentKeepsItsBasis(t *testing.T) {
	clk := &clock.Fake{Time: time.Now().UTC()}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "w", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	info := sessionpkg.Info{ID: "s1", SessionNameMetadata: "w", Generation: "1", SleepIntent: "idle-stop-pending", DetachedAt: clk.Now().Add(-2 * time.Minute).Format(time.RFC3339)}
	policy := resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Effective: "60s", Capability: runtime.SessionSleepCapabilityFull, Duration: time.Minute}
	eval := wakeEvaluation{Reason: "assigned-work", Reasons: []WakeReason{WakeWork}, Policy: policy, AssignedWorkBeadID: "work-1"}
	sessFront := idleRespawnUnitStore(t, info)
	if err := sessFront.ApplyPatch(info.ID, sessionpkg.MetadataPatch{"sleep_intent": "idle-stop-pending"}); err != nil {
		t.Fatal(err)
	}
	dt := newDrainTracker()
	probe := dt.startIdleProbe(info.ID)
	dt.finishIdleProbe(info.ID, probe, true, clk.Now().Add(-time.Second))
	if began, _, err := beginIdleRespawnDrainIfIdle(info, eval, dt, sp, sessFront, clk); err != nil || !began {
		t.Fatalf("begin = %v, %v; want begun", began, err)
	}
	if err := sessFront.ApplyPatch(info.ID, sessionpkg.MetadataPatch{"sleep_intent": ""}); err != nil { // the tick's clear
		t.Fatal(err)
	}
	if holds, err := sessFront.Holds(dt.get(info.ID).basis); err != nil || !holds {
		t.Fatalf("the respawn's basis (sleep_intent %q) does not hold after the tick's clear: %v", dt.get(info.ID).basis.Info().SleepIntent, err)
	}
}

// TestUnreadableAckSourceDefers: an ack whose source cannot be read is
// neither the agent's nor the controller's. Over a consumed hold, the mark
// and the timeout both defer: the row is not marked stop-pending, the drain
// stays tracked, the ack stays set, and nothing is stopped.
func TestUnreadableAckSourceDefers(t *testing.T) {
	for _, path := range []string{"mark", "timeout"} {
		t.Run(path, func(t *testing.T) {
			env, snapshot, work, dops := heldDrainAckFixture(t, true)
			operatorResume(t, env, snapshot.ID)
			env.sp.GetMetaErrors["worker"] = map[string]error{reconcilerDrainAckSourceKey: errors.New("tmux: no server")}
			if path == "mark" {
				reconcileDrainAckTick(env, mustGetBead(t, env.store, snapshot.ID), work, dops)
			} else {
				env.clk.Advance(2 * time.Minute) // past the drain's deadline
				advanceSessionDrainsWithSessionsTraced(env.city, env.dt, env.sp, env.store, func(id string) (sessionpkg.Info, bool) {
					return env.sessionInfo(id), id == snapshot.ID
				}, map[string]wakeEvaluation{}, env.cfg, env.clk, nil)
			}
			assertNotStopPending(t, env, snapshot.ID)
			if env.dt.get(snapshot.ID) == nil {
				t.Fatal("the drain was dropped on an unreadable ack source")
			}
			delete(env.sp.GetMetaErrors, "worker")
			if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "1" {
				t.Fatal("the ack was cleared on an unreadable source")
			}
		})
	}
}
