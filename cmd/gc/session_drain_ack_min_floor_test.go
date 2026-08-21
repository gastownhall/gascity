package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// minFloorAckFixture builds the exact live shape sc-j27j0d was measured on: a
// pool with min_active_sessions=1 holding a single warm session that has
// self-acked its own drain (agent-sourced ack, no drain ever requested of it).
func minFloorAckFixture() (map[string]sessionpkg.Info, *runtime.Fake, *fakeDrainOps) {
	infoByID := map[string]sessionpkg.Info{
		"s-a": {ID: "s-a", Template: "worker", State: sessionpkg.StateActive, SessionNameMetadata: "worker-1"},
	}
	sp := runtime.NewFake()
	// What `gc runtime drain-ack` writes: source=agent, GC_DRAIN_ACK=1, and no
	// GC_DRAIN — nobody asked this session to stop, it asked itself.
	_ = sp.SetMeta("worker-1", reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	_ = sp.SetMeta("worker-1", "GC_DRAIN_ACK", "1")
	dops := newFakeDrainOps()
	dops.acked["worker-1"] = true
	return infoByID, sp, dops
}

// TestCancelSelfInitiatedDrainAckAtMinFloorCuresTheBootRetireLoop pins the
// positive arm: the single floor session's self-initiated ack is CANCELED, so
// it stays warm instead of being stopped, closed, and cold-recreated by min_fill
// on the next tick (the 8-sessions-in-29-minutes loop measured on
// dip/refinery.refinery 2026-08-21).
func TestCancelSelfInitiatedDrainAckAtMinFloorCuresTheBootRetireLoop(t *testing.T) {
	infoByID, sp, dops := minFloorAckFixture()
	if !cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), dops, sp, newDrainTracker(), "worker", "worker-1", "s-a", io.Discard) {
		t.Fatal("the sole min_active_sessions=1 floor session's self-initiated ack must be canceled, not honored")
	}
	if len(dops.clearDrainCalls) != 1 || dops.clearDrainCalls[0] != "worker-1" {
		t.Fatalf("clearDrain calls = %v, want exactly one for worker-1 — an uncleared ack re-enters this branch every tick", dops.clearDrainCalls)
	}
}

// TestCancelSelfInitiatedDrainAckAtMinFloorRefusals pins every arm that must
// still stop the session. Each is a fail-closed precondition: the floor is a
// reason to keep a seat warm, never a veto over a drain somebody ordered.
func TestCancelSelfInitiatedDrainAckAtMinFloorRefusals(t *testing.T) {
	t.Run("above the floor: an elastic session still retires on its own ack", func(t *testing.T) {
		infoByID, sp, dops := minFloorAckFixture()
		// s-a is the floor member; s-b and s-c are elastic. s-b acked.
		infoByID["s-b"] = sessionpkg.Info{ID: "s-b", Template: "worker", State: sessionpkg.StateActive, SessionNameMetadata: "worker-2"}
		infoByID["s-c"] = sessionpkg.Info{ID: "s-c", Template: "worker", State: sessionpkg.StateActive, SessionNameMetadata: "worker-3"}
		_ = sp.SetMeta("worker-2", reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), dops, sp, newDrainTracker(), "worker", "worker-2", "s-b", io.Discard) {
			t.Fatal("s-b is above a floor of 1 — honoring its ack leaves the floor covered, so it must retire")
		}
		if len(dops.clearDrainCalls) != 0 {
			t.Fatalf("clearDrain must not run for a refused cancel; calls = %v", dops.clearDrainCalls)
		}
	})

	t.Run("reconciler-owned ack: the reconciler's own cancel/stop rules win", func(t *testing.T) {
		infoByID, sp, dops := minFloorAckFixture()
		infoByID["s-a"] = sessionpkg.Info{ID: "s-a", Template: "worker", State: sessionpkg.StateActive, SessionNameMetadata: "worker-1", Generation: "7"}
		_ = sp.SetMeta("worker-1", reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue)
		_ = sp.SetMeta("worker-1", reconcilerDrainAckReasonKey, "orphaned")
		_ = sp.SetMeta("worker-1", reconcilerDrainAckGenerationKey, "7")
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), dops, sp, newDrainTracker(), "worker", "worker-1", "s-a", io.Discard) {
			t.Fatal("a reconciler-minted ack is not self-initiated and must not be canceled here")
		}
	})

	t.Run("STALE reconciler-owned ack is refused too", func(t *testing.T) {
		// The generation no longer matches, so reconcilerDrainAckMatchesSessionInfo
		// reports "not reconciler-owned". Gating on the SOURCE is what keeps this
		// ack — which the stale-ack arm at the call site owns — from slipping
		// through as if the agent had chosen it.
		infoByID, sp, dops := minFloorAckFixture()
		infoByID["s-a"] = sessionpkg.Info{ID: "s-a", Template: "worker", State: sessionpkg.StateActive, SessionNameMetadata: "worker-1", Generation: "9"}
		_ = sp.SetMeta("worker-1", reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue)
		_ = sp.SetMeta("worker-1", reconcilerDrainAckReasonKey, "orphaned")
		_ = sp.SetMeta("worker-1", reconcilerDrainAckGenerationKey, "7")
		if _, owned := reconcilerDrainAckMatchesSessionInfo(infoByID["s-a"], sp, "worker-1"); owned {
			t.Fatal("fixture is wrong: this ack must read as NOT reconciler-owned (stale generation)")
		}
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), dops, sp, newDrainTracker(), "worker", "worker-1", "s-a", io.Discard) {
			t.Fatal("a stale reconciler ack is still not agent-sourced and must not be canceled here")
		}
	})

	t.Run("ack with no source at all is refused", func(t *testing.T) {
		infoByID, _, dops := minFloorAckFixture()
		bare := runtime.NewFake()
		_ = bare.SetMeta("worker-1", "GC_DRAIN_ACK", "1")
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), dops, bare, newDrainTracker(), "worker", "worker-1", "s-a", io.Discard) {
			t.Fatal("an ack with no GC_DRAIN_ACK_SOURCE cannot be proven self-initiated and must fail closed")
		}
	})

	t.Run("outstanding operator drain: GC_DRAIN outranks the floor", func(t *testing.T) {
		infoByID, sp, dops := minFloorAckFixture()
		dops.draining["worker-1"] = true
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), dops, sp, newDrainTracker(), "worker", "worker-1", "s-a", io.Discard) {
			t.Fatal("somebody ordered this session to drain — the floor must not veto that order")
		}
	})

	t.Run("unreadable GC_DRAIN fails closed", func(t *testing.T) {
		infoByID, sp, dops := minFloorAckFixture()
		dops.err = errors.New("runtime unreachable")
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), dops, sp, newDrainTracker(), "worker", "worker-1", "s-a", io.Discard) {
			t.Fatal("an unreadable drain flag must be treated as an outstanding drain, never as an absent one")
		}
	})

	t.Run("live drainTracker entry: a drain is already in flight", func(t *testing.T) {
		infoByID, sp, dops := minFloorAckFixture()
		dt := newDrainTracker()
		dt.set("s-a", &drainState{reason: "pool-excess"})
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), dops, sp, dt, "worker", "worker-1", "s-a", io.Discard) {
			t.Fatal("a tracked drain is in flight for this session — it must be honored, not canceled")
		}
	})

	t.Run("no floor configured: nothing to protect", func(t *testing.T) {
		infoByID, sp, dops := minFloorAckFixture()
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(0), dops, sp, newDrainTracker(), "worker", "worker-1", "s-a", io.Discard) {
			t.Fatal("min_active_sessions=0 is no floor at all — the ack must be honored")
		}
	})

	t.Run("a failed clear falls through to the ordinary stop", func(t *testing.T) {
		infoByID, sp, dops := minFloorAckFixture()
		failing := &clearDrainErrorOps{fakeDrainOps: dops, err: errors.New("tmux gone")}
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), failing, sp, newDrainTracker(), "worker", "worker-1", "s-a", io.Discard) {
			t.Fatal("returning true with GC_DRAIN_ACK still set would park the session and re-enter this branch every tick")
		}
	})

	t.Run("nil dops / unknown session id", func(t *testing.T) {
		infoByID, sp, dops := minFloorAckFixture()
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), nil, sp, newDrainTracker(), "worker", "worker-1", "s-a", io.Discard) {
			t.Error("nil drainOps must refuse")
		}
		if cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), dops, sp, newDrainTracker(), "worker", "worker-9", "s-nope", io.Discard) {
			t.Error("a session id absent from the snapshot must refuse")
		}
	})
}

// clearDrainErrorOps fails only clearDrain, leaving every other read healthy —
// fakeDrainOps' injected error is global, which would short-circuit isDraining
// before the clear is ever reached.
type clearDrainErrorOps struct {
	*fakeDrainOps
	err error
}

func (o *clearDrainErrorOps) clearDrain(string) error { return o.err }

// TestIsMinFloorProtectedDrainAckSessionTracksTheIdlePath pins the deliberate
// delegation: a session the drain-ack site keeps alive is exactly a session the
// idle-timeout path keeps warm. If the two ever disagree, a session saved here
// is killed a tick later by the idle path and the loop returns with one extra
// hop.
func TestIsMinFloorProtectedDrainAckSessionTracksTheIdlePath(t *testing.T) {
	infoByID := map[string]sessionpkg.Info{"s-a": warm("s-a"), "s-b": warm("s-b")}
	for _, cfg := range []*config.City{floorCfg(0), floorCfg(1), floorCfg(2)} {
		for _, id := range []string{"s-a", "s-b"} {
			want := isMinFloorExemptIdleSession(infoByID, cfg, "worker", id)
			if got := isMinFloorProtectedDrainAckSession(infoByID, cfg, "worker", id); got != want {
				t.Errorf("%s: drain-ack protection = %v, idle exemption = %v — the two sites must agree", id, got, want)
			}
		}
	}
}

// TestReconcileSessionBeads_MinFloorAgentAckKeepsTheSeatWarm is the tick-level
// proof for sc-j27j0d: the sole session of a pool with min_active_sessions=1
// self-acks (the pool-worker protocol's response to an empty ready queue) and
// the reconciler must NOT mark it stop-pending. Honoring the ack there is what
// dropped the pool below its floor and let min_fill boot a replacement that
// acked again — 8 sessions in 29 minutes on dip/refinery.refinery, 2026-08-21.
//
// It is the deliberate counterpart of
// TestReconcileSessionBeads_AgentAckStopProceedsDespiteStoreQueryPartial: same
// arm, same agent-sourced ack, and it still stops there because that pool
// declares no floor.
func TestReconcileSessionBeads_MinFloorAgentAckKeepsTheSeatWarm(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(1)}}}
	env.addDesired("worker", "worker", false)
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	// What `gc runtime drain-ack` actually writes on the runtime: source=agent.
	if err := env.sp.SetMeta("worker", reconcilerDrainAckSourceKey, drainAckSourceAgentValue); err != nil {
		t.Fatalf("SetMeta(source): %v", err)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state_reason"] == sessionpkg.DrainAckStopPendingReason {
		t.Fatal("the sole min_active_sessions=1 session was marked stop-pending on its own ack — that is the boot/retire loop (sc-j27j0d)")
	}
	if got.Metadata["state"] == string(sessionpkg.StateDraining) {
		t.Fatal("state = draining: the floor session must stay warm and idle under idle_timeout, not be destroyed and cold-recreated")
	}
	foundClear := false
	for _, n := range dops.clearDrainCalls {
		if n == "worker" {
			foundClear = true
		}
	}
	if !foundClear {
		t.Fatal("the ack was left set — the reconciler would re-enter this branch every tick")
	}
}
