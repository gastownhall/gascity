package opjournal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// dedupRuntime models a runtime that deduplicates by delivery key: the first
// content under a key is shown to the agent; a later delivery under that key
// is dropped and acked as delivered.
type dedupRuntime struct {
	shown      map[string][]string // key → op ids the agent saw
	transcript []string
	// failFirst makes the first delivery end Unknown without landing.
	failFirst bool
}

func newDedupRuntime(h *laneHarness, exec *fakeExec) *dedupRuntime {
	rt := &dedupRuntime{shown: map[string][]string{}, failFirst: true}
	exec.probe.Caps.Dedup = true
	exec.deliver = func(d Delivery) DeliveryResult {
		if rt.failFirst {
			rt.failFirst = false
			return Unknown{Cause: errors.New("bridge timeout")}
		}
		if _, ok := rt.shown[d.Key]; !ok {
			for _, op := range d.Ops {
				rt.shown[d.Key] = append(rt.shown[d.Key], op.ID)
				rt.transcript = append(rt.transcript, op.ID)
			}
		}
		return Delivered{Record: DeliveryRecord{At: h.clk.Now(), Ack: AckDelivered}}
	}
	return rt
}

// assertShownIfSucceeded fails when an op closed succeeded but the agent never
// saw it.
func assertShownIfSucceeded(t *testing.T, h *laneHarness, rt *dedupRuntime, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if got := h.get(t, id); got.Outcome.Arm == ArmSucceeded && !slices.Contains(rt.transcript, id) {
			t.Fatalf("op %s closed succeeded under key %s but the deduplicating runtime never showed it (saw %v)", id, got.DeliveryKey, rt.transcript)
		}
	}
}

// A maybe-delivered group [steer, boundary notice] must not be split on a
// deduplicating runtime: mid-turn, the steer waits for its group.
func TestLaneDedupReplayGroupIsNotSplitMidTurn(t *testing.T) {
	h := newLaneHarness(t)
	s := h.propose(t, laneMsg("s", RoleNotice, "steer me", withWhen(WhenSteer)))
	h.clk.Advance(time.Millisecond)
	b := h.propose(t, laneMsg("b", RoleNotice, "at the boundary"))
	exec := newFakeExec(h.clk)
	exec.probe.Caps.Steer = true
	rt := newDedupRuntime(h, exec)
	l := h.lane(t, "ctl")
	mustPass(t, l, view(), exec) // [s, b] under K: Unknown, not landed
	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)

	exec.probe.Boundary = TriNo
	res := mustPass(t, l, view(), exec)
	if res.Took != TakeNone {
		t.Fatalf("mid-turn pass took %v: a replay group was split", res.Taken)
	}
	if w := waitsOf(res.Waits); w[s.ID] != WaitReplayGroup || w[b.ID] != WaitBusy {
		t.Fatalf("waits %v", res.Waits)
	}
	exec.probe.Boundary = TriYes
	res = mustPass(t, l, view(), exec)
	if !slices.Equal(res.Taken, []string{s.ID, b.ID}) {
		t.Fatalf("boundary pass took %v", res.Taken)
	}
	assertShownIfSucceeded(t, h, rt, s.ID, b.ID)
	for _, d := range exec.delivered() {
		if d.Key != exec.delivered()[0].Key {
			t.Fatalf("the replay used key %s, the first attempt %s", d.Key, exec.delivered()[0].Key)
		}
	}
}

// T3's Stop-with-hold: content_hold holds the notice while the urgent steer
// bypasses it. The group must still go whole.
func TestLaneDedupReplayGroupIsNotSplitByContentHold(t *testing.T) {
	h := newLaneHarness(t)
	s := h.propose(t, laneMsg("s", RolePrompt, "steer now", withWhen(WhenSteer)))
	h.clk.Advance(time.Millisecond)
	b := h.propose(t, laneMsg("b", RoleNotice, "fyi"))
	exec := newFakeExec(h.clk)
	rt := newDedupRuntime(h, exec)
	l := h.lane(t, "ctl")
	mustPass(t, l, view(), exec)
	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)

	held := view()
	held.ContentHold = true
	res := mustPass(t, l, held, exec)
	if res.Took != TakeNone {
		t.Fatalf("held pass took %v: a replay group was split", res.Taken)
	}
	if w := waitsOf(res.Waits); w[s.ID] != WaitReplayGroup || w[b.ID] != WaitContentHold {
		t.Fatalf("waits %v", res.Waits)
	}
	res = mustPass(t, l, view(), exec)                  // Resume
	if !slices.Equal(res.Taken, []string{b.ID, s.ID}) { // notices before the prompt
		t.Fatalf("resumed pass took %v", res.Taken)
	}
	assertShownIfSucceeded(t, h, rt, s.ID, b.ID)
}

// A shown [after_turn rider, if_idle starter] group whose lease expired stays
// whole: mid-turn the starter waits with its rider instead of closing
// skipped{busy}, which would leave the rider to replay alone, and at the
// boundary the group replays under its key.
func TestLaneDedupReplayGroupKeepsItsIfIdleStarter(t *testing.T) {
	h := newLaneHarness(t)
	r := h.propose(t, laneMsg("r", RoleNotice, "when this turn ends", withWhen(WhenAfterTurn)))
	h.clk.Advance(time.Millisecond)
	s := h.propose(t, laneMsg("s", RolePrompt, "if idle", withWhen(WhenIfIdle)))
	exec := newFakeExec(h.clk)
	rt := newDedupRuntime(h, exec)
	rt.failFirst = false
	a := h.lane(t, "ctl")
	a.hooks.at = crashAt(stageAfterEffect)
	if _, err := a.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
		t.Fatalf("pass = %v", err)
	}
	h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)

	l := h.lane(t, "cli")
	exec.probe.Boundary = TriNo
	res := mustPass(t, l, view(), exec)
	if w := waitsOf(res.Waits); res.Took != TakeNone || w[r.ID] != WaitBusy || w[s.ID] != WaitBusy {
		t.Fatalf("mid-turn pass took %v, waits %v", res.Taken, res.Waits)
	}
	exec.probe.Boundary = TriYes
	res = mustPass(t, l, view(), exec)
	if !slices.Equal(res.Taken, []string{r.ID, s.ID}) {
		t.Fatalf("boundary pass took %v", res.Taken)
	}
	if ds := exec.delivered(); len(ds) != 2 || ds[1].Key != ds[0].Key || !ds[1].Redelivery {
		t.Fatalf("%d deliveries; the replay must go under the first key", len(ds))
	}
	for _, id := range []string{r.ID, s.ID} {
		if got := h.get(t, id); got.Outcome.Arm != ArmSucceeded {
			t.Fatalf("op %s %+v", id, got.Outcome)
		}
	}
	assertShownIfSucceeded(t, h, rt, r.ID, s.ID)
}

// A member that closed leaves the group; the rest replays under the key.
func TestLaneDedupReplayGroupDropsClosedMembers(t *testing.T) {
	h := newLaneHarness(t)
	a := h.propose(t, laneMsg("a", RoleNotice, "one"))
	h.clk.Advance(time.Millisecond)
	b := h.propose(t, laneMsg("b", RoleNotice, "two"))
	exec := newFakeExec(h.clk)
	newDedupRuntime(h, exec)
	l := h.lane(t, "ctl")
	mustPass(t, l, view(), exec)
	if _, _, err := h.store.Cancel(b.ID, "operator", ""); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	res := mustPass(t, l, view(), exec)
	if !slices.Equal(res.Taken, []string{a.ID}) {
		t.Fatalf("took %v", res.Taken)
	}
	if ds := exec.delivered(); ds[1].Key != ds[0].Key {
		t.Fatalf("replay key %s, first %s", ds[1].Key, ds[0].Key)
	}
}

// On a runtime that does not deduplicate, the steer may go ahead of its
// group: nothing is lost, and the note marks the repeat.
func TestLaneNonDedupReplayMayGoInPart(t *testing.T) {
	h := newLaneHarness(t)
	s := h.propose(t, laneMsg("s", RoleNotice, "steer me", withWhen(WhenSteer)))
	h.clk.Advance(time.Millisecond)
	h.propose(t, laneMsg("b", RoleNotice, "at the boundary"))
	exec := newFakeExec(h.clk)
	exec.probe.Caps.Steer = true
	first := true
	exec.deliver = func(Delivery) DeliveryResult {
		if first {
			first = false
			return Unknown{Cause: errors.New("bridge timeout")}
		}
		return Delivered{Record: DeliveryRecord{At: h.clk.Now(), Ack: AckDelivered}}
	}
	l := h.lane(t, "ctl")
	mustPass(t, l, view(), exec)
	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	exec.probe.Boundary = TriNo
	if res := mustPass(t, l, view(), exec); !slices.Equal(res.Taken, []string{s.ID}) {
		t.Fatalf("took %v", res.Taken)
	}
}

// L3: after a key rejection a later Take mints K'; that attempt may reach the
// agent before the lane crashes. The re-take must reuse K', not mint K”.
func TestLaneRetakeAfterKeyRejectedKeepsTheNewKey(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "x"))
	exec := newFakeExec(h.clk)
	exec.probe.Caps.Dedup = true
	l := h.lane(t, "ctl")
	exec.deliver = func(Delivery) DeliveryResult { return Unknown{Cause: errors.New("bridge timeout")} }
	mustPass(t, l, view(), exec) // unknown: the lane cannot tell whether :1 landed
	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	exec.deliver = func(Delivery) DeliveryResult { return Rejected{KeyRejected: true, Reason: "nudge_rejected"} }
	mustPass(t, l, view(), exec) // :1 is refused, so it never landed: the next send re-keys
	if got := h.get(t, op.ID); !strings.HasPrefix(got.LastError, lastErrorKeyRejected) {
		t.Fatalf("last_error %q", got.LastError)
	}
	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	exec.deliver = nil
	l.hooks.at = crashAt(stageAfterEffect)
	if _, err := l.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
		t.Fatalf("pass = %v", err)
	}
	l.hooks.at = nil
	h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
	mustPass(t, h.lane(t, "cli"), view(), exec)
	ds := exec.delivered()
	if len(ds) != 4 {
		t.Fatalf("%d deliveries", len(ds))
	}
	if ds[2].Key == ds[1].Key {
		t.Fatalf("the send after the key rejection reused the refused key %s", ds[1].Key)
	}
	if ds[3].Key != ds[2].Key {
		t.Fatalf("the re-take used key %s, but its maybe-delivered attempt used %s (L3)", ds[3].Key, ds[2].Key)
	}
}

// A maybe-delivered after_turn rider whose starter closed replays alone: it
// may already have reached the agent with that turn, and waiting for a
// starter would hold it until start_by.
func TestLaneRiderReplaysAloneWhenItsStarterClosed(t *testing.T) {
	for _, dedup := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "dedup"}[dedup], func(t *testing.T) {
			h := newLaneHarness(t)
			r := h.propose(t, laneMsg("r", RoleNotice, "context", withWhen(WhenAfterTurn)))
			h.clk.Advance(time.Millisecond)
			s := h.propose(t, laneMsg("s", RolePrompt, "starter"))
			exec := newFakeExec(h.clk)
			exec.probe.Caps.Dedup = dedup
			a := h.lane(t, "ctl")
			a.hooks.at = crashAt(stageAfterEffect)
			if _, err := a.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
				t.Fatalf("pass = %v", err)
			}
			if _, _, err := h.store.Cancel(s.ID, "operator", ""); err != nil {
				t.Fatal(err)
			}
			h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
			l := h.lane(t, "cli")
			res := mustPass(t, l, view(), exec)
			if res.Took == TakeNone {
				// The first pass may only close the canceled starter.
				res = mustPass(t, l, view(), exec)
			}
			if !slices.Equal(res.Taken, []string{r.ID}) {
				t.Fatalf("took %v, waits %+v", res.Taken, res.Waits)
			}
			ds := exec.delivered()
			if last := ds[len(ds)-1]; last.Key != ds[0].Key || !last.Redelivery {
				t.Fatalf("rider replay key %s redelivery %v, first key %s", last.Key, last.Redelivery, ds[0].Key)
			}
			if got := h.get(t, r.ID); got.Outcome.Arm != ArmSucceeded {
				t.Fatalf("rider %s %+v", got.Status, got.Outcome)
			}
		})
	}
}

// A rider whose group still holds a starter waits for it.
func TestLaneRiderReplayWaitsForItsOpenStarter(t *testing.T) {
	p := newPlanner(t)
	key := "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa:3"
	released := func(op *Op) {
		op.Status, op.Attempt, op.DeliveryKey, op.MaybeDelivered = StatusAccepted, 1, key, 1
	}
	rider := p.op(laneMsg("r", RoleNotice, "x", withWhen(WhenAfterTurn)), released)
	starter := p.op(laneMsg("s", RolePrompt, "y"), released, func(op *Op) { op.RetryAt = t0.Add(48 * time.Hour) })
	now := t0.Add(time.Hour)
	rp := planRules(snapOf(rider, starter), view(), now, "lane:ctl")
	tp, _ := planTake(rp, idleProbe(), view(), now)
	if tp.take != nil {
		t.Fatalf("took %v without its starter", takenOf(tp))
	}
	if w := waitsOf(tp.waits)[rider.ID]; w != WaitAfterTurn {
		t.Fatalf("rider wait %q", w)
	}
}

// A group member the rules keep out of the content class (its after target
// cannot be read on this pass) is still in its replay group: on a
// deduplicating runtime the rest of the group waits for it instead of
// replaying under the group's key without it, which would leave its own
// later send under that key dropped while it closed succeeded unseen.
func TestLaneDedupReplayGroupCountsMembersOutsideTheClass(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	exec := newFakeExec(h.clk)
	l := h.lane(t, "ctl")
	rt := newDedupRuntime(h, exec)
	rt.failFirst = false
	target := h.propose(t, laneMsg("t", RolePrompt, "first"))
	mustPass(t, l, view(), exec)
	if got := h.get(t, target.ID); got.Outcome.Arm != ArmSucceeded {
		t.Fatalf("after target: %+v", got.Outcome)
	}
	h.clk.Advance(time.Millisecond)
	a := h.propose(t, laneMsg("a", RoleNotice, "one"))
	h.clk.Advance(time.Millisecond)
	b := h.propose(t, laneMsg("b", RoleNotice, "two", func(r *Request) { r.After = target.ID }))
	rt.failFirst = true
	if res := mustPass(t, l, view(), exec); !slices.Equal(res.Taken, []string{a.ID, b.ID}) {
		t.Fatalf("first attempt took %v, want [%s %s]", res.Taken, a.ID, b.ID)
	}
	key := h.get(t, a.ID).DeliveryKey

	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	fs.setFail(func(id string) bool { return id == target.ID })
	res := mustPass(t, l, view(), exec)
	if len(res.Taken) != 0 {
		t.Fatalf("replay with a member's after target unreadable took %v; the group must wait whole", res.Taken)
	}
	if !slices.ContainsFunc(res.Waits, func(w OpWait) bool { return w.ID == a.ID && w.Reason == WaitReplayGroup }) {
		t.Fatalf("head %s should wait %s: %+v", a.ID, WaitReplayGroup, res.Waits)
	}
	unreadable := slices.IndexFunc(res.Waits, func(w OpWait) bool { return w.ID == b.ID && w.Reason == WaitAfterUnreadable })
	if unreadable < 0 || res.Waits[unreadable].Until.IsZero() || res.Next.IsZero() {
		t.Fatalf("member %s should wait %s with a retry time that sets Next: waits %+v next %v", b.ID, WaitAfterUnreadable, res.Waits, res.Next)
	}

	fs.setFail(nil)
	if res := mustPass(t, l, view(), exec); !slices.Equal(res.Taken, []string{a.ID, b.ID}) {
		t.Fatalf("replay took %v, want the whole group [%s %s]", res.Taken, a.ID, b.ID)
	}
	if got := h.get(t, b.ID).DeliveryKey; got != key {
		t.Fatalf("replay key %s, want the group's %s", got, key)
	}
	assertShownIfSucceeded(t, h, rt, a.ID, b.ID)
}

// A rider whose group's starter the rules keep out of the class (its after
// target cannot be read on this pass) still waits for it, on any runtime:
// the starter has not closed, so the rider is not alone in its group.
func TestLaneRiderReplayWaitsForAStarterOutsideTheClass(t *testing.T) {
	for _, dedup := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "dedup"}[dedup], func(t *testing.T) {
			p := newPlanner(t)
			key := "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa:3"
			target := "gc-op-bbbbbbbbbbbbbbbbbbbbbbbbbb"
			released := func(op *Op) {
				op.Status, op.Attempt, op.DeliveryKey, op.MaybeDelivered = StatusAccepted, 1, key, 1
			}
			rider := p.op(laneMsg("r", RoleNotice, "x", withWhen(WhenAfterTurn)), released)
			starter := p.op(laneMsg("s", RolePrompt, "y", func(r *Request) { r.After = target }), released)
			snap := snapOf(rider, starter)
			snap.after[target] = afterTarget{state: afterUnreadable, err: errFlakyRead}
			now := t0.Add(time.Hour)
			rp := planRules(snap, view(), now, "lane:ctl")
			probe := idleProbe()
			probe.Caps.Dedup = dedup
			tp, _ := planTake(rp, probe, view(), now)
			if tp.take != nil {
				t.Fatalf("took %v without its starter", takenOf(tp))
			}
			if w := waitsOf(append(rp.waits, tp.waits...))[starter.ID]; w != WaitAfterUnreadable {
				t.Fatalf("starter wait %q", w)
			}
		})
	}
}
