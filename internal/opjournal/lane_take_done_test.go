package opjournal

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

// A failed attempt does not count toward the transient backoff: after one,
// the first transient release backs off TransientBackoffBase (attempt 2 −
// failures 1).
func TestLaneTransientBackoffSkipsFailedAttempts(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	l := h.lane(t, "ctl")
	exec.deliver = func(Delivery) DeliveryResult { return NotDelivered{Cause: errors.New("boom")} }
	mustPass(t, l, view(), exec)
	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	exec.deliver = func(Delivery) DeliveryResult { return NotDelivered{Transient: true} }
	mustPass(t, l, view(), exec)
	cur := h.get(t, op.ID)
	if backoff := cur.RetryAt.Sub(h.clk.Now()); cur.Attempt != 2 || cur.Failures != 1 || backoff != TransientBackoffBase {
		t.Fatalf("attempt %d, failures %d, backoff %s; want 2, 1, %s", cur.Attempt, cur.Failures, backoff, TransientBackoffBase)
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

// A Take that did not commit while another lane took the conversation
// resolves as lost: it leaves the other lane's lease on the runtime row (I3).
func TestLaneAmbiguousTakeLeavesAnotherLanesLease(t *testing.T) {
	h := newLaneHarness(t)
	h.propose(t, laneMsg("a", RolePrompt, "x"))
	mustPass(t, h.lane(t, "ctl"), view(), newFakeExec(h.clk)) // the runtime row now exists
	h.clk.Advance(time.Millisecond)
	h.propose(t, laneMsg("b", RolePrompt, "y"))
	n0 := len(h.batcher.batches())
	var other Lease
	nested := -1
	h.batcher.setFault(func(n int, _ beads.GuardedBatch) (bool, error) {
		switch {
		case n == n0: // ctl's Take does not commit; meanwhile lane other takes b
			nested = 0
			_, _ = h.lane(t, "other").Pass(context.Background(), view(), newFakeExec(h.clk))
			other = h.runtime(t).Lease
			nested = -1
			return false, errLinkDropped
		case nested >= 0: // other's Take commits; its Done never lands
			nested++
			if nested == 1 {
				return false, nil
			}
			return false, errLinkDropped
		}
		return false, nil
	})
	mustPass(t, h.lane(t, "ctl"), view(), newFakeExec(h.clk))
	if other.IsZero() {
		t.Fatal("setup: lane other holds no lease")
	}
	if got := h.runtime(t).Lease; got != other {
		t.Fatalf("row lease %v after ctl's ambiguous Take, want lane other's %v", got, other)
	}
}

// A rule close batch that ends unknown re-plans: one that did not commit is
// written again and reported; one that committed is not reported, since the
// re-read no longer sees the op it closed (PassResult.Closed).
func TestLaneAmbiguousRuleClose(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, laneMsg("old", RolePrompt, "x", func(r *Request) { r.StartBy = t0.Add(time.Minute) }))
			h.clk.Advance(2 * time.Minute)
			n0 := len(h.batcher.batches())
			h.batcher.setFault(func(n int, _ beads.GuardedBatch) (bool, error) {
				if n == n0 {
					return commit, errLinkDropped
				}
				return false, nil
			})
			res := mustPass(t, h.lane(t, "ctl"), view(), newFakeExec(h.clk))
			if got := h.get(t, op.ID); got.Status != StatusClosed || got.Outcome.Reason != ReasonExpiredBeforeStart {
				t.Fatalf("op %s/%s{%s}, want closed{%s}", got.Status, got.Outcome.Arm, got.Outcome.Reason, ReasonExpiredBeforeStart)
			}
			var want []ClosedOp
			if !commit {
				want = []ClosedOp{{ID: op.ID, Arm: ArmFailed, Reason: ReasonExpiredBeforeStart}}
			}
			if !slices.Equal(res.Closed, want) {
				t.Fatalf("closed %v, want %v", res.Closed, want)
			}
		})
	}
}

// TestLaneAmbiguousTakeRereadRetries: the re-reads that resolve an unknown
// Take retry a failed read on Done's backoff while the lease lasts, and the
// error they end with names the batch's cause too.
func TestLaneAmbiguousTakeRereadRetries(t *testing.T) {
	rowID, err := RuntimeRowID("gc", "gc-1")
	if err != nil {
		t.Fatal(err)
	}
	// ambiguous commits the Take, reports a dropped link and from then on
	// fails the reads fail says to.
	ambiguous := func(h *laneHarness, fs *flakyGetStore, fail func(id string) bool) {
		h.batcher.setFault(func(n int, _ beads.GuardedBatch) (bool, error) {
			if n > 0 {
				return false, nil
			}
			fs.setFail(fail)
			return true, errLinkDropped
		})
	}
	for name, target := range map[string]func(Op) string{
		"runtime row": func(Op) string { return rowID },
		"op row":      func(op Op) string { return op.ID },
	} {
		t.Run(name+" fails once", func(t *testing.T) {
			h, fs := newLaneHarnessFlaky(t)
			op := h.propose(t, laneMsg("p", RolePrompt, "x"))
			id, fails := target(op), 0
			ambiguous(h, fs, func(got string) bool {
				if got != id || fails > 0 {
					return false
				}
				fails++
				return true
			})
			exec := newFakeExec(h.clk)
			res := mustPass(t, h.lane(t, "ctl"), view(), exec)
			if fails != 1 || res.Done != DoneAccepted || len(exec.delivered()) != 1 {
				t.Fatalf("%d failed reads; pass %+v, %d deliveries", fails, res, len(exec.delivered()))
			}
			if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded || got.Attempt != 1 {
				t.Fatalf("op %+v", got)
			}
		})
	}
	t.Run("op row keeps failing", func(t *testing.T) {
		h, fs := newLaneHarnessFlaky(t)
		op := h.propose(t, laneMsg("p", RolePrompt, "x"))
		fails := 0
		ambiguous(h, fs, func(got string) bool {
			if got != op.ID {
				return false
			}
			fails++
			return true
		})
		exec := newFakeExec(h.clk)
		l := h.lane(t, "ctl")
		var slept []time.Duration
		l.sleep = func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}
		_, err := l.Pass(context.Background(), view(), exec)
		if !errors.Is(err, errLinkDropped) || !errors.Is(err, errFlakyRead) || fails != casAttempts || len(exec.delivered()) != 0 {
			t.Fatalf("Pass = %v after %d failed reads, %d deliveries", err, fails, len(exec.delivered()))
		}
		ms := time.Millisecond
		if want := []time.Duration{100 * ms, 200 * ms, 400 * ms, 800 * ms, 1600 * ms, 2 * time.Second, 2 * time.Second}; !slices.Equal(slept, want) {
			t.Fatalf("slept %v between re-reads, want %v", slept, want)
		}
	})
	t.Run("canceled mid re-read", func(t *testing.T) {
		h, fs := newLaneHarnessFlaky(t)
		op := h.propose(t, laneMsg("p", RolePrompt, "x"))
		fails := 0
		ambiguous(h, fs, func(got string) bool {
			if got != op.ID {
				return false
			}
			fails++
			return true
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		exec := newFakeExec(h.clk)
		l := h.lane(t, "ctl")
		l.sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}
		_, err := l.Pass(ctx, view(), exec)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, errLinkDropped) || !errors.Is(err, errFlakyRead) || fails != 1 || len(exec.delivered()) != 0 {
			t.Fatalf("Pass = %v after %d failed reads, %d deliveries; want the cancel to end the re-reads after one", err, fails, len(exec.delivered()))
		}
	})
	t.Run("past the lease", func(t *testing.T) {
		h, fs := newLaneHarnessFlaky(t)
		op := h.propose(t, laneMsg("p", RolePrompt, "x"))
		fails := 0
		ambiguous(h, fs, func(got string) bool {
			if got != op.ID {
				return false
			}
			fails++
			h.clk.Advance(DefaultLeaseTTL)
			return true
		})
		_, err := h.lane(t, "ctl").Pass(context.Background(), view(), newFakeExec(h.clk))
		if !errors.Is(err, errLinkDropped) || !errors.Is(err, errFlakyRead) || fails != 1 {
			t.Fatalf("Pass = %v after %d failed reads, want one: the lease is over", err, fails)
		}
	})
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
			raised := h.raise(t, "perm-1")
			op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
			f := Fact{Kind: FactInteractionResolved, Conversation: "gc-1", Generation: 1, Interaction: "perm-1#1", Basis: FactBasisObserved, At: t0}
			exec := newFakeExec(h.clk)
			exec.probe.Caps.Respond, exec.probe.Pending = true, "perm-1"
			if collide {
				// Another writer records the resolution while the effect
				// runs: Done's create is refused and finds the same
				// occurrence.
				exec.onEffect = func() {
					if _, err := h.store.RecordFact(f); err != nil {
						t.Error(err)
					}
				}
			}
			res := mustPass(t, h.lane(t, "ctl"), view(), exec)
			if res.Took != TakeControl || res.Done != DoneAccepted || len(exec.responds) != 1 {
				t.Fatalf("pass %+v", res)
			}
			if c := exec.responds[0]; c.Interaction != "perm-1#1" || c.RequestID != "perm-1" || c.Response.Action != "approve" {
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
	op := h.propose(t, laneRespond(t, h.raise(t, "perm-1"), "operator", RespondPayload{Action: "approve"}))
	exec := newFakeExec(h.clk)
	exec.probe.Pending = "perm-1"
	mustPass(t, h.lane(t, "ctl"), view(), exec)
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
	if want := h.clk.Now().Add(TransientBackoffBase); res.Took != TakeNone || res.Waits[0].Reason != WaitIncarnation || !res.Next.Equal(want) {
		t.Fatalf("stale view pass %+v, want next %s", res, want)
	}
}

// TestLaneIncarnationMismatchRetriesOnATimer: no re-pass clears a mismatch
// between the view and the row or the probe's fence, so the wait retries on
// a timer ahead of now, and a host sleeping until Next does not spin.
func TestLaneIncarnationMismatchRetriesOnATimer(t *testing.T) {
	waitsOnTimer := func(t *testing.T, h *laneHarness, l *ConversationLane, exec Executor) {
		t.Helper()
		for range 3 {
			now := h.clk.Now()
			res := mustPass(t, l, view(), exec)
			if res.Took != TakeNone || len(res.Waits) != 1 || res.Waits[0].Reason != WaitIncarnation ||
				!res.Next.After(now) || !res.Waits[0].Until.Equal(res.Next) {
				t.Fatalf("pass at %s: %+v", now, res)
			}
			h.clk.Advance(res.Next.Sub(now))
		}
	}
	t.Run("view behind the row", func(t *testing.T) {
		h := newLaneHarness(t)
		h.propose(t, laneMsg("first", RoleNotice, "x"))
		exec := newFakeExec(h.clk)
		v2 := view()
		v2.Generation = 2
		mustPass(t, h.lane(t, "ctl"), v2, exec)
		late := h.propose(t, laneMsg("late", RoleNotice, "y"))
		l := h.lane(t, "cli")
		waitsOnTimer(t, h, l, exec)
		if res := mustPass(t, l, v2, exec); res.Took != TakeContent || !slices.Equal(res.Taken, []string{late.ID}) {
			t.Fatalf("pass with the current view %+v", res)
		}
	})
	t.Run("probe fence ahead of the view", func(t *testing.T) {
		h := newLaneHarness(t)
		op := h.propose(t, laneMsg("p", RolePrompt, "x"))
		exec := newFakeExec(h.clk)
		exec.probe.Fence = Fence{Conversation: "gc-1", Generation: 2, Epoch: 1}
		l := h.lane(t, "ctl")
		waitsOnTimer(t, h, l, exec)
		if exec.probes != 3 {
			t.Fatalf("%d probes over 3 passes", exec.probes)
		}
		exec.probe.Fence = Fence{}
		if res := mustPass(t, l, view(), exec); res.Took != TakeContent || !slices.Equal(res.Taken, []string{op.ID}) {
			t.Fatalf("pass once the fence agrees %+v", res)
		}
	})
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

// TestLaneLostTakeReportsTheFreshPlan: a pass that loses its Take re-plans
// from a fresh read, and Busy and More come from that plan, not from the
// lost one.
func TestLaneLostTakeReportsTheFreshPlan(t *testing.T) {
	for name, boundary := range map[string]Tri{"busy": TriNo, "more": TriYes} {
		t.Run(name, func(t *testing.T) {
			h := newLaneHarness(t)
			h.propose(t, interruptReq())
			h.propose(t, laneMsg("n", RoleNotice, "x"))
			exec := newFakeExec(h.clk)
			exec.probe.Boundary = boundary
			winner := h.lane(t, "winner")
			taken, release := make(chan struct{}), make(chan struct{})
			winner.hooks.at = func(s laneStage) error {
				if s == stageAfterTake {
					close(taken)
					<-release
				}
				return nil
			}
			var wres PassResult
			var werr error
			wdone := make(chan struct{})
			loser := h.lane(t, "loser")
			first := true
			loser.hooks.at = func(s laneStage) error {
				if s != stageAfterRead || !first {
					return nil
				}
				// The winner takes the interrupt this pass read as open.
				first = false
				go func() {
					defer close(wdone)
					wres, werr = winner.Pass(context.Background(), view(), exec)
				}()
				select {
				case <-taken:
				case <-wdone:
				}
				return nil
			}
			res, err := loser.Pass(context.Background(), view(), exec)
			close(release)
			<-wdone
			if err != nil || werr != nil || wres.Took != TakeControl {
				t.Fatalf("loser: %v; winner %+v, %v", err, wres, werr)
			}
			if res.Took != TakeNone || res.Busy || res.More || len(res.Waits) != 2 {
				t.Fatalf("loser %+v", res)
			}
			for _, w := range res.Waits {
				if w.Reason != WaitInFlight {
					t.Fatalf("loser waits %+v", res.Waits)
				}
			}
		})
	}
}

// TestLaneLostTakeProbesAgain: a probe answers for the state its pass read,
// so a pass that re-reads, after losing its Take or a close planTake
// decided, probes again.
func TestLaneLostTakeProbesAgain(t *testing.T) {
	t.Run("lost take", func(t *testing.T) {
		h := newLaneHarness(t)
		first := h.propose(t, laneMsg("n1", RoleNotice, "x"))
		exec := newFakeExec(h.clk)
		var second Op
		stale := &staleProbeExec{fakeExec: exec, during: func() {
			// Another host delivers the first notice, a second one
			// arrives and the session starts a turn.
			mustPass(t, h.lane(t, "winner"), view(), newFakeExec(h.clk))
			second = h.propose(t, laneMsg("n2", RoleNotice, "y"))
			exec.mu.Lock()
			exec.probe.Boundary = TriNo
			exec.mu.Unlock()
		}}
		res := mustPass(t, h.lane(t, "loser"), view(), stale)
		if n := len(exec.delivered()); n != 0 || exec.probes != 2 {
			t.Fatalf("%d deliveries after %d probes", n, exec.probes)
		}
		if res.Took != TakeNone || !res.Busy || waitsOf(res.Waits)[second.ID] != WaitBusy {
			t.Fatalf("pass %+v", res)
		}
		if got := h.get(t, first.ID); got.Outcome.Arm != ArmSucceeded || got.Attempt != 1 {
			t.Fatalf("first %+v", got)
		}
	})
	t.Run("refused close", func(t *testing.T) {
		h := newLaneHarness(t)
		op := h.propose(t, laneMsg("n", RoleNotice, "x", withWhen(WhenIfIdle)))
		exec := newFakeExec(h.clk)
		exec.probe.Boundary = TriNo
		h.batcher.setFault(func(n int, b beads.GuardedBatch) (bool, error) {
			if n > 0 {
				return false, nil
			}
			// The busy close of the if_idle op is refused, and the session
			// goes idle before the pass re-reads.
			exec.mu.Lock()
			exec.probe.Boundary = TriYes
			exec.mu.Unlock()
			return false, &beads.BatchItemError{ID: b.Items[0].Update.ID, Err: errors.New("revision moved")}
		})
		res := mustPass(t, h.lane(t, "ctl"), view(), exec)
		if res.Took != TakeContent || len(exec.delivered()) != 1 || exec.probes != 2 {
			t.Fatalf("pass %+v, %d deliveries after %d probes", res, len(exec.delivered()), exec.probes)
		}
		if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded {
			t.Fatalf("op %+v", got)
		}
	})
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
		"time past year 9999": {
			d: func(time.Time) Delivered {
				return Delivered{Record: DeliveryRecord{At: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), Ack: AckDelivered}}
			},
			check: func(t *testing.T, o Outcome, preCall time.Time) {
				if !o.Result.Delivery.At.Equal(preCall) || o.Degradation != DegradationExecutorContract {
					t.Fatalf("at %s degradation %q, want the call time %s", o.Result.Delivery.At, o.Degradation, preCall)
				}
			},
		},
		"message time past year 9999": {
			d: func(now time.Time) Delivered {
				return Delivered{Record: DeliveryRecord{At: now, Ack: AckDelivered, MessageAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}
			},
			check: func(t *testing.T, o Outcome, _ time.Time) {
				if !o.Result.Delivery.MessageAt.IsZero() || o.Degradation != DegradationExecutorContract {
					t.Fatalf("message at %s degradation %q", o.Result.Delivery.MessageAt, o.Degradation)
				}
			},
		},
		"message time before year 0": {
			d: func(now time.Time) Delivered {
				return Delivered{Record: DeliveryRecord{At: now, Ack: AckDelivered, MessageAt: time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)}}
			},
			check: func(t *testing.T, o Outcome, _ time.Time) {
				if !o.Result.Delivery.MessageAt.IsZero() || o.Degradation != DegradationExecutorContract {
					t.Fatalf("message at %s degradation %q", o.Result.Delivery.MessageAt, o.Degradation)
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

// The longest conversation id Propose accepts leaves room for the longest
// lane lease around it: the largest generation, the longest host and a
// deadline with nine fractional digits make a lease of exactly bd's assignee
// limit, and the Take that writes it goes through.
func TestLaneLeaseAtTheConversationLimitFits(t *testing.T) {
	h := newLaneHarness(t)
	long := strings.Repeat("c", maxConversationLen)
	if _, _, err := h.store.Propose(laneMsg("p", RolePrompt, "x", func(r *Request) { r.Conversation = long + "c" })); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Propose(%d-byte conversation) = %v, want ErrInvalidRequest", maxConversationLen+1, err)
	}
	h.propose(t, laneMsg("p", RolePrompt, "x", func(r *Request) { r.Conversation = long }))
	v := view()
	v.ID, v.Generation = long, math.MaxInt64
	h.clk.Advance(123456789) // a deadline with nanoseconds
	exec := newFakeExec(h.clk)
	res := mustPass(t, h.lane(t, strings.Repeat("h", maxLaneHostLen)), v, exec)
	if res.Took != TakeContent || res.Done != DoneAccepted {
		t.Fatalf("pass %+v", res)
	}
	if n := utf8.RuneCountInString(exec.delivered()[0].Ops[0].Lease.String()); n != maxLeaseLen {
		t.Fatalf("lease is %d runes, want exactly %d", n, maxLeaseLen)
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
	for _, host := range []string{"", "CTL", "1ctl", "ctl.x", strings.Repeat("a", maxLaneHostLen+1)} {
		if _, err := NewLane(h.store, host, h.clk); err == nil {
			t.Errorf("NewLane accepted host %q", host)
		}
	}
	if _, err := NewLane(h.store, "ctl", nil); err == nil {
		t.Error("NewLane accepted a nil clock")
	}
}

// A fresh op the probe rules out closes (TestPlanTakeWhenTable), recording
// that it never ran. Once an attempt may have reached the executor, because
// its lease expired or it was released after an Unknown result, the op waits
// instead, busy, with its bookkeeping kept: it is replayed when the probe
// allows it, or start_by closes it failed{deadline_in_flight}. A respond never
// waits so: it is at most once (TestLaneRespondIsNeverReplayed).
func TestLaneMaybeDeliveredOpWaitsForTheProbe(t *testing.T) {
	kinds := []struct {
		name string
		req  Request
		deny func(*Probe)
		wait WaitReason
	}{
		{"if_idle", laneMsg("p", RolePrompt, "x", withWhen(WhenIfIdle)), func(p *Probe) { p.Boundary = TriNo }, WaitBusy},
		{"steer fallback_fail", laneMsg("s", RolePrompt, "x", withWhen(WhenSteer), func(r *Request) {
			r.Policy.Message.OnUnsupported = FallbackFail
		}), func(p *Probe) { p.Boundary, p.Caps.Steer = TriNo, false }, WaitBusy},
	}
	ways := []struct {
		name string
		// attempt runs the attempt that may reach the executor, then lets
		// the clock pass until the op can be taken again.
		attempt func(t *testing.T, h *laneHarness, exec *fakeExec)
		lastErr func(string) bool
	}{
		{"lease expired", func(t *testing.T, h *laneHarness, exec *fakeExec) {
			a := h.lane(t, "ctl")
			a.hooks.at = crashAt(stageAfterEffect)
			if _, err := a.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
				t.Fatalf("pass = %v", err)
			}
			h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
		}, func(s string) bool { return s == lastErrorLeaseExpired }},
		{"released after unknown", func(t *testing.T, h *laneHarness, exec *fakeExec) {
			exec.deliver = func(Delivery) DeliveryResult { return Unknown{Cause: errors.New("bridge timeout")} }
			mustPass(t, h.lane(t, "ctl"), view(), exec)
			exec.deliver = nil
			h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
		}, func(s string) bool { return strings.HasPrefix(s, lastErrorUnknown+": ") }},
	}
	for _, k := range kinds {
		effects := func(exec *fakeExec) int { return len(exec.delivered()) }
		for _, w := range ways {
			for _, replayed := range []bool{true, false} {
				ending := "start_by"
				if replayed {
					ending = "replayed"
				}
				t.Run(k.name+"/"+w.name+"/"+ending, func(t *testing.T) {
					h := newLaneHarness(t)
					op := h.propose(t, k.req)
					exec := newFakeExec(h.clk)
					allow := exec.probe
					w.attempt(t, h, exec)
					if n := effects(exec); n != 1 {
						t.Fatalf("%d effects on the first attempt", n)
					}

					k.deny(&exec.probe)
					res := mustPass(t, h.lane(t, "cli"), view(), exec)
					got := h.get(t, op.ID)
					if got.Status == StatusClosed {
						t.Fatalf("an op that may have been delivered closed %+v", got.Outcome)
					}
					if res.Took != TakeNone || waitsOf(res.Waits)[op.ID] != k.wait || res.Busy != (k.wait == WaitBusy) || effects(exec) != 1 {
						t.Fatalf("pass %+v after %d effects; want a %s wait", res, effects(exec), k.wait)
					}
					if !res.Next.Equal(got.StartBy) {
						t.Fatalf("next %s, want start_by %s", res.Next, got.StartBy)
					}

					if replayed {
						exec.probe = allow
						res = mustPass(t, h.lane(t, "cli"), view(), exec)
						if !slices.Equal(res.Taken, []string{op.ID}) || effects(exec) != 2 {
							t.Fatalf("took %v, %d effects", res.Taken, effects(exec))
						}
						got = h.get(t, op.ID)
						if got.Outcome.Arm != ArmSucceeded || got.Attempt != 2 {
							t.Fatalf("op attempt %d %+v", got.Attempt, got.Outcome)
						}
						if ds := exec.delivered(); !ds[1].Redelivery || ds[1].Key != ds[0].Key {
							t.Fatalf("replay redelivery %v key %s, first key %s", ds[1].Redelivery, ds[1].Key, ds[0].Key)
						}
					} else {
						h.clk.Advance(got.StartBy.Sub(h.clk.Now()) + time.Nanosecond)
						mustPass(t, h.lane(t, "cli"), view(), exec)
						got = h.get(t, op.ID)
						if got.Status != StatusClosed || got.Outcome.Arm != ArmFailed || got.Outcome.Reason != ReasonDeadlineInFlight {
							t.Fatalf("op past start_by: %s %+v", got.Status, got.Outcome)
						}
					}
					if got.MaybeDelivered != 1 || got.Failures != 1 || !w.lastErr(got.LastError) {
						t.Fatalf("bookkeeping lost: maybe %d failures %d last_error %q", got.MaybeDelivered, got.Failures, got.LastError)
					}
				})
			}
		}
	}
}

// applyLocal leaves a row as the store's own applier does, an emptied key
// included, so the row a Take keeps for Done is the row it wrote.
func TestApplyLocalMatchesTheStore(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc"}
	b, err := mem.Create(beads.Bead{Title: "row", Metadata: map[string]string{"gc.a": "1", "gc.b": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	assignee, status := "conversation@gc-1#1#ctl.n@2026-10-13T12:02:00Z", "in_progress"
	opts := beads.UpdateOpts{Metadata: map[string]string{"gc.a": "", "gc.c": "3"}, Assignee: &assignee, Status: &status}
	want := applyLocal(b, opts)
	rev := b.Revision
	item := beads.GuardedItem{Update: &beads.GuardedUpdate{ID: b.ID, Opts: opts, ExpectedRevision: &rev}}
	if _, err := mem.ApplyGuarded(context.Background(), beads.GuardedBatch{Reason: "test", Items: []beads.GuardedItem{item}}); err != nil {
		t.Fatal(err)
	}
	got, err := mem.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got.Metadata, want.Metadata) || got.Assignee != want.Assignee || got.Status != want.Status {
		t.Fatalf("store row metadata %v assignee %q status %q; applyLocal %v %q %q",
			got.Metadata, got.Assignee, got.Status, want.Metadata, want.Assignee, want.Status)
	}
}

// Done re-reads its items after a refusal only when the refused row is one
// of them; a refusal that names no row is never taken for one of its own.
func TestRefusedByPending(t *testing.T) {
	pending := []*doneItem{{id: "gc-op-a"}, {id: "gc-row-1"}}
	refusal := func(id string) error {
		return fmt.Errorf("applying: %w", &beads.BatchItemError{Index: 1, ID: id, Err: errors.New("revision moved")})
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"a pending row", refusal("gc-row-1"), true},
		{"another row", refusal("gc-op-z"), false},
		{"no row named", errors.New("refused"), false},
	} {
		if got := refusedByPending(tc.err, pending); got != tc.want {
			t.Errorf("%s: refusedByPending(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
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

// Done outlives a pass canceled during its effect: the effect ran, so Done
// still records it.
func TestLaneDoneOutlivesACanceledPass(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := newFakeExec(h.clk)
	ex.onEffect = cancel
	res, err := h.lane(t, "ctl").Pass(ctx, view(), ex)
	if err != nil || res.Done != DoneAccepted {
		t.Fatalf("pass %+v, %v", res, err)
	}
	if got := h.get(t, op.ID); got.Status != StatusClosed || got.Outcome.Arm != ArmSucceeded {
		t.Fatalf("op %s/%s after a pass canceled during its effect, want closed/succeeded", got.Status, got.Outcome.Arm)
	}
	if n := len(ex.delivered()); n != 1 {
		t.Fatalf("%d deliveries", n)
	}
}

// While an op is in flight, every candidate is reported waiting in_flight
// until the live lease can end.
func TestLaneCandidatesWaitWhileAnOpIsInFlight(t *testing.T) {
	h := newLaneHarness(t)
	first := h.propose(t, laneMsg("a", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	a := h.lane(t, "ctl")
	a.hooks.at = crashAt(stageAfterEffect)
	if _, err := a.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
		t.Fatalf("pass = %v", err)
	}
	until := h.get(t, first.ID).Lease.Deadline.Add(LeaseSkew)
	h.clk.Advance(time.Second)
	later := h.propose(t, laneMsg("b", RolePrompt, "y"))
	res := mustPass(t, h.lane(t, "cli"), view(), exec)
	if res.Took != TakeNone {
		t.Fatalf("took %v while an op is in flight", res.Taken)
	}
	var got *OpWait
	for i := range res.Waits {
		if res.Waits[i].ID == later.ID {
			got = &res.Waits[i]
		}
	}
	if got == nil || got.Reason != WaitInFlight || !got.Until.Equal(until) {
		t.Fatalf("waits %+v, want %s in_flight until %s", res.Waits, later.ID, until)
	}
}

// An ambiguous Take that committed is resolved from the rows as re-read: a
// cancel recorded in the window reaches the effect, and an op that closed in
// the window is dropped; the Take is then released without its effect, since
// its text names content that is no longer open.
func TestLaneAmbiguousTakeUsesTheRowsAsReRead(t *testing.T) {
	t.Run("cancel recorded", func(t *testing.T) {
		h := newLaneHarness(t)
		op := h.propose(t, laneMsg("p", RolePrompt, "x"))
		exec := newFakeExec(h.clk)
		h.batcher.setFault(func(n int, b beads.GuardedBatch) (bool, error) {
			if n != 0 {
				return false, nil
			}
			if _, err := h.mem.ApplyGuarded(context.Background(), b); err != nil {
				t.Errorf("commit: %v", err)
			}
			if res, _, err := h.store.Cancel(op.ID, "operator", "read"); err != nil || res != CancelRequested {
				t.Errorf("Cancel = %s, %v", res, err)
			}
			return false, errLinkDropped
		})
		res := mustPass(t, h.lane(t, "ctl"), view(), exec)
		ds := exec.delivered()
		if res.Done != DoneAccepted || len(ds) != 1 || !ds[0].Ops[0].Cancel.Recorded() {
			t.Fatalf("pass %+v, deliveries %+v", res, ds)
		}
	})
	t.Run("op closed", func(t *testing.T) {
		h := newLaneHarness(t)
		a := h.propose(t, laneMsg("a", RoleNotice, "x"))
		h.clk.Advance(time.Millisecond)
		b := h.propose(t, laneMsg("b", RoleNotice, "y"))
		exec := newFakeExec(h.clk)
		h.batcher.setFault(func(n int, batch beads.GuardedBatch) (bool, error) {
			if n != 0 {
				return false, nil
			}
			if _, err := h.mem.ApplyGuarded(context.Background(), batch); err != nil {
				t.Errorf("commit: %v", err)
			}
			if err := h.mem.Close(b.ID); err != nil {
				t.Errorf("Close: %v", err)
			}
			return false, errLinkDropped
		})
		res, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec)
		if err != nil || res.Done != DoneReleased || !slices.Equal(res.Taken, []string{a.ID}) {
			t.Fatalf("pass = %+v, %v", res, err)
		}
		if n := len(exec.delivered()); n != 0 {
			t.Fatalf("the effect ran %d times with a closed op's text", n)
		}
		got := h.get(t, a.ID)
		if got.Status != StatusAccepted || !got.Lease.IsZero() || got.MaybeDelivered != 0 || got.Failures != 0 {
			t.Fatalf("op a %+v", got)
		}
		if !h.runtime(t).Lease.IsZero() {
			t.Fatal("the cell is still held")
		}
		if res := mustPass(t, h.lane(t, "ctl"), view(), exec); res.Took != TakeNone {
			// Released with a transient backoff: not taken at once.
			t.Fatalf("took %v during the backoff", res.Taken)
		}
	})
}

// When every op of an ambiguous Take closed in the window, Done only clears
// the cell.
func TestLaneAmbiguousTakeWithEveryOpClosed(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	h.batcher.setFault(func(n int, batch beads.GuardedBatch) (bool, error) {
		if n != 0 {
			return false, nil
		}
		if _, err := h.mem.ApplyGuarded(context.Background(), batch); err != nil {
			t.Errorf("commit: %v", err)
		}
		if err := h.mem.Close(op.ID); err != nil {
			t.Errorf("Close: %v", err)
		}
		return false, errLinkDropped
	})
	res, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec)
	if err != nil || res.Done != DoneReleased || len(res.Taken) != 0 || len(exec.delivered()) != 0 {
		t.Fatalf("pass = %+v, %v", res, err)
	}
	if !h.runtime(t).Lease.IsZero() {
		t.Fatal("the cell is still held")
	}
}

// An after target that no longer decodes (a newer build's schema, say) will
// not decode on the next pass either: the dependent op waits without a timer,
// as after_pending does, and says why. A target the store failed to read is
// retried on a timer. Both name the cause in Detail.
func TestLaneAfterUnreadableTimerOnlyForReadErrors(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	exec := newFakeExec(h.clk)
	l := h.lane(t, "ctl")
	target := h.propose(t, laneMsg("t", RoleNotice, "target"))
	mustPass(t, l, view(), exec)
	if got := h.get(t, target.ID); got.Status != StatusClosed {
		t.Fatalf("target %s", got.Status)
	}
	h.clk.Advance(time.Millisecond)
	b := h.propose(t, laneMsg("b", RoleNotice, "after", func(r *Request) { r.After = target.ID }))
	waitOf := func(res PassResult) OpWait {
		t.Helper()
		i := slices.IndexFunc(res.Waits, func(w OpWait) bool { return w.ID == b.ID })
		if i < 0 || res.Waits[i].Reason != WaitAfterUnreadable {
			t.Fatalf("%s should wait %s: %+v", b.ID, WaitAfterUnreadable, res.Waits)
		}
		return res.Waits[i]
	}

	fs.setFail(func(id string) bool { return id == target.ID })
	res := mustPass(t, l, view(), exec)
	w := waitOf(res)
	if want := h.clk.Now().Add(TransientBackoffBase); !w.Until.Equal(want) || !res.Next.Equal(want) {
		t.Fatalf("read error: until %v next %v, want a retry at %v", w.Until, res.Next, want)
	}
	if !strings.Contains(w.Detail, errFlakyRead.Error()) {
		t.Fatalf("read error detail %q does not name the cause", w.Detail)
	}

	fs.setFail(nil)
	if err := h.mem.SetMetadataBatch(target.ID, map[string]string{"gc.op.schema": "99"}); err != nil {
		t.Fatalf("setting a newer schema: %v", err)
	}
	res = mustPass(t, l, view(), exec)
	w = waitOf(res)
	if !w.Until.IsZero() {
		t.Fatalf("undecodable target: until %v, want no timer", w.Until)
	}
	if want := h.get(t, b.ID).StartBy; res.Next.Before(want) {
		t.Fatalf("undecodable target: next %v, want nothing before start_by %v", res.Next, want)
	}
	if !strings.Contains(w.Detail, ErrUnsupportedSchema.Error()) {
		t.Fatalf("undecodable target detail %q does not name the cause", w.Detail)
	}
}
