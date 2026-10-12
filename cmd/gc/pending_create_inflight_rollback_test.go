package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// runtimeFirstStartProvider is a slow remote runtime (the crucible exec
// runtime on EKS): Start creates the box under the session name at once, so
// IsRunning reports it, but returns only once the box is ready. It keeps no
// per-session metadata, so GC_SESSION_ID and GC_INSTANCE_TOKEN read unset, and
// it ignores cancellation once the create is accepted.
type runtimeFirstStartProvider struct {
	*runtime.Fake
	created chan string
	release chan struct{}
}

func (p *runtimeFirstStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.created <- name
	<-p.release
	return nil
}

// TestReconcileSessionBeads_InFlightStartOfUnstampedRuntimeIsNotRolledBack
// pins the AWS-cell orphan: a reconciler tick that lands while the row's own
// Start is still creating the box sees a live runtime with no identity. The
// rollback closed the row without stopping the box ("live runtime belongs to
// another session"), the late Start then found the row closed and could not
// attribute the box either, so the box ran with no row until the cell's reaper
// removed it. The in-flight start owns that window: the row stays pending and
// the start commits it.
func TestReconcileSessionBeads_InFlightStartOfUnstampedRuntimeIsNotRolledBack(t *testing.T) {
	env := newReconcilerTestEnv(t)
	sp := &runtimeFirstStartProvider{Fake: env.sp, created: make(chan string, 1), release: make(chan struct{})}
	released := false
	releaseStart := func() {
		if !released {
			released = true
			close(sp.release)
		}
	}
	starts := &asyncStartTracker{}
	t.Cleanup(func() {
		releaseStart()
		starts.wait(5 * time.Second)
	})
	env.startOptions = append(env.startOptions, withAsyncStartExecution(), withAsyncStartTracker(starts))
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                     "creating",
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now()),
	})

	tick := func() {
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Error(err)
			return
		}
		cfgNames := configuredSessionNames(env.cfg, "", env.store)
		reconcileSessionBeadsTraced(
			context.Background(), env.city, []beads.Bead{got}, env.desiredState, cfgNames, env.cfg, sp,
			env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, nil,
			env.startOptions...,
		)
	}

	tick()
	select {
	case <-sp.created:
	case <-time.After(5 * time.Second):
		t.Fatalf("the start never created the box; stderr=%s", env.stderr.String())
	}
	env.clk.Advance(2 * time.Second)
	// The next tick lands while the start is still creating the box. A
	// rollback waits on the start's session lock, so the start is released
	// once the tick returns or blocks.
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		tick()
	}()
	select {
	case <-ticked:
	case <-time.After(time.Second):
	}
	releaseStart()
	select {
	case <-ticked:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick did not finish once the start returned")
	}
	if !starts.wait(5 * time.Second) {
		t.Fatal("async start did not finish")
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "closed" && sp.IsRunning("worker") {
		t.Fatalf("orphan: the row closed (%s) with its runtime still running; stderr=%s", got.Metadata["close_reason"], env.stderr.String())
	}
	if got.Status == "closed" || got.Metadata["pending_create_claim"] != "" || got.Metadata["state"] != "active" {
		t.Fatalf("status=%q claim=%q state=%q, want the start committed; stderr=%s",
			got.Status, got.Metadata["pending_create_claim"], got.Metadata["state"], env.stderr.String())
	}
	if !sp.IsRunning("worker") {
		t.Fatal("the committed session's runtime is gone")
	}
}

// TestReconcileSessionBeads_AlivePendingCreateRollbackInFlightFence pins the
// fence's edges: only a runtime with no identity at all is spared, and only
// while the row's own start is in flight. Positive evidence of another owner
// still rolls back at once, and an unstamped runtime rolls back once the
// in-flight window ends.
func TestReconcileSessionBeads_AlivePendingCreateRollbackInFlightFence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wokeAgo      time.Duration
		meta         map[string]string
		wantRollback bool
	}{
		{name: "unstamped, start in flight", wokeAgo: 2 * time.Second, wantRollback: false},
		{name: "unstamped, in-flight window over", wokeAgo: 30 * time.Minute, wantRollback: true},
		{name: "another session's ID, start in flight", wokeAgo: 2 * time.Second, meta: map[string]string{"GC_SESSION_ID": "gc-other"}, wantRollback: true},
		{name: "another incarnation's token, start in flight", wokeAgo: 2 * time.Second, meta: map[string]string{"GC_INSTANCE_TOKEN": "other-token"}, wantRollback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session := newAlivePendingCreateEnv(t)
			env.setSessionMetadata(&session, map[string]string{
				"last_woke_at": env.clk.Now().Add(-tc.wokeAgo).UTC().Format(time.RFC3339),
			})
			for k, v := range tc.meta {
				_ = env.sp.SetMeta("worker", k, v)
			}
			trace := newPoolDesiredStateTestTrace("worker")
			reconcileTraced(env, []beads.Bead{session}, trace)
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if rolledBack := got.Status == "closed"; rolledBack != tc.wantRollback {
				t.Fatalf("rolled back = %v, want %v; stderr=%s", rolledBack, tc.wantRollback, env.stderr.String())
			}
			deferred := 0
			for _, r := range trace.records {
				if r.SiteCode == TraceSiteReconcilerPendingCreate && r.ReasonCode == TraceReasonPendingCreateRollback &&
					r.OutcomeCode == TraceOutcomeDeferred && r.Fields["attribution"] == "start_in_flight" {
					deferred++
				}
			}
			if want := map[bool]int{true: 0, false: 1}[tc.wantRollback]; deferred != want {
				t.Fatalf("start_in_flight deferrals = %d, want %d", deferred, want)
			}
			if !env.sp.IsRunning("worker") {
				t.Fatal("the rollback stopped the runtime; a foreign or deferred runtime is never stopped here")
			}
		})
	}
}
