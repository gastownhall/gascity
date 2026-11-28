package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The wait park of gastownhall/gascity#5561, end to end through the legacy
// reconciler: a pool-managed seat runs `gc session wait --sleep` while it
// still holds a claimed step, then drain-acks. The with-work finalize
// completes it as an idle sleep that keeps the wait (D-J 3), and the
// pool-slot gate must read that wait (isPoolSessionSlotFreeableInfo) so the
// parked seat keeps its bead, slot and claim until the wait wakes it or ends.

// newWaitParkedSeatEnv builds the repro shape: a desired, running pool worker
// that holds an in_progress claim and — when wait is set — parked on
// `gc session wait --sleep` through the tree's own writer. The returned bead
// is the freshly read row carrying every stamp.
func newWaitParkedSeatEnv(t *testing.T, sleep config.SessionSleepConfig, wait bool) (*reconcilerTestEnv, beads.Bead, beads.Bead, *recordingRecorder) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace:    config.Workspace{Name: "test-city"},
		SessionSleep: sleep,
		Agents:       []config.Agent{{Name: "worker"}},
	}
	rec := &recordingRecorder{}
	env.rec = rec
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{"pool_managed": "true"})
	if wait {
		// gc session wait --sleep: the wait hold, through the tree's own writer.
		if err := setSessionWaitHold(sessionFrontDoor(env.store), session.ID); err != nil {
			t.Fatalf("setSessionWaitHold: %v", err)
		}
	}
	task, err := env.store.Create(beads.Bead{Title: "claimed task", Type: "task"})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	status, assignee := "in_progress", session.ID
	if err := env.store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(task): %v", err)
	}
	task, err = env.store.Get(task.ID)
	if err != nil {
		t.Fatalf("Get(task): %v", err)
	}
	row, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	return env, row, task, rec
}

// drainAckAndWaitPark drives the drain-ack tick and the stop-pending
// finalize, returning the terminal row.
func drainAckAndWaitPark(t *testing.T, env *reconcilerTestEnv, session beads.Bead) beads.Bead {
	t.Helper()
	cfgNames := map[string]bool{"worker": true}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	if woken := reconcileSessionBeadsAtPath(
		context.Background(), env.city, []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, dops, nil, nil, nil, env.dt,
		map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...); woken != 0 {
		t.Fatalf("woken on the drain-ack tick = %d, want 0; stderr=%s", woken, env.stderr.String())
	}
	return env.reconcileStopPendingToTerminal(t, env.sp, session, dops, cfgNames)
}

// TestReconcileSessionBeads_WaitParkedPoolSeatKeepsSlotAndClaim is the bug's
// repro: a wait-parked pool seat that drain-acks holding an in_progress claim
// must stay parked — its bead open and asleep as idle with the wait keys
// kept, its claim intact, and no session.stranded diagnostic — through the
// finalize tick and ticks past strandedRepairConfirmGrace.
func TestReconcileSessionBeads_WaitParkedPoolSeatKeepsSlotAndClaim(t *testing.T) {
	env, session, task, rec := newWaitParkedSeatEnv(t, config.SessionSleepConfig{}, true)
	final := drainAckAndWaitPark(t, env, session)
	if m := final.Metadata; final.Status == "closed" || m["state"] != "asleep" || m["sleep_reason"] != string(sessionpkg.SleepReasonIdle) ||
		m["sleep_intent"] != string(sessionpkg.SleepReasonWaitHold) || m["wait_hold"] != "true" {
		t.Fatalf("after the drain-ack: status=%s state=%q sleep_reason=%q sleep_intent=%q wait_hold=%q, want an open asleep-idle seat keeping the wait (D-J 3)",
			final.Status, m["state"], m["sleep_reason"], m["sleep_intent"], m["wait_hold"])
	}

	// Ticks at +0 and past the stranded confirmation window: the repair arm
	// must never unclaim the work or close the parked bead.
	for _, advance := range []time.Duration{0, strandedRepairConfirmGrace + time.Minute} {
		env.clk.Time = env.clk.Now().Add(advance)
		cur, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", session.ID, err)
		}
		reconcileSessionBeadsAtPath(
			context.Background(), env.city, []beads.Bead{cur}, env.desiredState, map[string]bool{"worker": true},
			env.cfg, env.sp, env.store, nil, []beads.Bead{task}, nil, nil, env.dt,
			map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Errorf("the wait-parked pool seat was closed by the reaper; metadata=%v", got.Metadata)
	}
	if m := got.Metadata; m["state"] != "asleep" || m["sleep_reason"] != string(sessionpkg.SleepReasonIdle) ||
		m["sleep_intent"] != string(sessionpkg.SleepReasonWaitHold) || m["wait_hold"] != "true" {
		t.Errorf("the park did not survive: state=%q sleep_reason=%q sleep_intent=%q wait_hold=%q", m["state"], m["sleep_reason"], m["sleep_intent"], m["wait_hold"])
	}
	if marker := strings.TrimSpace(got.Metadata[strandedEventEmittedKey]); marker != "" {
		t.Errorf("stranded_event_emitted_at = %q, want empty: a deliberate wait park is not stranding", marker)
	}
	work, err := env.store.Get(task.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", task.ID, err)
	}
	if work.Assignee != session.ID || work.Status != "in_progress" {
		t.Errorf("work = %s/%q, want in_progress claimed by the parked session", work.Status, work.Assignee)
	}
	if evts := rec.ofType(events.SessionStranded); len(evts) != 0 {
		t.Errorf("recorded %d session.stranded events for a parked seat, want none", len(evts))
	}
}

// TestReconcileSessionBeads_WaitParkedSeatWakesInPlaceOnWaitReady continues
// the park: with a sleep policy and a current fingerprint, once the wait
// turns ready the same bead wakes in place (wait-ready) and no fresh bead is
// minted for the pool.
func TestReconcileSessionBeads_WaitParkedSeatWakesInPlaceOnWaitReady(t *testing.T) {
	env, session, task, _ := newWaitParkedSeatEnv(t, config.SessionSleepConfig{InteractiveResume: "5m", InteractiveFresh: "5m"}, true)
	drainAckAndWaitPark(t, env, session)
	// A tick persists the sleep policy (the fingerprint), as every tick does.
	policy := resolveSessionSleepPolicyInfo(env.sessionInfo(session.ID), env.cfg, env.sp)
	cur, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	env.setSessionMetadata(&cur, map[string]string{"sleep_policy_fingerprint": policy.Fingerprint})

	// Ticks past the stranded confirmation window: the seat must survive them
	// parked, or there is no bead left to wake.
	for _, advance := range []time.Duration{0, strandedRepairConfirmGrace + time.Minute} {
		env.clk.Time = env.clk.Now().Add(advance)
		row, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", session.ID, err)
		}
		reconcileSessionBeadsAtPath(
			context.Background(), env.city, []beads.Bead{row}, env.desiredState, map[string]bool{"worker": true},
			env.cfg, env.sp, env.store, nil, []beads.Bead{task}, nil, nil, env.dt,
			map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...)
	}
	if got, _ := env.store.Get(session.ID); got.Status == "closed" {
		t.Fatalf("the parked seat was closed before its wait could turn ready; metadata=%v", got.Metadata)
	}

	// The wait turns ready: the same session wakes in place.
	env.clk.Time = env.clk.Now().Add(10 * time.Minute)
	row, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	var out, errw strings.Builder
	reconcileSessionBeadsAtPath(
		context.Background(), env.city, []beads.Bead{row}, env.desiredState, map[string]bool{"worker": true},
		env.cfg, env.sp, env.store, nil, []beads.Bead{task}, nil, map[string]bool{session.ID: true}, env.dt,
		map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &out, &errw, env.startOptions...)
	if !env.sp.IsRunning("worker") {
		after, _ := env.store.Get(session.ID)
		t.Errorf("wait ready: the waiter was not woken; state=%q config_wake_suppressed=%q; stderr=%s",
			after.Metadata["state"], after.Metadata["config_wake_suppressed"], errw.String())
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Errorf("the woken waiter's bead is closed")
	}
	// No fresh bead for the pool: the same bead woke in place.
	rows, err := env.store.List(beads.ListQuery{Type: sessionBeadType, IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("List(session): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != session.ID {
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.ID + "/" + r.Status
		}
		t.Errorf("session beads = %v, want exactly the parked bead %s woken in place", ids, session.ID)
	}
}

// TestReconcileSessionBeads_WaitParkedSeatFreesWhenItsWaitEnds bounds the
// park: once the wait ends without waking the seat (a cancel or expiry
// clears the hold), the gate reads the idle row freeable again and the next
// reconcile treats the seat exactly as base treats an unheld asleep-idle
// seat holding started work — the assigned-work wake restarts it in place on
// its own bead, claim intact (the control test below pins that base outcome).
func TestReconcileSessionBeads_WaitParkedSeatFreesWhenItsWaitEnds(t *testing.T) {
	env, session, task, _ := newWaitParkedSeatEnv(t, config.SessionSleepConfig{}, true)
	drainAckAndWaitPark(t, env, session)

	// While the wait stands the seat is parked: no marker.
	env.clk.Time = env.clk.Now().Add(time.Minute)
	row, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	reconcileSessionBeadsAtPath(
		context.Background(), env.city, []beads.Bead{row}, env.desiredState, map[string]bool{"worker": true},
		env.cfg, env.sp, env.store, nil, []beads.Bead{task}, nil, nil, env.dt,
		map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...)
	if got, _ := env.store.Get(session.ID); strings.TrimSpace(got.Metadata[strandedEventEmittedKey]) != "" {
		t.Fatalf("while the wait stands the seat was marked stranded: %q", got.Metadata[strandedEventEmittedKey])
	}

	// The wait ends without waking it. Legacy's end-of-wait clear applies the
	// same WaitEnd cell the v2 wait step's clearSessionWaitHoldFenced does.
	if err := clearSessionWaitHoldIfIdle(sessionFrontDoor(env.store), session.ID); err != nil {
		t.Fatalf("clearSessionWaitHoldIfIdle: %v", err)
	}
	info := env.sessionInfo(session.ID)
	if info.WaitHold != "" || info.SleepIntent != "" {
		t.Fatalf("the clear left wait_hold=%q sleep_intent=%q, want both gone", info.WaitHold, info.SleepIntent)
	}
	if !isPoolSessionSlotFreeableInfo(info, env.clk.Now()) {
		t.Errorf("after the wait ended the gate must read the idle row freeable again")
	}

	// Base handling resumes on the next reconcile: the wake the wait had
	// suppressed fires, and the seat restarts in place on its claimed work.
	row, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	var out, errw strings.Builder
	reconcileSessionBeadsAtPath(
		context.Background(), env.city, []beads.Bead{row}, env.desiredState, map[string]bool{"worker": true},
		env.cfg, env.sp, env.store, nil, []beads.Bead{task}, nil, nil, env.dt,
		map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &out, &errw, env.startOptions...)
	if !env.sp.IsRunning("worker") {
		after, _ := env.store.Get(session.ID)
		t.Errorf("after the wait ended the seat was not restarted on its assigned work; state=%q; stderr=%s", after.Metadata["state"], errw.String())
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Errorf("the restarted seat's bead is closed")
	}
	work, err := env.store.Get(task.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", task.ID, err)
	}
	if work.Assignee != session.ID || work.Status != "in_progress" {
		t.Errorf("work = %s/%q, want still in_progress claimed by the restarted seat", work.Status, work.Assignee)
	}
}
