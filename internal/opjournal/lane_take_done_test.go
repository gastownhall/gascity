package opjournal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func mustPass(t *testing.T, l *ConversationLane, conv ConversationView, exec Executor) PassResult {
	t.Helper()
	res, err := l.Pass(context.Background(), conv, exec)
	if err != nil {
		t.Fatalf("Pass: %v (result %+v)", err, res)
	}
	return res
}

func TestLaneHappyPathDeliversOneBatch(t *testing.T) {
	h := newLaneHarness(t)
	n1 := h.propose(t, laneMsg("n1", RoleNotice, "first"))
	h.clk.Advance(time.Millisecond)
	n2 := h.propose(t, laneMsg("n2", RoleNotice, "second"))
	h.clk.Advance(time.Millisecond)
	p := h.propose(t, laneMsg("p", RolePrompt, "do the thing", func(r *Request) { r.Producer.Principal = "operator" }))
	h.clk.Advance(time.Millisecond)
	n3 := h.propose(t, laneMsg("n3", RoleNotice, "third"))
	exec := newFakeExec(h.clk)
	exec.probe.Turn, exec.probe.KnownTurns = "", []string{"old-turn"}
	exec.deliver = func(Delivery) DeliveryResult {
		return Delivered{
			Record: DeliveryRecord{At: h.clk.Now(), Ack: AckDelivered},
			Evidence: TurnEvidence{
				HasSource: true, SameClock: true,
				Seen:      []SeenTurn{{ID: "old-turn", Generation: 1, StartedAt: t0.Add(-time.Hour)}, {ID: "new-turn", Generation: 1, StartedAt: h.clk.Now()}},
				FirstRead: FirstTurnRead{Done: true, Current: "new-turn"},
			},
			Via: "tmux",
		}
	}
	l := h.lane(t, "ctl")
	res := mustPass(t, l, view(), exec)
	if res.Took != TakeContent || res.Done != DoneAccepted || res.More {
		t.Fatalf("pass = %+v", res)
	}
	want := []string{n1.ID, n2.ID, n3.ID, p.ID} // notices first, then the prompt
	if !slices.Equal(res.Taken, want) {
		t.Fatalf("taken %v, want %v", res.Taken, want)
	}
	ds := exec.delivered()
	if len(ds) != 1 {
		t.Fatalf("%d deliveries", len(ds))
	}
	d := ds[0]
	if d.Seq != 1 || d.Key != n1.ID+":1" || d.Mode != LinkStarts || d.Redelivery || d.Steer {
		t.Fatalf("delivery %+v", d)
	}
	if d.Fence != (Fence{Conversation: "gc-1", Generation: 1, Epoch: 1, Token: "tok-1"}) {
		t.Fatalf("fence %+v", d.Fence)
	}
	if !strings.Contains(d.Text, "You have 3 deferred reminders") || !strings.HasSuffix(d.Text, "\ndo the thing") {
		t.Fatalf("text %q", d.Text)
	}
	assertNoIdentifiers(t, d.Text, d.Ops, d.Key)
	for i, id := range want {
		op := h.get(t, id)
		r := op.Outcome.Result
		if op.Status != StatusClosed || op.Outcome.Arm != ArmSucceeded || op.LaneSeq != uint64(i+1) || op.Attempt != 1 {
			t.Fatalf("op %d %+v", i, op)
		}
		if r.Delivery.Seq != 1 || r.Delivery.Ack != AckDelivered || r.Turn != "new-turn" ||
			r.TurnLink != LinkCorrelated || r.TurnBasis != BasisHistoryDiff || r.Generation != 1 || r.ContextEpoch != 1 || r.Via != "tmux" {
			t.Fatalf("op %d result %+v", i, r)
		}
		if op.Outcome.Degradation != "" || op.DeliveryKey != n1.ID+":1" || !op.Lease.IsZero() {
			t.Fatalf("op %d %+v", i, op)
		}
	}
	rt := h.runtime(t)
	if rt.Seq != 4 || !rt.Lease.IsZero() || rt.Generation != 1 || rt.Epoch != 1 {
		t.Fatalf("runtime row %+v", rt)
	}
	if n := len(h.batcher.batches()); n != 2 {
		t.Fatalf("%d guarded batches, want Take and Done", n)
	}
	// Nothing left: a second pass takes nothing and writes nothing.
	again := mustPass(t, l, view(), exec)
	if again.Took != TakeNone || len(h.batcher.batches()) != 2 {
		t.Fatalf("second pass %+v", again)
	}
}

func TestLaneUnfencedDeliveryIsRecorded(t *testing.T) {
	for name, tc := range map[string]struct {
		token string
		input FenceMode
	}{"no token": {"", FenceAtomic}, "no fence": {"tok", FenceNone}} {
		t.Run(name, func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, laneMsg("p", RolePrompt, "x"))
			exec := newFakeExec(h.clk)
			exec.probe.Caps.Input = tc.input
			v := view()
			v.Token = tc.token
			mustPass(t, h.lane(t, "ctl"), v, exec)
			got := h.get(t, op.ID)
			if got.Outcome.Degradation != DegradationUnfencedDelivery || got.Outcome.Result.TurnLink != LinkNone ||
				got.Outcome.Result.TurnBasis != BasisNoSource {
				t.Fatalf("outcome %+v", got.Outcome)
			}
		})
	}
}

func TestLaneDoneArms(t *testing.T) {
	cause := errors.New("pane said no")
	cases := []struct {
		name     string
		result   DeliveryResult
		mods     []func(*Request)
		wantSt   Status
		wantArm  Arm
		reason   string
		failures int
		maybe    int
		retryIn  time.Duration
		lastErr  string
		done     DoneArm
	}{
		{"transient", NotDelivered{Transient: true, Cause: cause}, nil, StatusAccepted, "", "", 0, 0, 2 * time.Second, "transient: pane said no", DoneReleased},
		{"not delivered", NotDelivered{Cause: cause}, nil, StatusAccepted, "", "", 1, 0, ExecutorErrorBackoff, "not_delivered: pane said no", DoneReleased},
		{"unknown", Unknown{Cause: cause}, nil, StatusAccepted, "", "", 1, 1, ExecutorErrorBackoff, "unknown: pane said no", DoneReleased},
		{"unknown at most once", Unknown{Cause: cause}, []func(*Request){atMostOnce}, StatusClosed, ArmFailed, ReasonAmbiguousDelivery, 0, 1, 0, "", DoneReleased},
		{"nil result", nil, nil, StatusAccepted, "", "", 1, 1, ExecutorErrorBackoff, "unknown: executor returned delivery result <nil>", DoneReleased},
		{"key rejected", Rejected{KeyRejected: true, Reason: "nudge_rejected"}, nil, StatusAccepted, "", "", 1, 0, ExecutorErrorBackoff, "key_rejected: nudge_rejected", DoneReleased},
		{"rejected failed", Rejected{Arm: ArmFailed, Reason: ReasonUnsupported}, nil, StatusClosed, ArmFailed, ReasonUnsupported, 0, 0, 0, "", DoneRejected},
		{"rejected skipped", Rejected{Arm: ArmSkipped, Reason: ReasonNotNeeded}, nil, StatusClosed, ArmSkipped, ReasonNotNeeded, 0, 0, 0, "", DoneRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, laneMsg("p", RolePrompt, "x", tc.mods...))
			exec := newFakeExec(h.clk)
			exec.deliver = func(Delivery) DeliveryResult { return tc.result }
			res, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec)
			if (tc.name == "nil result") != (err != nil) {
				t.Fatalf("Pass error %v", err)
			}
			if res.Done != tc.done {
				t.Fatalf("done %s, want %s", res.Done, tc.done)
			}
			got := h.get(t, op.ID)
			if got.Status != tc.wantSt || got.Outcome.Arm != tc.wantArm || got.Outcome.Reason != tc.reason ||
				got.Failures != tc.failures || got.MaybeDelivered != tc.maybe || got.LastError != tc.lastErr {
				t.Fatalf("op %+v", got)
			}
			if tc.retryIn != 0 && !got.RetryAt.Equal(h.clk.Now().Add(tc.retryIn)) {
				t.Fatalf("retry_at %s, want now+%s", got.RetryAt, tc.retryIn)
			}
			if !got.Lease.IsZero() || !h.runtime(t).Lease.IsZero() {
				t.Fatalf("a lease survived Done: op %v row %v", got.Lease, h.runtime(t).Lease)
			}
		})
	}
}

func TestLaneTransientBackoffSequence(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	exec.deliver = func(Delivery) DeliveryResult { return NotDelivered{Transient: true} }
	l := h.lane(t, "ctl")
	var got []time.Duration
	for range 10 {
		mustPass(t, l, view(), exec)
		cur := h.get(t, op.ID)
		got = append(got, cur.RetryAt.Sub(h.clk.Now()))
		h.clk.Time = cur.RetryAt.Add(time.Millisecond)
	}
	want := []time.Duration{2, 4, 8, 16, 32, 64, 128, 256, 300, 300}
	for i := range want {
		want[i] *= time.Second
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backoff %v, want %v", got, want)
	}
	if cur := h.get(t, op.ID); cur.Failures != 0 || cur.Attempt != 10 {
		t.Fatalf("transient releases charged failures: %+v", cur)
	}
}

func TestLanePoisonAtFive(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	exec.deliver = func(Delivery) DeliveryResult { return NotDelivered{Cause: errors.New("boom")} }
	l := h.lane(t, "ctl")
	for i := 1; i <= MaxFailures; i++ {
		mustPass(t, l, view(), exec)
		cur := h.get(t, op.ID)
		if i < MaxFailures && (cur.Status != StatusAccepted || cur.Failures != i) {
			t.Fatalf("attempt %d: %+v", i, cur)
		}
		h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	}
	cur := h.get(t, op.ID)
	if cur.Outcome.Arm != ArmFailed || cur.Outcome.Reason != ReasonPoison || cur.Failures != MaxFailures || cur.Attempt != MaxFailures {
		t.Fatalf("after %d failures: %+v", MaxFailures, cur)
	}
}

func TestLaneCancelDuringFlight(t *testing.T) {
	results := map[string]DeliveryResult{
		"delivered":     nil, // the default Delivered
		"transient":     NotDelivered{Transient: true},
		"not delivered": NotDelivered{},
		"unknown":       Unknown{},
		"key rejected":  Rejected{KeyRejected: true, Reason: "x"},
		"rejected":      Rejected{Arm: ArmFailed, Reason: ReasonUnsupported},
	}
	for name, r := range results {
		t.Run(name, func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, laneMsg("p", RolePrompt, "x"))
			exec := newFakeExec(h.clk)
			exec.onEffect = func() {
				if res, _, err := h.store.Cancel(op.ID, "operator", "changed my mind"); err != nil || res != CancelRequested {
					t.Errorf("Cancel in flight = %s, %v", res, err)
				}
			}
			if r != nil {
				exec.deliver = func(Delivery) DeliveryResult { return r }
			}
			mustPass(t, h.lane(t, "ctl"), view(), exec)
			got := h.get(t, op.ID)
			want := ArmCanceled
			if name == "delivered" {
				want = ArmSucceeded // the cancel lost the race
			}
			if got.Status != StatusClosed || got.Outcome.Arm != want || !got.Cancel.Recorded() {
				t.Fatalf("op %+v", got)
			}
		})
	}
}

// crashAt returns a hook that ends the pass at stage.
func crashAt(stage laneStage) func(laneStage) error {
	return func(s laneStage) error {
		if s == stage {
			return errCrash
		}
		return nil
	}
}

var errCrash = errors.New("simulated crash")

func TestLaneRetakeAfterCrashKeepsTheKey(t *testing.T) {
	for _, dedup := range []bool{false, true} {
		for _, stage := range []laneStage{stageAfterTake, stageAfterEffect} {
			t.Run(fmt.Sprintf("dedup=%v/%s", dedup, stage), func(t *testing.T) {
				h := newLaneHarness(t)
				n := h.propose(t, laneMsg("n", RoleNotice, "fyi"))
				p := h.propose(t, laneMsg("p", RolePrompt, "go"))
				exec := newFakeExec(h.clk)
				exec.probe.Caps.Dedup = dedup
				a := h.lane(t, "ctl")
				a.hooks.at = crashAt(stage)
				if _, err := a.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
					t.Fatalf("crashing pass = %v", err)
				}
				first := h.get(t, n.ID)
				// Before the lease expires (plus skew), nobody re-takes.
				h.clk.Advance(DefaultLeaseTTL + LeaseSkew)
				b := h.lane(t, "cli")
				if res := mustPass(t, b, view(), exec); res.Took != TakeNone || res.Waits[0].Reason != WaitInFlight {
					t.Fatalf("pass inside the skew took %+v", res)
				}
				h.clk.Advance(time.Millisecond)
				res := mustPass(t, b, view(), exec)
				if res.Took != TakeContent || res.Done != DoneAccepted {
					t.Fatalf("re-take pass %+v", res)
				}
				ds := exec.delivered()
				last := ds[len(ds)-1]
				if last.Key != first.DeliveryKey || !last.Redelivery {
					t.Fatalf("re-take key %s (was %s), redelivery %v", last.Key, first.DeliveryKey, last.Redelivery)
				}
				hasNote := strings.Contains(last.Text, RedeliveryNote)
				if hasNote == dedup {
					t.Fatalf("dedup=%v but note=%v:\n%s", dedup, hasNote, last.Text)
				}
				if dedup && len(ds) == 2 && ds[0].Text != last.Text {
					t.Fatalf("deduplicating redelivery differs from the first attempt:\n%q\n%q", ds[0].Text, last.Text)
				}
				for _, id := range []string{n.ID, p.ID} {
					got := h.get(t, id)
					if got.Outcome.Arm != ArmSucceeded || got.Attempt != 2 || got.MaybeDelivered != 1 || got.Failures != 1 ||
						got.LastError != lastErrorLeaseExpired || got.DeliveryKey != first.DeliveryKey {
						t.Fatalf("op %+v", got)
					}
				}
			})
		}
	}
}

func TestLaneRetakeOfAtMostOnceFailsAmbiguous(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x", atMostOnce))
	exec := newFakeExec(h.clk)
	a := h.lane(t, "ctl")
	a.hooks.at = crashAt(stageAfterEffect)
	_, _ = a.Pass(context.Background(), view(), exec)
	h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
	res := mustPass(t, h.lane(t, "cli"), view(), exec)
	if res.Took != TakeNone || len(exec.delivered()) != 1 {
		t.Fatalf("at_most_once was delivered again: %+v", res)
	}
	got := h.get(t, op.ID)
	if got.Outcome.Reason != ReasonAmbiguousDelivery || got.MaybeDelivered != 1 {
		t.Fatalf("op %+v", got)
	}
}

func TestLaneZombieTakeLoses(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	a, b := h.lane(t, "ctl"), h.lane(t, "cli")
	ran := false
	a.hooks.at = func(s laneStage) error {
		if s == stageAfterRead && !ran {
			ran = true
			if res := mustPass(t, b, view(), exec); res.Done != DoneAccepted {
				t.Errorf("inner pass %+v", res)
			}
		}
		return nil
	}
	res := mustPass(t, a, view(), exec)
	if res.Took != TakeNone {
		t.Fatalf("the zombie took %+v", res)
	}
	if len(exec.delivered()) != 1 || h.get(t, op.ID).Outcome.Arm != ArmSucceeded {
		t.Fatalf("deliveries %d", len(exec.delivered()))
	}
}

func TestLaneDoneAfterRetakeWritesNothing(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	a, b := h.lane(t, "ctl"), h.lane(t, "cli")
	// A's effect outlives its lease; B re-takes and delivers; then A's
	// Done runs and must write nothing for the op.
	a.hooks.at = func(s laneStage) error {
		if s == stageAfterEffect {
			h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
			bexec := newFakeExec(h.clk)
			bexec.deliver = func(Delivery) DeliveryResult {
				return Delivered{Record: DeliveryRecord{At: h.clk.Now(), Ack: AckDelivered, MessageID: "b-msg"}}
			}
			if res := mustPass(t, b, view(), bexec); res.Done != DoneAccepted {
				t.Errorf("B's pass %+v", res)
			}
		}
		return nil
	}
	exec.deliver = func(Delivery) DeliveryResult {
		return Delivered{Record: DeliveryRecord{At: h.clk.Now(), Ack: AckDelivered, MessageID: "a-msg"}}
	}
	res := mustPass(t, a, view(), exec)
	if res.Done != DoneSuperseded {
		t.Fatalf("A's Done = %s", res.Done)
	}
	got := h.get(t, op.ID)
	if got.Outcome.Result.Delivery.MessageID != "b-msg" || got.Attempt != 2 {
		t.Fatalf("op %+v", got)
	}
}

func TestLaneAmbiguousTakeCommitted(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	h.batcher.setFault(func(n int, _ beads.GuardedBatch) (bool, error) {
		if n == 0 {
			return true, errLinkDropped
		}
		return false, nil
	})
	res := mustPass(t, h.lane(t, "ctl"), view(), exec)
	if res.Done != DoneAccepted || len(exec.delivered()) != 1 {
		t.Fatalf("pass %+v, %d deliveries", res, len(exec.delivered()))
	}
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded || got.Attempt != 1 {
		t.Fatalf("op %+v", got)
	}
}

func TestLaneAmbiguousTakeNotCommitted(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	h.batcher.setFault(func(n int, _ beads.GuardedBatch) (bool, error) {
		if n == 0 {
			return false, errLinkDropped
		}
		return false, nil
	})
	res := mustPass(t, h.lane(t, "ctl"), view(), exec)
	if res.Done != DoneAccepted || len(exec.delivered()) != 1 {
		t.Fatalf("pass %+v, %d deliveries", res, len(exec.delivered()))
	}
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded || got.Attempt != 1 || got.LaneSeq != 1 {
		t.Fatalf("op %+v", got)
	}
}

func TestLaneAmbiguousDone(t *testing.T) {
	for _, commit := range []bool{true, false} {
		for name, result := range map[string]DeliveryResult{
			"delivered": nil, "released": NotDelivered{Cause: errors.New("x")},
		} {
			t.Run(fmt.Sprintf("%s/commit=%v", name, commit), func(t *testing.T) {
				h := newLaneHarness(t)
				op := h.propose(t, laneMsg("p", RolePrompt, "x"))
				exec := newFakeExec(h.clk)
				if result != nil {
					exec.deliver = func(Delivery) DeliveryResult { return result }
				}
				h.batcher.setFault(func(n int, _ beads.GuardedBatch) (bool, error) {
					if n == 1 {
						return commit, errLinkDropped
					}
					return false, nil
				})
				res := mustPass(t, h.lane(t, "ctl"), view(), exec)
				want := DoneAccepted
				if result != nil {
					want = DoneReleased
				}
				if res.Done != want {
					t.Fatalf("pass %+v", res)
				}
				got := h.get(t, op.ID)
				if !got.Lease.IsZero() || !h.runtime(t).Lease.IsZero() {
					t.Fatalf("lease left after an ambiguous Done: %+v", got)
				}
				if result == nil && got.Outcome.Arm != ArmSucceeded {
					t.Fatalf("op %+v", got)
				}
				if result != nil && (got.Status != StatusAccepted || got.Failures != 1) {
					t.Fatalf("op %+v", got)
				}
			})
		}
	}
}

func TestLaneRuntimeRowMovedDuringTheEffect(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	exec.onEffect = func() {
		// The observer writes a turn cell under the row's revision.
		b, _, err := h.store.runtimeBead("gc-1")
		if err != nil {
			t.Error(err)
			return
		}
		row, _ := DecodeRuntimeRow(b)
		row.State = RuntimeReady
		opts, err := RuntimeRowPatch(b, row)
		if err != nil {
			t.Error(err)
			return
		}
		if err := h.mem.UpdateIfMatch(b.ID, b.Revision, opts); err != nil {
			t.Error(err)
		}
	}
	res := mustPass(t, h.lane(t, "ctl"), view(), exec)
	if res.Done != DoneAccepted || h.get(t, op.ID).Outcome.Arm != ArmSucceeded {
		t.Fatalf("pass %+v", res)
	}
	rt := h.runtime(t)
	if !rt.Lease.IsZero() || rt.State != RuntimeReady {
		t.Fatalf("row %+v", rt)
	}
}

func TestLaneRespondWritesTheFact(t *testing.T) {
	for _, collide := range []bool{false, true} {
		t.Run(fmt.Sprintf("collide=%v", collide), func(t *testing.T) {
			h := newLaneHarness(t)
			req := Request{
				Conversation: "gc-1", Kind: KindRespond, Executor: ExecutorLane, ClientKey: "r1",
				Producer: Producer{Principal: "operator", Actor: ActorOperator}, Payload: "allow",
				Scope: Scope{Kind: ScopeSession, Generation: 1, Interaction: "perm-1#1"},
			}
			op := h.propose(t, req)
			f := Fact{Kind: FactInteractionResolved, Conversation: "gc-1", Generation: 1, Interaction: "perm-1#1", Basis: FactBasisObserved, At: t0}
			if collide {
				if _, err := h.store.RecordFact(f); err != nil {
					t.Fatal(err)
				}
			}
			exec := newFakeExec(h.clk)
			exec.probe.Caps.Respond = true
			res := mustPass(t, h.lane(t, "ctl"), view(), exec)
			if res.Took != TakeControl || res.Done != DoneAccepted || len(exec.responds) != 1 {
				t.Fatalf("pass %+v", res)
			}
			if c := exec.responds[0]; c.Interaction != "perm-1#1" || c.Payload != "allow" {
				t.Fatalf("respond call %+v", c)
			}
			if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded {
				t.Fatalf("op %+v", got)
			}
			id, _ := FactID("gc", f)
			b, err := h.mem.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := DecodeFact(b)
			wantBasis := FactBasisLane
			if collide {
				wantBasis = FactBasisObserved
			}
			if got.Basis != wantBasis || b.Status != "closed" {
				t.Fatalf("fact %+v", got)
			}
		})
	}
}

func TestLaneRespondWithoutTheCapabilityFails(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, Request{
		Conversation: "gc-1", Kind: KindRespond, Executor: ExecutorLane, ClientKey: "r1",
		Producer: Producer{Principal: "operator", Actor: ActorOperator}, Payload: "allow",
		Scope: Scope{Kind: ScopeSession, Generation: 1, Interaction: "perm-1"},
	})
	mustPass(t, h.lane(t, "ctl"), view(), newFakeExec(h.clk))
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmFailed || got.Outcome.Reason != ReasonUnsupported {
		t.Fatalf("op %+v", got)
	}
}

func TestLaneInterruptSucceedsWithItsTurn(t *testing.T) {
	h := newLaneHarness(t)
	msg := h.propose(t, laneMsg("p", RolePrompt, "x"))
	op := h.propose(t, Request{
		Conversation: "gc-1", Kind: KindInterrupt, Executor: ExecutorLane, ClientKey: "stop:1:t1",
		Producer: Producer{Principal: "operator", Actor: ActorOperator},
		Scope:    Scope{Kind: ScopeSession, Generation: 1, Turn: "t1"},
	})
	exec := newFakeExec(h.clk)
	l := h.lane(t, "ctl")
	res := mustPass(t, l, view(), exec)
	if res.Took != TakeControl || !res.More || res.Taken[0] != op.ID {
		t.Fatalf("control did not go first: %+v", res)
	}
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded || got.Outcome.Result.Turn != "t1" || got.Outcome.Result.Generation != 1 {
		t.Fatalf("interrupt %+v", got)
	}
	if res := mustPass(t, l, view(), exec); res.Took != TakeContent || res.Taken[0] != msg.ID {
		t.Fatalf("second pass %+v", res)
	}
}

func TestLaneKeepsUnknownKeys(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	if err := h.mem.SetMetadata(op.ID, "gc.op.from_the_future", "kept"); err != nil {
		t.Fatal(err)
	}
	// A runtime row with a newer build's key exists before the Take.
	rowBead, err := EncodeRuntimeRow("gc", RuntimeRow{Conversation: "gc-1", Generation: 1, Epoch: 1, Extra: map[string]string{"gc.rt.from_the_future": "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.mem.Create(rowBead); err != nil {
		t.Fatal(err)
	}
	mustPass(t, h.lane(t, "ctl"), view(), newFakeExec(h.clk))
	b, _ := h.mem.Get(op.ID)
	if b.Metadata["gc.op.from_the_future"] != "kept" || b.Status != "closed" {
		t.Fatalf("op row %+v", b.Metadata)
	}
	rt := h.runtime(t)
	if rt.Extra["gc.rt.from_the_future"] != "kept" || rt.Seq != 1 {
		t.Fatalf("runtime row %+v", rt)
	}
}

func TestLaneClosedConversation(t *testing.T) {
	h := newLaneHarness(t)
	a := h.propose(t, laneMsg("a", RolePrompt, "x"))
	b := h.propose(t, laneMsg("b", RoleNotice, "y"))
	exec := newFakeExec(h.clk)
	l := h.lane(t, "ctl")
	closedView := view()
	closedView.Closed = true
	// a is in flight when the conversation closes: it waits for its lease.
	crash := h.lane(t, "cli")
	crash.hooks.at = crashAt(stageAfterTake)
	exec.deliver = func(Delivery) DeliveryResult { return NotDelivered{} }
	h.propose(t, laneMsg("c", RolePrompt, "z"))
	if _, err := crash.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	res := mustPass(t, l, closedView, exec)
	if res.Took != TakeNone {
		t.Fatalf("took on a closed conversation: %+v", res)
	}
	for _, w := range res.Waits {
		if w.Reason != WaitInFlight {
			t.Fatalf("wait %+v", w)
		}
	}
	if h.runtime(t).Closed {
		t.Fatal("the runtime row closed with an op in flight")
	}
	h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
	res = mustPass(t, l, closedView, exec)
	for _, id := range []string{a.ID, b.ID} {
		if got := h.get(t, id); got.Outcome.Reason != ReasonConversationClosed {
			t.Fatalf("op %+v", got)
		}
	}
	if got := h.get(t, a.ID); got.MaybeDelivered != 1 {
		t.Fatalf("expired op closed without maybe_delivered: %+v", got)
	}
	if rt := h.runtime(t); !rt.Closed || !rt.Lease.IsZero() {
		t.Fatalf("runtime row %+v", rt)
	}
	// An open view over the closed row is refused.
	h.propose(t, laneMsg("late", RolePrompt, "z"))
	if _, err := l.Pass(context.Background(), view(), exec); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("open view over a closed row = %v", err)
	}
}

func TestLaneGenerationAdvanceSkipsStaleOps(t *testing.T) {
	h := newLaneHarness(t)
	old := h.propose(t, laneMsg("old", RolePrompt, "x", func(r *Request) {
		r.Scope = Scope{Kind: ScopeSession, Generation: 1}
	}))
	conv := h.propose(t, laneMsg("conv", RoleNotice, "y"))
	exec := newFakeExec(h.clk)
	v := view()
	v.Generation = 2
	res := mustPass(t, h.lane(t, "ctl"), v, exec)
	if got := h.get(t, old.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
		t.Fatalf("old %+v", got)
	}
	if res.Took != TakeContent || res.Taken[0] != conv.ID {
		t.Fatalf("pass %+v", res)
	}
	if rt := h.runtime(t); rt.Generation != 2 {
		t.Fatalf("row %+v", rt)
	}
	// A stale view (the row says generation 2) never lowers the row.
	h.propose(t, laneMsg("late", RoleNotice, "z"))
	res = mustPass(t, h.lane(t, "cli"), view(), exec)
	if res.Took != TakeNone || res.Waits[0].Reason != WaitIncarnation || !res.Next.Equal(h.clk.Now()) {
		t.Fatalf("stale view pass %+v", res)
	}
}

func TestLaneConcurrentPassesTakeOnce(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	const n = 16
	var ready sync.WaitGroup
	ready.Add(n)
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]PassResult, n)
	errs := make([]error, n)
	for i := range n {
		l := h.lane(t, fmt.Sprintf("h%d", i))
		first := true
		l.hooks.at = func(s laneStage) error {
			if s == stageAfterRead && first {
				first = false
				ready.Done()
				<-release
			}
			return nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = l.Pass(context.Background(), view(), exec)
		}()
	}
	ready.Wait()
	close(release)
	wg.Wait()
	takes := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("pass %d: %v", i, errs[i])
		}
		if results[i].Took != TakeNone {
			takes++
		}
	}
	if takes != 1 || len(exec.delivered()) != 1 {
		t.Fatalf("%d takes, %d deliveries", takes, len(exec.delivered()))
	}
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded || got.Attempt != 1 {
		t.Fatalf("op %+v", got)
	}
}

func TestLaneQuarantinesUndecodableRows(t *testing.T) {
	h := newLaneHarness(t)
	bad := h.propose(t, laneMsg("bad", RoleNotice, "x"))
	if err := h.mem.SetMetadata(bad.ID, keyWhen, "from_the_future"); err != nil {
		t.Fatal(err)
	}
	good := h.propose(t, laneMsg("good", RolePrompt, "y"))
	res := mustPass(t, h.lane(t, "ctl"), view(), newFakeExec(h.clk))
	if len(res.Quarantined) != 1 || res.Quarantined[0].ID != bad.ID || !errors.Is(res.Quarantined[0].Err, ErrUnsupportedValue) {
		t.Fatalf("quarantined %+v", res.Quarantined)
	}
	if res.Took != TakeContent || res.Taken[0] != good.ID {
		t.Fatalf("pass %+v", res)
	}
	b, _ := h.mem.Get(bad.ID)
	if b.Status != "open" || b.Assignee != "" || b.Metadata[keyWhen] != "from_the_future" {
		t.Fatalf("the lane wrote a quarantined row: %+v", b)
	}
}

func TestLaneUndecodableRuntimeRowStopsTheTake(t *testing.T) {
	h := newLaneHarness(t)
	stale := h.propose(t, laneMsg("old", RolePrompt, "x", func(r *Request) { r.StartBy = t0.Add(time.Minute) }))
	h.propose(t, laneMsg("p", RolePrompt, "y"))
	id, _ := RuntimeRowID("gc", "gc-1")
	if _, err := h.mem.Create(beads.Bead{ID: id, Type: BeadType, Labels: []string{LabelRuntime}, Metadata: map[string]string{keyRTSchema: "1"}}); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(2 * time.Minute)
	res, err := h.lane(t, "ctl").Pass(context.Background(), view(), newFakeExec(h.clk))
	if !errors.Is(err, ErrCorruptOp) || res.Took != TakeNone {
		t.Fatalf("pass %+v, %v", res, err)
	}
	if got := h.get(t, stale.ID); got.Outcome.Reason != ReasonExpiredBeforeStart {
		t.Fatalf("rule closes did not run: %+v", got)
	}
}

func TestLaneNonLaneOpsAreIgnored(t *testing.T) {
	h := newLaneHarness(t)
	legacy := h.propose(t, laneMsg("legacy", RolePrompt, "x", func(r *Request) { r.Executor = ExecutorLegacyAPI }))
	life := h.propose(t, Request{
		Conversation: "gc-1", Kind: KindWake, Executor: ExecutorLane, ClientKey: "w",
		Producer: Producer{Principal: "operator", Actor: ActorOperator},
	})
	res := mustPass(t, h.lane(t, "ctl"), view(), newFakeExec(h.clk))
	if res.Took != TakeNone || len(h.batcher.batches()) != 0 {
		t.Fatalf("pass %+v", res)
	}
	for _, id := range []string{legacy.ID, life.ID} {
		if got := h.get(t, id); got.Status != StatusSubmitted {
			t.Fatalf("op %+v", got)
		}
	}
}

func TestTransitionStillRefusesTheLaneEdges(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []Status{StatusAccepted, StatusExecuting} {
		if _, err := h.store.Transition(op.ID, StatusExecuting, to, "lane"); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("executing -> %s = %v", to, err)
		}
	}
}

func TestLaneProbeFailureBurnsNothing(t *testing.T) {
	h := newLaneHarness(t)
	h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	exec.probeErr = errors.New("tmux gone")
	if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec); err == nil {
		t.Fatal("probe error not returned")
	}
	exec.probeErr = nil
	exec.probe.Available = false
	res := mustPass(t, h.lane(t, "ctl"), view(), exec)
	if res.Took != TakeNone || res.Waits[0].Reason != WaitUnavailable || len(h.batcher.batches()) != 0 {
		t.Fatalf("pass %+v", res)
	}
	exec.probe = idleProbe()
	exec.probe.Caps.LeaseTTL = time.Second
	if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec); err == nil {
		t.Fatal("a lease below the minimum was accepted")
	}
}

// An executor result without a valid ack is Unknown: nothing says the
// runtime took the input.
func TestLaneDeliveredWithoutAValidAckIsUnknown(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	exec.deliver = func(Delivery) DeliveryResult {
		return Delivered{Record: DeliveryRecord{At: h.clk.Now(), Ack: "telepathy"}}
	}
	if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec); err == nil {
		t.Fatal("contract breach not reported")
	}
	if got := h.get(t, op.ID); got.Status != StatusAccepted || got.MaybeDelivered != 1 || got.Failures != 1 {
		t.Fatalf("op %+v", got)
	}
}

// Once the runtime acked a delivery, a defect in another recording field is
// reported but never turns the delivery into a repeat: the op succeeds with
// the field clamped, dropped or replaced.
func TestLaneDeliveredContractBreachStillRecordsTheDelivery(t *testing.T) {
	for name, tc := range map[string]struct {
		d     func(now time.Time) Delivered
		check func(t *testing.T, o Outcome, preCall time.Time)
	}{
		"no time": {
			d: func(time.Time) Delivered { return Delivered{Record: DeliveryRecord{Ack: AckDelivered}} },
			check: func(t *testing.T, o Outcome, preCall time.Time) {
				if !o.Result.Delivery.At.Equal(preCall) {
					t.Fatalf("at %s, want the call time %s", o.Result.Delivery.At, preCall)
				}
			},
		},
		"before the call": {
			d: func(now time.Time) Delivered {
				return Delivered{Record: DeliveryRecord{At: now.Add(-time.Second), Ack: AckDelivered}}
			},
			check: func(t *testing.T, o Outcome, preCall time.Time) {
				if !o.Result.Delivery.At.Equal(preCall) {
					t.Fatalf("at %s, want the call time %s", o.Result.Delivery.At, preCall)
				}
			},
		},
		"degradation not a token": {
			d: func(now time.Time) Delivered {
				return Delivered{Record: DeliveryRecord{At: now, Ack: AckDelivered}, Degradation: "Turn Start Unobserved"}
			},
			check: func(t *testing.T, o Outcome, _ time.Time) {
				if o.Degradation != DegradationExecutorContract {
					t.Fatalf("degradation %q", o.Degradation)
				}
			},
		},
		"via with control characters": {
			d: func(now time.Time) Delivered {
				return Delivered{Record: DeliveryRecord{At: now, Ack: AckDelivered}, Via: "bridge\x00"}
			},
			check: func(t *testing.T, o Outcome, _ time.Time) {
				if o.Result.Via != "" || o.Degradation != DegradationExecutorContract {
					t.Fatalf("via %q degradation %q", o.Result.Via, o.Degradation)
				}
			},
		},
		"turn id with surrounding space": {
			d: func(now time.Time) Delivered {
				return Delivered{Record: DeliveryRecord{At: now, Ack: AckDelivered, TurnID: " t-1 "}}
			},
			check: func(t *testing.T, o Outcome, _ time.Time) {
				if o.Result.Turn != "" || o.Result.TurnLink == LinkExact {
					t.Fatalf("turn %q link %s", o.Result.Turn, o.Result.TurnLink)
				}
			},
		},
		"message id too long": {
			d: func(now time.Time) Delivered {
				return Delivered{Record: DeliveryRecord{At: now, Ack: AckDelivered, MessageID: strings.Repeat("m", maxMessageIDLen+1)}}
			},
			check: func(t *testing.T, o Outcome, _ time.Time) {
				if o.Result.Delivery.MessageID != "" {
					t.Fatalf("message id %q", o.Result.Delivery.MessageID)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, laneMsg("p", RolePrompt, "deploy now"))
			exec := newFakeExec(h.clk)
			exec.deliver = func(Delivery) DeliveryResult { return tc.d(h.clk.Now()) }
			preCall := h.clk.Now()
			l := h.lane(t, "ctl")
			res, err := l.Pass(context.Background(), view(), exec)
			if err == nil {
				t.Fatal("contract breach not reported")
			}
			if res.Done != DoneAccepted {
				t.Fatalf("pass %+v", res)
			}
			got := h.get(t, op.ID)
			if got.Outcome.Arm != ArmSucceeded || got.MaybeDelivered != 0 || got.Failures != 0 {
				t.Fatalf("op %s %+v maybe %d failures %d", got.Status, got.Outcome, got.MaybeDelivered, got.Failures)
			}
			tc.check(t, got.Outcome, preCall)
			h.clk.Advance(ExecutorErrorBackoff + time.Second)
			mustPass(t, l, view(), exec)
			if n := len(exec.delivered()); n != 1 {
				t.Fatalf("the runtime got the input %d times", n)
			}
		})
	}
}

// The same holds for a control result: a settled interrupt is never sent
// again for a cosmetic defect.
func TestLaneSettledContractBreachStillSettles(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, interruptReq())
	exec := newFakeExec(h.clk)
	exec.interrupt = func(InterruptCall) ControlResult { return Settled{Turn: " t ", Via: "x", Degradation: "Not A Token"} }
	if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec); err == nil {
		t.Fatal("contract breach not reported")
	}
	got := h.get(t, op.ID)
	if got.Outcome.Arm != ArmSucceeded || got.Outcome.Degradation != DegradationExecutorContract || got.Outcome.Result.Turn != "" {
		t.Fatalf("op %+v", got.Outcome)
	}
}

func TestLaneLeaseTooLongForTheConversationIsRefused(t *testing.T) {
	h := newLaneHarness(t)
	long := strings.Repeat("c", maxConversationLen)
	h.propose(t, laneMsg("p", RolePrompt, "x", func(r *Request) { r.Conversation = long }))
	v := view()
	v.ID = long
	h.clk.Advance(123456789) // a deadline with nanoseconds pushes the lease past 255 runes
	if _, err := h.lane(t, "ctl").Pass(context.Background(), v, newFakeExec(h.clk)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("pass = %v", err)
	}
	if n := len(h.batcher.batches()); n != 0 {
		t.Fatalf("%d batches written", n)
	}
}

func TestNewLaneRefusals(t *testing.T) {
	h := newLaneHarness(t)
	plain, err := NewStore(beads.SessionStore{Store: h.mem}, h.clk, WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLane(plain, "ctl", h.clk); !errors.Is(err, beads.ErrGuardedBatchUnsupported) {
		t.Errorf("NewLane(non-lane store) = %v", err)
	}
	for _, host := range []string{"", "CTL", "1ctl", "ctl.x", strings.Repeat("a", 17)} {
		if _, err := NewLane(h.store, host, h.clk); err == nil {
			t.Errorf("NewLane accepted host %q", host)
		}
	}
	if _, err := NewLane(h.store, "ctl", nil); err == nil {
		t.Error("NewLane accepted a nil clock")
	}
}

// An expired op that planTake closes keeps the expiry bookkeeping every
// planRules close of an expired op records: maybe_delivered, failures and
// last_error.
func TestLanePlanTakeClosesOfExpiredOpsKeepMaybeDelivered(t *testing.T) {
	ifIdle := laneMsg("p", RolePrompt, "x", withWhen(WhenIfIdle))
	steerFail := laneMsg("s", RolePrompt, "x", withWhen(WhenSteer), func(r *Request) {
		r.Policy.Message.OnUnsupported = FallbackFail
	})
	respond := Request{
		Conversation: "gc-1", Kind: KindRespond, Executor: ExecutorLane, ClientKey: "r1",
		Producer: Producer{Principal: "operator", Actor: ActorOperator}, Payload: "allow",
		Scope: Scope{Kind: ScopeSession, Generation: 1, Interaction: "perm-1"},
	}
	for name, tc := range map[string]struct {
		req    Request
		before func(*Probe)
		after  func(*Probe)
		want   string
	}{
		"if_idle busy":      {req: ifIdle, after: func(p *Probe) { p.Boundary = TriNo }, want: ReasonBusy},
		"steer unsupported": {req: steerFail, after: func(p *Probe) { p.Boundary, p.Caps.Steer = TriNo, false }, want: ReasonUnsupported},
		"respond unsupported": {
			req: respond, before: func(p *Probe) { p.Caps.Respond = true },
			after: func(p *Probe) { p.Caps.Respond = false }, want: ReasonUnsupported,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, tc.req)
			exec := newFakeExec(h.clk)
			if tc.before != nil {
				tc.before(&exec.probe)
			}
			a := h.lane(t, "ctl")
			a.hooks.at = crashAt(stageAfterEffect)
			if _, err := a.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
				t.Fatalf("pass = %v", err)
			}
			h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
			tc.after(&exec.probe)
			mustPass(t, h.lane(t, "cli"), view(), exec)
			got := h.get(t, op.ID)
			if got.Status != StatusClosed || got.Outcome.Reason != tc.want {
				t.Fatalf("op %s %+v", got.Status, got.Outcome)
			}
			if got.MaybeDelivered != got.Attempt || got.Failures != 1 || got.LastError != lastErrorLeaseExpired {
				t.Fatalf("expiry bookkeeping lost: attempt %d maybe %d failures %d last_error %q",
					got.Attempt, got.MaybeDelivered, got.Failures, got.LastError)
			}
		})
	}
}

// A canceled context ends the pass with the context's error, not with
// ErrContention after re-planning casAttempts times.
func TestLaneCanceledContextIsReported(t *testing.T) {
	t.Run("before the pass", func(t *testing.T) {
		h := newLaneHarness(t)
		h.propose(t, laneMsg("p", RolePrompt, "x"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := h.lane(t, "ctl").Pass(ctx, view(), newFakeExec(h.clk))
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrContention) {
			t.Fatalf("pass = %v", err)
		}
		if n := len(h.batcher.batches()); n != 0 {
			t.Fatalf("%d batches written", n)
		}
	})
	t.Run("during the pass", func(t *testing.T) {
		h := newLaneHarness(t)
		op := h.propose(t, laneMsg("p", RolePrompt, "x"))
		ctx, cancel := context.WithCancel(context.Background())
		h.batcher.setFault(func(int, beads.GuardedBatch) (bool, error) {
			cancel()
			return false, ctx.Err()
		})
		_, err := h.lane(t, "ctl").Pass(ctx, view(), newFakeExec(h.clk))
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrContention) {
			t.Fatalf("pass = %v", err)
		}
		if n := len(h.batcher.batches()); n != 1 {
			t.Fatalf("%d batches written", n)
		}
		if got := h.get(t, op.ID); got.Status != StatusSubmitted && got.Status != StatusAccepted {
			t.Fatalf("op %s", got.Status)
		}
	})
	t.Run("in the rule closes", func(t *testing.T) {
		h := newLaneHarness(t)
		h.propose(t, laneMsg("p", RolePrompt, "x"))
		h.clk.Advance(72 * time.Hour) // past start_by: the rules close it
		ctx, cancel := context.WithCancel(context.Background())
		h.batcher.setFault(func(int, beads.GuardedBatch) (bool, error) {
			cancel()
			return false, ctx.Err()
		})
		if _, err := h.lane(t, "ctl").Pass(ctx, view(), newFakeExec(h.clk)); !errors.Is(err, context.Canceled) {
			t.Fatalf("pass = %v", err)
		}
		if n := len(h.batcher.batches()); n != 1 {
			t.Fatalf("%d batches written", n)
		}
	})
}
