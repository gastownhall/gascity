package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

// laneStopRequest is an operator Stop's interrupt on c's conversation.
func laneStopRequest(c *interruptCase, key string) opjournal.Request {
	return opjournal.Request{
		Conversation: c.f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: key,
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
	}
}

// heldStopCompanion is a Stop's hold as a producer writes it beside the
// interrupt, counting the solo writes.
func heldStopCompanion(c *interruptCase, hold session.ContentHold, soloWrites *int) opjournal.Companion {
	front := sessionFrontDoor(c.f.store)
	return opjournal.Companion{
		Tag:   session.ContentHoldKey,
		Item:  func() (beads.GuardedItem, error) { return front.ContentHoldItem(c.f.id, hold) },
		Write: func() error { *soloWrites++; return front.SetContentHold(c.f.id, hold) },
	}
}

// TestLaneHeldStopReplaysAfterItsRowCloses: a held Stop retried after its
// row closed replays the stored interrupt, while a first held Stop on the
// closed row is refused and writes nothing.
func TestLaneHeldStopReplaysAfterItsRowCloses(t *testing.T) {
	c := newInterruptCase(t, laneAtomicTurnCaps)
	hold := laneHold(t)
	soloWrites := 0
	first, created, err := c.st.ProposeWith(context.Background(), laneStopRequest(c, "stop"), heldStopCompanion(c, hold, &soloWrites))
	if err != nil || !created {
		t.Fatalf("ProposeWith = created %v, %v", created, err)
	}
	if err := c.f.store.Close(c.f.id); err != nil {
		t.Fatal(err)
	}
	again, created, err := c.st.ProposeWith(context.Background(), laneStopRequest(c, "stop"), heldStopCompanion(c, hold, &soloWrites))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry after the close = %s, created %v, %v; want a replay of %s", again.ID, created, err, first.ID)
	}
	fresh := laneStopRequest(c, "stop-2")
	preview, err := c.st.Preview(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.st.ProposeWith(context.Background(), fresh, heldStopCompanion(c, hold, &soloWrites)); !errors.Is(err, session.ErrSessionClosed) {
		t.Fatalf("first held Stop on a closed row = %v, want ErrSessionClosed", err)
	}
	if _, err := c.f.store.Get(preview.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("the refused Stop's op = %v, want none written", err)
	}
	if soloWrites != 0 {
		t.Fatalf("the hold was written on its own %d times", soloWrites)
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
	hold := laneHold(t)
	soloWrites := 0
	op, created, err := c.st.ProposeWith(context.Background(), laneStopRequest(c, "stop"), heldStopCompanion(c, hold, &soloWrites))
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

// TestExecutionBackstopHoldsAHeldSeat drives the execution backstop through
// a whole budget's ticks on a held seat, quiet and holding a claim it never
// started: it sends nothing, spends no attempt, does not drain the seat and
// names the hold. Once the hold clears, its nudges and its drain resume from
// the pacing it held.
func TestExecutionBackstopHoldsAHeldSeat(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	hold := laneHold(t)
	if err := sessionFrontDoor(f.store).SetContentHold(f.session.ID, hold); err != nil {
		t.Fatalf("SetContentHold: %v", err)
	}
	f.tickStale(t)
	if n, drains, stalled := f.nudgeCount(), len(f.drained), f.stalledEvents(); n != 0 || drains != 0 || stalled != 0 {
		t.Fatalf("held seat: %d nudges, %d drains, %d stalled events; want none; stdout=%s", n, drains, stalled, f.stdout.String())
	}
	if got := f.sessionMeta(t, executionClaimNudgeCountKey); got != "0" {
		t.Fatalf("attempts spent on a held seat = %q, want 0", got)
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, backstopHoldContentHeld+" ") {
		t.Fatalf("hold breadcrumb = %q, want %s", got, backstopHoldContentHeld)
	}
	if cleared, standing, err := sessionFrontDoor(f.store).ClearContentHold(f.session.ID, hold.String()); err != nil || !cleared {
		t.Fatalf("ClearContentHold = %v, %q, %v", cleared, standing, err)
	}
	f.tickStale(t)
	if n, drains := f.nudgeCount(), len(f.drained); n != idleClaimNudgeMaxAttempts || drains != 1 {
		t.Fatalf("after the clear: %d nudges, %d drains; want %d nudges, then one drain; stdout=%s", n, drains, idleClaimNudgeMaxAttempts, f.stdout.String())
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); got != "" {
		t.Fatalf("hold breadcrumb after the clear = %q, want none", got)
	}
}

// TestPoolClaimBackstopDoesNotDrainAHeldSlot: a slot at its nudge cap,
// quiet, whose trigger is still open and unclaimed, is not drained while its
// conversation is held, and is once the hold is gone.
func TestPoolClaimBackstopDoesNotDrainAHeldSlot(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(idleClaimNudgeBackoff + time.Second)
	sp := quietClaimFake(t, now)
	s := atCapSession(base, "tok-1")
	s.Metadata[session.ContentHoldKey] = laneHold(t).String()
	store := beads.NewMemStoreFrom(0, []beads.Bead{s, {ID: "work-a", Status: "open"}}, nil)
	open := []beads.Bead{{ID: "work-a", Status: "open"}}
	drains := &drainRecorder{}
	var out bytes.Buffer
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{s}, open, nil, now, liveClaimDrain(store, drains.request), &out)
	if got := mustGetTestBead(t, store, s.ID); len(drains.ids) != 0 || sp.CountCalls("Nudge", "session-a") != 0 || got.Metadata[idleClaimNudgeStalledKey] != "" {
		t.Fatalf("held slot: drains %v, %d nudges, latch %q; want none", drains.ids, sp.CountCalls("Nudge", "session-a"), got.Metadata[idleClaimNudgeStalledKey])
	}
	if err := store.SetMetadata(s.ID, session.ContentHoldKey, ""); err != nil {
		t.Fatal(err)
	}
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{mustGetTestBead(t, store, s.ID)}, open, nil, now, liveClaimDrain(store, drains.request), &out)
	if len(drains.ids) != 1 || drains.ids[0] != s.ID {
		t.Fatalf("drains after the clear = %v, want the slot drained once", drains.ids)
	}
}

// TestRunNudgeBackstopProposesNothingToAHeldConversation: under the lane, a
// held conversation gets no notice op and spends no attempt across a whole
// budget's backoffs, and a backstop at its cap takes no terminal action;
// once the hold clears, both resume.
func TestRunNudgeBackstopProposesNothingToAHeldConversation(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	holdFixture(t, f)
	exhausted := 0
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1", exhaustedCalls: &exhausted}
	var out bytes.Buffer
	rows := backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour))
	for range idleClaimNudgeMaxAttempts + 1 {
		runNoticeBackstop(f, rows, pred, &out)
		f.clk.Advance(idleClaimNudgeBackoff)
		rows = []beads.Bead{f.row(t)}
	}
	if n, spent := len(f.openOps(t)), f.meta(t, noticeTestAttemptsKey); n != 0 || spent != "0" {
		t.Fatalf("held conversation: %d open ops, attempts %q; want no notice and none spent; journal %q", n, spent, out.String())
	}
	runNoticeBackstop(f, backstopRows(t, f, idleClaimNudgeMaxAttempts, f.clk.Now().Add(-time.Hour)), pred, &out)
	if exhausted != 0 {
		t.Fatalf("terminal actions on a held conversation = %d, want none", exhausted)
	}
	releaseFixture(t, f)
	runNoticeBackstop(f, []beads.Bead{f.row(t)}, pred, &out)
	if exhausted != 1 {
		t.Fatalf("terminal actions after the clear = %d, want 1", exhausted)
	}
	last := f.clk.Now().Add(-time.Hour)
	runNoticeBackstop(f, backstopRows(t, f, 0, last), pred, &out)
	f.get(t, f.laneStore(t), noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(last), 1))
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("the backstop typed %d nudges itself, want 0", n)
	}
}

// TestDrainReminderHoldsForAHeldConversation: a due drain reminder for a
// held row is neither proposed nor nudged, and spends no attempt across a
// whole budget's cadence, with the lane or without it. Once the hold clears
// it goes out by its route.
func TestDrainReminderHoldsForAHeldConversation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds []string
		after drainReminderOutcome
	}{{"lane", []string{"message"}, drainReminderProposed}, {"direct", nil, drainReminderDelivered}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			newNoticeLaneHost(t, f, tc.kinds...)
			_, info := wedgeDrainReminderRow(t, f)
			holdFixture(t, f)
			var out bytes.Buffer
			for range drainReminderMaxAttempts + 1 {
				if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderHeld {
					t.Fatalf("held row: outcome %v, want held; journal %q", got, out.String())
				}
				f.clk.Advance(drainReminderInterval)
			}
			if n, ops, spent := len(f.nudges()), len(f.openOps(t)), f.meta(t, drainReminderCountKey); n != 0 || ops != 0 || spent != "" {
				t.Fatalf("held row: %d nudges, %d open ops, %s %q; want nothing sent or spent", n, ops, drainReminderCountKey, spent)
			}
			if got := f.meta(t, drainReminderHoldKey); got != drainReminderHoldContentHeld {
				t.Fatalf("%s = %q, want %s", drainReminderHoldKey, got, drainReminderHoldContentHeld)
			}
			releaseFixture(t, f)
			if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != tc.after {
				t.Fatalf("after the clear: outcome %v, want %v; journal %q", got, tc.after, out.String())
			}
		})
	}
}

// TestDrainReminderAnnouncesABudgetSpentBeforeTheHold: the hold stops new
// attempts, not the record of spent ones, so an exhausted budget is still
// announced on a held row.
func TestDrainReminderAnnouncesABudgetSpentBeforeTheHold(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f)
	_, info := wedgeDrainReminderRow(t, f)
	row := f.row(t)
	if !writeDrainReminderMarker(f.store, info, row, drainReminderIdentity(row), drainReminderMaxAttempts, 0, drainReminderMaxAttempts,
		f.clk.Now().Add(-drainReminderInterval), io.Discard) {
		t.Fatal("seeding the spent budget failed")
	}
	holdFixture(t, f)
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderExhausted {
		t.Fatalf("outcome = %v, want exhausted; journal %q", got, out.String())
	}
	if got := f.meta(t, drainReminderHoldKey); got != drainReminderHoldExhausted {
		t.Fatalf("%s = %q, want %s", drainReminderHoldKey, got, drainReminderHoldExhausted)
	}
}
