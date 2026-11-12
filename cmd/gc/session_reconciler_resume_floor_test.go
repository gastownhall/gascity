package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// reconcileIdleSessionsWithFloor reconciles n quiet pool-style sessions, each
// holding a 72h-old activity stamp, through the real idle tracker with the
// given activity floor, and returns how many the pass stopped.
func reconcileIdleSessionsWithFloor(t *testing.T, env *reconcilerTestEnv, names []string, sessions []beads.Bead, it *memoryIdleTracker, floor activityFloor) int {
	t.Helper()
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsAtPath(
		context.Background(), env.city, sessions, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withAsyncDrainAckStopTracker(env.stops),
		withActivityFloor(floor),
	)
	stopped := 0
	for _, name := range names {
		if !env.sp.IsRunning(name) {
			stopped++
		}
	}
	return stopped
}

func newQuietIdleFixture(t *testing.T, n int) (*reconcilerTestEnv, []string, []beads.Bead, *memoryIdleTracker) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	it := newIdleTracker()
	var names []string
	var sessions []beads.Bead
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("worker-%d", i)
		env.addDesired(name, "worker", true)
		session := env.createSessionBead(name, "worker")
		env.markSessionActive(&session)
		if err := env.sp.SetMeta(name, "GC_SESSION_ID", session.ID); err != nil {
			t.Fatalf("SetMeta(%s, GC_SESSION_ID): %v", name, err)
		}
		env.sp.SetActivity(name, env.clk.Now().Add(-72*time.Hour))
		it.setTimeout(name, 15*time.Minute)
		names = append(names, name)
		sessions = append(sessions, session)
	}
	return env, names, sessions, it
}

// TestReconcileSessionBeads_ResumeFloorSparesQuietSessionsOnFirstPass is the
// ga-w21nzz incident: after a host suspend every session's wall-clock
// activity stamp is hours old, and without a floor the first pass stops all
// of them at once.
func TestReconcileSessionBeads_ResumeFloorSparesQuietSessionsOnFirstPass(t *testing.T) {
	env, names, sessions, it := newQuietIdleFixture(t, 20)
	floor := activityFloor{at: env.clk.Now().Add(-5 * time.Second)}
	if got := reconcileIdleSessionsWithFloor(t, env, names, sessions, it, floor); got != 0 {
		t.Fatalf("first pass after a detected resume stopped %d of %d sessions, want 0", got, len(names))
	}
}

func TestReconcileSessionBeads_ResumeFloorSessionsStopOnceIdleTimeoutElapsesPastResume(t *testing.T) {
	env, names, sessions, it := newQuietIdleFixture(t, 20)
	floor := activityFloor{at: env.clk.Now().Add(-16 * time.Minute)}
	if got := reconcileIdleSessionsWithFloor(t, env, names, sessions, it, floor); got != len(names) {
		t.Fatalf("stopped %d of %d sessions once idle_timeout elapsed past the resume, want all", got, len(names))
	}
}

func TestReconcileSessionBeads_NoResumeControlStopsStaleSessionsFirstPass(t *testing.T) {
	env, names, sessions, it := newQuietIdleFixture(t, 20)
	if got := reconcileIdleSessionsWithFloor(t, env, names, sessions, it, activityFloor{}); got != len(names) {
		t.Fatalf("control: stopped %d of %d stale sessions with no resume, want all", got, len(names))
	}
}

func TestReconcileSessionBeads_ResumeFloorProgressStallClaimless(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.startOptions = append(env.startOptions, withActivityFloor(activityFloor{at: env.clk.Now().Add(-time.Minute)}))

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("claim-less progress-stall recycled a session 1m after a detected resume; stall time is measured from the resume")
	}
}

func TestReconcileSessionBeads_ResumeFloorProgressStallRecyclesOncePastResume(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.startOptions = append(env.startOptions, withActivityFloor(activityFloor{at: env.clk.Now().Add(-31 * time.Minute)}))

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("progress-stall did not recycle a session stalled 31m past the resume (threshold 30m)")
	}
}

// newClaimHolderStallResumeEnv is the progress-stall fixture with a 10m
// claim-less stall and a 20m claim-holder stall, and an in_progress bead
// assigned to the session: the 1h-old activity stamp can only recycle it
// through sessionClaimHolderStalled, never the claim-less path. The 10m
// claim-less timeout is what opens the reconciler's cheap time gate (the
// smaller of the two) for a stamp floored 15m back, so the claim-holder
// decision itself, not the gate, is what a floor between 10m and 20m tests.
func newClaimHolderStallResumeEnv(t *testing.T) (*restartRequestTestEnv, beads.Bead, string) {
	t.Helper()
	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Session.ProgressStallTimeout = "10m"
	env.cfg.Session.ClaimHolderStallTimeout = "20m"
	work, err := env.store.Create(beads.Bead{Title: "claimed work", Type: "task", Assignee: sessionName})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
	return env, session, sessionName
}

// The claim-holder stall measures from the resume the same way the claim-less
// stall does: passing the raw stamp to sessionClaimHolderStalled would recycle
// a confirmed holder one pass after a suspend.
func TestReconcileSessionBeads_ResumeFloorClaimHolderStallSparesHolderBeforeThreshold(t *testing.T) {
	env, session, sessionName := newClaimHolderStallResumeEnv(t)
	env.startOptions = append(env.startOptions, withActivityFloor(activityFloor{at: env.clk.Now().Add(-15 * time.Minute)}))

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("claim-holder stall recycled a holder 15m after a detected resume (threshold 20m); stall time is measured from the resume")
	}
	if strings.Contains(env.stderr.String(), "claim-holder-stalled") {
		t.Fatalf("stderr = %q, want no claim-holder stall diagnostic", env.stderr.String())
	}
}

func TestReconcileSessionBeads_ResumeFloorClaimHolderStallRecyclesOncePastResume(t *testing.T) {
	env, session, sessionName := newClaimHolderStallResumeEnv(t)
	env.startOptions = append(env.startOptions, withActivityFloor(activityFloor{at: env.clk.Now().Add(-21 * time.Minute)}))

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("claim-holder stall did not recycle a holder stalled 21m past the resume (threshold 20m)")
	}
	if !strings.Contains(env.stderr.String(), "claim-holder-stalled") {
		t.Fatalf("stderr = %q, want claim-holder-stalled diagnostic", env.stderr.String())
	}
}

// The idle-respawn readers order a raw activity stamp against an instant that
// is itself a wall time (the drain's start, the probe's completion). A floor
// moves a stamp the host's suspend made old to the resume instant, which is
// after that instant, so a floored read would call a quiet session "active
// since" and cancel the respawn. These tests pin the readers to the raw stamp.

// An idle-respawn drain acknowledged before the resume still stops the session
// on the pass after it: neither the drain-ack revalidation
// (idleRespawnAckUnsafeToStop) nor the drain advance's cancel check
// (advanceSessionDrainsWithSessionsTraced) reads the floored stamp.
func TestReconcileSessionBeads_ResumeFloorDoesNotCancelIdleRespawnAck(t *testing.T) {
	env, session, work := newIdleRespawnReconcilerTest(t, "open", 2*time.Minute)
	dops := newDrainOps(env.sp)
	idleGate := make(chan struct{})
	env.sp.WaitForIdleErrors["worker"] = nil
	env.sp.WaitForIdleGates["worker"] = idleGate

	reconcileIdleRespawnTestTickWithDrainOps(t, env, session, work, true, dops)
	close(idleGate)
	waitForIdleProbeReady(t, env.dt, session.ID)
	fresh, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("reload session before drain begin: %v", err)
	}
	reconcileIdleRespawnTestTickWithDrainOps(t, env, fresh, work, true, dops)
	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != idleRespawnDrainReason {
		t.Fatalf("idle-respawn drain did not begin: %+v", ds)
	}
	if ack, err := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); err != nil || ack != "1" {
		t.Fatalf("drain acknowledgement = %q, %v; want 1", ack, err)
	}

	// The host suspends after the drain began and resumes; the controller
	// detects it on this pass. The session's own stamp predates the drain.
	env.sp.SetActivity("worker", ds.startedAt.Add(-time.Hour))
	env.clk.Advance(10 * time.Second)
	floor := activityFloor{at: env.clk.Now().Add(-2 * time.Second)}
	if !floor.at.After(ds.startedAt) {
		t.Fatalf("test setup: floor %v is not after the drain start %v", floor.at, ds.startedAt)
	}
	fresh, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("reload session before stop tick: %v", err)
	}
	reconcileIdleRespawnTestTickWithDrainOps(t, env, fresh, work, true, dops, withActivityFloor(floor))
	waitAsyncStopsForTest(t, env.stops)

	if env.sp.IsRunning("worker") {
		t.Fatalf("a detected resume canceled the idle-respawn stop; stderr = %q", env.stderr.String())
	}
}

// An idle probe that completed before the resume still licenses the drain on
// the pass after it: shouldBeginIdleDrainInfo compares the raw stamp with the
// probe's completion time.
func TestReconcileSessionBeads_ResumeFloorDoesNotVetoIdleRespawnBegin(t *testing.T) {
	env, session, work := newIdleRespawnReconcilerTest(t, "open", 2*time.Minute)
	idleGate := make(chan struct{})
	env.sp.WaitForIdleErrors["worker"] = nil
	env.sp.WaitForIdleGates["worker"] = idleGate

	reconcileIdleRespawnTestTick(t, env, session, work, true)
	close(idleGate)
	waitForIdleProbeReady(t, env.dt, session.ID)
	probe, ok := env.dt.idleProbe(session.ID)
	if !ok {
		t.Fatal("idle probe missing after it completed")
	}

	// The stamp is older than the probe; the resume instant is after it.
	env.sp.SetActivity("worker", probe.completedAt.Add(-time.Hour))
	env.clk.Advance(10 * time.Second)
	floor := activityFloor{at: env.clk.Now().Add(-2 * time.Second)}
	if !floor.at.After(probe.completedAt) {
		t.Fatalf("test setup: floor %v is not after the probe completion %v", floor.at, probe.completedAt)
	}
	fresh, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	reconcileIdleRespawnTestTick(t, env, fresh, work, true, withActivityFloor(floor))

	if ds := env.dt.get(session.ID); ds == nil || ds.reason != idleRespawnDrainReason {
		t.Fatalf("a detected resume vetoed the idle-respawn drain begin: %+v", ds)
	}
}

// A drain begun before the resume and not yet acknowledged is acknowledged on
// the pass that detects it: the drain advance's cancel check
// (advanceSessionDrainsWithSessionsTraced) compares the raw stamp with the
// drain's start.
func TestReconcileSessionBeads_ResumeFloorDoesNotCancelUnackedIdleRespawnDrain(t *testing.T) {
	env, session, work := newIdleRespawnReconcilerTest(t, "open", 2*time.Minute)
	if !beginSessionDrainInfo(env.sessionInfo(session.ID), env.sp, env.dt, idleRespawnDrainReason, env.clk, defaultDrainTimeout) {
		t.Fatal("begin idle-respawn drain")
	}
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("idle-respawn drain missing after it began")
	}
	env.sp.SetActivity("worker", ds.startedAt.Add(-time.Hour))
	env.clk.Advance(10 * time.Second)
	floor := activityFloor{at: env.clk.Now().Add(-2 * time.Second)}
	if !floor.at.After(ds.startedAt) {
		t.Fatalf("test setup: floor %v is not after the drain start %v", floor.at, ds.startedAt)
	}
	fresh, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	reconcileIdleRespawnTestTick(t, env, fresh, work, true, withActivityFloor(floor))

	if ds := env.dt.get(session.ID); ds == nil || ds.reason != idleRespawnDrainReason {
		t.Fatalf("a detected resume canceled the unacknowledged idle-respawn drain: %+v", ds)
	}
	if ack, err := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); err != nil || ack != "1" {
		t.Fatalf("drain acknowledgement = %q, %v; want 1", ack, err)
	}
}
