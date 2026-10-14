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
	mustPass(t, l, view(), exec) // :1 may have landed
	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	exec.deliver = func(Delivery) DeliveryResult { return Rejected{KeyRejected: true, Reason: "nudge_rejected"} }
	mustPass(t, l, view(), exec) // :1 is refused: the next send re-keys
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
