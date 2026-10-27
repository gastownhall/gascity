package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// These tests pin the live-incarnation hold of a pending-create rollback
// (#2081): a live runtime under the row's name that carries the row's own
// GC_SESSION_ID, another GC_INSTANCE_TOKEN and a GC_RUNTIME_EPOCH other than
// the row's generation is an earlier or later incarnation of the same
// session. The rollback holds the row while it runs: nothing is stopped, the
// row keeps its pending_create_claim, and its work stays assigned.

// liveIncarnationKind is the standing condition the hold reports.
const liveIncarnationKind = "live_incarnation.pending_create_rollback"

// stampLiveIncarnation seeds the restarted pending create of class at
// generation 2 and stamps the box under its name as another incarnation of
// the same session at epoch.
func stampLiveIncarnation(t *testing.T, class teardownRowClass, epoch string) (*reconcilerTestEnv, beads.Bead, string) {
	t.Helper()
	env, session, name := newRestartedPendingCreate(t, class)
	env.setSessionMetadata(&session, map[string]string{"generation": "2", "instance_token": "tok-row"})
	for key, value := range map[string]string{"GC_SESSION_ID": session.ID, "GC_INSTANCE_TOKEN": "tok-other", "GC_RUNTIME_EPOCH": epoch} {
		if err := env.sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
	return env, session, name
}

// assignLiveIncarnationWork creates an in_progress work bead routed to the worker pool and
// assigned to the session name.
func assignLiveIncarnationWork(t *testing.T, store beads.Store, assignee string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "routed work",
		Assignee: assignee,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	return work
}

// releaseOrphanedWork runs the orphaned pool-work release over the env's open
// session rows, as the controller does after a tick.
func releaseOrphanedWork(t *testing.T, env *reconcilerTestEnv, sessionID string, work beads.Bead) beads.Bead {
	t.Helper()
	var open []sessionpkg.Info
	if row, err := env.store.Get(sessionID); err != nil {
		t.Fatal(err)
	} else if row.Status != "closed" {
		open = sessionInfosFromBeads([]beads.Bead{row})
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
	releaseOrphanedPoolAssignments(env.store, beads.SessionStore{Store: env.store}, cfg, "", open, []beads.Bead{work}, nil, nil, nil, nil, nil, nil)
	got, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// liveIncarnationDeferrals counts the tick's Deferred pending-create rollback
// records attributed to a live incarnation.
func liveIncarnationDeferrals(trace *sessionReconcilerTraceCycle) int {
	n := 0
	for _, r := range trace.records {
		if r.SiteCode == TraceSiteReconcilerPendingCreate && r.ReasonCode == TraceReasonPendingCreateRollback &&
			r.OutcomeCode == TraceOutcomeDeferred && r.Fields["attribution"] == "live_incarnation" {
			n++
		}
	}
	return n
}

// reconcileLiveIncarnationTick runs one traced legacy reconciler tick over the
// row on sp.
func reconcileLiveIncarnationTick(t *testing.T, env *reconcilerTestEnv, sp runtime.Provider, id string, trace *sessionReconcilerTraceCycle) {
	t.Helper()
	cur, err := env.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsTraced(
		context.Background(), env.city, []beads.Bead{cur}, env.desiredState, cfgNames, env.cfg, sp,
		env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, trace,
		env.startOptions...,
	)
}

// assertLiveIncarnationHeld ticks the legacy reconciler twice over the row and
// checks the hold on each: the box runs unstopped, the row stays open with its
// claim, one Deferred trace per tick, and the standing condition said once.
func assertLiveIncarnationHeld(t *testing.T, env *reconcilerTestEnv, sp runtime.Provider, session beads.Bead, name string) {
	t.Helper()
	for tick := 1; tick <= 2; tick++ {
		trace := newPoolDesiredStateTestTrace("worker")
		reconcileLiveIncarnationTick(t, env, sp, session.ID, trace)
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "closed" || got.Metadata["pending_create_claim"] != "true" || got.Metadata["state"] == string(sessionpkg.StateFailedCreate) {
			t.Fatalf("tick %d: status=%q state=%q claim=%q, want the pending create held open while its live incarnation runs; stderr=%s",
				tick, got.Status, got.Metadata["state"], got.Metadata["pending_create_claim"], env.stderr.String())
		}
		if !env.sp.IsRunning(name) || countCalls(env.sp, "Stop") != 0 {
			t.Fatalf("tick %d: running=%v stops=%d, want the live incarnation left running", tick, env.sp.IsRunning(name), countCalls(env.sp, "Stop"))
		}
		if n := liveIncarnationDeferrals(trace); n != 1 {
			t.Fatalf("tick %d: live_incarnation deferral records = %d, want 1; records=%+v", tick, n, trace.records)
		}
	}
	if _, ok := env.dt.drainSkips[drainSkipKey{beadID: session.ID, kind: liveIncarnationKind}]; !ok {
		t.Fatalf("standing condition %q not recorded for %s", liveIncarnationKind, session.ID)
	}
	if n := strings.Count(env.stderr.String(), "(live_incarnation)"); n != 1 {
		t.Fatalf("live_incarnation lines = %d, want 1 (transition only); stderr=%s", n, env.stderr.String())
	}
}

// TestPendingCreateRollback_HoldsPoolRowsLiveIncarnation is AC-1: a
// bead-scoped pool row's expired pending create whose name holds a live
// earlier (epoch 1) or later (epoch 3) incarnation of the same session is
// held, and its in_progress work keeps its assignee.
func TestPendingCreateRollback_HoldsPoolRowsLiveIncarnation(t *testing.T) {
	for _, epoch := range []string{"1", "3"} {
		t.Run("epoch "+epoch, func(t *testing.T) {
			env, session, name := stampLiveIncarnation(t, rowPool, epoch)
			work := assignLiveIncarnationWork(t, env.store, name)

			assertLiveIncarnationHeld(t, env, env.sp, session, name)

			got := releaseOrphanedWork(t, env, session.ID, work)
			if got.Status != "in_progress" || got.Assignee != name {
				t.Fatalf("work status=%q assignee=%q, want in_progress assigned to %q", got.Status, got.Assignee, name)
			}
		})
	}
}

// TestPendingCreateRollback_HoldsNamedAndManualRowsLiveIncarnation is AC-2: a
// configured named or a manual row's expired pending create, whose name holds
// a live incarnation of the same session on a runtime that stamps identity,
// is not closed failed-create while that runtime keeps running.
func TestPendingCreateRollback_HoldsNamedAndManualRowsLiveIncarnation(t *testing.T) {
	for _, class := range []teardownRowClass{rowNamed, rowManual} {
		for _, epoch := range []string{"1", "3"} {
			t.Run(string(class)+"/epoch "+epoch, func(t *testing.T) {
				env, session, name := stampLiveIncarnation(t, class, epoch)
				sp := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: env.sp})

				assertLiveIncarnationHeld(t, env, sp, session, name)
			})
		}
	}
}
