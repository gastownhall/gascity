package opjournal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// A batch of MaxBatchOps-ish notices is delivered while producers cancel all
// but one of them (each Cancel moves the op's revision). Done must settle in a
// bounded number of tries: the cancels lost the race, every op succeeded, and
// the cell is clear.
func TestLaneDoneSettlesManyOpsMovedDuringTheEffect(t *testing.T) {
	const n = 10 // more moved ops than casAttempts
	if n-1 < casAttempts {
		t.Fatalf("the test needs more moved ops than casAttempts (%d)", casAttempts)
	}
	h := newLaneHarness(t)
	var ids []string
	for i := range n {
		op := h.propose(t, laneMsg(fmt.Sprintf("n%d", i), RoleNotice, "x"))
		ids = append(ids, op.ID)
		h.clk.Advance(time.Millisecond)
	}
	exec := newFakeExec(h.clk)
	exec.onEffect = func() {
		for _, id := range ids[1:] {
			if res, _, err := h.store.Cancel(id, "operator", "read"); err != nil || res != CancelRequested {
				t.Errorf("Cancel(%s) = %s, %v", id, res, err)
			}
		}
	}
	res, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec)
	if err != nil || res.Done != DoneAccepted {
		t.Fatalf("pass = %+v, %v", res, err)
	}
	for _, id := range ids {
		if got := h.get(t, id); got.Outcome.Arm != ArmSucceeded {
			t.Fatalf("op %s: %s %+v", id, got.Status, got.Outcome)
		}
	}
	if rt := h.runtime(t); !rt.Lease.IsZero() {
		t.Fatalf("the cell still holds %s", rt.Lease)
	}
}

// When Done runs out of tries it names the last refusal.
func TestLaneDoneExhaustionNamesTheLastError(t *testing.T) {
	h := newLaneHarness(t)
	h.propose(t, laneMsg("p", RolePrompt, "x"))
	errStale := errors.New("revision moved")
	refused := &beads.BatchItemError{Index: 0, Err: errStale}
	h.batcher.setFault(func(n int, b beads.GuardedBatch) (bool, error) {
		if n == 0 {
			return false, nil
		}
		refused.ID = b.Items[0].Update.ID
		return false, refused
	})
	_, err := h.lane(t, "ctl").Pass(context.Background(), view(), newFakeExec(h.clk))
	if !errors.Is(err, ErrContention) || !errors.Is(err, errStale) || strings.Contains(err.Error(), "%!w") {
		t.Fatalf("pass = %v", err)
	}
}

// I9 at scale: while the effect runs past its lease, another lane re-takes
// and settles every op of the batch. The zombie's Done writes nothing and
// reports superseded, without running out of tries.
func TestLaneDoneSupersededByManyRetakes(t *testing.T) {
	h := newLaneHarness(t)
	for i := range 10 {
		h.propose(t, laneMsg(fmt.Sprintf("n%d", i), RoleNotice, "x"))
		h.clk.Advance(time.Millisecond)
	}
	other := newFakeExec(h.clk)
	exec := newFakeExec(h.clk)
	exec.onEffect = func() {
		h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
		if res := mustPass(t, h.lane(t, "cli"), view(), other); res.Done != DoneAccepted || len(res.Taken) != 10 {
			t.Errorf("re-take pass %+v", res)
		}
	}
	res, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec)
	if err != nil || res.Done != DoneSuperseded {
		t.Fatalf("pass = %+v, %v", res, err)
	}
}

// A transient read error while Done reconciles is retried within Done's
// budget, as an unknown batch error is: the settle still lands and the cell
// is cleared.
func TestLaneDoneRetriesATransientReconcileRead(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	exec.onEffect = func() {
		// A cancel recorded during the effect moves the op's revision, so
		// Done is refused and re-reads the op; that read fails once.
		if _, _, err := h.store.Cancel(op.ID, "operator", "read"); err != nil {
			t.Errorf("Cancel: %v", err)
		}
		failed := false
		fs.setFail(func(id string) bool {
			if id != op.ID || failed {
				return false
			}
			failed = true
			return true
		})
	}
	res, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec)
	fs.setFail(nil)
	if err != nil || res.Done != DoneAccepted {
		t.Fatalf("pass = %+v, %v", res, err)
	}
	if got := h.get(t, op.ID); got.Status != StatusClosed || got.Outcome.Arm != ArmSucceeded {
		t.Fatalf("op %s %+v", got.Status, got.Outcome)
	}
	if rt := h.runtime(t); !rt.Lease.IsZero() {
		t.Fatalf("cell still holds %s", rt.Lease)
	}
}

// When Done cannot settle at all, the pass names the read error and reports
// no Done arm: nothing it planned was written.
func TestLaneDoneThatCannotSettleReportsNoArm(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	exec.onEffect = func() {
		if _, _, err := h.store.Cancel(op.ID, "operator", "read"); err != nil {
			t.Errorf("Cancel: %v", err)
		}
		fs.setFail(func(id string) bool { return id == op.ID })
	}
	res, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec)
	fs.setFail(nil)
	if !errors.Is(err, errFlakyRead) {
		t.Fatalf("pass err = %v, want the read error", err)
	}
	if res.Done != "" || len(res.Closed) != 0 {
		t.Fatalf("pass reported done %q closed %v though nothing settled", res.Done, res.Closed)
	}
	if got := h.get(t, op.ID); got.Status != StatusExecuting {
		t.Fatalf("op %s, want executing until its lease ends", got.Status)
	}
}

// The same holds when another lane re-took part of the batch: a Done that
// returns an error reports no arm, superseded included.
func TestLaneDoneThatCannotSettleReportsNoSuperseded(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	a := h.propose(t, laneMsg("a", RoleNotice, "one"))
	h.clk.Advance(time.Millisecond)
	b := h.propose(t, laneMsg("b", RoleNotice, "two"))
	exec := newFakeExec(h.clk)
	other := newFakeExec(h.clk)
	exec.onEffect = func() {
		// The effect overruns its lease; another lane re-takes and settles
		// both ops, then b's row becomes unreadable for this Done.
		h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
		if res, err := h.lane(t, "cli").Pass(context.Background(), view(), other); err != nil {
			t.Errorf("re-take pass: %+v %v", res, err)
		}
		fs.setFail(func(id string) bool { return id == b.ID })
	}
	res, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec)
	fs.setFail(nil)
	if !errors.Is(err, errFlakyRead) {
		t.Fatalf("pass err = %v, want the read error", err)
	}
	if res.Done != "" {
		t.Fatalf("pass reported done %q alongside %v (a=%s b=%s)", res.Done, err, a.ID, b.ID)
	}
}

// settleByExec is a fake executor whose deliveries name a SettleBy of in after
// the delivery, and which calls effect, when set, as each runs.
func settleByExec(h *laneHarness, in time.Duration, effect func()) *fakeExec {
	exec := newFakeExec(h.clk)
	exec.deliver = func(Delivery) DeliveryResult {
		if effect != nil {
			effect()
		}
		at := h.clk.Now()
		return Delivered{Record: DeliveryRecord{At: at, Ack: AckDelivered}, Via: "fake", SettleBy: at.Add(in)}
	}
	return exec
}

// A delivery whose SettleBy has passed when Done runs is not recorded: Done
// writes nothing, the batch stays executing, and once its lease lapses a pass
// takes it again as maybe delivered: redelivered with the note, or, at most
// once, closed failed{ambiguous_delivery}.
func TestLaneDonePastSettleByLeavesTheBatchExecuting(t *testing.T) {
	for _, tc := range []struct {
		name string
		mods []func(*Request)
	}{
		{name: "at_least_once"},
		{name: "at_most_once", mods: []func(*Request){atMostOnce}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, laneMsg("n", RoleNotice, "fyi", tc.mods...))
			before := len(h.batcher.batches())
			res, err := h.lane(t, "ctl").Pass(context.Background(), view(), settleByExec(h, 0, nil))
			if !errors.Is(err, errSettleByPassed) {
				t.Fatalf("pass err = %v, want the settle-by cause", err)
			}
			if res.Done != "" || len(res.Closed) != 0 {
				t.Fatalf("pass reported done %q closed %v though nothing settled", res.Done, res.Closed)
			}
			if n := len(h.batcher.batches()) - before; n != 1 {
				t.Fatalf("the pass applied %d guarded batches, want the Take's alone", n)
			}
			if got := h.get(t, op.ID); got.Status != StatusExecuting {
				t.Fatalf("op %s, want executing until its lease ends", got.Status)
			}
			h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Millisecond)
			again := newFakeExec(h.clk)
			mustPass(t, h.lane(t, "cli"), view(), again)
			got := h.get(t, op.ID)
			if got.MaybeDelivered != 1 || got.LastError != lastErrorLeaseExpired {
				t.Fatalf("op %+v, want maybe delivered by its lapsed lease", got)
			}
			if tc.mods != nil {
				if got.Outcome.Arm != ArmFailed || got.Outcome.Reason != ReasonAmbiguousDelivery || len(again.delivered()) != 0 {
					t.Fatalf("at-most-once op %+v after %d deliveries, want failed{ambiguous_delivery} undelivered", got.Outcome, len(again.delivered()))
				}
				return
			}
			if ds := again.delivered(); len(ds) != 1 || !ds[0].Redelivery || !strings.Contains(ds[0].Text, RedeliveryNote) {
				t.Fatalf("re-take deliveries %+v, want one redelivery with the note", ds)
			}
			if got.Outcome.Arm != ArmSucceeded {
				t.Fatalf("op %+v, want succeeded on the redelivery", got.Outcome)
			}
		})
	}
}

// stallingBatcher holds each guarded batch while stall is set until the
// batch's context ends, recording the time that context had left.
type stallingBatcher struct {
	inner beads.GuardedBatcher
	stall atomic.Bool
	mu    sync.Mutex
	left  []time.Duration
}

func (s *stallingBatcher) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	if !s.stall.Load() {
		return s.inner.ApplyGuarded(ctx, b)
	}
	deadline, ok := ctx.Deadline()
	s.mu.Lock()
	s.left = append(s.left, time.Until(deadline))
	s.mu.Unlock()
	if !ok {
		return beads.GuardedResult{}, errors.New("the Done's batch has no deadline")
	}
	<-ctx.Done()
	return beads.GuardedResult{}, ctx.Err()
}

func (s *stallingBatcher) stalled() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.left...)
}

// A Done still waiting on the store when its delivery's SettleBy comes ends
// then, not at its own budget of at least doneMinBudget, and tries no further
// batch: the batch stays executing for its lease to return. The lane backs
// off with its real sleep, which names the cause.
func TestLaneDoneEndsAtSettleBy(t *testing.T) {
	const settleIn = 50 * time.Millisecond
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("n", RoleNotice, "fyi"))
	sb := &stallingBatcher{inner: h.batcher.inner}
	h.batcher.inner = sb
	exec := settleByExec(h, settleIn, func() { sb.stall.Store(true) })
	lane := h.lane(t, "ctl")
	lane.sleep = sleepCtx
	res, err := lane.Pass(context.Background(), view(), exec)
	if !errors.Is(err, errSettleByPassed) {
		t.Fatalf("pass err = %v, want the settle-by cause", err)
	}
	if res.Done != "" || len(res.Closed) != 0 {
		t.Fatalf("pass reported done %q closed %v though nothing settled", res.Done, res.Closed)
	}
	left := sb.stalled()
	if len(left) != 1 {
		t.Fatalf("the Done tried %d batches, want one before its SettleBy ended it", len(left))
	}
	if left[0] > settleIn {
		t.Fatalf("the Done's batch had %s left, want at most the %s to its SettleBy", left[0], settleIn)
	}
	if got := h.get(t, op.ID); got.Status != StatusExecuting {
		t.Fatalf("op %s, want executing until its lease ends", got.Status)
	}
}

// A Done that lands before its delivery's SettleBy records the delivery as
// usual.
func TestLaneDoneBeforeSettleBySucceeds(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("n", RoleNotice, "fyi"))
	res, err := h.lane(t, "ctl").Pass(context.Background(), view(), settleByExec(h, 10*time.Second, nil))
	if err != nil || res.Done != DoneAccepted {
		t.Fatalf("pass = %+v, %v", res, err)
	}
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded || got.MaybeDelivered != 0 {
		t.Fatalf("op %s %+v", got.Status, got.Outcome)
	}
}

// errNoTimeToWrite is releaseExec's transient cause.
var errNoTimeToWrite = errors.New("no time left to write the delivery")

// releaseExec is a fake executor whose deliveries write nothing and answer
// NotDelivered as transient, naming a SettleBy of in after the call, and which
// calls effect, when set, as each runs.
func releaseExec(h *laneHarness, in time.Duration, effect func()) *fakeExec {
	exec := newFakeExec(h.clk)
	exec.deliver = func(Delivery) DeliveryResult {
		if effect != nil {
			effect()
		}
		return NotDelivered{Transient: true, Cause: errNoTimeToWrite, SettleBy: h.clk.Now().Add(in)}
	}
	return exec
}

// A release whose Done is still waiting on the store when its SettleBy comes
// ends then, as a delivery's does, and tries no further batch: the batch stays
// executing, with no failure counted and nothing maybe delivered.
func TestLaneDoneReleaseEndsAtSettleBy(t *testing.T) {
	const settleIn = 50 * time.Millisecond
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("n", RoleNotice, "fyi"))
	sb := &stallingBatcher{inner: h.batcher.inner}
	h.batcher.inner = sb
	lane := h.lane(t, "ctl")
	lane.sleep = sleepCtx
	res, err := lane.Pass(context.Background(), view(), releaseExec(h, settleIn, func() { sb.stall.Store(true) }))
	if !errors.Is(err, errSettleByPassed) {
		t.Fatalf("pass err = %v, want the settle-by cause", err)
	}
	if res.Done != "" || len(res.Closed) != 0 {
		t.Fatalf("pass reported done %q closed %v though nothing settled", res.Done, res.Closed)
	}
	if left := sb.stalled(); len(left) != 1 || left[0] > settleIn {
		t.Fatalf("the Done's batches had %v left, want one with at most the %s to its SettleBy", left, settleIn)
	}
	if got := h.get(t, op.ID); got.Status != StatusExecuting || got.Failures != 0 || got.MaybeDelivered != 0 {
		t.Fatalf("op %s failures %d maybe_delivered %d, want executing with neither counted", got.Status, got.Failures, got.MaybeDelivered)
	}
}

// A release that lands before its SettleBy backs off as transient, counting
// no failure and marking nothing maybe delivered.
func TestLaneDoneReleaseBeforeSettleByBacksOff(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("n", RoleNotice, "fyi"))
	res, err := h.lane(t, "ctl").Pass(context.Background(), view(), releaseExec(h, 10*time.Second, nil))
	if err != nil || res.Done != DoneReleased {
		t.Fatalf("pass = %+v, %v", res, err)
	}
	got := h.get(t, op.ID)
	if got.Status != StatusAccepted || got.Failures != 0 || got.MaybeDelivered != 0 || got.LastError != lastError(lastErrorTransient, errNoTimeToWrite) {
		t.Fatalf("op %s failures %d maybe_delivered %d last_error %q, want released as transient with neither counted",
			got.Status, got.Failures, got.MaybeDelivered, got.LastError)
	}
	if want := h.clk.Now().Add(TransientBackoffBase); !got.RetryAt.Equal(want) {
		t.Fatalf("op retries at %s, want %s", got.RetryAt, want)
	}
}
