package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
