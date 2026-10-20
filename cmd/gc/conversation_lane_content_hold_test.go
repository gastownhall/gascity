package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

var contentHoldAt = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func laneHold(t *testing.T) session.ContentHold {
	t.Helper()
	h, err := session.NewContentHold("operator", contentHoldAt)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// holdFixture holds f's conversation through the session front door.
func holdFixture(t *testing.T, f *laneFixture) {
	t.Helper()
	if err := sessionFrontDoor(f.store).SetContentHold(f.id, laneHold(t)); err != nil {
		t.Fatalf("SetContentHold: %v", err)
	}
}

// releaseFixture clears the hold f's row holds, as a resume that read it.
func releaseFixture(t *testing.T, f *laneFixture) {
	t.Helper()
	_, info := f.view(t)
	if cleared, standing, err := sessionFrontDoor(f.store).ClearContentHold(f.id, info.ContentHoldRaw); err != nil || !cleared {
		t.Fatalf("ClearContentHold = %v, %q, %v", cleared, standing, err)
	}
}

func laneWaitOf(res opjournal.PassResult, id string) opjournal.WaitReason {
	for _, w := range res.Waits {
		if w.ID == id {
			return w.Reason
		}
	}
	return ""
}

// TestConversationLaneViewContentHold: any non-blank cell holds the view,
// readable or not; the other fields read as without one.
func TestConversationLaneViewContentHold(t *testing.T) {
	base := session.Info{ID: "gc-1", MetadataState: "active", Generation: "3", ContinuationEpoch: "2", InstanceToken: "tok"}
	want := opjournal.ConversationView{ID: "gc-1", Generation: 3, Epoch: 2, Token: "tok"}
	for raw, held := range map[string]bool{
		"operator@2026-10-20T12:00:00Z": true, "garbage": true, "": false, "  ": false,
	} {
		info := base
		info.ContentHoldRaw = raw
		w := want
		w.ContentHold = held
		if got := conversationLaneView(info); got != w {
			t.Errorf("cell %q: view %+v, want %+v", raw, got, w)
		}
	}
}

// TestLaneHoldsContentUntilItsHoldClears runs the real read path: a held
// row's notice waits content_hold and types nothing until the hold clears.
func TestLaneHoldsContentUntilItsHoldClears(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	l := f.lane(t, st)
	holdFixture(t, f)
	op := f.propose(t, st, f.msg("k1", opjournal.RoleNotice, "fyi"))
	res, err := f.pass(context.Background(), t, l, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Took != opjournal.TakeNone || laneWaitOf(res, op.ID) != opjournal.WaitContentHold || len(f.nudges()) != 0 {
		t.Fatalf("held pass = %+v, %d nudges; want the notice waiting content_hold", res, len(f.nudges()))
	}
	releaseFixture(t, f)
	if _, err := f.pass(context.Background(), t, l, st, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
		t.Fatalf("after the clear op = %+v, %d nudges; want one delivery", got.Outcome, len(f.nudges()))
	}
}

// TestLaneHostHoldsContentUntilItsHoldClears: the host reads the row live,
// so the poke after a clear delivers.
func TestLaneHostHoldsContentUntilItsHoldClears(t *testing.T) {
	f := newLaneFixture(t)
	holdFixture(t, f)
	hh := newLaneHostHarness(t, f)
	hh.start()
	hh.h.wg.Wait()
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	hh.h.Poke(f.id)
	if p := hh.nextPass(t); p.out.err != nil || p.out.res.Took != opjournal.TakeNone || laneWaitOf(p.out.res, op.ID) != opjournal.WaitContentHold {
		t.Fatalf("held pass = %+v, want the prompt waiting content_hold", p.out)
	}
	hh.nextExit(t)
	releaseFixture(t, f)
	hh.h.Poke(f.id)
	if p := hh.nextPass(t); p.out.err != nil || p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass after the clear = %+v, want the delivery", p.out)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
		t.Fatalf("op = %+v, %d nudges; want one succeeded delivery", got.Outcome, len(f.nudges()))
	}
}

// TestLaneStopWithHoldRunsTheInterruptAndHoldsTheQueue composes a Stop as a
// producer does: the interrupt op and its hold in one batch, the interrupt
// run first, the queue held until the hold clears.
func TestLaneStopWithHoldRunsTheInterruptAndHoldsTheQueue(t *testing.T) {
	c := newInterruptCase(t, laneAtomicTurnCaps)
	c.read = func(context.Context) (runtime.TurnHistory, error) {
		if len(c.requests()) == 0 { // t7 runs until the Stop interrupts it
			return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t7"}}, nil
		}
		return runtime.TurnHistory{}, nil
	}
	queued := c.f.propose(t, c.st, c.f.msg("queued", opjournal.RolePrompt, "next"))
	front := sessionFrontDoor(c.f.store)
	hold := laneHold(t)
	item, err := front.ContentHoldItem(c.f.id, hold)
	if err != nil {
		t.Fatal(err)
	}
	soloWrites := 0
	stop := opjournal.Request{
		Conversation: c.f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "stop",
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
	}
	op, created, err := c.st.ProposeWith(context.Background(), stop, opjournal.Companion{
		Tag: session.ContentHoldKey, Item: item,
		Write: func() error { soloWrites++; return front.SetContentHold(c.f.id, hold) },
	})
	if err != nil || !created {
		t.Fatalf("ProposeWith = created %v, %v", created, err)
	}
	if soloWrites != 0 {
		t.Fatalf("the hold was written on its own %d times; want it in the interrupt's batch", soloWrites)
	}
	if _, info := c.f.view(t); info.ContentHoldRaw != hold.String() {
		t.Fatalf("hold = %q, want %q beside the interrupt", info.ContentHoldRaw, hold.String())
	}
	if res := c.pass(t); res.Took != opjournal.TakeControl || len(c.requests()) != 1 {
		t.Fatalf("pass 1 = %+v, %d interrupts; want the interrupt run first", res, len(c.requests()))
	}
	if got := c.f.get(t, c.st, op.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("interrupt = %s %+v, want closed succeeded", got.Status, got.Outcome)
	}
	if res := c.pass(t); res.Took != opjournal.TakeNone || laneWaitOf(res, queued.ID) != opjournal.WaitContentHold || len(c.f.nudges()) != 0 {
		t.Fatalf("pass 2 = %+v, %d nudges; want the prompt held", res, len(c.f.nudges()))
	}
	releaseFixture(t, c.f)
	c.pass(t)
	if got := c.f.get(t, c.st, queued.ID); got.Outcome.Arm != opjournal.ArmSucceeded || len(c.f.nudges()) != 1 {
		t.Fatalf("after the clear the prompt = %+v, %d nudges; want delivered", got.Outcome, len(c.f.nudges()))
	}
}

// TestDrainReminderNoticeHeldByTheContentHold: the drain-protocol exemption
// admits the drain reminder's notice past draining, not past the hold.
func TestDrainReminderNoticeHeldByTheContentHold(t *testing.T) {
	f := newLaneFixture(t, func(meta map[string]string) {
		meta["state"] = string(session.StateDraining)
		meta["state_reason"] = session.DrainAckStopPendingReason
	})
	holdFixture(t, f)
	st := f.laneStore(t)
	l := f.lane(t, st)
	notice := f.propose(t, st, f.msg("drain", opjournal.RoleNotice, "ack your drain", func(r *opjournal.Request) {
		r.Producer = opjournal.Producer{Principal: laneDrainNoticeClass.Principal, Actor: opjournal.ActorController}
		r.Policy.Message = opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly}
	}))
	res, err := f.pass(context.Background(), t, l, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if laneWaitOf(res, notice.ID) != opjournal.WaitContentHold || len(f.nudges()) != 0 {
		t.Fatalf("held drain notice: pass %+v, %d nudges; want content_hold", res, len(f.nudges()))
	}
	releaseFixture(t, f)
	if _, err := f.pass(context.Background(), t, l, st, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, st, notice.ID); got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
		t.Fatalf("after the clear the notice = %+v, %d nudges; want delivered past draining", got.Outcome, len(f.nudges()))
	}
}
