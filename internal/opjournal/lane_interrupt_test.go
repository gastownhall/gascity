package opjournal

import (
	"errors"
	"testing"
	"time"
)

// laneInterrupt is a lane interrupt on conversation gc-1's generation 1,
// keyed key, scoped to turn when one is named.
func laneInterrupt(key, turn string) Request {
	return Request{
		Conversation: "gc-1", Kind: KindInterrupt, Executor: ExecutorLane, ClientKey: key,
		Producer: Producer{Principal: "operator", Actor: ActorOperator},
		Scope:    Scope{Kind: ScopeSession, Generation: 1, Turn: turn},
	}
}

// An interrupt released because the runtime was unavailable, and never taken
// again before its start_by, closes provider_unavailable (X11 §5.4); a
// message in the same state expires before start, as before.
func TestLaneInterruptUnavailableUntilStartByIsProviderUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		want string
	}{
		{"interrupt", laneInterrupt("i", "t1"), ReasonProviderUnavailable},
		{"message", laneMsg("m", RolePrompt, "x", func(r *Request) { r.StartBy = t0.Add(time.Minute) }), ReasonExpiredBeforeStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, tc.req)
			exec := newFakeExec(h.clk)
			unavailable := NotDelivered{Transient: true, Cause: errors.New("runtime unavailable")}
			exec.interrupt = func(InterruptCall) ControlResult { return unavailable }
			exec.deliver = func(Delivery) DeliveryResult { return unavailable }
			l := h.lane(t, "ctl")
			if res := mustPass(t, l, view(), exec); res.Done != DoneReleased {
				t.Fatalf("first pass %+v, want released", res)
			}
			h.clk.Advance(2 * time.Minute)
			mustPass(t, l, view(), exec)
			if got := h.get(t, op.ID); got.Status != StatusClosed || got.Outcome.Arm != ArmFailed || got.Outcome.Reason != tc.want {
				t.Fatalf("op %s/%s{%s}, want failed{%s}", got.Status, got.Outcome.Arm, got.Outcome.Reason, tc.want)
			}
		})
	}
}

// Two clicks are one op only for a resolved turn: two proposals under the
// turn's InterruptKey are one op, while two under minted keys are two, and
// each closes as its executor answers (skipped{stale} once no turn runs).
func TestLaneInterruptTwoClicksAreOneOpOnlyForAResolvedTurn(t *testing.T) {
	h := newLaneHarness(t)
	key, err := InterruptKey(1, "t1")
	if err != nil {
		t.Fatal(err)
	}
	first := h.propose(t, laneInterrupt(key, "t1"))
	again, created, err := h.store.Propose(laneInterrupt(key, "t1"))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("second click on the resolved turn = %s created %v, %v; want op %s again", again.ID, created, err, first.ID)
	}

	a := h.propose(t, laneInterrupt("click-a", ""))
	b := h.propose(t, laneInterrupt("click-b", ""))
	if a.ID == b.ID {
		t.Fatal("two clicks without a turn share an op")
	}
	exec := newFakeExec(h.clk)
	// The resolved op stops t1; the clicks without a turn find none
	// running by the time each is taken, as the executor reads it.
	exec.interrupt = func(c InterruptCall) ControlResult {
		if c.Turn == "t1" {
			return Settled{Turn: "t1", Degradation: DegradationUnfencedInterrupt}
		}
		return Rejected{Arm: ArmSkipped, Reason: ReasonStale}
	}
	l := h.lane(t, "ctl")
	for range 3 {
		mustPass(t, l, view(), exec)
	}
	if got := h.get(t, first.ID); got.Outcome.Arm != ArmSucceeded || got.Outcome.Result.Turn != "t1" || got.Outcome.Degradation != DegradationUnfencedInterrupt {
		t.Fatalf("resolved-turn op %+v, want succeeded{t1, unfenced_interrupt}", got.Outcome)
	}
	for _, id := range []string{a.ID, b.ID} {
		if got := h.get(t, id); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
			t.Fatalf("late click %s %+v, want skipped{stale}", id, got.Outcome)
		}
	}
	if n := len(exec.interrupts); n != 3 {
		t.Fatalf("interrupt effects = %d, want 3 (one per op)", n)
	}
}
