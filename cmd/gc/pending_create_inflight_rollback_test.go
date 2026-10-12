package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
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

// inFlightDeferrals counts the start-in-flight deferrals of the alive
// pending-create rollback in trace.
func inFlightDeferrals(trace *sessionReconcilerTraceCycle) int {
	n := 0
	for _, r := range trace.records {
		if r.SiteCode == TraceSiteReconcilerPendingCreate && r.ReasonCode == TraceReasonWake &&
			r.OutcomeCode == TraceOutcomeStartInFlight && r.Fields["attribution"] == "start_in_flight" {
			n++
		}
	}
	return n
}

// runSlowStart starts a pending create of "worker" on a runtimeFirstStartProvider
// over store, then runs a second reconciler tick advance later, while the
// start is still creating the box, and releases the start once that tick
// returns or blocks. It returns the env, the provider, the row and the second
// tick's trace.
func runSlowStart(t *testing.T, store beads.Store, advance time.Duration) (*reconcilerTestEnv, *runtimeFirstStartProvider, beads.Bead, *sessionReconcilerTraceCycle) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	if store != nil {
		env.store = store
	}
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

	tick := func(trace *sessionReconcilerTraceCycle) {
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Error(err)
			return
		}
		cfgNames := configuredSessionNames(env.cfg, "", env.store)
		reconcileSessionBeadsTraced(
			context.Background(), env.city, []beads.Bead{got}, env.desiredState, cfgNames, env.cfg, sp,
			env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, trace,
			env.startOptions...,
		)
	}

	tick(nil)
	select {
	case <-sp.created:
	case <-time.After(5 * time.Second):
		t.Fatalf("the start never created the box; stderr=%s", env.stderr.String())
	}
	env.clk.Advance(advance)
	// The next tick lands while the start is still creating the box. On a
	// store without the atomic close a rollback waits on the start's session
	// lock, so the start is released once the tick returns or blocks.
	trace := newPoolDesiredStateTestTrace("worker")
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		tick(trace)
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
	return env, sp, session, trace
}

// TestReconcileSessionBeads_InFlightStartOfUnstampedRuntimeIsNotRolledBack
// pins the AWS-cell orphan: a reconciler tick that lands while the row's own
// Start is still creating the box sees a live runtime with no identity. The
// rollback closed the row without stopping the box ("live runtime belongs to
// another session"), the late Start then found the row closed and could not
// attribute the box either, so the box ran with no row until the cell's reaper
// removed it. The in-flight start owns that window: the row stays pending and
// the start commits it. It runs on MemStore and on FileStore, the AWS cell's
// store, whose atomic close lands the rollback mid-start at once. The second
// tick lands inside the in-flight window and, ten minutes in, past it, where
// the start's live runtime lease alone holds the fence.
func TestReconcileSessionBeads_InFlightStartOfUnstampedRuntimeIsNotRolledBack(t *testing.T) {
	for _, store := range []string{"mem", "file"} {
		for _, advance := range []time.Duration{2 * time.Second, 10 * time.Minute} {
			t.Run(store+"/"+advance.String(), func(t *testing.T) {
				var st beads.Store
				if store == "file" {
					fs, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
					if err != nil {
						t.Fatal(err)
					}
					st = fs
				}
				env, sp, session, trace := runSlowStart(t, st, advance)
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
				if n := inFlightDeferrals(trace); n != 1 {
					t.Fatalf("start-in-flight deferrals = %d, want 1; stderr=%s", n, env.stderr.String())
				}
			})
		}
	}
}

// TestReconcileSessionBeads_AlivePendingCreateRollbackInFlightFence pins the
// fence's edges with no start holding the runtime lease: only a runtime with
// no identity at all is spared, and only while the row's own start is inside
// its in-flight window (startup_timeout + slack, the configured timeout
// included). Positive evidence of another owner still rolls back at once, an
// unstamped runtime rolls back once the window ends, and a last_woke_at far
// in the future (a clock stepped back) holds nothing.
func TestReconcileSessionBeads_AlivePendingCreateRollbackInFlightFence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		startupTimeout string
		wokeAgo        time.Duration
		meta           map[string]string
		wantRollback   bool
	}{
		{name: "unstamped, start in flight", wokeAgo: 2 * time.Second, wantRollback: false},
		{name: "unstamped, in-flight window over", wokeAgo: 30 * time.Minute, wantRollback: true},
		{name: "unstamped, 120s into a 180s startup timeout", startupTimeout: "180s", wokeAgo: 120 * time.Second, wantRollback: false},
		{name: "unstamped, 200s past a 180s startup timeout", startupTimeout: "180s", wokeAgo: 200 * time.Second, wantRollback: true},
		{name: "unstamped, woken a minute in the future", wokeAgo: -time.Minute, wantRollback: false},
		{name: "unstamped, woken six hours in the future", wokeAgo: -6 * time.Hour, wantRollback: true},
		{name: "another session's ID, start in flight", wokeAgo: 2 * time.Second, meta: map[string]string{"GC_SESSION_ID": "gc-other"}, wantRollback: true},
		{name: "another incarnation's token, start in flight", wokeAgo: 2 * time.Second, meta: map[string]string{"GC_INSTANCE_TOKEN": "other-token"}, wantRollback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session := newAlivePendingCreateEnv(t)
			env.cfg.Session.StartupTimeout = tc.startupTimeout
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
			if want := map[bool]int{true: 0, false: 1}[tc.wantRollback]; inFlightDeferrals(trace) != want {
				t.Fatalf("start_in_flight deferrals = %d, want %d", inFlightDeferrals(trace), want)
			}
			for _, r := range trace.records {
				if shouldAutoArmForTrace(r.ReasonCode, r.OutcomeCode) && !tc.wantRollback {
					t.Fatalf("a deferral armed auto-trace: %+v", r)
				}
			}
			if !env.sp.IsRunning("worker") {
				t.Fatal("the rollback stopped the runtime; a foreign or deferred runtime is never stopped here")
			}
		})
	}
}
