package opjournal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
				!strings.HasPrefix(got.LastError, lastErrorNotAttempted+": ") || !strings.Contains(got.LastError, tc.why) {
				t.Fatalf("message %s retry %s failures %d maybe %d last_error %q", got.Status, got.RetryAt, got.Failures, got.MaybeDelivered, got.LastError)
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
// settle the two ops; a nil delivery after a settled interrupt may have
// carried the message (Unknown, reported); and a probe that claims
// FusedNow for an executor without DeliverNow takes nothing.
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
	t.Run("nil delivery after a settled interrupt", func(t *testing.T) {
		h, exec, m := setup(t, func(NowCall) NowResult { return NowResult{Interrupt: Settled{Turn: "t1"}} })
		if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec); err == nil {
			t.Fatal("the contract breach was not reported")
		}
		got := h.get(t, m.ID)
		if got.Status != StatusAccepted || got.MaybeDelivered != 1 {
			t.Fatalf("message %+v, want released maybe delivered", got)
		}
	})
	t.Run("nil delivery after a stale interrupt", func(t *testing.T) {
		h, exec, m := setup(t, func(NowCall) NowResult {
			return NowResult{Interrupt: Rejected{Arm: ArmSkipped, Reason: ReasonStale}}
		})
		mustPass(t, h.lane(t, "ctl"), view(), exec)
		got := h.get(t, m.ID)
		if got.Status != StatusAccepted || got.MaybeDelivered != 0 || !strings.HasPrefix(got.LastError, lastErrorNotAttempted) {
			t.Fatalf("message %+v", got)
		}
		wantOutcome(t, h.get(t, nowID(t, m)), ArmSkipped, ReasonStale)
	})
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
