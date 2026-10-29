package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/logutil"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
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

// waitNotice reads the notice op of wait's delivery attempt.
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

// waitLastError reads wait's last_error.
func waitLastError(t *testing.T, f *laneFixture, wait sessionpkg.WaitInfo) string {
	t.Helper()
	b, err := f.store.Get(wait.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata["last_error"]
}

// prepareWaits runs the ready-wait prepare pass over f's stores and returns
// its ready-wait set.
func prepareWaits(t *testing.T, f *laneFixture) map[string]bool {
	t.Helper()
	ready, err := prepareWaitWakeStateWithSnapshot(sessionFrontDoor(f.store), waitDependencyReaderOver(f.store, nil), beads.NudgesStore{Store: f.store}, time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("prepareWaitWakeStateWithSnapshot: %v", err)
	}
	return ready
}

// A closed notice is its attempt's last word, since its key is never
// proposed again: whatever its outcome, it ends the wait, and only an open
// notice keeps it ready.
func TestWaitFinalizesFromItsNoticeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		outcome   *opjournal.Outcome
		state     string
		lastError string
		done      bool
	}{
		{"succeeded", &opjournal.Outcome{Arm: opjournal.ArmSucceeded}, waitStateClosed, "", true},
		{"failed", &opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary}, waitStateFailed, opjournal.ReasonNoBoundary, true},
		{"skipped stale", &opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}, waitStateCanceled, "notice skipped: stale", true},
		{"skipped conversation closed", &opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonConversationClosed}, waitStateCanceled, "notice skipped: conversation_closed", true},
		{"canceled", nil, waitStateCanceled, "notice canceled by operator: not now", true},
		{"open", nil, waitStateReady, "", false},
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
				if _, _, err := st.Cancel(op.ID, "operator", "not now"); err != nil {
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
			if got := waitLastError(t, f, wait); got != tc.lastError {
				t.Fatalf("wait last_error = %q, want %q", got, tc.lastError)
			}
		})
	}
}

// A notice the lane closes undelivered (its session archived, which the
// lane reads as a closed conversation, or an operator's cancel through the
// API) ends its wait on the next prepare pass: the session leaves the
// ready-wait set, sheds the wait's hold, and gets no second notice.
func TestWaitEndsOnceTheLaneClosesItsNoticeUndelivered(t *testing.T) {
	for _, tc := range []struct {
		name      string
		close     func(t *testing.T, f *laneFixture, st *opjournal.Store, op opjournal.Op)
		lastError string
	}{
		{"archived session", func(t *testing.T, f *laneFixture, st *opjournal.Store, _ opjournal.Op) {
			if err := f.store.SetMetadata(f.id, "state", string(sessionpkg.StateArchived)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pass(context.Background(), t, f.lane(t, st), st, nil); err != nil {
				t.Fatalf("lane pass: %v", err)
			}
		}, "notice skipped: conversation_closed"},
		{"operator cancel", func(t *testing.T, _ *laneFixture, st *opjournal.Store, op opjournal.Op) {
			if _, _, err := st.Cancel(op.ID, "operator", "not now"); err != nil {
				t.Fatal(err)
			}
		}, "notice canceled by operator: not now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			newNoticeLaneHost(t, f, "message")
			st := f.laneStore(t)
			wait := readyWait(t, f, "end me")
			if err := setSessionWaitHold(sessionFrontDoor(f.store), f.id); err != nil {
				t.Fatalf("setting the wait hold: %v", err)
			}
			dispatchWaits(t, f)
			op := waitNotice(t, f, st, wait)
			tc.close(t, f, st, op)
			if got := f.get(t, st, op.ID); got.Status != opjournal.StatusClosed {
				t.Fatalf("notice = %+v, want it closed", got)
			}
			if ready := prepareWaits(t, f); ready[f.id] {
				t.Fatalf("ready-wait set = %v, want the session out of it", ready)
			}
			if got := waitNow(t, f, wait); got.State != waitStateCanceled {
				t.Fatalf("wait state = %s, want %s", got.State, waitStateCanceled)
			}
			if got := waitLastError(t, f, wait); got != tc.lastError {
				t.Fatalf("wait last_error = %q, want %q", got, tc.lastError)
			}
			if d := sessionpkg.DispositionOfInfo(f.rowInfo(t)); d.Wait || d.WaitIntent {
				t.Fatalf("disposition = %+v, want the wait hold gone", d)
			}
			dispatchWaits(t, f)
			if open := f.openOps(t); len(open) != 0 {
				t.Fatalf("open ops = %+v, want no second notice", open)
			}
		})
	}
}

// A note the op store refuses (past the inline payload limit, or not UTF-8)
// takes the legacy queue, which carries it, and does not stop the pass: the
// other ready wait still gets its notice.
func TestWaitNudgeRefusedNoticeTakesTheQueue(t *testing.T) {
	for _, tc := range []struct {
		name string
		note string
	}{
		{"oversized", strings.Repeat("x", opjournal.MaxInlinePayload+1)},
		{"not UTF-8", "bad \xff note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			newNoticeLaneHost(t, f, "message")
			st := f.laneStore(t)
			ordinary := readyWait(t, f, "ordinary")
			refused := readyWait(t, f, tc.note)
			dispatchWaits(t, f)
			if got := waitNow(t, f, refused).NudgeID; got != waitNudgeID(refused) {
				t.Fatalf("refused wait nudge_id = %q, want the queue item's %q", got, waitNudgeID(refused))
			}
			if _, open, err := nudgeShadows(beads.NudgesStore{Store: f.store}).Find(waitNudgeID(refused)); err != nil || !open {
				t.Fatalf("refused wait's queue item open %v, %v; want it open", open, err)
			}
			id, err := opjournal.DeriveID(st.Prefix(), f.id, waitNoticePrincipal, waitNoticeKey(waitNudgeID(refused)))
			if err != nil {
				t.Fatal(err)
			}
			noticeOpAbsent(t, st, id)
			op := waitNotice(t, f, st, ordinary)
			if got := waitNow(t, f, ordinary).NudgeID; got != op.ID {
				t.Fatalf("ordinary wait nudge_id = %q, want its notice %s", got, op.ID)
			}
		})
	}
}

// waitNudgeIDRefusingStore is a sessions store that refuses to record a
// nudge_id on the wait refuse names.
type waitNudgeIDRefusingStore struct {
	laneTestStore
	refuse string
}

func (s *waitNudgeIDRefusingStore) SetMetadata(id, key, value string) error {
	if id == s.refuse && key == "nudge_id" {
		return errors.New("nudge_id write refused")
	}
	return s.laneTestStore.SetMetadata(id, key, value)
}

// A wait records its notice's op id before it proposes the notice, so an op
// never exists that its wait does not name: when the record fails, nothing
// is proposed, the pass reports it and still dispatches the other wait, and
// a rollback then queues the attempt exactly once. A crash between the two
// writes leaves a nudge_id naming no op, which reads as not found.
func TestWaitNudgeNoticeIsRecordedBeforeItIsProposed(t *testing.T) {
	store := &waitNudgeIDRefusingStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	f.cfg.Ops.Authoritative = []string{"message"}
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	ordinary := readyWait(t, f, "ordinary")
	unrecorded := readyWait(t, f, "unrecorded")
	store.refuse = unrecorded.ID
	err := dispatchReadyWaitNudgesWithSnapshot(f.dir, f.cfg, sessionFrontDoor(f.store), beads.NudgesStore{Store: f.store}, time.Now().UTC(), nil)
	if err == nil || !strings.Contains(err.Error(), unrecorded.ID) {
		t.Fatalf("dispatch = %v, want the unrecorded wait's error", err)
	}
	id, derr := opjournal.DeriveID(st.Prefix(), f.id, waitNoticePrincipal, waitNoticeKey(waitNudgeID(unrecorded)))
	if derr != nil {
		t.Fatal(derr)
	}
	noticeOpAbsent(t, st, id)
	op := waitNotice(t, f, st, ordinary)
	if got := waitNow(t, f, ordinary).NudgeID; got != op.ID {
		t.Fatalf("ordinary wait nudge_id = %q, want its notice %s", got, op.ID)
	}

	store.refuse = ""
	f.cfg.Ops.Authoritative = nil
	newNoticeLaneHost(t, f)
	dispatchWaits(t, f)
	noticeOpAbsent(t, st, id)
	if got := waitNow(t, f, unrecorded).NudgeID; got != waitNudgeID(unrecorded) {
		t.Fatalf("after the rollback: nudge_id %q, want the queue item's %q", got, waitNudgeID(unrecorded))
	}
	if _, open, err := nudgeShadows(beads.NudgesStore{Store: f.store}).Find(waitNudgeID(unrecorded)); err != nil || !open {
		t.Fatalf("after the rollback: queue item open %v, %v; want it open", open, err)
	}
	if _, open, err := nudgeShadows(beads.NudgesStore{Store: f.store}).Find(waitNudgeID(ordinary)); err != nil || open {
		t.Fatalf("after the rollback: the noticed attempt's queue item open %v, %v; want none", open, err)
	}

	// The crash window: the record landed and the proposal did not. The
	// recorded id names no op, so a rollback queues the attempt.
	crashed := readyWait(t, f, "crashed")
	cid, derr := opjournal.DeriveID(st.Prefix(), f.id, waitNoticePrincipal, waitNoticeKey(waitNudgeID(crashed)))
	if derr != nil {
		t.Fatal(derr)
	}
	if err := sessionFrontDoor(f.store).SetWaitNudgeID(crashed.ID, cid); err != nil {
		t.Fatal(err)
	}
	dispatchWaits(t, f)
	noticeOpAbsent(t, st, cid)
	if _, open, err := nudgeShadows(beads.NudgesStore{Store: f.store}).Find(waitNudgeID(crashed)); err != nil || !open {
		t.Fatalf("crash window: queue item open %v, %v; want it open", open, err)
	}
}

// A protocol-mode runtime (t3bridge) that no other host serves is one no
// lane can type into, so its ready wait keeps the queue item even while the
// controller's host executes message.
func TestWaitNudgeProtocolModeSessionKeepsTheQueue(t *testing.T) {
	f := newLaneFixture(t)
	f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	f.cfg.Ops.Authoritative = []string{"message"}
	newNoticeLaneHost(t, f, "message")
	wait := readyWait(t, f, "protocol")
	dispatchWaits(t, f)
	if got := waitNow(t, f, wait).NudgeID; got != waitNudgeID(wait) {
		t.Fatalf("wait nudge_id = %q, want the queue item's %q", got, waitNudgeID(wait))
	}
	if _, open, err := nudgeShadows(beads.NudgesStore{Store: f.store}).Find(waitNudgeID(wait)); err != nil || !open {
		t.Fatalf("queue item open %v, %v; want it open", open, err)
	}
	if open := f.openOps(t); len(open) != 0 {
		t.Fatalf("open ops = %+v, want none", open)
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

// The CLI's withdrawal cancels a wait's notice in the sessions class, which
// it reaches through the city store it opened, never through the nudges
// class: here the nudges class is relocated onto a store that holds no op.
func TestWithdrawQueuedWaitNudgesCancelsTheNoticeInTheSessionsClass(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	wait := readyWait(t, f, "withdraw from the CLI")
	dispatchWaits(t, f)
	op := waitNotice(t, f, st, wait)
	nudgesClass := newLaneTestStore()
	prev := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
		return beads.NudgesStore{Store: nudgesClass}, f.store
	}
	t.Cleanup(func() { openOwnedNudgeBeadStore = prev })
	if err := withdrawQueuedWaitNudges(f.dir, []string{op.ID, "wait-legacy-1-1"}); err != nil {
		t.Fatalf("withdrawQueuedWaitNudges: %v", err)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != waitNoticePrincipal {
		t.Fatalf("notice after the CLI withdraw = %+v", got)
	}
}

// With no city store opened, the CLI's withdrawal of a notice says so.
func TestWithdrawQueuedWaitNudgesReportsAnUnopenedCityStore(t *testing.T) {
	prev := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) { return beads.NudgesStore{}, nil }
	t.Cleanup(func() { openOwnedNudgeBeadStore = prev })
	err := withdrawQueuedWaitNudges(t.TempDir(), []string{"gc-op-" + strings.Repeat("a", 26)})
	if err == nil || !strings.Contains(err.Error(), "city store did not open") {
		t.Fatalf("withdrawQueuedWaitNudges = %v, want the unopened city store reported", err)
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

// neverProposedNoticeWait is a ready wait left as a crash between recording
// its notice and proposing it, then a rollback of message, leave it:
// nudge_id names the notice, which no op has, and nothing is queued for the
// attempt. It returns the wait and its notice's op id.
func neverProposedNoticeWait(t *testing.T, f *laneFixture) (sessionpkg.WaitInfo, string) {
	t.Helper()
	wait := readyWait(t, f, "never proposed")
	opID, err := opjournal.DeriveID(f.laneStore(t).Prefix(), f.id, waitNoticePrincipal, waitNoticeKey(waitNudgeID(wait)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionFrontDoor(f.store).SetWaitNudgeID(wait.ID, opID); err != nil {
		t.Fatal(err)
	}
	f.cfg.Ops.Authoritative = nil
	newNoticeLaneHost(t, f)
	return waitNow(t, f, wait), opID
}

// queueWaitAttempt queues wait's current attempt as the legacy queue's item,
// leaving the wait's nudge_id as it is, and returns the item's id.
func queueWaitAttempt(t *testing.T, f *laneFixture, wait sessionpkg.WaitInfo) string {
	t.Helper()
	id := waitNudgeID(wait)
	item := newQueuedNudgeWithOptions("worker", "Wait satisfied ("+wait.ID+")", "wait", time.Now().UTC(), queuedNudgeOptions{
		ID:                id,
		SessionID:         f.id,
		ContinuationEpoch: wait.RegisteredEpoch,
		Reference:         &nudgeReference{Kind: "bead", ID: wait.ID},
	})
	if err := enqueueQueuedNudgeWithStore(f.dir, beads.NudgesStore{Store: f.store}, item); err != nil {
		t.Fatalf("queueing wait %s's attempt: %v", wait.ID, err)
	}
	return id
}

// After a rollback, a nudge_id naming a notice that was never proposed
// leaves the attempt to the queue. The queue records its item as the wait's
// nudge_id before it queues it, so a refused record queues nothing and
// leaves the attempt to the next pass. A wait that names the notice while
// the queue holds the attempt is closed by the next prepare from the item
// once the queue delivers it, which sheds the wait's hold.
func TestWaitNeverProposedNoticeLeavesTheAttemptToItsQueueItem(t *testing.T) {
	t.Run("recorded before it is queued", func(t *testing.T) {
		store := &waitNudgeIDRefusingStore{laneTestStore: newLaneTestStore()}
		f := newLaneFixtureOver(t, store)
		wait, opID := neverProposedNoticeWait(t, f)
		queueID := waitNudgeID(wait)
		shadows := nudgeShadows(beads.NudgesStore{Store: f.store})
		store.refuse = wait.ID
		err := dispatchReadyWaitNudgesWithSnapshot(f.dir, f.cfg, sessionFrontDoor(f.store), beads.NudgesStore{Store: f.store}, time.Now().UTC(), nil)
		if err == nil || !strings.Contains(err.Error(), "setting wait nudge_id") {
			t.Fatalf("dispatch = %v, want the queue item's refused record", err)
		}
		if _, ok, err := shadows.FindIncludingTerminal(queueID); err != nil || ok {
			t.Fatalf("queue item %s after the refused record: exists %v, %v; want nothing queued", queueID, ok, err)
		}
		if got := waitNow(t, f, wait).NudgeID; got != opID {
			t.Fatalf("nudge_id after the refused record = %q, want the never-proposed notice %s", got, opID)
		}
		store.refuse = ""
		dispatchWaits(t, f)
		if got := waitNow(t, f, wait).NudgeID; got != queueID {
			t.Fatalf("nudge_id = %q, want the queue item %s in place of %s", got, queueID, opID)
		}
		if _, open, err := shadows.Find(queueID); err != nil || !open {
			t.Fatalf("queue item %s open %v, %v; want it open", queueID, open, err)
		}
		noticeOpAbsent(t, f.laneStore(t), opID)
	})
	t.Run("finalize", func(t *testing.T) {
		f := newLaneFixture(t)
		if err := setSessionWaitHold(sessionFrontDoor(f.store), f.id); err != nil {
			t.Fatalf("setting the wait hold: %v", err)
		}
		wait, _ := neverProposedNoticeWait(t, f)
		queueID := queueWaitAttempt(t, f, wait)
		shadows := nudgequeue.NewStore(beads.NudgesStore{Store: f.store})
		shadow, _, err := shadows.Find(queueID)
		if err != nil {
			t.Fatal(err)
		}
		if err := shadows.Terminalize(nudgequeue.Item{ID: queueID, BeadID: shadow.BeadID}, "injected", "", "provider-nudge-return", time.Now().UTC()); err != nil {
			t.Fatalf("delivering the queue item: %v", err)
		}
		if ready := prepareWaits(t, f); ready[f.id] {
			t.Fatalf("ready-wait set = %v, want the session out of it", ready)
		}
		b, err := f.store.Get(wait.ID)
		if err != nil {
			t.Fatal(err)
		}
		if m := b.Metadata; m["state"] != waitStateClosed || m["nudge_id"] != queueID || m["commit_boundary"] != "provider-nudge-return" {
			t.Fatalf("wait = %v, want closed from the queue item %s", m, queueID)
		}
		if d := sessionpkg.DispositionOfInfo(f.rowInfo(t)); d.Wait || d.WaitIntent {
			t.Fatalf("disposition = %+v, want the wait hold gone", d)
		}
	})
}

// gc wait cancel withdraws the queue item a rollback queues for a wait whose
// nudge_id named a notice that was never proposed, even when the cancel
// lands the moment the item is queued: the item is the wait's nudge_id by
// then, so the cancel withdraws it, not the never-proposed notice, and the
// canceled wait's reminder is never delivered.
func TestWaitCancelAsTheQueueTakesANeverProposedNoticesAttemptWithdrawsIt(t *testing.T) {
	c := cliWaitCity(t)
	wait, opID := neverProposedNoticeWait(t, c.laneFixture)
	queueID := waitNudgeID(wait)
	var cancel struct {
		ran    bool
		code   int
		stderr string
	}
	prevPing := pingNudgeWakeSocket
	pingNudgeWakeSocket = func(string) {
		if !cancel.ran {
			cancel.ran = true
			_, cancel.code, cancel.stderr = gcWait(wait.ID, waitStateCanceled)
		}
	}
	t.Cleanup(func() { pingNudgeWakeSocket = prevPing })
	dispatchWaits(t, c.laneFixture)
	if !cancel.ran || cancel.code != 0 {
		t.Fatalf("gc wait cancel as the item was queued: ran %v, exit %d, stderr %q", cancel.ran, cancel.code, cancel.stderr)
	}
	if _, open, err := nudgeShadows(beads.NudgesStore{Store: c.store}).Find(queueID); err != nil || open {
		t.Fatalf("queue item %s after gc wait cancel open %v, %v; want it withdrawn", queueID, open, err)
	}
	if got := waitNow(t, c.laneFixture, wait); got.State != waitStateCanceled || got.NudgeID != queueID {
		t.Fatalf("wait after gc wait cancel is %s naming %q, want canceled naming the queue item %s", got.State, got.NudgeID, queueID)
	}
	noticeOpAbsent(t, c.laneStore(t), opID)
}

// unreadableNotice makes op's row one this build cannot decode: a schema
// revision a newer build would write.
func unreadableNotice(t *testing.T, f *laneFixture, op opjournal.Op) {
	t.Helper()
	if err := f.store.SetMetadata(op.ID, "gc.op.schema", "99"); err != nil {
		t.Fatalf("rewriting the notice's schema revision: %v", err)
	}
	if _, err := f.laneStore(t).Get(op.ID); !errors.Is(err, opjournal.ErrUnsupportedSchema) {
		t.Fatalf("Get(%s) = %v, want it unreadable", op.ID, err)
	}
}

// A re-arm keeps the attempt while its delivery is in flight, a lane notice
// as an open queue item: an open notice, executing or not, keeps it, as does
// a never-proposed one whose queue item is open. A closed or unreadable
// notice, or a never-proposed one with no queue item, holds nothing, and the
// re-arm takes the next attempt.
func TestWaitReArmReadsItsNoticeForTheNextAttempt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *laneFixture) sessionpkg.WaitInfo
		next  string
	}{
		{"open", func(t *testing.T, f *laneFixture) sessionpkg.WaitInfo {
			newNoticeLaneHost(t, f, "message")
			wait := readyWait(t, f, "open")
			dispatchWaits(t, f)
			return wait
		}, ""},
		{"executing", func(t *testing.T, f *laneFixture) sessionpkg.WaitInfo {
			newNoticeLaneHost(t, f, "message")
			wait := readyWait(t, f, "executing")
			dispatchWaits(t, f)
			st := f.laneStore(t)
			if _, err := st.Transition(waitNotice(t, f, st, wait).ID, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
				t.Fatal(err)
			}
			return wait
		}, ""},
		{"closed", func(t *testing.T, f *laneFixture) sessionpkg.WaitInfo {
			newNoticeLaneHost(t, f, "message")
			wait := readyWait(t, f, "closed")
			dispatchWaits(t, f)
			st := f.laneStore(t)
			if _, err := st.Close(waitNotice(t, f, st, wait).ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl"); err != nil {
				t.Fatal(err)
			}
			return wait
		}, "2"},
		{"unreadable", func(t *testing.T, f *laneFixture) sessionpkg.WaitInfo {
			newNoticeLaneHost(t, f, "message")
			wait := readyWait(t, f, "unreadable")
			dispatchWaits(t, f)
			unreadableNotice(t, f, waitNotice(t, f, f.laneStore(t), wait))
			return wait
		}, "2"},
		{"never proposed", func(t *testing.T, f *laneFixture) sessionpkg.WaitInfo {
			wait, _ := neverProposedNoticeWait(t, f)
			return wait
		}, "2"},
		{"never proposed, queued", func(t *testing.T, f *laneFixture) sessionpkg.WaitInfo {
			wait, _ := neverProposedNoticeWait(t, f)
			queueWaitAttempt(t, f, wait)
			return wait
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			w := waitNow(t, f, tc.setup(t, f))
			if !opjournal.IsOpID(w.NudgeID) {
				t.Fatalf("nudge_id = %q, want a notice's op id", w.NudgeID)
			}
			w.State = waitStateCanceled
			next, err := nextWaitDeliveryAttempt(sessionFrontDoor(f.store), nudgeShadows(beads.NudgesStore{Store: f.store}), w)
			if err != nil || next != tc.next {
				t.Fatalf("nextWaitDeliveryAttempt = %q, %v; want %q", next, err, tc.next)
			}
		})
	}
}

// reArmedWhileBeingDelivered is a ready wait canceled while its notice was
// being delivered, then re-armed: the withdrawal cannot stop the delivery,
// so it records gc:wait's cancel and logs that, and the re-arm of the same
// wait bead (one whose terminal write left it open) keeps the attempt on
// the executing notice, which it returns.
func reArmedWhileBeingDelivered(t *testing.T, f *laneFixture, st *opjournal.Store) (sessionpkg.WaitInfo, opjournal.Op) {
	t.Helper()
	logs := captureStdLog(t)
	wait := readyWait(t, f, "re-arm")
	dispatchWaits(t, f)
	op := waitNotice(t, f, st, wait)
	if _, err := st.Transition(op.ID, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMetadata(wait.ID, "state", waitStateCanceled); err != nil {
		t.Fatal(err)
	}
	if err := withdrawWaitNotices(beads.SessionStore{Store: f.store}, []string{op.ID}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusExecuting || got.Cancel.By != waitNoticePrincipal {
		t.Fatalf("notice after the withdrawal = %+v, want it executing with gc:wait's cancel recorded", got)
	}
	if !strings.Contains(logs.String(), "wait notice "+op.ID+" is being delivered") {
		t.Fatalf("log = %q, want the withdrawal that could not stop the delivery reported", logs.String())
	}
	sessFront := sessionFrontDoor(f.store)
	next, err := nextWaitDeliveryAttempt(sessFront, nudgeShadows(beads.NudgesStore{Store: f.store}), waitNow(t, f, wait))
	if err != nil || next != "" {
		t.Fatalf("next attempt = %q, %v; want none while the notice is open", next, err)
	}
	if err := sessFront.MarkWaitReadyForRedelivery(wait.ID, next, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return wait, op
}

// A wait canceled while its notice is being delivered, then re-armed, keeps
// its attempt, so the re-arm proposes no second notice, as a re-arm over an
// in-flight queue item queues none. The notice's outcome then settles the
// attempt: delivered, it closes the wait; not delivered (the lane closes it
// canceled, under the withdrawal's cancel), the attempt is void, and the
// wait stays ready on the next one, whose notice the next dispatch proposes.
func TestWaitReArmWhileItsNoticeIsBeingDeliveredSendsNoSecondNotice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome opjournal.Outcome
		state   string
		attempt string
	}{
		{"delivered", opjournal.Outcome{Arm: opjournal.ArmSucceeded}, waitStateClosed, "1"},
		{"not delivered", opjournal.Outcome{Arm: opjournal.ArmCanceled}, waitStateReady, "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			newNoticeLaneHost(t, f, "message")
			st := f.laneStore(t)
			wait, op := reArmedWhileBeingDelivered(t, f, st)
			dispatchWaits(t, f)
			if open := f.openOps(t); len(open) != 1 || open[0].ID != op.ID {
				t.Fatalf("open ops = %+v, want only the notice being delivered", open)
			}
			if got := waitNow(t, f, wait); got.NudgeID != op.ID || got.DeliveryAttempt != "1" {
				t.Fatalf("wait nudge_id %q attempt %q, want attempt 1 still on its notice %s", got.NudgeID, got.DeliveryAttempt, op.ID)
			}

			if _, err := st.Close(op.ID, tc.outcome, "lane:ctl"); err != nil {
				t.Fatal(err)
			}
			ready := prepareWaits(t, f)
			got := waitNow(t, f, wait)
			if got.State != tc.state || got.DeliveryAttempt != tc.attempt || ready[f.id] != (tc.state == waitStateReady) {
				t.Fatalf("after the notice closed %s: wait %s on attempt %q, ready-wait set %v; want %s on attempt %q",
					tc.outcome.Arm, got.State, got.DeliveryAttempt, ready, tc.state, tc.attempt)
			}
			dispatchWaits(t, f)
			open := f.openOps(t)
			if tc.state == waitStateClosed {
				if len(open) != 0 {
					t.Fatalf("open ops = %+v, want no second notice for a delivered attempt", open)
				}
				return
			}
			next := waitNotice(t, f, st, waitNow(t, f, wait))
			if len(open) != 1 || open[0].ID != next.ID || next.ID == op.ID {
				t.Fatalf("open ops = %+v, want only attempt %s's notice %s", open, tc.attempt, next.ID)
			}
			if got := waitNow(t, f, wait); got.NudgeID != next.ID {
				t.Fatalf("wait nudge_id = %q, want its next notice %s", got.NudgeID, next.ID)
			}
		})
	}
}

// Only a wait still ready on the withdrawn notice moves to its next attempt:
// the finalize reads the wait again before it writes, so a wait that ended
// after the pass read it ready (gc wait cancel, which withdraws the notice)
// stays ended, and a wait already moved past the notice, by an earlier
// finalize from the same read, is not moved a second time.
func TestWaitWithdrawnNoticeMovesOnlyAWaitStillOnIt(t *testing.T) {
	t.Run("ended since the pass read it", func(t *testing.T) {
		f := newLaneFixture(t)
		newNoticeLaneHost(t, f, "message")
		st := f.laneStore(t)
		wait := readyWait(t, f, "ended")
		dispatchWaits(t, f)
		read := waitNow(t, f, wait)
		sessFront := sessionFrontDoor(f.store)
		if err := sessFront.CancelWait(wait.ID, time.Now().UTC(), ""); err != nil {
			t.Fatal(err)
		}
		if err := withdrawWaitNotices(beads.SessionStore{Store: f.store}, []string{read.NudgeID}); err != nil {
			t.Fatalf("withdraw: %v", err)
		}
		if got := f.get(t, st, read.NudgeID); got.Status != opjournal.StatusClosed || got.Cancel.By != waitNoticePrincipal {
			t.Fatalf("notice after the withdrawal = %+v, want it closed canceled by gc:wait", got)
		}
		done, err := finalizeReadyWaitFromNudge(sessFront, beads.NudgesStore{Store: f.store}, read, time.Now().UTC())
		if err != nil || !done {
			t.Fatalf("finalize = %v, %v; want done", done, err)
		}
		if got := waitNow(t, f, wait); got.State != waitStateCanceled || got.Status != "closed" || got.DeliveryAttempt != "1" {
			t.Fatalf("wait %s (%s) on attempt %q, want it still canceled on attempt 1", got.State, got.Status, got.DeliveryAttempt)
		}
	})
	t.Run("already past it", func(t *testing.T) {
		f := newLaneFixture(t)
		newNoticeLaneHost(t, f, "message")
		st := f.laneStore(t)
		wait, op := reArmedWhileBeingDelivered(t, f, st)
		if _, err := st.Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmCanceled}, "lane:ctl"); err != nil {
			t.Fatal(err)
		}
		read := waitNow(t, f, wait)
		sessFront := sessionFrontDoor(f.store)
		for pass := range 2 {
			done, err := finalizeReadyWaitFromNudge(sessFront, beads.NudgesStore{Store: f.store}, read, time.Now().UTC())
			if err != nil || done {
				t.Fatalf("finalize %d = %v, %v; want the wait kept ready", pass, done, err)
			}
		}
		if got := waitNow(t, f, wait); got.State != waitStateReady || got.DeliveryAttempt != "2" || got.NudgeID != "" {
			t.Fatalf("wait %s on attempt %q nudge_id %q, want it ready on attempt 2 with no notice yet", got.State, got.DeliveryAttempt, got.NudgeID)
		}
	})
}

// cliWaitCity is a lane city on disk, with a lane host executing message,
// whose gc wait commands open the fixture's store, as an operator's would.
func cliWaitCity(t *testing.T) *cliLaneCity {
	t.Helper()
	clearGCEnv(t)
	c := newCLILaneCity(t)
	c.cityOnDisk(t)
	prevFactory, prevNudges := openStoreFactoryForCity, openOwnedNudgeBeadStore
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: c.store}, nil
	}
	openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
		return beads.NudgesStore{Store: c.store}, c.store
	}
	t.Cleanup(func() { openStoreFactoryForCity, openOwnedNudgeBeadStore = prevFactory, prevNudges })
	newNoticeLaneHost(t, c.laneFixture, "message")
	return c
}

// gcWait runs gc wait cancel or gc wait ready, as state names, on wait id,
// and returns its result, exit code and stderr.
func gcWait(id, state string) (waitSetStateResult, int, string) {
	var stdout, stderr bytes.Buffer
	result, code := cmdWaitSetStateResult(id, state, &stdout, &stderr)
	return result, code, stderr.String()
}

// gc wait cancel, then gc wait ready, delivers the reminder once, whichever
// side of the notice's step from submitted to executing the cancel lands
// on. Before it, the cancel closes the notice, and the retry is a new wait
// on the next attempt. While the notice is being delivered, the cancel is
// only recorded, so the retry, a new wait whose notice would have a key of
// its own, is refused until the notice closes; it then takes the next
// attempt, and one notice is proposed for it.
func TestWaitCancelThenReadyProposesOneNotice(t *testing.T) {
	for _, tc := range []struct {
		name      string
		executing bool
	}{
		{"canceled before delivery", false},
		{"canceled while being delivered", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cliWaitCity(t)
			st := c.laneStore(t)
			wait := readyWait(t, c.laneFixture, "cancel, then ready")
			dispatchWaits(t, c.laneFixture)
			op := waitNotice(t, c.laneFixture, st, wait)
			if tc.executing {
				if _, err := st.Transition(op.ID, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
					t.Fatal(err)
				}
			}
			if _, code, stderr := gcWait(wait.ID, waitStateCanceled); code != 0 {
				t.Fatalf("gc wait cancel = %d; stderr %q", code, stderr)
			}
			if tc.executing {
				_, code, stderr := gcWait(wait.ID, waitStateReady)
				if code == 0 || !strings.Contains(stderr, "notice "+op.ID+" is still open") {
					t.Fatalf("gc wait ready while the notice is being delivered = %d, stderr %q; want it refused, naming the notice", code, stderr)
				}
				if open, err := sessionFrontDoor(c.store).WaitsForSession(c.id); err != nil || len(open) != 0 {
					t.Fatalf("open waits after the refused retry = %+v, %v; want none", open, err)
				}
				dispatchWaits(t, c.laneFixture)
				if open := c.openOps(t); len(open) != 1 || open[0].ID != op.ID {
					t.Fatalf("open ops = %+v, want only the notice being delivered", open)
				}
				if _, err := st.Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmCanceled}, "lane:ctl"); err != nil {
					t.Fatal(err)
				}
			}
			if got := c.get(t, st, op.ID); got.Status != opjournal.StatusClosed || got.Cancel.By != waitNoticePrincipal {
				t.Fatalf("notice before the retry = %+v, want it closed under gc:wait's cancel", got)
			}
			result, code, stderr := gcWait(wait.ID, waitStateReady)
			if code != 0 || !result.Retried {
				t.Fatalf("gc wait ready = %d (retried %v); stderr %q", code, result.Retried, stderr)
			}
			retried := waitNow(t, c.laneFixture, sessionpkg.WaitInfo{ID: result.ReadyWaitID})
			if retried.State != waitStateReady || retried.DeliveryAttempt != "2" {
				t.Fatalf("retried wait %s on attempt %q, want it ready on attempt 2", retried.State, retried.DeliveryAttempt)
			}
			dispatchWaits(t, c.laneFixture)
			next := waitNotice(t, c.laneFixture, st, retried)
			if open := c.openOps(t); len(open) != 1 || open[0].ID != next.ID {
				t.Fatalf("open ops = %+v, want only the retried wait's notice %s", open, next.ID)
			}
		})
	}
}

// An expired wait's notice is not withdrawn, so it may still be open when
// gc wait ready retries the wait: the retry is refused, naming the remedy,
// and once gc wait cancel has withdrawn the notice, the retry takes the
// next attempt and one notice is proposed for it.
func TestWaitReadyOfAnExpiredWaitWithdrawsItsOpenNoticeFirst(t *testing.T) {
	c := cliWaitCity(t)
	st := c.laneStore(t)
	wait := readyWait(t, c.laneFixture, "expired")
	dispatchWaits(t, c.laneFixture)
	op := waitNotice(t, c.laneFixture, st, wait)
	if err := sessionFrontDoor(c.store).ExpireWait(wait.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_, code, stderr := gcWait(wait.ID, waitStateReady)
	if code == 0 || !strings.Contains(stderr, "notice "+op.ID+" is still open") || !strings.Contains(stderr, `"gc wait cancel `+wait.ID+`"`) {
		t.Fatalf("gc wait ready over the open notice = %d, stderr %q; want it refused, naming the notice and gc wait cancel", code, stderr)
	}
	if _, code, stderr := gcWait(wait.ID, waitStateCanceled); code != 0 {
		t.Fatalf("gc wait cancel = %d; stderr %q", code, stderr)
	}
	if got := c.get(t, st, op.ID); got.Status != opjournal.StatusClosed || got.Cancel.By != waitNoticePrincipal {
		t.Fatalf("notice after gc wait cancel = %+v, want it closed under gc:wait's cancel", got)
	}
	result, code, stderr := gcWait(wait.ID, waitStateReady)
	if code != 0 || !result.Retried {
		t.Fatalf("gc wait ready = %d (retried %v); stderr %q", code, result.Retried, stderr)
	}
	retried := waitNow(t, c.laneFixture, sessionpkg.WaitInfo{ID: result.ReadyWaitID})
	dispatchWaits(t, c.laneFixture)
	next := waitNotice(t, c.laneFixture, st, retried)
	if open := c.openOps(t); retried.DeliveryAttempt != "2" || len(open) != 1 || open[0].ID != next.ID {
		t.Fatalf("retried wait on attempt %q, open ops %+v; want attempt 2 and only its notice %s", retried.DeliveryAttempt, open, next.ID)
	}
}

// gc wait cancel over a notice this build cannot read cancels the wait and
// sheds its hold: the lane never delivers such a notice, so the withdrawal
// logs it and leaves it, rather than failing the command after the wait has
// ended.
func TestWaitCancelLeavesAnUnreadableNotice(t *testing.T) {
	logs := captureStdLog(t)
	c := cliWaitCity(t)
	if err := setSessionWaitHold(sessionFrontDoor(c.store), c.id); err != nil {
		t.Fatalf("setting the wait hold: %v", err)
	}
	wait := readyWait(t, c.laneFixture, "unreadable")
	dispatchWaits(t, c.laneFixture)
	op := waitNotice(t, c.laneFixture, c.laneStore(t), wait)
	unreadableNotice(t, c.laneFixture, op)
	if _, code, stderr := gcWait(wait.ID, waitStateCanceled); code != 0 {
		t.Fatalf("gc wait cancel = %d; stderr %q", code, stderr)
	}
	if got := waitNow(t, c.laneFixture, wait); got.State != waitStateCanceled || got.Status != "closed" {
		t.Fatalf("wait %s (%s), want it canceled", got.State, got.Status)
	}
	if d := sessionpkg.DispositionOfInfo(c.rowInfo(t)); d.Wait || d.WaitIntent {
		t.Fatalf("disposition = %+v, want the wait hold gone", d)
	}
	if !strings.Contains(logs.String(), "wait notice "+op.ID+" is unreadable") {
		t.Fatalf("log = %q, want the unreadable notice named", logs.String())
	}
}

// pendingDepsWait creates a pending deps wait for f's session whose one
// dependency is closed, so a prepare pass marks it ready.
func pendingDepsWait(t *testing.T, f *laneFixture) sessionpkg.WaitInfo {
	t.Helper()
	dep, err := f.store.Create(beads.Bead{Title: "dependency", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(dep.ID); err != nil {
		t.Fatal(err)
	}
	b, err := f.store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel, "session:" + f.id},
		Metadata: map[string]string{
			"session_id":       f.id,
			"session_name":     f.name,
			"kind":             "deps",
			"state":            waitStatePending,
			"dep_ids":          dep.ID,
			"dep_mode":         "all",
			"registered_epoch": "1",
			"delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatalf("creating the deps wait: %v", err)
	}
	return sessionpkg.WaitInfoFromBead(b)
}

// A wait whose notice this build cannot read (a row a newer build wrote, or
// a corrupt one) does not stall the wake-state pass: the lane never delivers
// such a row, so the wait fails as unreadable, and an unrelated deps wait
// whose dependency closed is still marked ready.
func TestWaitPrepareFailsAWaitWhoseNoticeIsUnreadable(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	unreadable := readyWait(t, f, "unreadable")
	dispatchWaits(t, f)
	op := waitNotice(t, f, f.laneStore(t), unreadable)
	unreadableNotice(t, f, op)
	deps := pendingDepsWait(t, f)
	ready := prepareWaits(t, f)
	if got := waitNow(t, f, deps); got.State != waitStateReady || !ready[f.id] {
		t.Fatalf("deps wait %s, ready-wait set %v; want it ready", got.State, ready)
	}
	if got := waitNow(t, f, unreadable); got.State != waitStateFailed || got.NudgeID != op.ID {
		t.Fatalf("unreadable wait %s on %q, want it failed on its notice %s", got.State, got.NudgeID, op.ID)
	}
	if got := waitLastError(t, f, unreadable); !strings.HasPrefix(got, "notice unreadable: ") || !strings.Contains(got, op.ID) {
		t.Fatalf("wait last_error = %q, want the unreadable notice named", got)
	}
}

// failingNoticeReadStore is a sessions store whose read of the bead fail
// names fails.
type failingNoticeReadStore struct {
	laneTestStore
	fail string
}

func (s *failingNoticeReadStore) Get(id string) (beads.Bead, error) {
	if s.fail != "" && id == s.fail {
		return beads.Bead{}, errors.New("notice read refused")
	}
	return s.laneTestStore.Get(id)
}

// A ready wait whose notice cannot be read right now is retained, still
// ready, while the pass goes on to the other waits; the failure is logged
// once however many passes repeat it.
func TestWaitPrepareRetainsAWaitWhoseNoticeReadFails(t *testing.T) {
	prev := waitFinalizeFailures
	waitFinalizeFailures = logutil.NewDedup(logutil.DefaultDedupCapacity)
	t.Cleanup(func() { waitFinalizeFailures = prev })
	logs := captureStdLog(t)
	store := &failingNoticeReadStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	newNoticeLaneHost(t, f, "message")
	wait := readyWait(t, f, "read refused")
	dispatchWaits(t, f)
	op := waitNotice(t, f, f.laneStore(t), wait)
	store.fail = op.ID
	deps := pendingDepsWait(t, f)
	for pass := range 2 {
		if ready := prepareWaits(t, f); !ready[f.id] {
			t.Fatalf("pass %d: ready-wait set = %v, want the session in it", pass, ready)
		}
	}
	if got := waitNow(t, f, wait); got.State != waitStateReady || got.NudgeID != op.ID {
		t.Fatalf("wait %s on %q, want it still ready on its notice %s", got.State, got.NudgeID, op.ID)
	}
	if got := waitNow(t, f, deps); got.State != waitStateReady {
		t.Fatalf("deps wait %s, want it ready", got.State)
	}
	if n := strings.Count(logs.String(), "wait "+wait.ID+": finalizing from nudge "+op.ID); n != 1 {
		t.Fatalf("logged the failure %d times, want once: %q", n, logs.String())
	}
}
