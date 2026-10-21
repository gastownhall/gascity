package legacyop

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

func TestSweep(t *testing.T) {
	e := newEnv(t)
	var expired []opjournal.Op
	for i, ex := range []opjournal.ExecutorKind{ExecutorAPI, ExecutorCLI, opjournal.ExecutorLegacy, ExecutorAPI, ExecutorCLI} {
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
	if err != nil || stats.Closed != 2 || !stats.Full || stats.Truncated {
		t.Fatalf("bounded pass = %+v, %v; want 2 closed, full", stats, err)
	}
	stats, err = Sweep(e.st, now, 100, "controller:op-sweep")
	if err != nil || stats.Closed != 3 || stats.Pending != 1 || stats.Full || stats.Truncated || len(stats.Undecodable) != 1 || stats.Undecodable[0] != "gc-op-bogus" {
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

// A head of open lane ops longer than the read's eight pages of limit hides
// the expired legacy op behind it; the pass says its read was truncated
// rather than looking like an empty one, and a larger limit reaches the op.
func TestSweepReportsATruncatedRead(t *testing.T) {
	e := newEnv(t)
	for i := range 9 * 2 {
		e.seed(t, msg(fmt.Sprint("lane-", i), "x"), opjournal.ExecutorLane)
	}
	hidden := e.seed(t, msg("hidden", "x"), ExecutorCLI)
	now := hidden.FinishBy.Add(time.Second)

	stats, err := Sweep(e.st, now, 2, "controller:op-sweep")
	if err != nil || stats.Listed != 0 || stats.Closed != 0 || !stats.Truncated {
		t.Fatalf("capped pass = %+v, %v; want nothing listed, truncated", stats, err)
	}
	if got, _ := e.st.Get(hidden.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("hidden op %s before a pass reached it", got.Status)
	}
	stats, err = Sweep(e.st, now, 100, "controller:op-sweep")
	if err != nil || stats.Listed != 1 || stats.Closed != 1 || stats.Truncated {
		t.Fatalf("full pass = %+v, %v; want the hidden op closed", stats, err)
	}
}

// limit counts expired ops, not ops read: older ops an adopter gave a later
// finish_by do not starve the expired op behind them, and count Pending.
func TestSweepPagesPastPendingOps(t *testing.T) {
	e := newEnv(t)
	var adopted []opjournal.Op
	for i := range 3 {
		adopted = append(adopted, e.seed(t, msg(fmt.Sprint("adopted-", i), "x"), ExecutorAPI))
	}
	expired := e.seed(t, msg("expired", "x"), ExecutorCLI)
	e.clk.Advance(ExecutionBound)
	for _, op := range adopted {
		if _, err := e.st.Adopt(op.ID, op.Attempt, string(ExecutorAPI), e.clk.Now().Add(ExecutionBound)); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := Sweep(e.st, expired.FinishBy.Add(time.Second), 2, "controller:op-sweep")
	if err != nil || stats.Listed != 4 || stats.Closed != 1 || stats.Pending != 3 || stats.Full || stats.Truncated {
		t.Fatalf("pass = %+v, %v; want the expired op closed past 3 pending", stats, err)
	}
	if got, _ := e.st.Get(expired.ID); got.Status != opjournal.StatusClosed {
		t.Fatalf("expired op %s behind the adopted ops", got.Status)
	}
	for _, op := range adopted {
		if got, _ := e.st.Get(op.ID); got.Status != opjournal.StatusSubmitted {
			t.Fatalf("adopted op %s swept: %s", op.ID, got.Status)
		}
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

// hookStore runs beforeUpdate once, just before the next UpdateIfMatch: the
// window between a closer's read and its write.
type hookStore struct {
	*beads.MemStore
	beforeUpdate func()
}

func (s *hookStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if f := s.beforeUpdate; f != nil {
		s.beforeUpdate = nil
		f()
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func newHookEnv(t *testing.T) (*hookStore, *opjournal.Store, *clock.Fake) {
	t.Helper()
	hs := &hookStore{MemStore: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
	clk := &clock.Fake{Time: t0}
	st, err := opjournal.NewStore(beads.SessionStore{Store: hs}, clk, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	return hs, st, clk
}

// An API retry that adopts an op after the sweeper listed it as expired keeps
// its op: the sweeper re-decides on the row it writes, and the adopted
// execution's Finish closes the op with its own outcome (I7). The API adopts
// only an op not past its finish_by, so the race needs the API's clock behind
// the sweeper's.
func TestSweepSparesAnOpAdoptedMidPass(t *testing.T) {
	hs, st, clk := newHookEnv(t)
	open := func() (*opjournal.Store, error) { return st, nil }
	apiClk := &clock.Fake{Time: t0}
	api := &Recorder{Mode: gate.Auto, Executor: ExecutorAPI, Open: open, Inflight: NewInflight(), Clock: apiClk, sleep: func(time.Duration) {}}
	first, err := api.Admit(msg("k1", "hello"))
	if err != nil || first.Decision != Execute {
		t.Fatalf("first = %+v, %v", first, err)
	}
	api.Inflight = NewInflight() // the API restarted: nothing in flight
	apiClk.Advance(ExecutionBound - time.Minute)
	clk.Advance(ExecutionBound + time.Minute)

	var adopted *Admission
	hs.beforeUpdate = func() {
		var err error
		adopted, err = api.Admit(msg("k1", "hello"))
		if err != nil || !adopted.Adopted {
			t.Fatalf("retry = %+v, %v", adopted, err)
		}
	}
	stats, err := Sweep(st, clk.Now(), 10, "controller:op-sweep")
	if err != nil || stats.Closed != 0 || stats.Pending != 1 {
		t.Fatalf("sweep = %+v, %v; want the adopted op pending", stats, err)
	}
	if got, _ := st.Get(first.Op.ID); got.Status != opjournal.StatusSubmitted || got.Attempt != 1 {
		t.Fatalf("after sweep: %s attempt %d", got.Status, got.Attempt)
	}
	if f := adopted.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)}); !f.Closed || f.Late {
		t.Fatalf("adopted Finish = %+v", f)
	}
}

// The CLI's expiry rule loses the same race: an op the API adopts between
// the CLI's read and its close, its clock behind the CLI's, stays open, and
// the CLI answers busy.
func TestAdmitCLIExpirySparesAnOpAdoptedMidAdmit(t *testing.T) {
	hs, st, clk := newHookEnv(t)
	open := func() (*opjournal.Store, error) { return st, nil }
	apiClk := &clock.Fake{Time: t0}
	api := &Recorder{Mode: gate.Auto, Executor: ExecutorAPI, Open: open, Inflight: NewInflight(), Clock: apiClk, sleep: func(time.Duration) {}}
	cli := &Recorder{Mode: gate.Auto, Executor: ExecutorCLI, Open: open, Clock: clk, sleep: func(time.Duration) {}}
	req := msg("k1", "hello")
	req.Principal = PrincipalOperator
	first, err := api.Admit(req)
	if err != nil {
		t.Fatal(err)
	}
	api.Inflight = NewInflight()
	apiClk.Advance(ExecutionBound - time.Minute)
	clk.Advance(ExecutionBound + time.Minute)
	hs.beforeUpdate = func() {
		if adm, err := api.Admit(req); err != nil || !adm.Adopted {
			t.Fatalf("API retry = %+v, %v", adm, err)
		}
	}
	if adm, err := cli.Admit(req); !errors.Is(err, ErrBusy) {
		t.Fatalf("CLI Admit = %+v, %v; want busy", adm, err)
	}
	if got, _ := st.Get(first.Op.ID); got.Status != opjournal.StatusSubmitted || got.Attempt != 1 {
		t.Fatalf("after CLI admit: %s attempt %d", got.Status, got.Attempt)
	}
}
