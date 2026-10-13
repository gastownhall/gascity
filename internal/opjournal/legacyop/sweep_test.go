package legacyop

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

func TestSweep(t *testing.T) {
	e := newEnv(t)
	var expired []opjournal.Op
	for i, ex := range []opjournal.Executor{ExecutorAPI, ExecutorCLI, opjournal.ExecutorLegacy, ExecutorAPI, ExecutorCLI} {
		expired = append(expired, e.seed(t, msg("old-"+string(rune('a'+i)), "x"), ex))
		e.clk.Advance(time.Second)
	}
	lane := e.seed(t, msg("lane", "x"), opjournal.ExecutorLane)
	e.clk.Advance(ExecutionBound)
	fresh := e.seed(t, msg("fresh", "x"), ExecutorAPI)
	if _, err := e.mem.Create(beads.Bead{
		ID: "gc-op-bogus", Type: opjournal.BeadType, Labels: []string{opjournal.LabelOp},
		Metadata: map[string]string{"gc.op.schema": "1", "gc.op.kind": "message", "gc.op.status": "submitted"},
	}); err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now().Add(time.Second)

	stats, err := Sweep(e.st, now, 2, "controller:op-sweep")
	if err != nil || stats.Closed != 2 {
		t.Fatalf("bounded pass = %+v, %v; want 2 closed", stats, err)
	}
	stats, err = Sweep(e.st, now, 100, "controller:op-sweep")
	if err != nil || stats.Closed != 3 || stats.Pending != 1 || len(stats.Undecodable) != 1 || stats.Undecodable[0] != "gc-op-bogus" {
		t.Fatalf("full pass = %+v, %v", stats, err)
	}
	for _, op := range expired {
		got, _ := e.st.Get(op.ID)
		if got.Status != opjournal.StatusClosed || got.Outcome != failed(opjournal.ReasonDeadlineInFlight) {
			t.Fatalf("%s (%s): %s %+v", op.ID, op.Executor, got.Status, got.Outcome)
		}
		if last := got.Transitions[len(got.Transitions)-1]; last.By != "controller:op-sweep" {
			t.Fatalf("closed by %q", last.By)
		}
	}
	for _, op := range []opjournal.Op{lane, fresh} {
		if got, _ := e.st.Get(op.ID); got.Status != opjournal.StatusSubmitted {
			t.Fatalf("%s (%s) swept: %s", op.ID, op.Executor, got.Status)
		}
	}
	if b, _ := e.mem.Get("gc-op-bogus"); b.Status != "open" || len(b.Metadata) != 3 {
		t.Fatalf("the undecodable row was written: %+v", b)
	}
	again, err := Sweep(e.st, now, 100, "controller:op-sweep")
	if err != nil || again.Closed != 0 || again.Pending != 1 {
		t.Fatalf("second pass = %+v, %v; want nothing closed", again, err)
	}
	// finish_by itself is not past it.
	atDeadline, err := Sweep(e.st, fresh.FinishBy, 100, "controller:op-sweep")
	if err != nil || atDeadline.Closed != 0 {
		t.Fatalf("sweep at finish_by = %+v, %v", atDeadline, err)
	}
}

// A sweep racing an executor's close loses cleanly to it.
func TestSweepLosesToAnExecutor(t *testing.T) {
	e := newEnv(t)
	op := e.seed(t, msg("k", "x"), ExecutorCLI)
	if _, err := e.st.Close(op.ID, succeeded(ViaLegacy), "legacy:cli"); err != nil {
		t.Fatal(err)
	}
	stale := op // as the sweeper listed it, before the close
	if _, err := e.st.CloseKnown(stale, failed(opjournal.ReasonDeadlineInFlight), "controller:op-sweep"); err == nil {
		t.Fatal("the sweeper's stale close overwrote the executor's outcome")
	}
	if got, _ := e.st.Get(op.ID); got.Outcome != succeeded(ViaLegacy) {
		t.Fatalf("outcome %+v", got.Outcome)
	}
}
