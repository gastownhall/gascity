package opjournal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// busyProbe is a running runtime with turn turn in progress.
func busyProbe(turn string) Probe {
	p := idleProbe()
	p.Boundary, p.TurnLive, p.Turn, p.KnownTurns = TriNo, TriYes, turn, []string{turn}
	return p
}

// settledT1 is an executor's replacement step that settled turn t1 and left
// the session as after says.
func settledT1(after TurnAfter) func(InterruptCall) ControlResult {
	return func(InterruptCall) ControlResult {
		return Settled{Turn: "t1", Via: "fake", Degradation: DegradationUnfencedInterrupt, After: after}
	}
}

// afterT1 is a replacement step's read once the replaced turn t1 ended and
// nothing else runs.
func afterT1() TurnAfter {
	return TurnAfter{Boundary: TriYes, Live: TriNo, Known: []string{"t1"}}
}

func wantOutcome(t *testing.T, op Op, arm Arm, reason string) {
	t.Helper()
	if op.Status != StatusClosed || op.Outcome.Arm != arm || op.Outcome.Reason != reason {
		t.Fatalf("op %s %s %s{%s}, want closed %s{%s}", op.ID, op.Status, op.Outcome.Arm, op.Outcome.Reason, arm, reason)
	}
}

// With no turn running the unit runs without its interrupt: one Take and
// one Done hold both ops, the interrupt closes skipped{stale} with no
// executor call, and the message goes at once as a boundary delivery.
func TestLaneNowDirect(t *testing.T) {
	h := newLaneHarness(t)
	m := h.propose(t, nowReq("a"))
	iid := nowID(t, m)
	exec := newFakeExec(h.clk)
	res := mustPass(t, h.lane(t, "ctl"), view(), exec)
	if !slices.Equal(res.Lowered, []string{m.ID}) || res.Took != TakeNow || !slices.Equal(res.Taken, []string{iid, m.ID}) || res.Done != DoneAccepted {
		t.Fatalf("pass %+v", res)
	}
	if len(exec.interrupts) != 0 {
		t.Fatalf("interrupt calls %+v, want none on an idle runtime", exec.interrupts)
	}
	ds := exec.delivered()
	text, _ := RenderDelivery([]Op{m}, TextReminder, false)
	if len(ds) != 1 || ds[0].Replacement || ds[0].Steer || ds[0].Redelivery || ds[0].Seq != 2 || ds[0].Key != m.ID+":2" ||
		ds[0].Mode != LinkStarts || len(ds[0].Ops) != 1 || ds[0].Ops[0].ID != m.ID || ds[0].Text != text {
		t.Fatalf("deliveries %+v", ds)
	}
	i := h.get(t, iid)
	wantOutcome(t, i, ArmSkipped, ReasonStale)
	if i.LaneSeq != 1 || i.DeliveryKey != "" || i.After != "" || i.Attempt != 1 || i.Causation != m.ID {
		t.Fatalf("interrupt %+v", i)
	}
	got := h.get(t, m.ID)
	wantOutcome(t, got, ArmSucceeded, "")
	if got.LaneSeq != 2 || got.DeliveryKey != m.ID+":2" || got.After != "" || got.Outcome.Result.Delivery.Seq != 2 {
		t.Fatalf("message %+v", got)
	}
	bs := h.batcher.batches()
	if len(bs) != 3 || len(bs[0].Items) != 2 || len(bs[1].Items) != 3 || len(bs[2].Items) != 3 {
		t.Fatalf("batches %d, want lower (2 items), take (3), done (3)", len(bs))
	}
	if rt := h.runtime(t); rt.Seq != 2 || !rt.Lease.IsZero() {
		t.Fatalf("runtime row %+v", rt)
	}
}

// A busy runtime's unit runs its replacement step, then the message, in one
// effect: the step names the unit's interrupt and target with
// ForReplacement, the message goes as a replacement at the boundary the step
// reached, both close in one Done, and the message's link takes the step's
// read, so the replaced turn is never its candidate (N8).
func TestLaneNowReplace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		req    Request
		target string
	}{
		{"untargeted", nowReq("a"), ""},
		{"targeted", nowReq("a", nowTarget(1, "t1")), "t1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			m := h.propose(t, tc.req)
			iid := nowID(t, m)
			exec := newFakeExec(h.clk)
			// The probe saw the session busy but could not read its turns.
			exec.probe = idleProbe()
			exec.probe.Boundary, exec.probe.TurnLive = TriNo, TriUnknown
			exec.interrupt = settledT1(afterT1())
			exec.deliver = func(Delivery) DeliveryResult {
				return Delivered{
					Record: DeliveryRecord{At: h.clk.Now(), Ack: AckDelivered},
					Evidence: TurnEvidence{
						HasSource: true,
						Seen:      []SeenTurn{{ID: "t1", Generation: 1}, {ID: "t2", Generation: 1}},
						FirstRead: FirstTurnRead{Done: true, Current: "t2"},
					},
					Via: "fake",
				}
			}
			res := mustPass(t, h.lane(t, "ctl"), view(), exec)
			if res.Took != TakeNow || res.Done != DoneAccepted || len(res.Closed) != 2 {
				t.Fatalf("pass %+v", res)
			}
			if len(exec.interrupts) != 1 {
				t.Fatalf("interrupt calls %d", len(exec.interrupts))
			}
			c := exec.interrupts[0]
			if !c.ForReplacement || c.Op.ID != iid || c.Turn != tc.target || c.Seq != 1 || c.Fence != (Fence{Conversation: "gc-1", Generation: 1, Epoch: 1, Token: "tok-1"}) {
				t.Fatalf("interrupt call %+v", c)
			}
			ds := exec.delivered()
			if len(ds) != 1 || !ds[0].Replacement || ds[0].Seq != 2 || ds[0].Key != m.ID+":2" || ds[0].Mode != LinkStarts {
				t.Fatalf("deliveries %+v", ds)
			}
			i := h.get(t, iid)
			wantOutcome(t, i, ArmSucceeded, "")
			if i.Outcome.Result.Turn != "t1" || i.Outcome.Degradation != DegradationUnfencedInterrupt {
				t.Fatalf("interrupt outcome %+v", i.Outcome)
			}
			got := h.get(t, m.ID)
			wantOutcome(t, got, ArmSucceeded, "")
			r := got.Outcome.Result
			if r.Turn != "t2" || r.TurnLink != LinkCorrelated || r.TurnBasis != BasisHistoryDiff || r.Delivery.Seq != 2 {
				t.Fatalf("message result %+v, want t2 correlated by history_diff", r)
			}
			if bs := h.batcher.batches(); len(bs) != 3 {
				t.Fatalf("%d batches, want lower, take and one done", len(bs))
			}
		})
	}
}

// Every way the replacement step can end without a boundary leaves the
// message open, released with no gate and no failure counted, and the next
// boundary delivers it alone, marked with the interrupt's failure; an
// interrupt that may have run is never called again (N5).
func TestLaneNowReplaceOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  ControlResult
		// arm and reason are the interrupt's close; "" arm: released.
		arm         Arm
		reason      string
		why         string
		degradation string
		failures    int
	}{
		{name: "a newer turn runs", res: Settled{Turn: "t1", After: TurnAfter{Boundary: TriNo, Live: TriYes, Turn: "t2", Known: []string{"t1", "t2"}}}, arm: ArmSucceeded, why: "another turn runs"},
		{name: "boundary unknown", res: Settled{Turn: "t1"}, arm: ArmSucceeded, why: "could not tell its boundary"},
		{name: "stale", res: Rejected{Arm: ArmSkipped, Reason: ReasonStale}, arm: ArmSkipped, reason: ReasonStale, why: "skipped{stale}"},
		{name: "unsupported", res: Rejected{Arm: ArmFailed, Reason: ReasonUnsupported}, arm: ArmFailed, reason: ReasonUnsupported, why: "failed{unsupported}", degradation: DegradationInterruptUnsupported},
		{name: "no boundary", res: Rejected{Arm: ArmFailed, Reason: ReasonNoBoundary, Turn: "t1"}, arm: ArmFailed, reason: ReasonNoBoundary, why: "failed{no_boundary}", degradation: DegradationInterruptFailed},
		{name: "not prepared", res: Rejected{Arm: ArmFailed, Reason: ReasonReplacementNotPrepared, Turn: "t1"}, arm: ArmFailed, reason: ReasonReplacementNotPrepared, why: "failed{replacement_not_prepared}", degradation: DegradationInterruptFailed},
		{name: "unknown", res: Unknown{Cause: errors.New("lost the answer")}, arm: ArmFailed, reason: ReasonAmbiguousDelivery, why: "unknown", degradation: DegradationInterruptFailed},
		{name: "transient", res: NotDelivered{Transient: true, Cause: errors.New("busy socket")}, why: "not_delivered"},
		{name: "not delivered", res: NotDelivered{Cause: errors.New("refused")}, why: "not_delivered", failures: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			m := h.propose(t, nowReq("a"))
			iid := nowID(t, m)
			exec := newFakeExec(h.clk)
			exec.probe = busyProbe("t1")
			exec.interrupt = func(InterruptCall) ControlResult { return tc.res }
			l := h.lane(t, "ctl")
			res := mustPass(t, l, view(), exec)
			if res.Took != TakeNow || res.Done != DoneReleased || len(exec.delivered()) != 0 {
				t.Fatalf("pass %+v deliveries %d", res, len(exec.delivered()))
			}
			i := h.get(t, iid)
			if tc.arm == "" {
				if i.Status != StatusAccepted || i.RetryAt.IsZero() || i.Failures != tc.failures {
					t.Fatalf("interrupt %s retry %s failures %d, want released", i.Status, i.RetryAt, i.Failures)
				}
			} else {
				wantOutcome(t, i, tc.arm, tc.reason)
				if !res.More {
					t.Fatalf("pass %+v: the message's post-unit pass is due at once", res)
				}
			}
			got := h.get(t, m.ID)
			if got.Status != StatusAccepted || !got.RetryAt.IsZero() || got.Failures != 0 || got.MaybeDelivered != 0 ||
				!got.Lease.IsZero() || got.Attempt != 0 ||
				!strings.HasPrefix(got.LastError, lastErrorNotAttempted+": ") || !strings.Contains(got.LastError, tc.why) {
				t.Fatalf("message %s lease %+v attempt %d retry %s failures %d maybe %d last_error %q", got.Status, got.Lease, got.Attempt, got.RetryAt, got.Failures, got.MaybeDelivered, got.LastError)
			}
			if tc.arm == "" {
				// The unit waits out the interrupt's backoff, holding the
				// message, then runs again: nothing was sent.
				res = mustPass(t, l, view(), exec)
				if w := waitOf(res.Waits, m.ID); res.Took != TakeNone || w.Reason != WaitRetryBackoff {
					t.Fatalf("backoff pass %+v", res)
				}
				h.clk.Advance(ExecutorErrorBackoff)
				exec.interrupt = settledT1(afterT1())
				if res = mustPass(t, l, view(), exec); res.Took != TakeNow || res.Done != DoneAccepted || len(exec.interrupts) != 2 {
					t.Fatalf("retry pass %+v interrupts %d", res, len(exec.interrupts))
				}
				wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
				return
			}
			// The next boundary delivers the message alone.
			exec.probe = idleProbe()
			res = mustPass(t, l, view(), exec)
			if res.Took != TakeContent || !slices.Equal(res.Taken, []string{m.ID}) || res.Done != DoneAccepted {
				t.Fatalf("post-unit pass %+v", res)
			}
			got = h.get(t, m.ID)
			wantOutcome(t, got, ArmSucceeded, "")
			if got.Outcome.Degradation != tc.degradation {
				t.Fatalf("message degradation %q, want %q", got.Outcome.Degradation, tc.degradation)
			}
			if d := exec.delivered()[0]; d.Replacement || d.Steer {
				t.Fatalf("post-unit delivery %+v", d)
			}
			if len(exec.interrupts) != 1 {
				t.Fatalf("interrupt calls %d, want 1 (N5)", len(exec.interrupts))
			}
		})
	}
	t.Run("a newer turn holds the message for the boundary", func(t *testing.T) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.interrupt = settledT1(TurnAfter{Boundary: TriNo, Live: TriYes, Turn: "t2"})
		l := h.lane(t, "ctl")
		mustPass(t, l, view(), exec)
		exec.probe = busyProbe("t2")
		res := mustPass(t, l, view(), exec)
		if w := waitOf(res.Waits, m.ID); res.Took != TakeNone || w.Reason != WaitBusy || !res.Busy {
			t.Fatalf("busy pass %+v", res)
		}
		exec.probe = idleProbe()
		mustPass(t, l, view(), exec)
		got := h.get(t, m.ID)
		wantOutcome(t, got, ArmSucceeded, "")
		if got.Outcome.Degradation != "" || len(exec.interrupts) != 1 {
			t.Fatalf("message %+v interrupts %d", got.Outcome, len(exec.interrupts))
		}
	})
	t.Run("a contract-breaking nil", func(t *testing.T) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.interrupt = func(InterruptCall) ControlResult { return nil }
		if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec); err == nil {
			t.Fatal("a nil interrupt result was not reported")
		}
		wantOutcome(t, h.get(t, nowID(t, m)), ArmFailed, ReasonAmbiguousDelivery)
		if got := h.get(t, m.ID); got.Status != StatusAccepted || len(exec.delivered()) != 0 {
			t.Fatalf("message %s deliveries %d", got.Status, len(exec.delivered()))
		}
	})
}

// After a settled replacement the message's own result settles it as any
// content's does: a transient miss backs off, an Unknown replays at the next
// boundary under its key (with the repeat line on a runtime that does not
// deduplicate, byte-identical on one that does), a refused key takes a new
// one, and an at most once message that may have landed fails ambiguous.
func TestLaneNowMessageResults(t *testing.T) {
	setup := func(t *testing.T, mods ...func(*Request)) (*laneHarness, *fakeExec, *ConversationLane, Op) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a", mods...))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.interrupt = settledT1(afterT1())
		return h, exec, h.lane(t, "ctl"), m
	}
	t.Run("transient", func(t *testing.T) {
		h, exec, l, m := setup(t)
		exec.deliver = func(Delivery) DeliveryResult { return NotDelivered{Transient: true, Cause: errors.New("busy")} }
		res := mustPass(t, l, view(), exec)
		wantOutcome(t, h.get(t, nowID(t, m)), ArmSucceeded, "")
		got := h.get(t, m.ID)
		if res.Done != DoneReleased || got.Status != StatusAccepted || !got.RetryAt.Equal(h.clk.Now().Add(TransientBackoffBase)) {
			t.Fatalf("pass %+v message %s retry %s", res, got.Status, got.RetryAt)
		}
	})
	for _, dedup := range []bool{false, true} {
		name := "unknown replays"
		if dedup {
			name += " on a deduplicating runtime"
		}
		t.Run(name, func(t *testing.T) {
			h, exec, l, m := setup(t)
			exec.probe.Caps.Dedup = dedup
			exec.deliver = func(Delivery) DeliveryResult { return Unknown{Cause: errors.New("lost")} }
			mustPass(t, l, view(), exec)
			got := h.get(t, m.ID)
			if got.Status != StatusAccepted || got.MaybeDelivered != 1 || got.Failures != 1 {
				t.Fatalf("message %+v", got)
			}
			first := exec.delivered()[0]
			h.clk.Advance(ExecutorErrorBackoff)
			exec.probe = idleProbe()
			exec.probe.Caps.Dedup = dedup
			exec.deliver = nil
			res := mustPass(t, l, view(), exec)
			if res.Took != TakeContent || !slices.Equal(res.Taken, []string{m.ID}) {
				t.Fatalf("replay pass %+v", res)
			}
			d := exec.delivered()[1]
			if !d.Redelivery || d.Key != first.Key || d.Replacement {
				t.Fatalf("replay %+v, want a redelivery under %s", d, first.Key)
			}
			if hasNote := strings.Contains(d.Text, RedeliveryNote); hasNote == dedup {
				t.Fatalf("replay text %q: note %v on dedup %v", d.Text, hasNote, dedup)
			}
			if dedup && d.Text != first.Text {
				t.Fatalf("deduplicated replay text %q, want %q", d.Text, first.Text)
			}
			wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
		})
	}
	t.Run("key rejected", func(t *testing.T) {
		h, exec, l, m := setup(t)
		exec.deliver = func(Delivery) DeliveryResult { return Rejected{KeyRejected: true, Reason: "nudge_rejected"} }
		mustPass(t, l, view(), exec)
		got := h.get(t, m.ID)
		if got.Status != StatusAccepted || got.Failures != 1 || !strings.HasPrefix(got.LastError, lastErrorKeyRejected) {
			t.Fatalf("message %+v", got)
		}
		h.clk.Advance(ExecutorErrorBackoff)
		exec.probe, exec.deliver = idleProbe(), nil
		mustPass(t, l, view(), exec)
		if d := exec.delivered()[1]; d.Key == exec.delivered()[0].Key || d.Redelivery {
			t.Fatalf("delivery after a refused key %+v", d)
		}
		wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
	})
	t.Run("at most once", func(t *testing.T) {
		h, exec, l, m := setup(t, atMostOnce)
		exec.deliver = func(Delivery) DeliveryResult { return Unknown{Cause: errors.New("lost")} }
		mustPass(t, l, view(), exec)
		wantOutcome(t, h.get(t, m.ID), ArmFailed, ReasonAmbiguousDelivery)
	})
}

// A replacement step that leaves too little of the lease does not send the
// message: it waits for the boundary.
func TestLaneNowLeaseBudget(t *testing.T) {
	h := newLaneHarness(t)
	m := h.propose(t, nowReq("a"))
	exec := newFakeExec(h.clk)
	exec.probe = busyProbe("t1")
	exec.interrupt = func(InterruptCall) ControlResult {
		h.clk.Advance(DefaultLeaseTTL - ExecutorMargin - nowDeliveryMinBudget + time.Second)
		return Settled{Turn: "t1", After: afterT1()}
	}
	mustPass(t, h.lane(t, "ctl"), view(), exec)
	got := h.get(t, m.ID)
	if len(exec.delivered()) != 0 || got.Status != StatusAccepted || !strings.Contains(got.LastError, "no time left") {
		t.Fatalf("deliveries %d message %s %q", len(exec.delivered()), got.Status, got.LastError)
	}
	wantOutcome(t, h.get(t, nowID(t, m)), ArmSucceeded, "")
}

// Cancels: a message canceled before its unit is taken cancels its
// interrupt too (the cascade, N7); a cancel while the unit runs loses to a
// delivered message and wins over one not sent; an interrupt canceled while
// it runs closes canceled and its message waits for the boundary unmarked.
func TestLaneNowCancel(t *testing.T) {
	t.Run("cascade", func(t *testing.T) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		exec := newFakeExec(h.clk)
		exec.probe.Available = false
		l := h.lane(t, "ctl")
		if res := mustPass(t, l, view(), exec); !slices.Equal(res.Lowered, []string{m.ID}) || res.Took != TakeNone {
			t.Fatalf("pass %+v", res)
		}
		if r, _, err := h.store.Cancel(m.ID, "operator", "changed my mind"); err != nil || r != CancelClosed {
			t.Fatalf("Cancel = %s, %v", r, err)
		}
		res := mustPass(t, l, view(), exec)
		i := h.get(t, nowID(t, m))
		wantOutcome(t, i, ArmCanceled, "")
		if i.Cancel.By != "lane:cascade" || !strings.Contains(i.Cancel.Reason, m.ID) || len(res.Closed) != 1 {
			t.Fatalf("interrupt cancel %+v closed %+v", i.Cancel, res.Closed)
		}
		if len(exec.interrupts)+len(exec.delivered()) != 0 {
			t.Fatal("a canceled unit ran")
		}
	})
	t.Run("delivered wins", func(t *testing.T) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		exec := newFakeExec(h.clk)
		exec.onEffect = func() {
			if _, _, err := h.store.Cancel(m.ID, "operator", ""); err != nil {
				t.Errorf("Cancel: %v", err)
			}
		}
		mustPass(t, h.lane(t, "ctl"), view(), exec)
		wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
	})
	t.Run("not sent loses", func(t *testing.T) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.interrupt = settledT1(TurnAfter{Boundary: TriNo, Live: TriYes, Turn: "t2"})
		exec.onEffect = func() {
			if _, _, err := h.store.Cancel(m.ID, "operator", ""); err != nil {
				t.Errorf("Cancel: %v", err)
			}
		}
		mustPass(t, h.lane(t, "ctl"), view(), exec)
		wantOutcome(t, h.get(t, m.ID), ArmCanceled, "")
		wantOutcome(t, h.get(t, nowID(t, m)), ArmSucceeded, "")
	})
	t.Run("interrupt canceled", func(t *testing.T) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		iid := nowID(t, m)
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.interrupt = func(InterruptCall) ControlResult {
			if _, _, err := h.store.Cancel(iid, "operator", ""); err != nil {
				t.Errorf("Cancel: %v", err)
			}
			return NotDelivered{Transient: true, Cause: errors.New("busy")}
		}
		l := h.lane(t, "ctl")
		mustPass(t, l, view(), exec)
		wantOutcome(t, h.get(t, iid), ArmCanceled, "")
		exec.probe = idleProbe()
		mustPass(t, l, view(), exec)
		got := h.get(t, m.ID)
		wantOutcome(t, got, ArmSucceeded, "")
		if got.Outcome.Degradation != "" {
			t.Fatalf("message degradation %q", got.Outcome.Degradation)
		}
	})
}

// No after link joins a unit (D-DN): the derived interrupt names none, the
// message's own after is unchanged, and the message goes whether its
// interrupt was skipped or failed; no op of a unit ever closes
// skipped{dependency_failed}.
func TestLaneNowNoAfterLink(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe Probe
		res   ControlResult
	}{
		{"skipped", idleProbe(), nil},
		{"failed", busyProbe("t1"), Rejected{Arm: ArmFailed, Reason: ReasonNoBoundary, Turn: "t1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			first := h.propose(t, laneMsg("first", RoleNotice, "x"))
			h.clk.Advance(time.Millisecond)
			m := h.propose(t, nowReq("a", func(r *Request) { r.After = first.ID }))
			exec := newFakeExec(h.clk)
			l := h.lane(t, "ctl")
			mustPass(t, l, view(), exec) // the notice
			exec.probe = tc.probe
			exec.interrupt = func(InterruptCall) ControlResult { return tc.res }
			mustPass(t, l, view(), exec)
			exec.probe = idleProbe()
			mustPass(t, l, view(), exec)
			i := h.get(t, nowID(t, m))
			got := h.get(t, m.ID)
			if i.After != "" || got.After != first.ID {
				t.Fatalf("interrupt after %q, message after %q", i.After, got.After)
			}
			wantOutcome(t, got, ArmSucceeded, "")
			for _, op := range []Op{i, got} {
				if op.Outcome.Reason == ReasonDependencyFailed {
					t.Fatalf("op %s closed dependency_failed", op.ID)
				}
			}
		})
	}
}

// fusedExec runs units as one command.
type fusedExec struct {
	*fakeExec
	mu    sync.Mutex
	calls []NowCall
	now   func(NowCall) NowResult
}

func (f *fusedExec) DeliverNow(_ context.Context, c NowCall) NowResult {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	return f.now(c)
}

// A fused executor runs the unit as one DeliverNow call; its two results
// settle the two ops; a nil delivery after an interrupt step that may have
// sent the command may have carried the message (Unknown, reported); and a
// probe that claims FusedNow for an executor without DeliverNow takes
// nothing.
func TestLaneNowFused(t *testing.T) {
	setup := func(t *testing.T, now func(NowCall) NowResult) (*laneHarness, *fusedExec, Op) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		exec := &fusedExec{fakeExec: newFakeExec(h.clk), now: now}
		exec.probe = busyProbe("t1")
		exec.probe.Caps.FusedNow = true
		return h, exec, m
	}
	t.Run("one command", func(t *testing.T) {
		var h *laneHarness
		h, exec, m := setup(t, func(NowCall) NowResult {
			return NowResult{Interrupt: Settled{Turn: "t1"}, Delivery: Delivered{Record: DeliveryRecord{At: h.clk.Now(), Ack: AckDelivered}}}
		})
		res := mustPass(t, h.lane(t, "ctl"), view(), exec)
		if res.Took != TakeNow || res.Done != DoneAccepted || len(exec.calls) != 1 || len(exec.interrupts) != 0 || len(exec.delivered()) != 0 {
			t.Fatalf("pass %+v calls %d interrupts %d deliveries %d", res, len(exec.calls), len(exec.interrupts), len(exec.delivered()))
		}
		c := exec.calls[0]
		if !c.Interrupt.ForReplacement || !c.Delivery.Replacement || c.Interrupt.Op.ID != nowID(t, m) || c.Delivery.Key != m.ID+":2" {
			t.Fatalf("call %+v", c)
		}
		i := h.get(t, nowID(t, m))
		wantOutcome(t, i, ArmSucceeded, "")
		if i.Outcome.Result.Turn != "t1" {
			t.Fatalf("interrupt %+v", i.Outcome)
		}
		wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
	})
	// A nil delivery says the command never reached the message, which
	// only an interrupt step that sent nothing can say; after any other
	// step the command may have carried it, so the message replays under
	// the command's key.
	for _, tc := range []struct {
		name string
		res  ControlResult
		sent bool
	}{
		{"settled", Settled{Turn: "t1"}, true},
		{"unknown", Unknown{Cause: errors.New("lost the answer")}, true},
		{"no boundary", Rejected{Arm: ArmFailed, Reason: ReasonNoBoundary, Turn: "t1"}, true},
		{"stale", Rejected{Arm: ArmSkipped, Reason: ReasonStale}, false},
		{"unsupported", Rejected{Arm: ArmFailed, Reason: ReasonUnsupported}, false},
		{"not delivered", NotDelivered{Transient: true, Cause: errors.New("busy socket")}, false},
	} {
		t.Run("nil delivery after "+tc.name, func(t *testing.T) {
			h, exec, m := setup(t, func(NowCall) NowResult { return NowResult{Interrupt: tc.res} })
			l := h.lane(t, "ctl")
			_, err := l.Pass(context.Background(), view(), exec)
			if (err != nil) != tc.sent {
				t.Fatalf("Pass = %v, want the contract breach reported %v", err, tc.sent)
			}
			got := h.get(t, m.ID)
			if !tc.sent {
				if got.Status != StatusAccepted || got.MaybeDelivered != 0 || !strings.HasPrefix(got.LastError, lastErrorNotAttempted) {
					t.Fatalf("message %+v, want released not attempted", got)
				}
				return
			}
			if got.Status != StatusAccepted || got.MaybeDelivered != 1 {
				t.Fatalf("message %+v, want released maybe delivered", got)
			}
			h.clk.Advance(ExecutorErrorBackoff)
			exec.probe = idleProbe()
			exec.probe.Caps.Dedup = true
			res := mustPass(t, l, view(), exec)
			ds := exec.delivered()
			if res.Took != TakeContent || len(ds) != 1 || !ds[0].Redelivery || ds[0].Key != exec.calls[0].Delivery.Key {
				t.Fatalf("pass %+v deliveries %+v, want a replay under the command's key %s", res, ds, exec.calls[0].Delivery.Key)
			}
			wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
		})
	}
	t.Run("fused_now without DeliverNow", func(t *testing.T) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.probe.Caps.FusedNow = true
		_, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec)
		if err == nil || !strings.Contains(err.Error(), "fused_now without DeliverNow") {
			t.Fatalf("Pass = %v", err)
		}
		if got := h.get(t, m.ID); got.Status != StatusAccepted || got.Attempt != 0 {
			t.Fatalf("message %+v, want lowered and never taken", got)
		}
	})
}

// Races: two lanes lowering one message write one derived interrupt (the
// loser's guarded batch is refused, and it reads again); a stale read that
// sees the interrupt without its message's move refuses nothing (its guard
// misses); and a Take lost to a cancel cascades.
func TestLaneNowRaces(t *testing.T) {
	t.Run("two lanes lowering", func(t *testing.T) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		exec := newFakeExec(h.clk)
		a, b := h.lane(t, "ctl"), h.lane(t, "cli")
		ran := false
		var inner PassResult
		a.hooks.at = func(s laneStage) error {
			if s == stageAfterRead && !ran {
				ran = true
				inner = mustPass(t, b, view(), exec)
			}
			return nil
		}
		outer := mustPass(t, a, view(), exec)
		if !slices.Equal(inner.Lowered, []string{m.ID}) || len(outer.Lowered) != 0 || outer.Took != TakeNone {
			t.Fatalf("inner %+v outer %+v, want one lowering and the loser taking nothing", inner, outer)
		}
		wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
		wantOutcome(t, h.get(t, nowID(t, m)), ArmSkipped, ReasonStale)
		if len(exec.delivered()) != 1 {
			t.Fatalf("%d deliveries, want one", len(exec.delivered()))
		}
	})
	t.Run("stale read sees the interrupt", func(t *testing.T) {
		h, fs := newLaneHarnessFlaky(t)
		m := h.propose(t, nowReq("a"))
		iid := nowID(t, m)
		exec := newFakeExec(h.clk)
		a, b := h.lane(t, "ctl"), h.lane(t, "cli")
		ran := false
		fs.setFail(func(id string) bool {
			if id == iid && !ran {
				ran = true
				mustPass(t, b, view(), exec)
			}
			return false
		})
		mustPass(t, a, view(), exec)
		wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
		if len(exec.delivered()) != 1 {
			t.Fatalf("%d deliveries", len(exec.delivered()))
		}
	})
	t.Run("take lost to a cancel", func(t *testing.T) {
		h := newLaneHarness(t)
		m := h.propose(t, nowReq("a"))
		base := newFakeExec(h.clk)
		l := h.lane(t, "ctl")
		base.probe.Available = false
		mustPass(t, l, view(), base) // lowers
		base.probe.Available = true
		exec := &staleProbeExec{fakeExec: base, during: func() {
			if _, _, err := h.store.Cancel(m.ID, "operator", ""); err != nil {
				t.Errorf("Cancel: %v", err)
			}
		}}
		res := mustPass(t, l, view(), exec)
		if res.Took != TakeNone || len(base.delivered())+len(base.interrupts) != 0 {
			t.Fatalf("pass %+v", res)
		}
		i := h.get(t, nowID(t, m))
		wantOutcome(t, i, ArmCanceled, "")
		if i.Cancel.By != "lane:cascade" {
			t.Fatalf("interrupt cancel %+v", i.Cancel)
		}
	})
}

// A unit's text is its message's alone, never carrying an op id, a delivery
// key or a principal (I14).
func TestLaneNowText(t *testing.T) {
	for _, role := range []Role{RolePrompt, RoleNotice} {
		h := newLaneHarness(t)
		m := h.propose(t, laneMsg("now-key-17", role, "look at the new logs", withWhen(WhenNow)))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.interrupt = settledT1(afterT1())
		mustPass(t, h.lane(t, "ctl"), view(), exec)
		d := exec.delivered()[0]
		want, _ := RenderDelivery([]Op{m}, TextReminder, false)
		if d.Text != want {
			t.Fatalf("%s text %q, want %q", role, d.Text, want)
		}
		i := h.get(t, nowID(t, m))
		assertNoIdentifiers(t, d.Text, []Op{h.get(t, m.ID), i}, d.Key)
	}
}

// U-ORDER across a failed read (N3): a lowered message whose producer after
// cannot be read on a later pass still forms its unit, so content that
// arrived after it is never taken while the unit is open, and the unit's
// interrupt never cuts a turn that content started.
func TestLaneNowUOrderAcrossAnAfterRead(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	x := h.propose(t, laneMsg("x", RoleNotice, "earlier notice"))
	exec := newFakeExec(h.clk)
	l := h.lane(t, "ctl")
	mustPass(t, l, view(), exec)
	wantOutcome(t, h.get(t, x.ID), ArmSucceeded, "")
	h.clk.Advance(time.Millisecond)
	m := h.propose(t, nowReq("now", func(r *Request) { r.After = x.ID }))
	exec.probe.Available = false
	if res := mustPass(t, l, view(), exec); !slices.Equal(res.Lowered, []string{m.ID}) {
		t.Fatalf("lowering pass %+v", res)
	}
	h.clk.Advance(time.Millisecond)
	n := h.propose(t, laneMsg("later", RolePrompt, "other work"))
	fs.setFail(func(id string) bool { return id == x.ID })
	exec.probe = idleProbe()
	res := mustPass(t, l, view(), exec)
	fs.setFail(nil)
	if res.Took != TakeNow || !slices.Equal(res.Taken, []string{nowID(t, m), m.ID}) {
		t.Fatalf("pass %+v, want the unit with its after unread", res)
	}
	if w := waitOf(res.Waits, n.ID); w.Reason != WaitBlocked {
		t.Fatalf("later content wait %+v, want blocked by the unit", w)
	}
	if ds := exec.delivered(); len(ds) != 2 || ds[1].Ops[0].ID != m.ID {
		t.Fatalf("deliveries %+v, want the message before the later content", ds)
	}
}

// U-ORDER across a failed read of an unlowered message's after (N3): a due
// deliver-now message a pass cannot lower only because its after target's
// read failed transiently holds the content, since another host that reads
// the target meanwhile lowers it: content taken then would run while that
// unit is open. Once the read succeeds the unit goes before the content. A
// message that is not due yet (not_before ahead) holds nothing.
func TestLaneNowUOrderAcrossAnUnloweredAfterRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		// notBefore delays the message past the pass.
		notBefore time.Duration
	}{{name: "due"}, {name: "not due", notBefore: time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs := newLaneHarnessFlaky(t)
			x := h.propose(t, laneMsg("x", RoleNotice, "earlier notice"))
			exec := newFakeExec(h.clk)
			outer := h.lane(t, "ctl")
			mustPass(t, outer, view(), exec)
			wantOutcome(t, h.get(t, x.ID), ArmSucceeded, "")
			h.clk.Advance(time.Millisecond)
			m := h.propose(t, nowReq("now", func(r *Request) {
				r.After = x.ID
				if tc.notBefore > 0 {
					r.NotBefore = h.clk.Now().Add(tc.notBefore)
				}
			}))
			h.clk.Advance(time.Millisecond)
			n := h.propose(t, laneMsg("later", RoleNotice, "other work"))
			// The other host reads through a store whose reads succeed.
			steady, err := NewLaneStore(beads.SessionStore{Store: &flakyGetStore{MemStore: h.mem, batcher: h.batcher}}, h.clk, WithIDPrefix("gc"))
			if err != nil {
				t.Fatal(err)
			}
			other, err := NewLane(steady, "cli", h.clk)
			if err != nil {
				t.Fatal(err)
			}
			other.sleep = func(context.Context, time.Duration) error { return nil }
			otherExec := newFakeExec(h.clk)
			otherExec.probe = notLiveProbe()
			var inner PassResult
			ran := false
			outer.hooks.at = func(s laneStage) error {
				if s == stageAfterRead && !ran {
					ran = true
					inner = mustPass(t, other, view(), otherExec)
				}
				return nil
			}
			fs.setFail(func(id string) bool { return id == x.ID })
			res := mustPass(t, outer, view(), exec)
			fs.setFail(nil)
			outer.hooks.at = nil
			if w := waitOf(res.Waits, m.ID); w.Reason != WaitAfterUnreadable && w.Reason != WaitNotBefore {
				t.Fatalf("message wait %+v, want its after unreadable or its not_before", w)
			}
			if tc.notBefore > 0 {
				if res.Took != TakeContent || !slices.Equal(res.Taken, []string{n.ID}) {
					t.Fatalf("pass %+v, want the content: a message not due holds nothing", res)
				}
				return
			}
			if !slices.Equal(inner.Lowered, []string{m.ID}) {
				t.Fatalf("other host's pass %+v, want the message lowered", inner)
			}
			if res.Took != TakeNone || len(exec.delivered()) != 1 {
				t.Fatalf("pass %+v after %d deliveries, want the content held", res, len(exec.delivered()))
			}
			if w := waitOf(res.Waits, n.ID); w.Reason != WaitBlocked {
				t.Fatalf("later content wait %+v, want blocked by the unit", w)
			}
			exec.probe = idleProbe()
			if res := mustPass(t, outer, view(), exec); res.Took != TakeNow || !slices.Equal(res.Taken, []string{nowID(t, m), m.ID}) {
				t.Fatalf("pass %+v, want the unit first", res)
			}
			mustPass(t, outer, view(), exec)
			if ds := exec.delivered(); len(ds) != 3 || ds[1].Ops[0].ID != m.ID || ds[2].Ops[0].ID != n.ID {
				t.Fatalf("deliveries %+v, want the message before the later content", ds)
			}
		})
	}
}

// A fused command whose interrupt did not settle leaves the message's link
// evidence the probe's: only a settled interrupt says no turn is live, so a
// link the probe's evidence grades ambiguous stays ambiguous (D23).
func TestLaneNowFusedUnsettledKeepsTheProbeEvidence(t *testing.T) {
	h := newLaneHarness(t)
	m := h.propose(t, nowReq("a", nowTarget(1, "t1")))
	exec := &fusedExec{fakeExec: newFakeExec(h.clk)}
	exec.probe = idleProbe()
	exec.probe.Boundary, exec.probe.TurnLive = TriNo, TriUnknown
	exec.probe.Caps.FusedNow = true
	exec.now = func(NowCall) NowResult {
		return NowResult{
			Interrupt: Rejected{Arm: ArmSkipped, Reason: ReasonStale},
			Delivery: Delivered{
				Record:   DeliveryRecord{At: h.clk.Now(), Ack: AckDelivered},
				Evidence: TurnEvidence{HasSource: true, Seen: []SeenTurn{{ID: "t3", Generation: 1}}, FirstRead: FirstTurnRead{Done: true, Current: "t3"}},
				Via:      "fake",
			},
		}
	}
	mustPass(t, h.lane(t, "ctl"), view(), exec)
	got := h.get(t, m.ID)
	if r := got.Outcome.Result; got.Outcome.Arm != ArmSucceeded || r.TurnLink != LinkAmbiguous {
		t.Fatalf("message %s link %s/%s, want ambiguous from the probe's evidence", got.Outcome.Arm, r.TurnLink, r.TurnBasis)
	}
}

// A unit Take that never sent its message is no attempt of it: after the
// unit's transient retries, the message's first real send is attempt 1 and
// a transient miss of it backs off from the base.
func TestLaneNowUnattemptedTakesAreNoAttempts(t *testing.T) {
	h := newLaneHarness(t)
	m := h.propose(t, nowReq("a"))
	exec := newFakeExec(h.clk)
	exec.probe = busyProbe("t1")
	l := h.lane(t, "ctl")
	exec.interrupt = func(InterruptCall) ControlResult {
		return NotDelivered{Transient: true, Cause: errors.New("busy socket")}
	}
	for range 3 {
		mustPass(t, l, view(), exec)
		if got := h.get(t, m.ID); got.Attempt != 0 {
			t.Fatalf("message attempt %d after an unsent unit", got.Attempt)
		}
		i := h.get(t, nowID(t, m))
		h.clk.Advance(i.RetryAt.Sub(h.clk.Now()) + time.Millisecond)
	}
	exec.interrupt = settledT1(afterT1())
	exec.deliver = func(Delivery) DeliveryResult { return NotDelivered{Transient: true, Cause: errors.New("pane busy")} }
	mustPass(t, l, view(), exec)
	got := h.get(t, m.ID)
	if got.Attempt != 1 || got.Failures != 0 || !got.RetryAt.Equal(h.clk.Now().Add(TransientBackoffBase)) {
		t.Fatalf("message attempt %d failures %d retry in %s, want attempt 1 backing off %s", got.Attempt, got.Failures, got.RetryAt.Sub(h.clk.Now()), TransientBackoffBase)
	}
}

// A plain lane interrupt keyed <X>:interrupt with causation X runs as any
// interrupt unless X is an op that is a deliver-now message: a causation
// that is no op id, a row that is no op, an op of a kind this build does
// not know, or another namespace's op id with no row here leaves it plain.
// An op id of this namespace with no row is an orphan, as before.
func TestLaneNowPlainInterruptShapes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		causation func(t *testing.T, h *laneHarness) string
		runs      bool
	}{
		{"a mail bead", func(t *testing.T, h *laneHarness) string {
			b, err := h.mem.Create(beads.Bead{Title: "a mail bead", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			return b.ID
		}, true},
		{"a producer's own id", func(*testing.T, *laneHarness) string { return "t3-msg-42" }, true},
		{"an op-shaped bead that is no op", func(t *testing.T, h *laneHarness) string {
			b, err := h.mem.Create(beads.Bead{ID: "gc-op-abcdefghijklmnopqrstuvwxyz", Title: "not an op", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			return b.ID
		}, true},
		{"an op of an unknown kind", func(t *testing.T, h *laneHarness) string {
			req := laneMsg("future", RoleNotice, "x")
			req.Conversation = "gc-2"
			op, err := h.store.Preview(req)
			if err != nil {
				t.Fatal(err)
			}
			b, err := EncodeOp(op)
			if err != nil {
				t.Fatal(err)
			}
			b.Metadata[keyKind] = "teleport"
			if _, err := h.mem.Create(b); err != nil {
				t.Fatal(err)
			}
			return op.ID
		}, true},
		{"another namespace's op id with no row", func(*testing.T, *laneHarness) string { return "old-op-abcdefghijklmnopqrstuvwxy2" }, true},
		{"an op id with no row", func(*testing.T, *laneHarness) string { return "gc-op-abcdefghijklmnopqrstuvwxy2" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			c := tc.causation(t, h)
			req := laneInterrupt(c+":interrupt", "")
			req.Causation = c
			x := h.propose(t, req)
			exec := newFakeExec(h.clk)
			exec.probe = busyProbe("t1")
			mustPass(t, h.lane(t, "ctl"), view(), exec)
			got := h.get(t, x.ID)
			if tc.runs {
				if len(exec.interrupts) != 1 {
					t.Fatalf("plain interrupt not run: %s %s{%s}", got.Status, got.Outcome.Arm, got.Outcome.Reason)
				}
				return
			}
			if len(exec.interrupts) != 0 {
				t.Fatalf("orphan interrupt ran")
			}
			wantOutcome(t, got, ArmSkipped, ReasonNotNeeded)
		})
	}
}

// storeIn is a lane store over h's beads that mints ids in namespace
// prefix: the store a conversation's ops lived in before they moved here
// with their ids.
func (h *laneHarness) storeIn(t *testing.T, prefix string) *Store {
	t.Helper()
	s, err := NewLaneStore(beads.SessionStore{Store: laneRecStore{OpRecordingStore: h.rec, batcher: h.batcher}}, h.clk, WithIDPrefix(prefix))
	if err != nil {
		t.Fatalf("NewLaneStore(%s): %v", prefix, err)
	}
	return s
}

// A deliver-now message proposed under another namespace's prefix (moved
// here with its id unchanged) is lowered by this lane, its derived
// interrupt pinned in this namespace, and the two run as one unit: the
// interrupt's replacement step, then the message at the boundary the step
// reached.
func TestLaneNowForeignPrefixMessage(t *testing.T) {
	h := newLaneHarness(t)
	m, created, err := h.storeIn(t, "old").Propose(nowReq("a"))
	if err != nil || !created {
		t.Fatalf("Propose: created %v, %v", created, err)
	}
	iid := nowID(t, m)
	exec := newFakeExec(h.clk)
	exec.probe = busyProbe("t1")
	exec.interrupt = settledT1(afterT1())
	res := mustPass(t, h.lane(t, "ctl"), view(), exec)
	if !slices.Equal(res.Lowered, []string{m.ID}) || res.Took != TakeNow || !slices.Equal(res.Taken, []string{iid, m.ID}) {
		t.Fatalf("pass %+v, want the message lowered here and its unit taken", res)
	}
	if len(exec.interrupts) != 1 || !exec.interrupts[0].ForReplacement || exec.interrupts[0].Op.ID != iid {
		t.Fatalf("interrupt calls %+v, want the unit's replacement step", exec.interrupts)
	}
	if ds := exec.delivered(); len(ds) != 1 || !ds[0].Replacement || ds[0].Ops[0].ID != m.ID {
		t.Fatalf("deliveries %+v, want the message after the step", ds)
	}
	wantOutcome(t, h.get(t, iid), ArmSucceeded, "")
	wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
}

// A deliver-now message that moved here with its id from another namespace
// runs as one unit with its derived interrupt whether this lane lowered it
// (the interrupt pinned in this namespace) or a lane of the message's own
// namespace did before the move (pinned in that one). While the interrupt
// waits out a backoff, the boundary types neither the message nor content
// proposed after it (U-ORDER, T5); the interrupt then runs as the unit's
// replacement step, with the message right after it. A cancel of the
// message while the interrupt waits cascades to the interrupt, and the
// session is not interrupted again.
func TestLaneNowForeignPrefixUnit(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lowerer is the namespace of the lane that lowers the message.
		lowerer string
	}{
		{"lowered here", "gc"},
		{"lowered in its namespace", "old"},
	} {
		// setup proposes deliver-now message m in namespace old, and a lane
		// of the lowerer's namespace lowers it and runs its unit once while
		// turn t1 runs: the step does not reach the runtime, so interrupt i
		// is released into its backoff and m is not attempted. l is this
		// namespace's lane.
		setup := func(t *testing.T) (h *laneHarness, exec *fakeExec, l *ConversationLane, m, i Op) {
			t.Helper()
			h = newLaneHarness(t)
			m, created, err := h.storeIn(t, "old").Propose(nowReq("a"))
			if err != nil || !created {
				t.Fatalf("Propose: created %v, %v", created, err)
			}
			l = h.lane(t, "ctl")
			lowerer := l
			if tc.lowerer != "gc" {
				if lowerer, err = NewLane(h.storeIn(t, tc.lowerer), tc.lowerer, h.clk); err != nil {
					t.Fatalf("NewLane: %v", err)
				}
			}
			exec = newFakeExec(h.clk)
			exec.probe = busyProbe("t1")
			exec.interrupt = func(InterruptCall) ControlResult {
				return NotDelivered{Transient: true, Cause: errors.New("busy socket")}
			}
			res := mustPass(t, lowerer, view(), exec)
			iid, err := NowInterruptID(tc.lowerer, m)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.Lowered, []string{m.ID}) || res.Took != TakeNow || !slices.Equal(res.Taken, []string{iid, m.ID}) {
				t.Fatalf("lowering pass %+v, want %s lowered and its unit taken", res, m.ID)
			}
			i = h.get(t, iid)
			if i.Status != StatusAccepted || !i.RetryAt.After(h.clk.Now()) {
				t.Fatalf("interrupt %s retry %s, want released into its backoff", i.Status, i.RetryAt)
			}
			return h, exec, l, m, i
		}
		t.Run(tc.name+": the unit holds its message and later content", func(t *testing.T) {
			h, exec, l, m, i := setup(t)
			c := h.propose(t, laneMsg("later", RolePrompt, "other work"))
			// The turn ends on its own before the interrupt's backoff does.
			exec.probe = idleProbe()
			res := mustPass(t, l, view(), exec)
			if res.Took != TakeNone || len(exec.delivered()) != 0 {
				t.Fatalf("boundary pass %+v deliveries %+v, want nothing typed while the unit waits", res, exec.delivered())
			}
			if w := waitOf(res.Waits, m.ID); w.Reason != WaitRetryBackoff {
				t.Fatalf("message wait %+v, want its unit's backoff", w)
			}
			if w := waitOf(res.Waits, c.ID); w.Reason != WaitBlocked {
				t.Fatalf("later content wait %+v, want blocked by the unit", w)
			}
			// After the backoff turn t2 runs: the unit replaces it.
			h.clk.Advance(i.RetryAt.Sub(h.clk.Now()) + time.Millisecond)
			exec.probe = busyProbe("t2")
			exec.interrupt = func(InterruptCall) ControlResult {
				return Settled{Turn: "t2", Via: "fake", After: TurnAfter{Boundary: TriYes, Live: TriNo, Known: []string{"t2"}}}
			}
			res = mustPass(t, l, view(), exec)
			if res.Took != TakeNow || !slices.Equal(res.Taken, []string{i.ID, m.ID}) {
				t.Fatalf("retry pass %+v, want the unit", res)
			}
			if n := len(exec.interrupts); n != 2 || !exec.interrupts[1].ForReplacement || exec.interrupts[1].Op.ID != i.ID {
				t.Fatalf("interrupt calls %+v, want the unit's replacement step", exec.interrupts)
			}
			if ds := exec.delivered(); len(ds) != 1 || !ds[0].Replacement || ds[0].Ops[0].ID != m.ID {
				t.Fatalf("deliveries %+v, want the message right after the step", ds)
			}
			wantOutcome(t, h.get(t, i.ID), ArmSucceeded, "")
			wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
			exec.probe = idleProbe()
			mustPass(t, l, view(), exec)
			if ds := exec.delivered(); len(ds) != 2 || ds[1].Ops[0].ID != c.ID {
				t.Fatalf("deliveries %+v, want the later content after the unit", ds)
			}
		})
		t.Run(tc.name+": a cancel of the message cascades", func(t *testing.T) {
			h, exec, l, m, i := setup(t)
			if r, _, err := h.store.Cancel(m.ID, "operator", "changed my mind"); err != nil || r != CancelClosed {
				t.Fatalf("Cancel = %s, %v", r, err)
			}
			h.clk.Advance(i.RetryAt.Sub(h.clk.Now()) + time.Millisecond)
			exec.probe = busyProbe("t2")
			mustPass(t, l, view(), exec)
			got := h.get(t, i.ID)
			wantOutcome(t, got, ArmCanceled, "")
			if got.Cancel.By != nowCascadeBy || len(exec.interrupts) != 1 || len(exec.delivered()) != 0 {
				t.Fatalf("interrupt cancel %+v, %d interrupt calls, %d deliveries: want the cascade and nothing run", got.Cancel, len(exec.interrupts), len(exec.delivered()))
			}
		})
	}
}

// A producer's lane interrupt keyed as a deliver-now message's derived
// interrupt, at the id the message's own namespace pins, runs plain while
// the message has no row here. Once the message is proposed, a pass that
// sees it beside that interrupt, in flight or after it ran, refuses it
// (now_interrupt_taken) rather than lowering it: no second interrupt is
// minted here, and the session is interrupted once (T5).
func TestLaneNowUnloweredBesideItsInterrupt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// inFlight proposes the message, and runs another host's pass over
		// it, while the interrupt is in flight.
		inFlight bool
	}{
		{"after the interrupt ran", false},
		{"while the interrupt is in flight", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			old := h.storeIn(t, "old")
			req := nowReq("a")
			m, err := old.Preview(req)
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			ireq := laneInterrupt(m.ID+nowInterruptKeySuffix, "")
			ireq.Producer, ireq.Causation = m.Producer, m.ID
			i, created, err := old.Propose(ireq)
			if err != nil || !created {
				t.Fatalf("Propose(interrupt): created %v, %v", created, err)
			}
			if own, err := NowInterruptID("old", m); err != nil || i.ID != own {
				t.Fatalf("interrupt %s, want the message's own-namespace pin %s (%v)", i.ID, own, err)
			}
			propose := func() {
				t.Helper()
				if _, created, err := old.Propose(req); err != nil || !created {
					t.Fatalf("Propose(message): created %v, %v", created, err)
				}
			}
			exec := newFakeExec(h.clk)
			exec.probe = busyProbe("t1")
			exec.interrupt = func(c InterruptCall) ControlResult {
				if tc.inFlight && c.Op.ID == i.ID {
					propose()
					mustPass(t, h.lane(t, "ctl2"), view(), newFakeExec(h.clk))
					wantOutcome(t, h.get(t, m.ID), ArmFailed, refusedNowInterruptTaken)
				}
				return Settled{Turn: "t1", Via: "fake"}
			}
			l := h.lane(t, "ctl")
			if res := mustPass(t, l, view(), exec); res.Took != TakeControl || !slices.Equal(res.Taken, []string{i.ID}) {
				t.Fatalf("pass %+v, want the interrupt taken plain", res)
			}
			if !tc.inFlight {
				propose()
			}
			exec.probe = busyProbe("t2")
			res := mustPass(t, l, view(), exec)
			if len(res.Lowered) != 0 || len(exec.interrupts) != 1 {
				t.Fatalf("pass %+v after %d interrupt calls, want %s never lowered and the session interrupted once", res, len(exec.interrupts), m.ID)
			}
			wantOutcome(t, h.get(t, m.ID), ArmFailed, refusedNowInterruptTaken)
			wantOutcome(t, h.get(t, i.ID), ArmSucceeded, "")
			if _, err := h.store.Get(nowID(t, m)); !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("Get(%s) = %v, want no interrupt minted here", nowID(t, m), err)
			}
		})
	}
}

// A fused command whose interrupt settled turn t1, run on a probe that
// could not read the session's turns (no known turns), still counts t1 as
// known, so the message's link never names the replaced turn: the one new
// turn t2 is correlated.
func TestLaneNowFusedSettledKnowsTheReplacedTurn(t *testing.T) {
	h := newLaneHarness(t)
	m := h.propose(t, nowReq("a"))
	exec := &fusedExec{fakeExec: newFakeExec(h.clk)}
	exec.probe = idleProbe()
	exec.probe.Boundary, exec.probe.TurnLive = TriNo, TriUnknown
	exec.probe.Caps.FusedNow = true
	exec.now = func(NowCall) NowResult {
		return NowResult{
			Interrupt: Settled{Turn: "t1", Via: "fake"},
			Delivery: Delivered{
				Record:   DeliveryRecord{At: h.clk.Now(), Ack: AckDelivered},
				Evidence: TurnEvidence{HasSource: true, Seen: []SeenTurn{{ID: "t1", Generation: 1}, {ID: "t2", Generation: 1}}, FirstRead: FirstTurnRead{Done: true, Current: "t2"}},
				Via:      "fake",
			},
		}
	}
	mustPass(t, h.lane(t, "ctl"), view(), exec)
	got := h.get(t, m.ID)
	r := got.Outcome.Result
	if got.Outcome.Arm != ArmSucceeded || r.Turn != "t2" || r.TurnLink != LinkCorrelated {
		t.Fatalf("message %s link %s/%s/%s, want t2 correlated (the replaced t1 is never a candidate)", got.Outcome.Arm, r.Turn, r.TurnLink, r.TurnBasis)
	}
}

// A deliver-now message whose derived id cannot be read on its first pass
// is not lowered, and content proposed after it waits: the next pass lowers
// it, and its unit runs before that content.
func TestLaneNowUnloweredUnreadableHoldsLaterContent(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	m := h.propose(t, nowReq("now"))
	h.clk.Advance(time.Millisecond)
	n := h.propose(t, laneMsg("later", RolePrompt, "other work"))
	iid := nowID(t, m)
	exec := newFakeExec(h.clk)
	exec.probe = busyProbe("t1")
	exec.interrupt = settledT1(afterT1())
	l := h.lane(t, "ctl")
	fs.setFail(func(id string) bool { return id == iid })
	res := mustPass(t, l, view(), exec)
	fs.setFail(nil)
	if res.Took == TakeContent || len(res.Lowered) != 0 {
		t.Fatalf("pass 1 took %s %v lowered %v, want nothing taken while %s waits its read", res.Took, res.Taken, res.Lowered, m.ID)
	}
	if w := waitOf(res.Waits, n.ID); w.Reason != WaitBlocked {
		t.Fatalf("content wait %+v, want blocked", w)
	}
	h.clk.Advance(TransientBackoffBase + time.Millisecond)
	res = mustPass(t, l, view(), exec)
	if !slices.Equal(res.Lowered, []string{m.ID}) || res.Took != TakeNow {
		t.Fatalf("pass 2 %+v, want the unit lowered and taken", res)
	}
	if got := h.get(t, n.ID); got.Status == StatusClosed {
		t.Fatalf("content %s closed before the unit's pass", n.ID)
	}
}
