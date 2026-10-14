package main

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// abandonedCommitRepairTick abandons the commit of an async start under the
// production lease on a wedged store, as the controller does when Dolt stalls,
// then advances time by advance and runs one reconciler tick, as a restarted
// controller would, over the row the abandoned commit left behind. While the
// commit is hung on the lease the reconciler defers to it
// (TestRecoveryCommitTakesTheRuntimeLease); the repair has only what the
// abandon path left on the row and on the runtime once that holder is gone. It returns the row after the tick and a release for the wedge.
func abandonedCommitRepairTick(t *testing.T, testCity, workDir string, advance time.Duration, afterAbandon func(sp *postStartProvider)) (beads.Bead, *postStartProvider, *reconcilerTestEnv, func()) {
	t.Helper()
	clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	inner, bead, item := noDriftBeadItem(t, clk, workDir)
	store := newWedgedStore(inner)
	store.snapshotID = bead.ID
	sp := newPostStartProvider()
	cleanup := func() {
		store.release()
		sp.release()
	}
	begin := time.Now()

	run := enqueueCommitPhaseStartShaped(context.Background(), t, testCity, item, sp, store, clk, immediateStartStabilityWaiter, func(t *testing.T, city string, item preparedStart) (preparedStart, func(), func()) {
		item, dropLease, cleanup := productionLease(store)(t, city, item)
		store.arm()
		return item, dropLease, cleanup
	})
	run.awaitFinished(t, sp, store, begin)
	requireBeadUnchanged(t, inner, store.atArm())
	if afterAbandon != nil {
		afterAbandon(sp)
	}

	env := newReconcilerTestEnv(t)
	env.store = inner
	env.clk = clk
	// The wedged commit is still hung on the old controller's lease, whose
	// flock a restarted controller does not inherit: the repair runs in a fresh
	// city directory over the same store, with the lease record the abandoned
	// start left on the row run out.
	env.city = t.TempDir()
	time.Sleep(advance)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	clk.Advance(advance)
	trace := newPoolDesiredStateTestTrace("worker")
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsTraced(
		context.Background(), env.city, []beads.Bead{mustGetBead(t, inner, bead.ID)}, env.desiredState, cfgNames, env.cfg, sp,
		env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, trace,
		env.startOptions...,
	)
	return mustGetBead(t, inner, bead.ID), sp, env, cleanup
}

// The abandon path leaves the row a pending create (SESSION-RUNTIME-017), so
// the repair that already exists for a pending create whose start went quiet
// has to take it from there. With the runtime alive and the start's in-flight
// window over, the reconciler commits the start itself and leaves the runtime
// running.
func TestAbandonedCommitIsRepairedByTheReconcilerWhileTheRuntimeIsAlive(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		got, sp, env, cleanup := abandonedCommitRepairTick(t, testCity, workDir, 10*time.Minute, nil)
		defer cleanup()
		if got.Status == "closed" || got.Metadata["pending_create_claim"] != "" || got.Metadata["state"] != "active" {
			t.Fatalf("status=%q state=%q pending_create_claim=%q, want the abandoned start committed by the reconciler; stderr=%s",
				got.Status, got.Metadata["state"], got.Metadata["pending_create_claim"], env.stderr.String())
		}
		if !sp.IsRunning("worker") || sp.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("runtime running=%v stops=%d, want the healthy runtime left running", sp.IsRunning("worker"), sp.CountCalls("Stop", "worker"))
		}
	})
}

// With the runtime dead too, the same abandoned row is rolled back once its
// pending-create lease has run out, instead of staying a pending create
// forever.
func TestAbandonedCommitIsRolledBackByTheReconcilerWhenTheRuntimeIsDead(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		got, sp, env, cleanup := abandonedCommitRepairTick(t, testCity, workDir, 2*time.Hour, func(sp *postStartProvider) {
			_ = sp.Fake.Stop("worker")
		})
		defer cleanup()
		if sp.IsRunning("worker") {
			t.Fatalf("runtime running, want it dead")
		}
		if got.Status != "closed" || got.Metadata["pending_create_claim"] != "" {
			t.Fatalf("status=%q pending_create_claim=%q state=%q, want the dead abandoned start rolled back; stderr=%s",
				got.Status, got.Metadata["pending_create_claim"], got.Metadata["state"], env.stderr.String())
		}
	})
}
