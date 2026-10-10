package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// newDesiredPoolSeatWithNoWakeReason builds a live, desired pool-managed seat
// that the awake set has no reason to keep awake: no pool demand, no hold.
func newDesiredPoolSeatWithNoWakeReason(t *testing.T) (*reconcilerTestEnv, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{"pool_managed": "true"})
	return env, session
}

func reconcileDesiredPoolSeat(env *reconcilerTestEnv, session beads.Bead, work []beads.Bead) {
	reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		nil,
		env.cfg,
		env.sp,
		env.store,
		newDrainOps(env.sp),
		work,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
}

// Control: a desired pool seat holding nothing drains for no-wake-reason
// today and must keep doing so.
func TestReconcileSessionBeads_PoolSeatWithoutWorkDrainsForNoWakeReason(t *testing.T) {
	env, session := newDesiredPoolSeatWithNoWakeReason(t)
	reconcileDesiredPoolSeat(env, session, nil)
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("expected a drain to begin; stdout=%q stderr=%q", env.stdout.String(), env.stderr.String())
	}
	if ds.reason != "no-wake-reason" {
		t.Fatalf("drain reason = %q, want no-wake-reason", ds.reason)
	}
}

// A live pool seat that still holds an assigned OPEN step carries no wake
// demand while that step is dependency-blocked (bd's ready projection says
// not ready), so the awake set has no reason to keep it — but the seat is
// mid-molecule. Draining it is acked at once by `gc hook --claim --drain-ack`
// and the seat abandons its run: the step strands and the work re-runs
// (gastownhall/gascity#5731, #5473). Held work must keep the seat alive, the
// way a heartbeat hold does.
func TestReconcileSessionBeads_PoolSeatHoldingAssignedOpenWorkIsNotDrained(t *testing.T) {
	env, session := newDesiredPoolSeatWithNoWakeReason(t)
	blocked := true
	work, err := env.store.Create(beads.Bead{
		Title:     "emit-report (waits on run-review)",
		Type:      "task",
		Status:    "open",
		Assignee:  session.ID,
		IsBlocked: &blocked,
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	reconcileDesiredPoolSeat(env, session, []beads.Bead{work})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain = %+v, want none: a live holder of assigned open work must not be drained", ds)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("holder runtime was stopped")
	}
	if strings.Contains(env.stdout.String(), "Draining session 'worker'") {
		t.Fatalf("unexpected drain line: %q", env.stdout.String())
	}
}

// The same seat holding an in_progress claim is protected by the same gate:
// the ack-time cancel already exists for that shape, this keeps the drain from
// being sent at all.
func TestReconcileSessionBeads_PoolSeatHoldingInProgressWorkIsNotDrained(t *testing.T) {
	env, session := newDesiredPoolSeatWithNoWakeReason(t)
	work, err := env.store.Create(beads.Bead{
		Title:    "run-review",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	reconcileDesiredPoolSeat(env, session, []beads.Bead{work})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("drain = %+v, want none", ds)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("holder runtime was stopped")
	}
}

// An explicit intent still drains a holder: `gc session suspend` must win
// over held work, exactly as it wins over a heartbeat hold.
func TestReconcileSessionBeads_PoolSeatHoldingWorkStillDrainsOnExplicitIntent(t *testing.T) {
	env, session := newDesiredPoolSeatWithNoWakeReason(t)
	env.setSessionMetadata(&session, map[string]string{"sleep_intent": "user-hold", "state": "suspended"})
	work, err := env.store.Create(beads.Bead{
		Title:    "run-review",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	reconcileDesiredPoolSeat(env, session, []beads.Bead{work})
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("expected the explicit-intent drain to begin; stdout=%q", env.stdout.String())
	}
	if ds.reason != "user-hold" {
		t.Fatalf("drain reason = %q, want user-hold", ds.reason)
	}
}
