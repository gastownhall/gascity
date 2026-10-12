package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestReconcilerWakeDemandOverridesSleepSuppressionForExplicitAndInteractiveRoutedDemand(t *testing.T) {
	interactive := resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveFresh}
	eval := wakeEvaluation{}

	routed := AwakeDecision{ShouldWake: true, Reason: "routed-demand"}
	if !wakeDemandOverridesSleepSuppression(routed, eval, interactive, nil, "olivia", "", false) {
		t.Fatal("routed demand must override interactive sleep suppression: the holder owns the canonical alias and no standby can serve it")
	}
	if wakeDemandOverridesSleepSuppression(routed, eval, interactive, nil, "olivia", "user-hold", false) {
		t.Fatal("explicit sleep intent should still override routed demand")
	}

	explicit := AwakeDecision{ShouldWake: true, Reason: "explicit-wake"}
	if !wakeDemandOverridesSleepSuppression(explicit, eval, interactive, nil, "olivia", "", true) {
		t.Fatal("a durable explicit wake request must override the idle latch")
	}
	// The awake set may re-label an explicitly woken named holder (e.g. to
	// "named-demand"); the durable wake_request still carries the override.
	relabeled := AwakeDecision{ShouldWake: true, Reason: "named-demand"}
	if !wakeDemandOverridesSleepSuppression(relabeled, eval, interactive, nil, "olivia", "", true) {
		t.Fatal("explicit wake must override regardless of the awake-set reason label")
	}
	if wakeDemandOverridesSleepSuppression(explicit, eval, interactive, nil, "olivia", "user-hold", true) {
		t.Fatal("explicit sleep intent should still win over an explicit wake")
	}

	scaled := AwakeDecision{ShouldWake: true, Reason: "scaled:demand"}
	if wakeDemandOverridesSleepSuppression(scaled, eval, interactive, map[string]int{"olivia": 1}, "olivia", "", false) {
		t.Fatal("ordinary interactive pool demand should still honor sleep suppression")
	}
}

// explicitWakePendingInfo bounds the explicit-wake override: a wake request is
// served once the session is up, or once it has slept since the request.
func TestExplicitWakePendingInfo(t *testing.T) {
	const (
		before = "2026-09-22T20:00:00Z"
		after  = "2026-09-22T22:00:00Z"
	)
	for _, tc := range []struct {
		name string
		info sessionpkg.Info
		want bool
	}{
		{name: "no request", info: sessionpkg.Info{MetadataState: "asleep"}, want: false},
		{name: "non-explicit request", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "work"}, want: false},
		{name: "asleep, requested after sleep", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "explicit", WakeRequestedAt: after, SleptAt: before}, want: true},
		{name: "asleep, requested before sleep", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "explicit", WakeRequestedAt: before, SleptAt: after}, want: false},
		{name: "asleep, requested same second as sleep", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "explicit", WakeRequestedAt: after, SleptAt: after}, want: true},
		{name: "asleep, no timestamps", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "explicit"}, want: true},
		{name: "running active", info: sessionpkg.Info{MetadataState: "active", WakeRequest: "explicit", WakeRequestedAt: after, SleptAt: before}, want: false},
		{name: "running awake", info: sessionpkg.Info{MetadataState: "awake", WakeRequest: " explicit ", WakeRequestedAt: after}, want: false},
		{name: "drained", info: sessionpkg.Info{MetadataState: "drained", WakeRequest: "explicit", WakeRequestedAt: after, SleptAt: before}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explicitWakePendingInfo(tc.info); got != tc.want {
				t.Fatalf("explicitWakePendingInfo(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}

// TestWaiterWithWorkWakesWhenItsWaitIsReady: a seat holding claimed work
// runs `gc session wait --sleep` under an enabled interactive sleep policy;
// the drain-ack completes it as a sleep (the assigned work), which keeps the
// wait's intent (D-J 3) and leaves the idle latch. When the wait turns ready
// the next tick wakes it through the latch: the wait's own intent is no
// explicit sleep intent (explicitSleepIntentInfo).
func TestWaiterWithWorkWakesWhenItsWaitIsReady(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace:    config.Workspace{Name: "test-city"},
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "5m", InteractiveFresh: "5m"},
		Agents:       []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	work, _ := env.store.Create(beads.Bead{Title: "claimed", Type: "task"})
	st, asg := "in_progress", session.ID
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &st, Assignee: &asg}); err != nil {
		t.Fatal(err)
	}
	work, _ = env.store.Get(work.ID)
	// gc session wait --sleep: the wait hold, through the tree's own writer.
	if err := setSessionWaitHold(sessionFrontDoor(env.store), session.ID); err != nil {
		t.Fatal(err)
	}
	// The drain-ack completes the drain (the seat keeps assigned work).
	var stderr bytes.Buffer
	finalizeDrainAckStoppedSession(env.cfg, env.store, testSeatWork(env.city, env.cfg, env.store, nil), env.sessionInfo(session.ID), "worker", false, nil, env.dt, env.clk, env.rec, &stderr)
	// A tick persists the sleep policy (the fingerprint), as every tick does.
	info := env.sessionInfo(session.ID)
	policy := resolveSessionSleepPolicyInfo(info, env.cfg, env.sp)
	env.setSessionMetadata(&session, map[string]string{"sleep_policy_fingerprint": policy.Fingerprint})
	row, _ := env.store.Get(session.ID)
	if m := row.Metadata; m["state"] != "asleep" || m["sleep_reason"] != "idle" || m["sleep_intent"] != "wait-hold" || m["wait_hold"] != "true" {
		t.Fatalf("after the drain-ack: state=%q sleep_reason=%q sleep_intent=%q wait_hold=%q, want the idle sleep keeping the wait (D-J 3)", m["state"], m["sleep_reason"], m["sleep_intent"], m["wait_hold"])
	}
	env.clk.Time = env.clk.Now().Add(10 * time.Minute)
	var out, errw bytes.Buffer
	reconcileSessionBeadsAtPath(context.Background(), env.city, []beads.Bead{row}, env.desiredState, configuredSessionNames(env.cfg, "", env.store),
		env.cfg, env.sp, env.store, nil, []beads.Bead{work}, nil, map[string]bool{session.ID: true}, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &out, &errw, env.startOptions...)
	if after, _ := env.store.Get(session.ID); !env.sp.IsRunning("worker") {
		t.Errorf("wait ready: running=false state=%q config_wake_suppressed=%q; want the waiter woken", after.Metadata["state"], after.Metadata["config_wake_suppressed"])
	}
}
