package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Wait nudges on the conversation lane (design slice 8).

// readyWait creates a ready wait for f's session.
func readyWait(t *testing.T, f *laneFixture, note string) sessionpkg.WaitInfo {
	t.Helper()
	b, err := f.store.Create(beads.Bead{
		Type:        waitBeadType,
		Labels:      []string{waitBeadLabel, "session:" + f.id},
		Description: note,
		Metadata: map[string]string{
			"session_id":       f.id,
			"session_name":     f.name,
			"kind":             "deps",
			"state":            waitStateReady,
			"dep_ids":          "gc-dep-1",
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("creating the wait: %v", err)
	}
	return sessionpkg.WaitInfoFromBead(b)
}

// dispatchWaits runs the ready-wait dispatch over f's stores.
func dispatchWaits(t *testing.T, f *laneFixture) {
	t.Helper()
	sessFront := sessionFrontDoor(f.store)
	if err := dispatchReadyWaitNudgesWithSnapshot(f.dir, f.cfg, sessFront, beads.NudgesStore{Store: f.store}, time.Now().UTC(), nil); err != nil {
		t.Fatalf("dispatchReadyWaitNudgesWithSnapshot: %v", err)
	}
}

// waitNotice reads wait's attempt-1 notice op.
func waitNotice(t *testing.T, f *laneFixture, st *opjournal.Store, wait sessionpkg.WaitInfo) opjournal.Op {
	t.Helper()
	id, err := opjournal.DeriveID(st.Prefix(), f.id, waitNoticePrincipal, waitNoticeKey(waitNudgeID(wait)))
	if err != nil {
		t.Fatal(err)
	}
	return f.get(t, st, id)
}

// waitNow re-reads wait.
func waitNow(t *testing.T, f *laneFixture, wait sessionpkg.WaitInfo) sessionpkg.WaitInfo {
	t.Helper()
	b, err := f.store.Get(wait.ID)
	if err != nil {
		t.Fatal(err)
	}
	return sessionpkg.WaitInfoFromBead(b)
}

func TestWaitNudgeUnderLaneProposesANotice(t *testing.T) {
	f := newLaneFixture(t)
	f.cfg.Ops.Authoritative = []string{"message"}
	h := newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	wait := readyWait(t, f, "Continue after review closes.")
	dispatchWaits(t, f)
	op := waitNotice(t, f, st, wait)
	m := op.Policy.Message
	if op.Executor != opjournal.ExecutorLane || op.Producer.Principal != waitNoticePrincipal || op.Causation != wait.ID ||
		m.Role != opjournal.RoleNotice || m.When != opjournal.WhenBoundary || m.Wake != opjournal.WakeLiveOnly ||
		op.Scope != (opjournal.Scope{Kind: opjournal.ScopeContext, Epoch: 1}) {
		t.Fatalf("op = %+v, want gc:wait's live-only notice scoped to epoch 1", op)
	}
	text, err := opjournal.RenderDelivery([]opjournal.Op{op}, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := reminderGolden("wait", "Wait satisfied ("+wait.ID+"): Continue after review closes."); text != want {
		t.Fatalf("delivered text = %q, want %q", text, want)
	}
	if got := waitNow(t, f, wait).NudgeID; got != op.ID {
		t.Fatalf("wait nudge_id = %q, want the op id %s", got, op.ID)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("the dispatch delivered %d nudges itself", n)
	}
	_ = h
}

// The attempt's op answers whether it is queued or resolved: a second pass
// proposes nothing, open or closed.
func TestWaitNudgeQueuedOrResolvedReadsTheOp(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	wait := readyWait(t, f, "again")
	dispatchWaits(t, f)
	dispatchWaits(t, f)
	op := waitNotice(t, f, st, wait)
	if _, err := st.Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary}, "lane:ctl"); err != nil {
		t.Fatal(err)
	}
	dispatchWaits(t, f)
	open, err := st.ListOpen(f.id)
	if err != nil || len(open) != 0 {
		t.Fatalf("open ops = %+v, %v; want no second notice", open, err)
	}
}

func TestWaitFinalizesFromItsNoticeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome *opjournal.Outcome
		state   string
		done    bool
	}{
		{"succeeded", &opjournal.Outcome{Arm: opjournal.ArmSucceeded}, waitStateClosed, true},
		{"failed", &opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary}, waitStateFailed, true},
		{"skipped", &opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}, waitStateReady, false},
		{"canceled", nil, waitStateReady, false},
		{"open", nil, waitStateReady, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			newNoticeLaneHost(t, f, "message")
			st := f.laneStore(t)
			wait := readyWait(t, f, "finalize")
			dispatchWaits(t, f)
			op := waitNotice(t, f, st, wait)
			switch {
			case tc.outcome != nil:
				if _, err := st.Close(op.ID, *tc.outcome, "lane:ctl"); err != nil {
					t.Fatal(err)
				}
			case tc.name == "canceled":
				if _, _, err := st.Cancel(op.ID, "operator", ""); err != nil {
					t.Fatal(err)
				}
			}
			sessFront := sessionFrontDoor(f.store)
			done, err := finalizeReadyWaitFromNudge(sessFront, beads.NudgesStore{Store: f.store}, waitNow(t, f, wait), time.Now().UTC())
			if err != nil || done != tc.done {
				t.Fatalf("finalize = %v, %v; want %v", done, err, tc.done)
			}
			if got := waitNow(t, f, wait); got.State != tc.state {
				t.Fatalf("wait state = %s, want %s", got.State, tc.state)
			}
		})
	}
}

func TestWaitWithdrawCancelsItsNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	wait := readyWait(t, f, "withdraw")
	dispatchWaits(t, f)
	op := waitNotice(t, f, st, wait)
	ops, queued := splitWaitNudgeIDs([]string{op.ID, "wait-legacy-1-1"})
	if len(ops) != 1 || ops[0] != op.ID || len(queued) != 1 || queued[0] != "wait-legacy-1-1" {
		t.Fatalf("split = %v / %v", ops, queued)
	}
	if err := withdrawWaitNotices(beads.SessionStore{Store: f.store}, ops); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != waitNoticePrincipal {
		t.Fatalf("notice after withdraw = %+v", got)
	}
}

// With no lane host executing message for the sessions store, a ready wait
// keeps its queue item.
func TestWaitNudgeWithoutLaneKeepsTheQueue(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	wait := readyWait(t, f, "legacy")
	dispatchWaits(t, f)
	if got := waitNow(t, f, wait).NudgeID; got != waitNudgeID(wait) || opjournal.IsOpID(got) {
		t.Fatalf("wait nudge_id = %q, want the queue item's %q", got, waitNudgeID(wait))
	}
	if open, err := st.ListOpen(f.id); err != nil || len(open) != 0 {
		t.Fatalf("open ops = %+v, %v; want none", open, err)
	}
}

// Flipping the switch: a ready wait whose attempt is already a queue item
// (nudge_id the queue id, its shadow open) keeps that item once the
// controller's host executes message, and gets no lane notice for the same
// attempt, so the attempt is delivered once.
func TestWaitNudgeFlipKeepsTheQueuedAttempt(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	wait := readyWait(t, f, "flip")
	dispatchWaits(t, f) // no lane host: the attempt is a queue item
	queuedID := waitNow(t, f, wait).NudgeID
	if _, open, err := nudgeShadows(beads.NudgesStore{Store: f.store}).Find(queuedID); err != nil || !open {
		t.Fatalf("before the flip: queue item %s open %v, %v; want it open", queuedID, open, err)
	}
	f.cfg.Ops.Authoritative = []string{"message"}
	newNoticeLaneHost(t, f, "message")
	dispatchWaits(t, f)
	if got := waitNow(t, f, wait).NudgeID; got != queuedID {
		t.Fatalf("after the flip: nudge_id %q, want the queue item's %q", got, queuedID)
	}
	if open, err := st.ListOpen(f.id); err != nil || len(open) != 0 {
		t.Fatalf("after the flip: open ops = %+v, %v; want none for the queued attempt", open, err)
	}
}

// Rolling the switch back: a ready wait whose attempt is an open lane notice
// gets no queue item for the same attempt once no host executes message
// (the open notice still drains), and keeps the op as its nudge_id.
func TestWaitNudgeRollbackKeepsTheOpenNotice(t *testing.T) {
	f := newLaneFixture(t)
	f.cfg.Ops.Authoritative = []string{"message"}
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	wait := readyWait(t, f, "rollback")
	dispatchWaits(t, f)
	op := waitNotice(t, f, st, wait)
	f.cfg.Ops.Authoritative = nil
	newNoticeLaneHost(t, f)
	dispatchWaits(t, f)
	if got := waitNow(t, f, wait).NudgeID; got != op.ID {
		t.Fatalf("after the rollback: nudge_id %q, want the notice's %q", got, op.ID)
	}
	if _, open, err := nudgeShadows(beads.NudgesStore{Store: f.store}).Find(waitNudgeID(wait)); err != nil || open {
		t.Fatalf("after the rollback: queue item for the attempt open %v, %v; want none", open, err)
	}
}
