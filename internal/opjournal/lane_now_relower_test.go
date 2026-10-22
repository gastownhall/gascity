package opjournal

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// relowerBy is the transition author of the re-lowerings these tests run.
const relowerBy = "gc:storage-migrate"

// oldLane is a lane of namespace old over h's beads: the lane a store that
// moved here with its ids ran before the move.
func (h *laneHarness) oldLane(t *testing.T) *ConversationLane {
	t.Helper()
	l, err := NewLane(h.storeIn(t, "old"), "old", h.clk)
	if err != nil {
		t.Fatalf("NewLane(old): %v", err)
	}
	l.sleep = func(context.Context, time.Duration) error { return nil }
	return l
}

// notLiveProbe is a probe on which no unit is taken.
func notLiveProbe() Probe {
	p := idleProbe()
	p.Caps.Live = false
	return p
}

// viewOf is view() for conversation conv.
func viewOf(conv string) ConversationView {
	v := view()
	v.ID = conv
	return v
}

// inConv moves a request to conversation conv.
func inConv(conv string) func(*Request) {
	return func(r *Request) { r.Conversation = conv }
}

// migratedUnit proposes deliver-now message req in namespace old and lets a
// lane of that namespace lower it, as a store that moved here with its ids
// holds the unit: the message accepted, its derived interrupt submitted at
// NowInterruptID("old", m). The probe is not live, so the unit is not taken.
func (h *laneHarness) migratedUnit(t *testing.T, req Request) (Op, Op) {
	t.Helper()
	m, created, err := h.storeIn(t, "old").Propose(req)
	if err != nil || !created {
		t.Fatalf("Propose: created %v, %v", created, err)
	}
	exec := newFakeExec(h.clk)
	exec.probe = notLiveProbe()
	if res := mustPass(t, h.oldLane(t), viewOf(m.Conversation), exec); !slices.Equal(res.Lowered, []string{m.ID}) || res.Took != TakeNone {
		t.Fatalf("lowering pass %+v, want %s lowered and nothing taken", res, m.ID)
	}
	iid, err := NowInterruptID("old", m)
	if err != nil {
		t.Fatal(err)
	}
	i := h.get(t, iid)
	if m = h.get(t, m.ID); m.Status != StatusAccepted || i.Status != StatusSubmitted {
		t.Fatalf("message %s interrupt %s, want accepted and submitted", m.Status, i.Status)
	}
	h.clk.Advance(time.Millisecond)
	return m, i
}

// raw is row id as stored.
func (h *laneHarness) raw(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := h.mem.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// revisions are the stored revisions of ids.
func (h *laneHarness) revisions(t *testing.T, ids ...string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, id := range ids {
		out[id] = h.raw(t, id).Revision
	}
	return out
}

// absent fails the test unless no row holds id.
func (h *laneHarness) absent(t *testing.T, id string) {
	t.Helper()
	if _, err := h.mem.Get(id); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(%s) = %v, want no row", id, err)
	}
}

// relower runs the step and fails the test on an error.
func (h *laneHarness) relower(t *testing.T) RelowerReport {
	t.Helper()
	rep, err := h.store.RelowerNowUnits(context.Background(), relowerBy)
	if err != nil {
		t.Fatalf("RelowerNowUnits: %v (report %+v)", err, rep)
	}
	if rep.Prefix != "gc" {
		t.Fatalf("report prefix %q, want gc", rep.Prefix)
	}
	return rep
}

// batchesSince are the batches issued after the first n.
func (h *laneHarness) batchesSince(n int) []beads.GuardedBatch {
	return h.batcher.batches()[n:]
}

// wantPointer fails the test unless row id is a forward pointer to to,
// closed by the step, with its bead closed and no open key.
func (h *laneHarness) wantPointer(t *testing.T, id, to string) {
	t.Helper()
	got := h.get(t, id)
	wantOutcome(t, got, ArmSkipped, ReasonCoalescedInto)
	if got.Outcome.Result.CoalescedInto != to || !isForwardPointer(got) {
		t.Fatalf("pointer %s names %q, want %s", id, got.Outcome.Result.CoalescedInto, to)
	}
	if last := got.Transitions[len(got.Transitions)-1]; last.By != relowerBy || last.Status != StatusClosed {
		t.Fatalf("pointer's last transition %+v, want closed by %s", last, relowerBy)
	}
	if b := h.raw(t, id); b.Metadata[keyOpen] != "" || b.Status != beadStatusClosed {
		t.Fatalf("pointer row open key %q bead status %s, want inert", b.Metadata[keyOpen], b.Status)
	}
}

// The step moves an open unit's derived interrupt to the id a lowering here
// gives it in one two-item batch: the copy created at NowInterruptID("gc",
// M), equal to the row it read but for its id, revision and migration stamp
// (keys this build does not own included), and that row closed skipped{coalesced_into} under
// the revision it read. The message is not written (RL1 to RL3).
func TestRelowerNowUnitsRepinsAnOpenUnit(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		// mod moves the unit after its lowering.
		mod func(t *testing.T, h *laneHarness, m, i Op)
	}{
		{name: "fresh", req: nowReq("a")},
		{name: "released", req: nowReq("a"), mod: func(t *testing.T, h *laneHarness, _, _ Op) {
			exec := newFakeExec(h.clk)
			exec.probe = busyProbe("t1")
			exec.interrupt = func(InterruptCall) ControlResult { return NotDelivered{Cause: errors.New("refused")} }
			if res := mustPass(t, h.oldLane(t), view(), exec); res.Took != TakeNow || res.Done != DoneReleased {
				t.Fatalf("release pass %+v", res)
			}
		}},
		{name: "targeted at a turn", req: nowReq("a", nowTarget(1, "t1"))},
		{name: "a short start_by", req: nowReq("a", func(r *Request) { r.StartBy = t0.Add(20 * time.Second) })},
		{name: "a key this build does not own", req: nowReq("a"), mod: func(t *testing.T, h *laneHarness, _, i Op) {
			if err := h.mem.Update(i.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.op.zz_future": "kept"}}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stamped by the migration", req: nowReq("a"), mod: func(t *testing.T, h *laneHarness, _, i Op) {
			if err := h.mem.Update(i.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.InfraMigratedFromMetadataKey: "work"}}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			m, i := h.migratedUnit(t, tc.req)
			if tc.mod != nil {
				tc.mod(t, h, m, i)
			}
			if tc.name == "released" {
				if i = h.get(t, i.ID); i.Status != StatusAccepted || i.RetryAt.IsZero() || i.Failures != 1 || i.LastError == "" {
					t.Fatalf("interrupt %s retry %s failures %d last_error %q, want released", i.Status, i.RetryAt, i.Failures, i.LastError)
				}
			}
			home := nowID(t, m)
			before := h.raw(t, i.ID)
			mBefore := h.raw(t, m.ID)
			n := len(h.batcher.batches())
			rep := h.relower(t)
			if !slices.Equal(rep.Relowered, []Relowered{{Message: m.ID, From: i.ID, To: home}}) || rep.Home != 0 || len(rep.Left) != 0 {
				t.Fatalf("report %+v, want %s moved to %s", rep, i.ID, home)
			}
			bs := h.batchesSince(n)
			if len(bs) != 1 || len(bs[0].Items) != 2 || bs[0].Items[0].Create == nil || bs[0].Items[0].Create.ID != home ||
				bs[0].Items[1].Update == nil || bs[0].Items[1].Update.ID != i.ID ||
				bs[0].Items[1].Update.ExpectedRevision == nil || *bs[0].Items[1].Update.ExpectedRevision != before.Revision {
				t.Fatalf("batches %+v, want one: create %s, update %s at revision %d", bs, home, i.ID, before.Revision)
			}
			after := h.raw(t, home)
			// The copy is the step's row, not the migration's: it carries
			// no migration stamp, and the pointer keeps the one it had.
			wantMeta := maps.Clone(before.Metadata)
			delete(wantMeta, beadmeta.InfraMigratedFromMetadataKey)
			if stamp, ok := before.Metadata[beadmeta.InfraMigratedFromMetadataKey]; ok && h.raw(t, i.ID).Metadata[beadmeta.InfraMigratedFromMetadataKey] != stamp {
				t.Fatalf("pointer %s lost its migration stamp %q", i.ID, stamp)
			}
			if !maps.Equal(after.Metadata, wantMeta) || !slices.Equal(after.Labels, before.Labels) || after.Type != before.Type || after.Title != before.Title || after.Status != before.Status {
				t.Fatalf("copy %+v\nwant the row it read %+v", after, before)
			}
			got := h.get(t, home)
			read, err := DecodeOp(before)
			if err != nil {
				t.Fatal(err)
			}
			got.ID, got.Revision = read.ID, read.Revision
			if !opsEqual(got, read) {
				t.Fatalf("copy decodes as %+v\nwant %+v", got, read)
			}
			h.wantPointer(t, i.ID, home)
			if mAfter := h.raw(t, m.ID); mAfter.Revision != mBefore.Revision || !maps.Equal(mAfter.Metadata, mBefore.Metadata) {
				t.Fatalf("message written: revision %d -> %d", mBefore.Revision, mAfter.Revision)
			}
		})
	}
}

// opsEqual compares two decoded ops field by field.
func opsEqual(a, b Op) bool {
	ea, errA := EncodeOp(a)
	eb, errB := EncodeOp(b)
	return errA == nil && errB == nil && maps.Equal(ea.Metadata, eb.Metadata) && a.Revision == b.Revision
}

// A moved unit runs as one unit under its new id: taken [copy, M], one
// replacement step on the copy, then M; while the copy backs off nothing is
// typed (U-ORDER); a cancel of M cascades to the copy; a step that ends
// past another turn leaves M post-unit, and a failed step marks it. The
// pointer is never taken and never decides an outcome, and the session is
// interrupted at most once per unit (D-DN for moved units).
func TestRelowerNowUnitsRunsAsOneUnit(t *testing.T) {
	// moved is a migrated unit after the step: m and its copy.
	moved := func(t *testing.T, h *laneHarness, req Request) (Op, Op, string) {
		t.Helper()
		m, i := h.migratedUnit(t, req)
		h.relower(t)
		return m, h.get(t, nowID(t, m)), i.ID
	}
	// noOldTake fails the test if any interrupt call ran the pointer.
	noOldTake := func(t *testing.T, exec *fakeExec, old string) {
		t.Helper()
		for _, c := range exec.interrupts {
			if c.Op.ID == old {
				t.Fatalf("the pointer %s was run", old)
			}
		}
	}
	t.Run("the unit runs", func(t *testing.T) {
		h := newLaneHarness(t)
		m, c, old := moved(t, h, nowReq("a"))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.interrupt = settledT1(afterT1())
		res := mustPass(t, h.lane(t, "ctl"), view(), exec)
		if res.Took != TakeNow || !slices.Equal(res.Taken, []string{c.ID, m.ID}) {
			t.Fatalf("pass %+v, want the unit [%s %s]", res, c.ID, m.ID)
		}
		if len(exec.interrupts) != 1 || !exec.interrupts[0].ForReplacement || exec.interrupts[0].Op.ID != c.ID {
			t.Fatalf("interrupt calls %+v, want one replacement step on %s", exec.interrupts, c.ID)
		}
		if ds := exec.delivered(); len(ds) != 1 || !ds[0].Replacement || ds[0].Ops[0].ID != m.ID {
			t.Fatalf("deliveries %+v, want the message after the step", ds)
		}
		wantOutcome(t, h.get(t, c.ID), ArmSucceeded, "")
		wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
		h.wantPointer(t, old, c.ID)
		noOldTake(t, exec, old)
	})
	t.Run("a backoff holds the boundary", func(t *testing.T) {
		h := newLaneHarness(t)
		m, i := h.migratedUnit(t, nowReq("a"))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.interrupt = func(InterruptCall) ControlResult {
			return NotDelivered{Transient: true, Cause: errors.New("busy socket")}
		}
		mustPass(t, h.oldLane(t), view(), exec)
		released := h.get(t, i.ID)
		h.relower(t)
		c := h.get(t, nowID(t, m))
		if c.Status != StatusAccepted || !c.RetryAt.After(h.clk.Now()) || !c.RetryAt.Equal(released.RetryAt) {
			t.Fatalf("copy %s retry %s, want the backoff kept", c.Status, c.RetryAt)
		}
		later := h.propose(t, laneMsg("later", RolePrompt, "other work"))
		l := h.lane(t, "ctl")
		exec.probe = idleProbe()
		res := mustPass(t, l, view(), exec)
		if res.Took != TakeNone || len(exec.delivered()) != 0 {
			t.Fatalf("boundary pass %+v, want nothing typed while the unit backs off", res)
		}
		if w := waitOf(res.Waits, later.ID); w.Reason != WaitBlocked {
			t.Fatalf("later content wait %+v, want blocked by the unit", w)
		}
		h.clk.Advance(c.RetryAt.Sub(h.clk.Now()) + time.Millisecond)
		exec.probe = busyProbe("t2")
		exec.interrupt = func(InterruptCall) ControlResult {
			return Settled{Turn: "t2", Via: "fake", After: TurnAfter{Boundary: TriYes, Live: TriNo, Known: []string{"t2"}}}
		}
		if res = mustPass(t, l, view(), exec); res.Took != TakeNow || !slices.Equal(res.Taken, []string{c.ID, m.ID}) {
			t.Fatalf("retry pass %+v, want the unit under its new id", res)
		}
		wantOutcome(t, h.get(t, m.ID), ArmSucceeded, "")
		if len(exec.interrupts) != 2 || exec.interrupts[1].Op.ID != c.ID || !exec.interrupts[1].ForReplacement {
			t.Fatalf("interrupt calls %+v, want the released one, then the retry on %s", exec.interrupts, c.ID)
		}
	})
	t.Run("a cancel of the message cascades", func(t *testing.T) {
		h := newLaneHarness(t)
		m, c, old := moved(t, h, nowReq("a"))
		if r, _, err := h.store.Cancel(m.ID, "operator", "changed my mind"); err != nil || r != CancelClosed {
			t.Fatalf("Cancel = %s, %v", r, err)
		}
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		mustPass(t, h.lane(t, "ctl"), view(), exec)
		got := h.get(t, c.ID)
		wantOutcome(t, got, ArmCanceled, "")
		if got.Cancel.By != nowCascadeBy || len(exec.interrupts) != 0 || len(exec.delivered()) != 0 {
			t.Fatalf("copy cancel %+v, %d interrupts, %d deliveries: want the cascade and nothing run", got.Cancel, len(exec.interrupts), len(exec.delivered()))
		}
		h.wantPointer(t, old, c.ID)
	})
	for _, tc := range []struct {
		name        string
		res         ControlResult
		degradation string
	}{
		{"another turn runs after the step", Settled{Turn: "t1", After: TurnAfter{Boundary: TriNo, Live: TriYes, Turn: "t2", Known: []string{"t1", "t2"}}}, ""},
		{"no boundary", Rejected{Arm: ArmFailed, Reason: ReasonNoBoundary, Turn: "t1"}, DegradationInterruptFailed},
		{"unsupported", Rejected{Arm: ArmFailed, Reason: ReasonUnsupported}, DegradationInterruptUnsupported},
	} {
		t.Run("post-unit: "+tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			m, c, old := moved(t, h, nowReq("a"))
			exec := newFakeExec(h.clk)
			exec.probe = busyProbe("t1")
			exec.interrupt = func(InterruptCall) ControlResult { return tc.res }
			l := h.lane(t, "ctl")
			if res := mustPass(t, l, view(), exec); res.Took != TakeNow || len(exec.delivered()) != 0 {
				t.Fatalf("unit pass %+v", res)
			}
			exec.probe = idleProbe()
			if res := mustPass(t, l, view(), exec); res.Took != TakeContent || !slices.Equal(res.Taken, []string{m.ID}) {
				t.Fatalf("post-unit pass %+v", res)
			}
			got := h.get(t, m.ID)
			wantOutcome(t, got, ArmSucceeded, "")
			if got.Outcome.Degradation != tc.degradation || len(exec.interrupts) != 1 {
				t.Fatalf("message degradation %q after %d interrupts, want %q after one", got.Outcome.Degradation, len(exec.interrupts), tc.degradation)
			}
			h.wantPointer(t, old, c.ID)
			noOldTake(t, exec, old)
		})
	}
}

// Home is this store's namespace, whatever the message's: an interrupt
// already pinned here is counted and untouched, and one pinned in any other
// namespace moves here, whichever namespace pinned it (RL4).
func TestRelowerNowUnitsHomeIsThisNamespace(t *testing.T) {
	for _, tc := range []struct {
		name         string
		msgNS, intNS string
		moves        bool
	}{
		{"message old, interrupt here", "old", "gc", false},
		{"message and interrupt old", "old", "old", true},
		{"message here, interrupt old (a stale prefix)", "gc", "old", true},
		{"message third, interrupt old", "third", "old", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			m, created, err := h.storeIn(t, tc.msgNS).Propose(nowReq("a"))
			if err != nil || !created {
				t.Fatalf("Propose: created %v, %v", created, err)
			}
			lowerer, err := NewLane(h.storeIn(t, tc.intNS), tc.intNS, h.clk)
			if err != nil {
				t.Fatal(err)
			}
			exec := newFakeExec(h.clk)
			exec.probe = notLiveProbe()
			mustPass(t, lowerer, view(), exec)
			iid, err := NowInterruptID(tc.intNS, m)
			if err != nil {
				t.Fatal(err)
			}
			rev := h.revisions(t, iid, m.ID)
			n := len(h.batcher.batches())
			rep := h.relower(t)
			home := nowID(t, m)
			if !tc.moves {
				if rep.Home != 1 || len(rep.Relowered) != 0 || len(h.batchesSince(n)) != 0 || !maps.Equal(h.revisions(t, iid, m.ID), rev) {
					t.Fatalf("report %+v, want the unit counted home and untouched", rep)
				}
				return
			}
			if !slices.Equal(rep.Relowered, []Relowered{{Message: m.ID, From: iid, To: home}}) {
				t.Fatalf("report %+v, want %s moved to %s", rep, iid, home)
			}
			h.wantPointer(t, iid, home)
		})
	}
}

// The step leaves every unit it must not move as it is, with no batch, and
// the lane then serves it as before: a message not yet lowered, a unit in
// flight (live or expired lease), a unit over, a lowered message with no
// interrupt, an orphan, two open derived interrupts, a home id held by
// another row, look-alike interrupts, and rows this build cannot read
// (RL1, RL8).
func TestRelowerNowUnitsLeavesWhatItMustNotMove(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup builds the world; ids are the rows whose revisions must not
		// move.
		setup func(t *testing.T, h *laneHarness) relowerWorld
		// left is the unit's reported reason; "" for none.
		left RelowerReason
		// then is the lane pass after the step.
		then func(t *testing.T, w relowerWorld, rep RelowerReport)
	}{
		{name: "not lowered", setup: func(t *testing.T, h *laneHarness) relowerWorld {
			m, _, err := h.storeIn(t, "old").Propose(nowReq("a"))
			if err != nil {
				t.Fatal(err)
			}
			return relowerWorld{h: h, m: m, ids: []string{m.ID}}
		}, then: func(t *testing.T, w relowerWorld, _ RelowerReport) {
			exec := newFakeExec(w.h.clk)
			exec.probe = notLiveProbe()
			res := mustPass(t, w.h.lane(t, "ctl"), view(), exec)
			if !slices.Equal(res.Lowered, []string{w.m.ID}) {
				t.Fatalf("pass %+v, want the message lowered here", res)
			}
			if i := w.h.get(t, nowID(t, w.m)); i.Status != StatusSubmitted {
				t.Fatalf("interrupt %s, want lowered at home", i.Status)
			}
		}},
		{name: "in flight, lease live", left: RelowerInFlight, setup: inFlightWorld(false), then: func(t *testing.T, w relowerWorld, _ RelowerReport) {
			if i := w.h.get(t, w.ids[1]); i.Status != StatusExecuting {
				t.Fatalf("interrupt %s, want still in flight", i.Status)
			}
		}},
		{name: "in flight, lease expired", left: RelowerInFlight, setup: inFlightWorld(true), then: func(t *testing.T, w relowerWorld, _ RelowerReport) {
			exec := newFakeExec(w.h.clk)
			l := w.h.lane(t, "ctl")
			mustPass(t, l, view(), exec)
			if got := w.h.get(t, w.ids[1]); got.Status != StatusClosed || got.Outcome.Arm != ArmFailed ||
				got.Outcome.Reason != ReasonAmbiguousDelivery && got.Outcome.Reason != ReasonDeadlineInFlight {
				t.Fatalf("interrupt %s %s{%s}, want closed failed: it may have run", got.Status, got.Outcome.Arm, got.Outcome.Reason)
			}
			for range 3 {
				mustPass(t, l, view(), exec)
			}
			got := w.h.get(t, w.m.ID)
			wantOutcome(t, got, ArmSucceeded, "")
			if got.Outcome.Degradation != DegradationInterruptFailed || len(exec.interrupts) != 0 {
				t.Fatalf("message %+v after %d interrupts, want post-unit interrupt_failed and none", got.Outcome, len(exec.interrupts))
			}
		}},
		{name: "over: the interrupt succeeded", setup: overWorld(Settled{Turn: "t1", After: TurnAfter{Boundary: TriNo, Live: TriYes, Turn: "t2", Known: []string{"t1", "t2"}}}), then: postUnitThen("")},
		{name: "over: the interrupt is unsupported", setup: overWorld(Rejected{Arm: ArmFailed, Reason: ReasonUnsupported}), then: postUnitThen(DegradationInterruptUnsupported)},
		{name: "over: the interrupt is stale", setup: overWorld(Rejected{Arm: ArmSkipped, Reason: ReasonStale}), then: postUnitThen("")},
		{name: "lowered with no interrupt row", setup: func(t *testing.T, h *laneHarness) relowerWorld {
			m, i := h.migratedUnit(t, nowReq("a"))
			if err := h.mem.Delete(i.ID); err != nil {
				t.Fatal(err)
			}
			return relowerWorld{h: h, m: m, ids: []string{m.ID}}
		}, then: postUnitThen(DegradationInterruptFailed)},
		{name: "an orphan", setup: func(t *testing.T, h *laneHarness) relowerWorld {
			m, i := h.migratedUnit(t, nowReq("a"))
			if r, _, err := h.store.Cancel(m.ID, "operator", "no"); err != nil || r != CancelClosed {
				t.Fatalf("Cancel = %s, %v", r, err)
			}
			return relowerWorld{h: h, m: m, ids: []string{m.ID, i.ID}}
		}, then: func(t *testing.T, w relowerWorld, _ RelowerReport) {
			mustPass(t, w.h.lane(t, "ctl"), view(), newFakeExec(w.h.clk))
			got := w.h.get(t, w.ids[1])
			wantOutcome(t, got, ArmCanceled, "")
			if got.Cancel.By != nowCascadeBy {
				t.Fatalf("orphan cancel %+v, want the cascade", got.Cancel)
			}
		}},
		{name: "the interrupt in flight, its message not", left: RelowerInFlight, setup: halfInFlightWorld(true)},
		{name: "the message in flight, its interrupt not", left: RelowerInFlight, setup: halfInFlightWorld(false)},
		{name: "the interrupt may have run", left: RelowerInFlight, setup: func(t *testing.T, h *laneHarness) relowerWorld {
			m, i := h.migratedUnit(t, nowReq("a"))
			h.forge(t, i.ID, func(op *Op) {
				op.Status, op.Attempt, op.MaybeDelivered = StatusAccepted, 1, 1
				op.Transitions = appendTransition(op.Transitions, Transition{Status: StatusAccepted, At: h.clk.Now(), By: "lane:old"})
			})
			if got := h.get(t, i.ID); got.Status != StatusAccepted || got.MaybeDelivered != 1 {
				t.Fatalf("forged interrupt %s maybe_delivered %d", got.Status, got.MaybeDelivered)
			}
			return relowerWorld{h: h, m: m, ids: []string{m.ID, i.ID}}
		}},
		{name: "two open derived interrupts", left: RelowerAmbiguous, setup: func(t *testing.T, h *laneHarness) relowerWorld {
			m, i := h.migratedUnit(t, nowReq("a"))
			twin := forgeDerived(t, h, "third", m)
			return relowerWorld{h: h, m: m, ids: []string{m.ID, i.ID, twin.ID}}
		}, then: func(t *testing.T, w relowerWorld, rep RelowerReport) {
			if got := rep.Left[0].Interrupts; len(got) != 2 || !slices.Contains(got, w.ids[1]) || !slices.Contains(got, w.ids[2]) {
				t.Fatalf("left interrupts %v, want both", got)
			}
		}},
		{name: "the home id is held", left: RelowerHomeTaken, setup: func(t *testing.T, h *laneHarness) relowerWorld {
			m, i := h.migratedUnit(t, nowReq("a"))
			twin := forgeDerived(t, h, "gc", m)
			if r, _, err := h.store.Cancel(twin.ID, "operator", "a forged twin"); err != nil || r != CancelClosed {
				t.Fatalf("Cancel = %s, %v", r, err)
			}
			return relowerWorld{h: h, m: m, ids: []string{m.ID, i.ID, twin.ID}}
		}},
		{name: "look-alikes", setup: func(t *testing.T, h *laneHarness) relowerWorld {
			m, i := h.migratedUnit(t, nowReq("a"))
			if err := h.mem.Delete(i.ID); err != nil {
				t.Fatal(err)
			}
			other := laneInterrupt(m.ID+nowInterruptKeySuffix, "")
			other.Causation = m.ID
			other.Producer.Principal = "someone-else"
			a, _, err := h.storeIn(t, "old").Propose(other)
			if err != nil {
				t.Fatal(err)
			}
			conv := laneInterrupt(m.ID+nowInterruptKeySuffix, "")
			conv.Causation, conv.Producer, conv.Conversation = m.ID, m.Producer, "gc-2"
			b, _, err := h.storeIn(t, "old").Propose(conv)
			if err != nil {
				t.Fatal(err)
			}
			plain := laneInterrupt("gc-mail-1"+nowInterruptKeySuffix, "")
			plain.Causation, plain.Producer = "gc-mail-1", m.Producer
			c, _, err := h.storeIn(t, "old").Propose(plain)
			if err != nil {
				t.Fatal(err)
			}
			return relowerWorld{h: h, m: m, ids: []string{m.ID, a.ID, b.ID, c.ID}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			w := tc.setup(t, h)
			rev := h.revisions(t, w.ids...)
			n := len(h.batcher.batches())
			rep := h.relower(t)
			if len(rep.Relowered) != 0 || len(h.batchesSince(n)) != 0 || !maps.Equal(h.revisions(t, w.ids...), rev) {
				t.Fatalf("report %+v after %d batches, want nothing moved or written", rep, len(h.batchesSince(n)))
			}
			if tc.left == "" {
				if len(rep.Left) != 0 {
					t.Fatalf("left %+v, want none", rep.Left)
				}
			} else if len(rep.Left) != 1 || rep.Left[0].Message != w.m.ID || rep.Left[0].Reason != tc.left {
				t.Fatalf("left %+v, want %s left %s", rep.Left, w.m.ID, tc.left)
			}
			if tc.then != nil {
				tc.then(t, w, rep)
			}
		})
	}
	t.Run("rows this build cannot read", func(t *testing.T) {
		h := newLaneHarness(t)
		m, _ := h.migratedUnit(t, nowReq("a"))
		op, err := h.store.Preview(laneMsg("future", RolePrompt, "from a newer build"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := EncodeOp(op)
		if err != nil {
			t.Fatal(err)
		}
		b.Metadata[keySchema] = "999"
		if _, err := h.mem.Create(b); err != nil {
			t.Fatal(err)
		}
		rev := h.revisions(t, op.ID)
		rep := h.relower(t)
		if len(rep.Quarantined) != 1 || rep.Quarantined[0].ID != op.ID || !maps.Equal(h.revisions(t, op.ID), rev) {
			t.Fatalf("quarantined %+v, want %s reported and untouched", rep.Quarantined, op.ID)
		}
		if len(rep.Relowered) != 1 || rep.Relowered[0].Message != m.ID {
			t.Fatalf("report %+v, want the readable unit still moved", rep)
		}
	})
}

// relowerWorld is a unit the step must leave alone: harness h, message m,
// and the rows ids whose revisions must not move.
type relowerWorld struct {
	h   *laneHarness
	m   Op
	ids []string
}

// forgeDerived proposes, in namespace prefix, a lane interrupt keyed as m's
// derived interrupt: what a forged twin, or a lowering on another prefix,
// leaves.
func forgeDerived(t *testing.T, h *laneHarness, prefix string, m Op) Op {
	t.Helper()
	req := laneInterrupt(m.ID+nowInterruptKeySuffix, "")
	req.Producer, req.Causation = m.Producer, m.ID
	i, created, err := h.storeIn(t, prefix).Propose(req)
	if err != nil || !created {
		t.Fatalf("Propose(twin): created %v, %v", created, err)
	}
	return i
}

// inFlightWorld is a migrated unit a lane of namespace old took before the
// stop and never settled; expired advances past its lease.
func inFlightWorld(expired bool) func(t *testing.T, h *laneHarness) relowerWorld {
	return func(t *testing.T, h *laneHarness) relowerWorld {
		t.Helper()
		m, i := h.migratedUnit(t, nowReq("a"))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		l := h.oldLane(t)
		l.hooks.at = crashAt(stageAfterTake)
		if _, err := l.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
			t.Fatalf("crashing pass = %v", err)
		}
		got := h.get(t, i.ID)
		if got.Status != StatusExecuting {
			t.Fatalf("interrupt %s, want in flight", got.Status)
		}
		if expired {
			h.clk.Advance(got.Lease.Deadline.Add(LeaseSkew).Sub(h.clk.Now()) + time.Second)
		}
		return relowerWorld{h: h, m: m, ids: []string{m.ID, i.ID}}
	}
}

// halfInFlightWorld is inFlightWorld (lease live) with one op put back as
// the lowering left it, so only the other is in flight: the message when
// interruptInFlight, else the interrupt. Each alone hides no unit that may
// have run from the step.
func halfInFlightWorld(interruptInFlight bool) func(t *testing.T, h *laneHarness) relowerWorld {
	return func(t *testing.T, h *laneHarness) relowerWorld {
		t.Helper()
		m, i := h.migratedUnit(t, nowReq("a"))
		back, flying := m.ID, i.ID
		if !interruptInFlight {
			back, flying = i.ID, m.ID
		}
		lowered := h.raw(t, back)
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		l := h.oldLane(t)
		l.hooks.at = crashAt(stageAfterTake)
		if _, err := l.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
			t.Fatalf("crashing pass = %v", err)
		}
		h.replaceRow(t, lowered)
		if got, want := h.get(t, back).Status, map[bool]Status{true: StatusAccepted, false: StatusSubmitted}[interruptInFlight]; got != want {
			t.Fatalf("op %s put back %s, want %s", back, got, want)
		}
		if got := h.get(t, flying); got.Status != StatusExecuting {
			t.Fatalf("op %s %s, want in flight", flying, got.Status)
		}
		return relowerWorld{h: h, m: m, ids: []string{m.ID, i.ID}}
	}
}

// replaceRow puts row b back as it is, under a fresh revision.
func (h *laneHarness) replaceRow(t *testing.T, b beads.Bead) {
	t.Helper()
	if err := h.mem.Delete(b.ID); err != nil {
		t.Fatalf("Delete(%s): %v", b.ID, err)
	}
	b.Revision = 0
	if _, err := h.mem.Create(b); err != nil {
		t.Fatalf("Create(%s): %v", b.ID, err)
	}
}

// forge rewrites op id as mod changes it: a state no lane writes, for the
// step's defensive checks.
func (h *laneHarness) forge(t *testing.T, id string, mod func(*Op)) {
	t.Helper()
	op := h.get(t, id)
	mod(&op)
	b, err := EncodeOp(op)
	if err != nil {
		t.Fatalf("EncodeOp(%s): %v", id, err)
	}
	h.replaceRow(t, b)
}

// overWorld is a migrated unit a lane of namespace old ran before the move,
// its interrupt ending res: the message is left open post-unit.
func overWorld(res ControlResult) func(t *testing.T, h *laneHarness) relowerWorld {
	return func(t *testing.T, h *laneHarness) relowerWorld {
		t.Helper()
		m, i := h.migratedUnit(t, nowReq("a", nowTarget(1, "t1")))
		exec := newFakeExec(h.clk)
		exec.probe = busyProbe("t1")
		exec.interrupt = func(InterruptCall) ControlResult { return res }
		if r := mustPass(t, h.oldLane(t), view(), exec); r.Took != TakeNow {
			t.Fatalf("unit pass %+v", r)
		}
		if got := h.get(t, i.ID); got.Status != StatusClosed {
			t.Fatalf("interrupt %s, want closed", got.Status)
		}
		if got := h.get(t, m.ID); got.Status != StatusAccepted {
			t.Fatalf("message %s, want open post-unit", got.Status)
		}
		return relowerWorld{h: h, m: m, ids: []string{m.ID, i.ID}}
	}
}

// postUnitThen is a boundary pass that delivers the message alone, marked
// degradation, with no interrupt call.
func postUnitThen(degradation string) func(t *testing.T, w relowerWorld, _ RelowerReport) {
	return func(t *testing.T, w relowerWorld, _ RelowerReport) {
		t.Helper()
		exec := newFakeExec(w.h.clk)
		if res := mustPass(t, w.h.lane(t, "ctl"), view(), exec); res.Took != TakeContent || !slices.Equal(res.Taken, []string{w.m.ID}) {
			t.Fatalf("boundary pass %+v, want the message alone", res)
		}
		got := w.h.get(t, w.m.ID)
		wantOutcome(t, got, ArmSucceeded, "")
		if got.Outcome.Degradation != degradation || len(exec.interrupts) != 0 {
			t.Fatalf("message %+v after %d interrupts, want marked %q and none", got.Outcome, len(exec.interrupts), degradation)
		}
	}
}

// A second run after a complete one counts every unit home and writes
// nothing (RL5).
func TestRelowerNowUnitsIsIdempotent(t *testing.T) {
	h := newLaneHarness(t)
	var ids []string
	for _, key := range []string{"a", "b"} {
		m, i := h.migratedUnit(t, nowReq(key))
		ids = append(ids, m.ID, i.ID, nowID(t, m))
	}
	if rep := h.relower(t); len(rep.Relowered) != 2 {
		t.Fatalf("first run %+v, want both units moved", rep)
	}
	rev := h.revisions(t, ids...)
	n := len(h.batcher.batches())
	rep := h.relower(t)
	if len(rep.Relowered) != 0 || rep.Home != 2 || len(rep.Left) != 0 || len(h.batchesSince(n)) != 0 || !maps.Equal(h.revisions(t, ids...), rev) {
		t.Fatalf("second run %+v after %d batches, want both home and nothing written", rep, len(h.batchesSince(n)))
	}
}

// Every move is all or nothing: a batch that fails without committing
// leaves the unit as it was and the step returns an error; one that
// committed before its error is read back as moved; a fault on one unit
// leaves the others moved; a re-run finishes (RL2, RL4).
func TestRelowerNowUnitsIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		commit bool
	}{{"refused before the commit", false}, {"lost after the commit", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			m, i := h.migratedUnit(t, nowReq("a"))
			home := nowID(t, m)
			h.batcher.setFault(func(int, beads.GuardedBatch) (bool, error) { return tc.commit, errLinkDropped })
			rep, err := h.store.RelowerNowUnits(context.Background(), relowerBy)
			h.batcher.setFault(nil)
			if tc.commit {
				if err != nil || !slices.Equal(rep.Relowered, []Relowered{{Message: m.ID, From: i.ID, To: home}}) {
					t.Fatalf("RelowerNowUnits = %+v, %v; want the committed move read back", rep, err)
				}
				h.wantPointer(t, i.ID, home)
				return
			}
			if !errors.Is(err, errLinkDropped) || errors.Is(err, ErrRelowerRaced) || len(rep.Relowered) != 0 {
				t.Fatalf("RelowerNowUnits = %+v, %v; want the batch's error", rep, err)
			}
			if got := h.get(t, i.ID); got.Status != StatusSubmitted {
				t.Fatalf("interrupt %s, want untouched", got.Status)
			}
			h.absent(t, home)
			if rep := h.relower(t); len(rep.Relowered) != 1 {
				t.Fatalf("re-run %+v, want the unit moved", rep)
			}
			h.wantPointer(t, i.ID, home)
		})
	}
	t.Run("one unit of three faults", func(t *testing.T) {
		h := newLaneHarness(t)
		var ms, is []Op
		for _, key := range []string{"a", "b", "c"} {
			m, i := h.migratedUnit(t, nowReq(key))
			ms, is = append(ms, m), append(is, i)
		}
		n := len(h.batcher.batches())
		h.batcher.setFault(func(k int, _ beads.GuardedBatch) (bool, error) {
			if k == n+1 {
				return false, errLinkDropped
			}
			return false, nil
		})
		rep, err := h.store.RelowerNowUnits(context.Background(), relowerBy)
		h.batcher.setFault(nil)
		if !errors.Is(err, errLinkDropped) || len(rep.Relowered) != 2 || slices.ContainsFunc(rep.Relowered, func(r Relowered) bool { return r.Message == ms[1].ID }) {
			t.Fatalf("RelowerNowUnits = %+v, %v; want the others moved and the error", rep, err)
		}
		if got := h.get(t, is[1].ID); got.Status != StatusSubmitted {
			t.Fatalf("faulted interrupt %s, want untouched", got.Status)
		}
		h.absent(t, nowID(t, ms[1]))
		rep = h.relower(t)
		if !slices.Equal(rep.Relowered, []Relowered{{Message: ms[1].ID, From: is[1].ID, To: nowID(t, ms[1])}}) || rep.Home != 2 {
			t.Fatalf("re-run %+v, want the faulted unit moved and the others home", rep)
		}
	})
}

// A unit that changes between the step's read and its batch is refused by
// the batch's guards: the step returns ErrRelowerRaced and writes nothing
// for it, so a copy never carries a stale revision (RL2).
func TestRelowerNowUnitsRacesAWriter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, h *laneHarness, m, i Op)
		// homeHeld says the racing write created the row at home.
		homeHeld bool
		// beforeRead races the write between the step's scan and its read
		// of the interrupt by id, rather than between that read and the
		// batch.
		beforeRead bool
	}{
		{name: "another host takes the unit before the read", beforeRead: true, write: func(t *testing.T, h *laneHarness, _, _ Op) {
			exec := newFakeExec(h.clk)
			exec.probe = busyProbe("t1")
			l := h.oldLane(t)
			l.hooks.at = crashAt(stageAfterTake)
			if _, err := l.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
				t.Fatalf("crashing pass = %v", err)
			}
		}},
		{name: "another host takes the unit", write: func(t *testing.T, h *laneHarness, _, _ Op) {
			exec := newFakeExec(h.clk)
			exec.probe = busyProbe("t1")
			l := h.oldLane(t)
			l.hooks.at = crashAt(stageAfterTake)
			if _, err := l.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
				t.Fatalf("crashing pass = %v", err)
			}
		}},
		{name: "a cancel of the interrupt", write: func(t *testing.T, h *laneHarness, _, i Op) {
			if _, _, err := h.store.Cancel(i.ID, "operator", "no"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a row at the home id", homeHeld: true, write: func(t *testing.T, h *laneHarness, m, _ Op) {
			forgeDerived(t, h, "gc", m)
		}},
		{name: "any change to the row", write: func(t *testing.T, h *laneHarness, _, i Op) {
			if err := h.mem.Update(i.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.op.zz_future": "moved"}}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			m, i := h.migratedUnit(t, nowReq("a"))
			home := nowID(t, m)
			ran := false
			race := func(Op) {
				if !ran {
					ran = true
					tc.write(t, h, m, i)
				}
			}
			if tc.beforeRead {
				h.store.beforeRelowerRead = race
			} else {
				h.store.beforeRelowerBatch = race
			}
			n := len(h.batcher.batches())
			rep, err := h.store.RelowerNowUnits(context.Background(), relowerBy)
			if !ran {
				t.Fatal("the racing write never ran")
			}
			if !errors.Is(err, ErrRelowerRaced) || len(rep.Relowered) != 0 {
				t.Fatalf("RelowerNowUnits = %+v, %v; want ErrRelowerRaced", rep, err)
			}
			if got := h.get(t, i.ID); isForwardPointer(got) {
				t.Fatalf("interrupt %+v, want no pointer written", got.Outcome)
			}
			if tc.beforeRead {
				if bs := h.batchesSince(n); len(bs) != 1 || bs[0].Reason == relowerUnitReason {
					t.Fatalf("batches %+v, want only the racing take", bs)
				}
				if got := h.get(t, i.ID); got.Status != StatusExecuting {
					t.Fatalf("interrupt %s, want the racing take's", got.Status)
				}
			}
			if tc.homeHeld {
				if got := h.get(t, home); got.Causation != m.ID || got.Status != StatusSubmitted {
					t.Fatalf("home row %+v, want the racing writer's", got)
				}
				return
			}
			h.absent(t, home)
		})
	}
}

// The step reads every conversation with an open lane op and moves every
// unit in each (RL1): more conversations than a paged scan (laneScanPages
// pages of one row per status) reaches, with two units in the first.
func TestRelowerNowUnitsScansEveryConversation(t *testing.T) {
	h := newLaneHarness(t)
	want := map[string]string{}
	for n := 1; n <= laneScanPages+2; n++ {
		conv := "gc-" + strconv.Itoa(n)
		keys := []string{"a"}
		if n == 1 {
			keys = append(keys, "b")
		}
		for _, key := range keys {
			m, i := h.migratedUnit(t, nowReq(key, inConv(conv)))
			want[i.ID] = nowID(t, m)
		}
	}
	rep := h.relower(t)
	got := map[string]string{}
	for _, r := range rep.Relowered {
		got[r.From] = r.To
	}
	if !maps.Equal(got, want) {
		t.Fatalf("moved %v, want every unit %v", got, want)
	}
	for from, to := range want {
		h.wantPointer(t, from, to)
	}
}

// capabilityFreeStore is a store with no write capability past beads.Store:
// no conditional writes, no guarded batches.
type capabilityFreeStore struct{ beads.Store }

// HasLoweredNowMessages is true exactly while the store holds a lowered
// deliver-now message (accepted, executing with a live or expired lease, or
// settling; when now or interrupt), from the lane scan's keyed reads, paged past other lane ops,
// on a store no lane store can open; a failed read is an error, never
// "none".
func TestHasLoweredNowMessages(t *testing.T) {
	h := newLaneHarness(t)
	probe := func(t *testing.T) bool {
		t.Helper()
		h.rec.Reset()
		got, err := HasLoweredNowMessages(beads.SessionStore{Store: capabilityFreeStore{h.rec}})
		if err != nil {
			t.Fatalf("HasLoweredNowMessages: %v", err)
		}
		for _, op := range h.rec.Ops() {
			if op.Kind != beadstest.OpList || op.Query.Limit != hasLoweredNowPage {
				t.Fatalf("probe op %+v, want paged lists", op)
			}
		}
		return got
	}
	if _, err := NewLaneStore(beads.SessionStore{Store: capabilityFreeStore{h.mem}}, h.clk); err == nil {
		t.Fatal("a lane store opened over a store with no guarded batches")
	}
	if probe(t) {
		t.Fatal("an empty store reports a lowered message")
	}
	h.propose(t, nowReq("unlowered", inConv("gc-1")))
	content := h.propose(t, laneMsg("content", RoleNotice, "fyi", inConv("gc-2")))
	exec := newFakeExec(h.clk)
	l := h.lane(t, "ctl")
	l.hooks.at = crashAt(stageAfterTake)
	if _, err := l.Pass(context.Background(), viewOf("gc-2"), exec); !errors.Is(err, errCrash) {
		t.Fatalf("crashing pass = %v", err)
	}
	if got := h.get(t, content.ID); got.Status != StatusExecuting {
		t.Fatalf("content %s, want in flight", got.Status)
	}
	if probe(t) {
		t.Fatal("an unlowered deliver-now message or plain content in flight reads as lowered")
	}
	t.Run("behind a page of other lane ops", func(t *testing.T) {
		h := newLaneHarness(t)
		for n := range hasLoweredNowPage + 1 {
			h.propose(t, laneMsg("c"+strconv.Itoa(n), RoleNotice, "fyi", inConv("gc-"+strconv.Itoa(n+10))))
		}
		for n := range hasLoweredNowPage + 1 {
			b, err := h.mem.List(beads.ListQuery{Label: LabelOp, Metadata: map[string]string{keyClientKey: "c" + strconv.Itoa(n)}})
			if err != nil || len(b) != 1 {
				t.Fatalf("List(c%d) = %d rows, %v", n, len(b), err)
			}
			h.forge(t, b[0].ID, func(op *Op) { op.Status = StatusAccepted })
		}
		h.clk.Advance(time.Second)
		h.migratedUnit(t, nowReq("a"))
		h.rec.Reset()
		got, err := HasLoweredNowMessages(beads.SessionStore{Store: capabilityFreeStore{h.rec}})
		if err != nil || !got {
			t.Fatalf("HasLoweredNowMessages = %v, %v; want the lowered message behind the page", got, err)
		}
		if n := len(h.rec.Ops()); n != 2 {
			t.Fatalf("%d reads, want two pages of accepted rows", n)
		}
	})
	for _, when := range []When{WhenNow, WhenInterrupt} {
		t.Run(string(when), func(t *testing.T) {
			h := newLaneHarness(t)
			h.migratedUnit(t, nowReq("a", withWhen(when)))
			h.rec.Reset()
			got, err := HasLoweredNowMessages(beads.SessionStore{Store: capabilityFreeStore{h.rec}})
			if err != nil || !got {
				t.Fatalf("HasLoweredNowMessages = %v, %v; want the lowered message seen", got, err)
			}
		})
	}
	for _, expired := range []bool{false, true} {
		t.Run("executing, lease expired "+strconv.FormatBool(expired), func(t *testing.T) {
			h := newLaneHarness(t)
			w := inFlightWorld(expired)(t, h)
			if got := h.get(t, w.m.ID); got.Status != StatusExecuting {
				t.Fatalf("message %s, want executing", got.Status)
			}
			got, err := HasLoweredNowMessages(beads.SessionStore{Store: capabilityFreeStore{h.mem}})
			if err != nil || !got {
				t.Fatalf("HasLoweredNowMessages = %v, %v; want the in-flight message seen", got, err)
			}
		})
	}
	t.Run("settling", func(t *testing.T) {
		h := newLaneHarness(t)
		m, _ := h.migratedUnit(t, nowReq("a"))
		if _, err := h.store.Transition(m.ID, StatusAccepted, StatusSettling, "test"); err != nil {
			t.Fatalf("settling the message: %v", err)
		}
		got, err := HasLoweredNowMessages(beads.SessionStore{Store: capabilityFreeStore{h.mem}})
		if err != nil || !got {
			t.Fatalf("HasLoweredNowMessages = %v, %v; want the settling message seen", got, err)
		}
	})
	if _, err := HasLoweredNowMessages(beads.SessionStore{Store: failingListStore{beads.NewMemStore()}}); err == nil {
		t.Fatal("a failed read reported no error")
	}
	var nilMem *beads.MemStore
	if _, err := HasLoweredNowMessages(beads.SessionStore{Store: nilMem}); err == nil {
		t.Fatal("a typed nil store reported no error")
	}
}
