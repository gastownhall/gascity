package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ackAtFirstRemovalProvider publishes a complete agent acknowledgment at the
// provider mutation boundary: the first RemoveMeta the hold's cancel issues,
// after the controller has read GC_DRAIN_ACK and decided the drain is its own
// to cancel. A deterministic scheduling seam, not a replacement ack path —
// the shape of Cherub's review probe for gastownhall/gascity#6178, whose
// original seam (RemoveMeta of GC_DRAIN_ACK itself) no longer exists: the
// reconciler never removes that key.
type ackAtFirstRemovalProvider struct {
	runtime.Provider
	fired  bool
	ackErr error
	keys   []string
}

func (p *ackAtFirstRemovalProvider) RemoveMeta(name, key string) error {
	p.keys = append(p.keys, key)
	if !p.fired {
		p.fired = true
		p.ackErr = newDrainOps(p.Provider).setDrainAck(name)
		if p.ackErr != nil {
			return p.ackErr
		}
	}
	return p.Provider.RemoveMeta(name, key)
}

// TestWoodhouseHeartbeatHoldPreservesConcurrentAgentAck: an agent ack that
// lands inside the hold's cancel — between the ownership read and the first
// removal — must survive it (the reconciler removes only its own keys), and
// the next tick must honor it by stopping the seat: not merely by declining
// to cancel again, which a skipped tick would also satisfy. The fixture's
// store is an ordinary reachable one that answers "no assigned work" for the
// seat (no fail-safe path is taken, and the test pins that), so the next
// tick's decision is the real one — stop-pending, the async stop, and the
// pool seat's close once the runtime is confirmed gone. Tracked and
// recovered forms of the reconciler's own ack.
func TestWoodhouseHeartbeatHoldPreservesConcurrentAgentAck(t *testing.T) {
	for _, tracked := range []bool{true, false} {
		name := "recovered"
		if tracked {
			name = "tracked"
		}
		t.Run(name, func(t *testing.T) {
			env, session, _ := newUndesiredPoolSeat(t, 45*time.Minute)
			ds := &drainState{startedAt: env.clk.Now(), deadline: env.clk.Now().Add(defaultDrainTimeout), reason: "orphaned", generation: 1, ackSet: true}
			if tracked {
				env.dt.set(session.ID, ds)
			}
			if err := setReconcilerDrainAckMetadata(env.sp, "worker", ds); err != nil {
				t.Fatal(err)
			}
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			provider := &ackAtFirstRemovalProvider{Provider: env.sp}
			reconcileSessionBeadsAtPath(context.Background(), "", []beads.Bead{got}, nil, nil, env.cfg, provider, env.store,
				newDrainOps(provider), nil, nil, nil, env.dt, map[string]int{}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr)
			if !provider.fired || provider.ackErr != nil {
				t.Fatalf("agent acknowledgment did not complete at the cancellation boundary: fired=%v error=%v removals=%v", provider.fired, provider.ackErr, provider.keys)
			}
			for _, key := range provider.keys {
				if key == "GC_DRAIN_ACK" {
					t.Fatalf("the hold's cancel removed GC_DRAIN_ACK, the agent's key; removals=%v", provider.keys)
				}
			}
			acked, err := newDrainOps(env.sp).isDrainAcked("worker")
			if err != nil {
				t.Fatal(err)
			}
			if !acked {
				t.Fatalf("successful concurrent agent acknowledgment was erased: running=%v removals=%v stdout=%s", env.sp.IsRunning("worker"), provider.keys, env.stdout.String())
			}
			if v, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); v != "1" {
				t.Fatalf("GC_DRAIN_ACK = %q after the cancel, want the agent's 1", v)
			}

			// The surviving agent ack outranks the hold on the next tick: the hold
			// lens must not cancel again, and the ack is honored — the seat is
			// marked stop-pending and its runtime stopped, the healthy store
			// having answered that nothing is assigned to it.
			got, err = env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := env.stdout.Len()
			reconcileSessionBeadsAtPath(context.Background(), "", []beads.Bead{got}, nil, nil, env.cfg, env.sp, env.store,
				newDrainOps(env.sp), nil, nil, nil, env.dt, map[string]int{}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr)
			if tail := env.stdout.String()[before:]; strings.Contains(tail, "(heartbeat hold)") {
				t.Fatalf("the hold canceled again over the agent's surviving ack: %s", tail)
			}
			if strings.Contains(env.stderr.String(), "checking assigned work for drain-acked") {
				t.Fatalf("the assigned-work check failed safe instead of the store answering:\n%s", env.stderr.String())
			}
			got, err = env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata["state_reason"] != sessionpkg.DrainAckStopPendingReason {
				t.Fatalf("surviving agent ack not honored on the next tick: state=%q state_reason=%q running=%v stdout=%s",
					got.Metadata["state"], got.Metadata["state_reason"], env.sp.IsRunning("worker"), env.stdout.String())
			}
			waitForProviderStopped(t, env.sp, "worker")

			// With the runtime confirmed gone, the tick after closes the pool
			// seat: the ack ran its whole course, so no slot stays occupied.
			got, err = env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			reconcileSessionBeadsAtPath(context.Background(), "", []beads.Bead{got}, nil, nil, env.cfg, env.sp, env.store,
				newDrainOps(env.sp), nil, nil, nil, env.dt, map[string]int{}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr)
			got, err = env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "closed" {
				t.Fatalf("stopped pool seat not finalized: status=%q state=%q state_reason=%q", got.Status, got.Metadata["state"], got.Metadata["state_reason"])
			}
		})
	}
}
