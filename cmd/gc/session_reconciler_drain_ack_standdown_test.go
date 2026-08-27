package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// keyedOwnershipAcquiredMidTick models the production ownership predicate
// (sessionStartLegacyExclusionPredicate) taking the row between legacy's
// loop-entry classification and its drain-acknowledgement arm. The predicate is
// not a constant: it reads live keyed lease state, so the two reads legacy takes
// for one row inside one tick can legitimately disagree. Legacy asks exactly
// twice per row on this path — once at the top-of-loop start exclusion
// (session_reconciler.go:1880) and once at the acknowledgement arm's stop gate
// (:2307 undesired, :2737 desired) — and the second read is the only one the
// drain-ack effects are gated on, so the drain-ack stand-down has to hold when
// the keyed owner arrives in between.
func keyedOwnershipAcquiredMidTick(sessionID string) func(sessionpkg.Info) bool {
	reads := 0
	return func(info sessionpkg.Info) bool {
		if info.ID != sessionID {
			return false
		}
		reads++
		return reads > 1
	}
}

// assertNoLegacyDrainAckMetadata is the row-scoped half of the v59 purity
// assertion: a keyed-owned row carries no legacy drain-acknowledgement state at
// all, not merely a state/state_reason pair that happens to read clean.
//
// The tracker clear is deliberately NOT asserted here. It rides inside the same
// gated block as the mark, but it is not independently observable from a single
// tick: the drain-advance scan is a separate ownership family
// (withLegacyDrainAdvanceExclusion / ownsDrainAdvance) and legitimately owns the
// tracker for a row this family has yielded, so the end-of-tick scan clears it
// either way. The durable row and the runtime are the effects the journey
// assertion is scoped to, and they are what this pins.
func assertNoLegacyDrainAckMetadata(t *testing.T, env *reconcilerTestEnv, id string) {
	t.Helper()
	stored, err := env.store.Get(id)
	if err != nil {
		t.Fatalf("read row after reconcile: %v", err)
	}
	for key, value := range stored.Metadata {
		if strings.HasPrefix(key, "drain_ack") && value != "" {
			t.Fatalf("keyed-owned row carries legacy drain-ack metadata %s=%q: %v", key, value, stored.Metadata)
		}
	}
}

// TestLegacyDrainAckStandDownCoversTheDurableMark pins ga-f7v2ft.147: the WD.6
// D-DRAIN stand-down for a keyed-owned row is CONJUNCTIVE. Legacy's
// acknowledgement arm applies three effects — the durable stop-pending mark, the
// in-memory tracker clear, and the asynchronous process stop — but only the stop
// was gated on the keyed-ownership exclusion. A stand-down that still writes the
// row is worse than none: the keyed owner's fenced transition then lands on a row
// legacy already moved, and the drained row carries a legacy drain effect with no
// effect_owner stamp.
//
// This is the v59 journey signature at unit level
// (session_start_real_bd_tmux_integration_test.go:1961, "legacy applied a drain
// effect to the drained row" with SiteCode=reconciler.session.drain_ack
// ReasonCode=acknowledged OutcomeCode=stop_pending). The trace record the journey
// catches is emitted from inside the same `ok` block as the mark, so a gate that
// covers the mark removes the record with it.
func TestLegacyDrainAckStandDownCoversTheDurableMark(t *testing.T) {
	// seedAckedDrain builds a desired, live, agent-acknowledged session — the
	// shape `gc runtime drain-ack` leaves behind — parked on a tracked drain.
	// Agent-sourced provenance keeps the arm off the reconciler-owned cancel
	// branches so the run reaches the stop-pending mark directly.
	seedAckedDrain := func(t *testing.T) (*reconcilerTestEnv, beads.Bead, drainOps) {
		t.Helper()
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
		env.addDesired("worker", "worker", true)
		session := env.createSessionBead("worker", "worker")
		env.markSessionActive(&session)
		dops := newDrainOps(env.sp)
		if err := dops.setDrain("worker"); err != nil {
			t.Fatalf("set drain: %v", err)
		}
		if err := dops.setDrainAck("worker"); err != nil {
			t.Fatalf("set drain acknowledgement: %v", err)
		}
		env.dt.set(session.ID, &drainState{
			startedAt: env.clk.Now().UTC(),
			deadline:  env.clk.Now().Add(defaultDrainTimeout).UTC(),
			reason:    "pool-excess",
			ackSet:    true,
		})
		return env, session, dops
	}

	t.Run("keyed_owned_row_receives_no_legacy_drain_ack_effect", func(t *testing.T) {
		env, session, dops := seedAckedDrain(t)
		env.startOptions = append(env.startOptions, withLegacyStartExclusion(keyedOwnershipAcquiredMidTick(session.ID)))

		env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, dops)

		after := env.sessionInfo(session.ID)
		if isDrainAckStopPendingInfo(after) {
			t.Fatalf("legacy marked a keyed-owned row drain-ack stop-pending: %+v", after)
		}
		if after.MetadataState != string(sessionpkg.StateActive) {
			t.Fatalf("keyed-owned row state = %q, want the untouched %q", after.MetadataState, sessionpkg.StateActive)
		}
		assertNoLegacyDrainAckMetadata(t, env, session.ID)
		if !env.sp.IsRunning("worker") {
			t.Fatal("legacy stopped the runtime of a keyed-owned row")
		}
	})

	t.Run("unowned_row_still_gets_the_full_legacy_drain_ack_path", func(t *testing.T) {
		env, session, dops := seedAckedDrain(t)

		env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, dops)

		after := env.sessionInfo(session.ID)
		if !isDrainAckStopPendingInfo(after) {
			t.Fatalf("legacy-only drain acknowledgement = %+v, want the stop-pending mark", after)
		}
	})
}

// TestLegacyDrainAckStandDownCoversTheDurableMarkOnUndesiredRows is the orphan
// arm of the same conjunctive stand-down. An undesired acknowledged row runs a
// second copy of the mark/clear/stop sequence behind the same partial gate, so it
// carries the identical defect and takes the identical fix.
func TestLegacyDrainAckStandDownCoversTheDurableMarkOnUndesiredRows(t *testing.T) {
	seedUndesiredAckedDrain := func(t *testing.T) (*reconcilerTestEnv, beads.Bead, drainOps) {
		t.Helper()
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
		// No addDesired: the row is live in the provider but outside the desired
		// set, which is what routes it through the orphan acknowledgement arm.
		session := env.createSessionBead("worker", "worker")
		env.markSessionActive(&session)
		env.setSessionMetadata(&session, map[string]string{
			"pool_managed": "true",
			"pool_slot":    "1",
			"last_woke_at": env.clk.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		})
		if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
			t.Fatalf("start runtime: %v", err)
		}
		dops := newDrainOps(env.sp)
		if err := dops.setDrain("worker"); err != nil {
			t.Fatalf("set drain: %v", err)
		}
		if err := dops.setDrainAck("worker"); err != nil {
			t.Fatalf("set drain acknowledgement: %v", err)
		}
		env.dt.set(session.ID, &drainState{
			startedAt: env.clk.Now().UTC(),
			deadline:  env.clk.Now().Add(defaultDrainTimeout).UTC(),
			reason:    "orphaned",
			ackSet:    true,
		})
		return env, session, dops
	}

	t.Run("keyed_owned_row_receives_no_legacy_drain_ack_effect", func(t *testing.T) {
		env, session, dops := seedUndesiredAckedDrain(t)
		env.startOptions = append(env.startOptions, withLegacyStartExclusion(keyedOwnershipAcquiredMidTick(session.ID)))

		env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{}, dops)

		after := env.sessionInfo(session.ID)
		if isDrainAckStopPendingInfo(after) {
			t.Fatalf("legacy marked a keyed-owned undesired row drain-ack stop-pending: %+v", after)
		}
		assertNoLegacyDrainAckMetadata(t, env, session.ID)
		if !env.sp.IsRunning("worker") {
			t.Fatal("legacy stopped the runtime of a keyed-owned undesired row")
		}
	})

	t.Run("unowned_row_still_gets_the_full_legacy_drain_ack_path", func(t *testing.T) {
		env, session, dops := seedUndesiredAckedDrain(t)

		env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{}, dops)

		after := env.sessionInfo(session.ID)
		if !isDrainAckStopPendingInfo(after) {
			t.Fatalf("legacy-only undesired drain acknowledgement = %+v, want the stop-pending mark", after)
		}
	})
}
