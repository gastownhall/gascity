package main

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
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
	got, sp, env, _, cleanup := abandonedCommitRepairScenario(t, testCity, workDir, advance, abandonShape{restart: true}, afterAbandon)
	return got, sp, env, cleanup
}

// abandonShape selects what the abandon leaves behind and who repairs it.
type abandonShape struct {
	// restart runs the repair tick in a fresh city directory, as a restarted
	// controller would: it does not inherit the flock of a commit that is still
	// hung. False runs the tick in the city the commit is hung in.
	restart bool
	// refused abandons the commit by refusing it (an observation of the session
	// is outstanding), so it never starts and the waiter abandons its lease.
	refused bool
}

// abandonedCommitRepairScenario is abandonedCommitRepairTick for the matrix of
// RC3 (ga-hlod480): it also returns the unwedged inner store and runs the tick
// in the city the shape names.
func abandonedCommitRepairScenario(t *testing.T, testCity, workDir string, advance time.Duration, shape abandonShape, afterAbandon func(sp *postStartProvider)) (beads.Bead, *postStartProvider, *reconcilerTestEnv, *wedgedStore, func()) {
	t.Helper()
	clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	inner, bead, item := noDriftBeadItem(t, clk, workDir)
	store := newWedgedStore(inner)
	store.snapshotID = bead.ID
	sp := newPostStartProvider()
	wedgedOut := make(chan struct{})
	cleanup := func() {
		close(wedgedOut)
		store.release()
		sp.release()
	}
	waiter := immediateStartStabilityWaiter
	if shape.refused {
		waiter = startStabilityWaiter(func(ctx context.Context, name string) bool {
			wctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = observeSessionBounded(wctx, testCity, "worker", func() (int, error) { <-wedgedOut; return 0, nil })
			return immediateStartStabilityWaiter(ctx, name)
		})
	}
	begin := time.Now()

	run := enqueueCommitPhaseStartShaped(context.Background(), t, testCity, item, sp, store, clk, waiter, func(t *testing.T, city string, item preparedStart) (preparedStart, func()) {
		item, cleanup := productionLease(store)(t, city, item)
		store.arm()
		return item, cleanup
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
	env.city = testCity
	if shape.restart {
		env.city = t.TempDir()
	}
	<-time.After(advance) // virtual time: the bubble's clock runs the lease record out
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
	return mustGetBead(t, inner, bead.ID), sp, env, store, cleanup
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

// The RC3 matrix (ga-hlod480): what the repair paths see for each combination of
// runtime, commit closure and lease the bounded commit can leave. M2 (the
// commit returned and committed, the lease dropped) has nothing to repair and is
// TestLateHealthyCommitKeepsItsLeaseAndTheWaiterTouchesNothing.

// M1: runtime alive, the commit closure still hung, its lease retained. A
// reconcile tick in the city the commit is hung in defers to it: the row is the
// pending create the abandon left, and the runtime is untouched. When the store
// answers the commit lands and the start is committed.
func TestRepairMatrixAliveHungCommitKeepsTheLeaseAndTheTickDefers(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		got, sp, env, store, cleanup := abandonedCommitRepairScenario(t, testCity, workDir, 10*time.Minute, abandonShape{}, nil)
		defer cleanup()
		if got.Status == "closed" || got.Metadata["pending_create_claim"] == "" || got.Metadata["state"] == "active" {
			t.Fatalf("status=%q state=%q pending_create_claim=%q, want the pending create left alone while the commit holds the lease; stderr=%s",
				got.Status, got.Metadata["state"], got.Metadata["pending_create_claim"], env.stderr.String())
		}
		if !sp.IsRunning("worker") || sp.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("runtime running=%v stops=%d, want it untouched", sp.IsRunning("worker"), sp.CountCalls("Stop", "worker"))
		}
		store.release()
		synctest.Wait()
		after := mustGetBead(t, env.store, got.ID)
		if after.Metadata["pending_create_claim"] != "" || after.Metadata["state"] != "active" || !sp.IsRunning("worker") {
			t.Fatalf("after the commit landed: state=%q pending_create_claim=%q running=%v, want the start committed",
				after.Metadata["state"], after.Metadata["pending_create_claim"], sp.IsRunning("worker"))
		}
	})
}

// M3: runtime alive, the commit refused (it never started), the lease abandoned:
// the flock is free and the record left to lapse. Once the start's in-flight
// window is over, the tick takes the free lease and commits the start.
func TestRepairMatrixAliveRefusedCommitIsCommittedByTheTick(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		got, sp, env, _, cleanup := abandonedCommitRepairScenario(t, testCity, workDir, 10*time.Minute, abandonShape{refused: true}, nil)
		defer cleanup()
		if got.Status == "closed" || got.Metadata["pending_create_claim"] != "" || got.Metadata["state"] != "active" {
			t.Fatalf("status=%q state=%q pending_create_claim=%q, want the refused start committed by the tick; stderr=%s",
				got.Status, got.Metadata["state"], got.Metadata["pending_create_claim"], env.stderr.String())
		}
		if !sp.IsRunning("worker") || sp.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("runtime running=%v stops=%d, want the healthy runtime left running", sp.IsRunning("worker"), sp.CountCalls("Stop", "worker"))
		}
	})
}

// M4: runtime dead, a non-pool row, the commit hung and its lease retained. The
// stale-create rollback runs once the claim's lease has expired, and the
// zombie commit that lands afterwards is fenced: it does not bring the closed
// row back.
func TestRepairMatrixDeadHungCommitIsRolledBackAndTheLateCommitIsFenced(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		got, _, env, store, cleanup := abandonedCommitRepairScenario(t, testCity, workDir, 2*time.Hour, abandonShape{}, func(sp *postStartProvider) {
			_ = sp.Fake.Stop("worker")
		})
		defer cleanup()
		if got.Status != "closed" || got.Metadata["pending_create_claim"] != "" {
			t.Fatalf("status=%q pending_create_claim=%q state=%q, want the dead start rolled back; stderr=%s",
				got.Status, got.Metadata["pending_create_claim"], got.Metadata["state"], env.stderr.String())
		}
		store.release()
		synctest.Wait()
		after := mustGetBead(t, env.store, got.ID)
		if after.Status != "closed" || after.Metadata["state"] == "active" {
			t.Fatalf("after the zombie commit: status=%q state=%q, want the rolled-back row fenced against it", after.Status, after.Metadata["state"])
		}
	})
}

// M6: runtime dead, the commit refused, the lease abandoned. The row still
// carries the pre-wake marker with no live holder, so the start counts as
// abandoned and the reconciler relaunches it. While a hung commit retains the
// lease it does not.
func TestRepairMatrixRefusedStartIsAbandonedWhileAHungOneIsNot(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	for _, tc := range []struct {
		name  string
		shape abandonShape
		want  bool
	}{
		{"refused commit, lease abandoned", abandonShape{refused: true}, true},
		{"hung commit, lease retained", abandonShape{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, _, env, store, cleanup := abandonedCommitRepairScenario(t, testCity, workDir, 0, tc.shape, func(sp *postStartProvider) {
					_ = sp.Fake.Stop("worker")
				})
				defer cleanup()
				_ = store
				rows, err := env.store.List(beads.ListQuery{Label: "gc:session", AllowScan: true})
				if err != nil || len(rows) == 0 {
					t.Fatalf("listing the session row: %v (%d rows)", err, len(rows))
				}
				info := sessiontest.SeedBead(t, rows[0])
				if got := startAbandoned(env.store, testCity, info); got != tc.want {
					t.Fatalf("startAbandoned = %v, want %v", got, tc.want)
				}
			})
		})
	}
}
