package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// An interactive session a human is attached to must not idle-sleep, however
// long ago its pane last changed: the idle window starts at detach
// (docs/reference/exec-session-provider.md, report-attachment). Seen with a
// remote exec runtime pack: an attached interactive session with
// sleep_after_idle 2m was drained "idle" three minutes into the attach.
func TestReconcileSessionBeads_AttachedInteractiveSessionDoesNotIdleSleep(t *testing.T) {
	// No pool demand for the template, as for a manual `gc session new`
	// session.
	assertAttachedInteractiveSessionStaysAwake(t, map[string]int{})
}

// The same holds for a template with pool demand. Pool demand does not keep an
// interactive session awake past its idle window (it honors the window, so a
// pool sibling can take the work), so only the attachment can hold it, and the
// attach probe must run for it.
func TestReconcileSessionBeads_AttachedInteractivePoolSessionDoesNotIdleSleep(t *testing.T) {
	assertAttachedInteractiveSessionStaysAwake(t, map[string]int{"worker": 1})
}

func assertAttachedInteractiveSessionStaysAwake(t *testing.T, poolDesired map[string]int) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{"last_woke_at": ts})
	env.sp.SetAttached("worker", true)
	env.sp.SetActivity("worker", env.clk.Time.Add(-2*time.Minute))
	env.sp.WaitForIdleErrors["worker"] = nil

	runAttachedSessionPasses(t, env, session.ID, poolDesired)
	assertNoIdleSleep(t, env, session.ID)
}

// An idle verdict the reconciler recorded before the attach
// (sleep_intent=idle-stop-pending, idle drain begun) must not drain the
// session once a terminal is attached: the marker is the reconciler's own
// bookkeeping, not anyone's request to sleep, so the attachment cancels the
// drain and clears it.
func TestReconcileSessionBeads_AttachAfterIdleVerdictCancelsIdleDrain(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	ts := env.clk.Time.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadataBatch(session.ID, map[string]string{
		"last_woke_at": ts,
		"sleep_intent": sleepIntentIdle,
	})
	if !beginSessionDrainInfo(env.sessionInfo(session.ID), env.sp, env.dt, "idle", env.clk, defaultDrainTimeout) {
		t.Fatal("could not begin the idle drain the attach must cancel")
	}
	env.sp.SetAttached("worker", true)
	env.sp.SetActivity("worker", env.clk.Time.Add(-2*time.Minute))
	env.sp.WaitForIdleErrors["worker"] = nil

	runAttachedSessionPasses(t, env, session.ID, map[string]int{})
	assertNoIdleSleep(t, env, session.ID)
}

// runAttachedSessionPasses runs three reconcile passes over the session, as
// three controller ticks would, letting any idle probe a pass launched report
// before the next pass reads it.
func runAttachedSessionPasses(t *testing.T, env *reconcilerTestEnv, beadID string, poolDesired map[string]int) {
	t.Helper()
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	for i := 0; i < 3; i++ {
		fresh, err := env.store.Get(beadID)
		if err != nil {
			t.Fatalf("store.Get(session): %v", err)
		}
		// Gate the probe so it cannot report inside the pass that launched
		// it (see waitForIdleProbeReady).
		gate := make(chan struct{})
		env.sp.WaitForIdleGates["worker"] = gate
		reconcileSessionBeadsAtPath(
			context.Background(), env.city, []beads.Bead{fresh}, env.desiredState, cfgNames, env.cfg, env.sp,
			env.store, nil, nil, nil, nil, env.dt, poolDesired, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
			withAsyncDrainAckStopTracker(env.stops),
		)
		close(gate)
		if _, ok := env.dt.idleProbe(beadID); ok {
			waitForIdleProbeReady(t, env.dt, beadID)
		}
	}
}

func assertNoIdleSleep(t *testing.T, env *reconcilerTestEnv, beadID string) {
	t.Helper()
	if ds := env.dt.get(beadID); ds != nil {
		t.Fatalf("attached session has a %q drain; an attached session must stay awake (stdout: %s)", ds.reason, env.stdout.String())
	}
	got, err := env.store.Get(beadID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if intent := got.Metadata["sleep_intent"]; intent != "" {
		t.Fatalf("sleep_intent = %q on an attached session, want none", intent)
	}
}

// The attached rule of wakeDemandOverridesSleepSuppression, for every sleep
// class and against the sleep intents: an attachment overrides the idle
// latch and the reconciler's own idle-stop-pending marker, and loses to any
// other sleep intent.
func TestReconcilerWakeDemandOverridesSleepSuppressionForAttached(t *testing.T) {
	attached := AwakeDecision{ShouldWake: true, Reason: "attached"}
	attachedEval := wakeEvaluation{Reasons: []WakeReason{WakeAttached}}
	// The awake set may label the wake otherwise while attachment is one of
	// its reasons.
	relabeled := AwakeDecision{ShouldWake: true, Reason: "scaled:demand"}
	for _, class := range []config.SessionSleepClass{
		config.SessionSleepInteractiveResume,
		config.SessionSleepInteractiveFresh,
		config.SessionSleepNonInteractive,
	} {
		policy := resolvedSessionSleepPolicy{Class: class}
		for _, tc := range []struct {
			name     string
			decision AwakeDecision
			eval     wakeEvaluation
			intent   string
			want     bool
		}{
			{"attached", attached, attachedEval, "", true},
			{"attached reason only", attached, wakeEvaluation{}, "", true},
			{"attached among reasons", relabeled, attachedEval, "", true},
			{"attached over idle-stop-pending", attached, attachedEval, sleepIntentIdle, true},
			{"user-hold wins over attached", attached, attachedEval, "user-hold", false},
			{"explicit sleep intent wins over attached", attached, attachedEval, "drain", false},
			{"idle-stop-pending without attach", relabeled, wakeEvaluation{}, sleepIntentIdle, false},
		} {
			got := wakeDemandOverridesSleepSuppression(tc.decision, tc.eval, policy, nil, "worker", tc.intent, false)
			if got != tc.want {
				t.Errorf("%s/%s: override = %v, want %v", class, tc.name, got, tc.want)
			}
		}
	}
}
