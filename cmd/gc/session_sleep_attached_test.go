package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// An interactive session a human is attached to must not idle-sleep, however
// long ago its pane last changed: the idle window starts at detach
// (docs/reference/exec-session-provider.md, report-attachment). Seen with a
// remote exec runtime pack: an attached interactive session with
// sleep_after_idle 2m was drained "idle" three minutes into the attach.
func TestReconcileSessionBeads_AttachedInteractiveSessionDoesNotIdleSleep(t *testing.T) {
	// No pool demand for the template, as for a manual `gc session new`
	// session.
	assertAttachedInteractiveSessionStaysAwake(t, map[string]int{}, false)
}

// The same holds for a template with pool demand. Pool demand does not keep an
// interactive session awake past its idle window (it honors the window, so a
// pool sibling can take the work), so only the attachment can hold it, and the
// attach probe must run for it.
func TestReconcileSessionBeads_AttachedInteractivePoolSessionDoesNotIdleSleep(t *testing.T) {
	assertAttachedInteractiveSessionStaysAwake(t, map[string]int{"worker": 1}, false)
}

// The same holds once a durable wait on the session turns ready. `gc session
// wait` without --sleep registers the wait with no wait hold, so the attach
// still wakes the session, and the ready wait relabels that wake
// "wait-ready". The attachment must still hold the session awake.
func TestReconcileSessionBeads_AttachedSessionWithReadyWaitDoesNotIdleSleep(t *testing.T) {
	assertAttachedInteractiveSessionStaysAwake(t, map[string]int{}, true)
}

func assertAttachedInteractiveSessionStaysAwake(t *testing.T, poolDesired map[string]int, readyWait bool) {
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
	var readyWaitSet map[string]bool
	if readyWait {
		readyWaitSet = map[string]bool{session.ID: true}
	}

	runAttachedSessionPasses(t, env, session.ID, poolDesired, readyWaitSet)
	assertNoIdleSleep(t, env, session.ID)
}

// An idle verdict the reconciler recorded before the attach
// (sleep_intent=idle-stop-pending, idle drain begun) must not drain the
// session once a terminal is attached: the marker is the reconciler's own
// bookkeeping, not anyone's request to sleep, so the attachment cancels the
// drain and clears it.
func TestReconcileSessionBeads_AttachAfterIdleVerdictCancelsIdleDrain(t *testing.T) {
	assertAttachCancelsIdleDrain(t, &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Agents: []config.Agent{{Name: "worker"}},
	}, map[string]int{})
}

// The same holds for a non-interactive session with pool demand. Pool demand
// keeps it awake over its idle latch but not over the marker, so only the
// attachment can cancel the drain, and the attach probe must run for it.
func TestReconcileSessionBeads_AttachAfterIdleVerdictCancelsNonInteractivePoolIdleDrain(t *testing.T) {
	noAttach := false
	assertAttachCancelsIdleDrain(t, &config.City{
		SessionSleep: config.SessionSleepConfig{
			NonInteractive: "60s",
		},
		Agents: []config.Agent{{Name: "worker", Attach: &noAttach}},
	}, map[string]int{"worker": 1})
}

func assertAttachCancelsIdleDrain(t *testing.T, cfg *config.City, poolDesired map[string]int) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = cfg
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

	runAttachedSessionPasses(t, env, session.ID, poolDesired, nil)
	assertNoIdleSleep(t, env, session.ID)
}

// runAttachedSessionPasses runs three reconcile passes over the session, as
// three controller ticks would, letting any idle probe a pass launched report
// before the next pass reads it.
func runAttachedSessionPasses(t *testing.T, env *reconcilerTestEnv, beadID string, poolDesired map[string]int, readyWaitSet map[string]bool) {
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
			env.store, nil, nil, nil, readyWaitSet, env.dt, poolDesired, false, nil, "",
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
// class and against the sleep intents as explicitSleepIntentInfo reads them
// off the session: an attachment overrides the idle latch and the
// reconciler's own idle-stop-pending marker, padded or not, and loses to any
// other sleep intent.
func TestReconcilerWakeDemandOverridesSleepSuppressionForAttached(t *testing.T) {
	attached := AwakeDecision{ShouldWake: true, Reason: "attached", Attached: true}
	attachedEval := wakeEvaluation{Reasons: []WakeReason{WakeAttached}}
	// A ready wait relabels an attached wake "wait-ready" and keeps the
	// attach on the decision; awakeSetToWakeEvals then reports only WakeWait.
	waitReady := AwakeDecision{ShouldWake: true, Reason: "wait-ready", Attached: true}
	waitEval := wakeEvaluation{Reasons: []WakeReason{WakeWait}}
	unattached := AwakeDecision{ShouldWake: true, Reason: "scaled:demand"}
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
			{"attached, relabeled by a ready wait", waitReady, waitEval, "", true},
			{"ready wait without attach", AwakeDecision{ShouldWake: true, Reason: "wait-ready"}, waitEval, "", false},
			{"attached over idle-stop-pending", attached, attachedEval, sleepIntentIdle, true},
			{"relabeled attach over idle-stop-pending", waitReady, waitEval, sleepIntentIdle, true},
			{"attached over padded idle-stop-pending", attached, attachedEval, " " + sleepIntentIdle + "\n", true},
			{"relabeled attach over padded idle-stop-pending", waitReady, waitEval, " " + sleepIntentIdle + "\n", true},
			{"user-hold wins over attached", attached, attachedEval, "user-hold", false},
			{"user-hold wins over relabeled attach", waitReady, waitEval, "user-hold", false},
			{"explicit sleep intent wins over attached", attached, attachedEval, "drain", false},
			{"idle-stop-pending without attach", unattached, wakeEvaluation{}, sleepIntentIdle, false},
		} {
			intent := explicitSleepIntentInfo(sessionpkg.Info{SleepIntent: tc.intent})
			got := wakeDemandOverridesSleepSuppression(tc.decision, tc.eval, policy, nil, "worker", intent, false)
			if got != tc.want {
				t.Errorf("%s/%s: override = %v, want %v", class, tc.name, got, tc.want)
			}
		}
	}
}

// With pool demand, the attach probe runs only where the attachment can
// change the decision: an interactive template whose effective sleep policy
// can idle-sleep, and any row carrying the reconciler's idle-stop-pending
// marker, which outranks pool demand and yields only to an attachment. A
// non-interactive template, a policy that is off, and an interactive
// template on a runtime without full sleep capability (its effective policy
// is off) otherwise keep the desired fast path free of attach probes.
func TestShouldProbeAttachmentForAwakeInputWithPoolDemand(t *testing.T) {
	noAttach := false
	poolDesired := map[string]int{"worker": 1}
	for _, tc := range []struct {
		name        string
		agent       config.Agent
		capability  runtime.SessionSleepCapability
		sleepIntent string
		want        bool
	}{
		{"interactive, full capability", config.Agent{Name: "worker"}, runtime.SessionSleepCapabilityFull, "", true},
		{"interactive, timed_only capability", config.Agent{Name: "worker"}, runtime.SessionSleepCapabilityTimedOnly, "", false},
		{"interactive, policy off", config.Agent{Name: "worker", SleepAfterIdle: config.SessionSleepOff}, runtime.SessionSleepCapabilityFull, "", false},
		{"non-interactive", config.Agent{Name: "worker", Attach: &noAttach}, runtime.SessionSleepCapabilityFull, "", false},
		{"non-interactive, idle-stop-pending", config.Agent{Name: "worker", Attach: &noAttach}, runtime.SessionSleepCapabilityFull, sleepIntentIdle, true},
		{"non-interactive, user-hold", config.Agent{Name: "worker", Attach: &noAttach}, runtime.SessionSleepCapabilityFull, "user-hold", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := sessionpkg.Info{MetadataState: "active", Template: "worker", SessionNameMetadata: "worker", SleepIntent: tc.sleepIntent}
			cfg := &config.City{
				SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s", NonInteractive: "60s"},
				Agents:       []config.Agent{tc.agent},
			}
			sp := runtime.NewFake()
			sp.SleepCapabilityValue = tc.capability
			if got := shouldProbeAttachmentForAwakeInput(info, true, cfg, sp, poolDesired); got != tc.want {
				t.Fatalf("shouldProbeAttachmentForAwakeInput = %v, want %v", got, tc.want)
			}
			// Without pool demand an active, never-detached session is probed
			// whatever its policy (the probe can still change the decision).
			if !shouldProbeAttachmentForAwakeInput(info, true, cfg, sp, nil) {
				t.Fatal("shouldProbeAttachmentForAwakeInput without pool demand = false, want true")
			}
		})
	}
}
