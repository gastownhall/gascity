package opjournal

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
