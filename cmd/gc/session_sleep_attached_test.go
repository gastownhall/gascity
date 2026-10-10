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
	session.Metadata["last_woke_at"] = ts
	env.sp.SetAttached("worker", true)
	env.sp.SetActivity("worker", env.clk.Time.Add(-2*time.Minute))
	env.sp.WaitForIdleErrors["worker"] = nil

	// No pool demand for the template: the attach probe runs (a template with
	// pool demand skips it, shouldProbeAttachmentForAwakeInput), as for a
	// manual `gc session new` session.
	poolDesired := map[string]int{}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	for i := 0; i < 3; i++ {
		fresh, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("store.Get(session): %v", err)
		}
		// Gate the idle probe so it cannot report inside the pass that
		// launched it (see waitForIdleProbeReady).
		gate := make(chan struct{})
		env.sp.WaitForIdleGates["worker"] = gate
		reconcileSessionBeadsAtPath(
			context.Background(), env.city, []beads.Bead{fresh}, env.desiredState, cfgNames, env.cfg, env.sp,
			env.store, nil, nil, nil, nil, env.dt, poolDesired, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
			withAsyncDrainAckStopTracker(env.stops),
		)
		close(gate)
		// Let an idle probe this pass launched report before the next pass
		// reads it, as a later controller tick would.
		if _, ok := env.dt.idleProbe(session.ID); ok {
			waitForIdleProbeReady(t, env.dt, session.ID)
		}
	}

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("attached session began a %q drain; an attached session must stay awake", ds.reason)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if intent := got.Metadata["sleep_intent"]; intent != "" {
		t.Fatalf("sleep_intent = %q on an attached session, want none", intent)
	}
}
