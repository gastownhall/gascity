package opjournal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// respondExec is a fake executor whose runtime shows pending, with respond
// capability.
func respondExec(h *laneHarness, pending string) *fakeExec {
	exec := newFakeExec(h.clk)
	exec.probe.Caps.Respond, exec.probe.Pending = true, pending
	return exec
}

// resolvedFact reads the interaction.resolved of occurrence occ in
// generation 1 of gc-1.
func resolvedFact(t *testing.T, h *laneHarness, occ string) (Fact, bool) {
	t.Helper()
	f, found, err := h.store.readFact(interactionFact(FactInteractionResolved, "gc-1", 1, occ))
	if err != nil {
		t.Fatalf("reading resolved %s: %v", occ, err)
	}
	return f, found
}

// Two identical tmux prompts in one generation share a request id (a
// content hash). Each raising is its own occurrence, so each gets its own
// respond op and its own answer; the first answer is never replayed to the
// second prompt.
func TestLaneTwoIdenticalPromptsGetTwoAnswers(t *testing.T) {
	h := newLaneHarness(t)
	exec := respondExec(h, "tmux-abc")
	l := h.lane(t, "ctl")

	first := h.raise(t, "tmux-abc")
	a := h.propose(t, laneRespond(t, first, "operator", RespondPayload{Action: "approve"}))
	if res := mustPass(t, l, view(), exec); res.Done != DoneAccepted || len(exec.responds) != 1 {
		t.Fatalf("first pass %+v", res)
	}
	if f, ok := resolvedFact(t, h, "tmux-abc#1"); !ok || f.Basis != FactBasisLane || len(f.CauseOps) != 1 || f.CauseOps[0] != a.ID {
		t.Fatalf("first raising resolved %v: %+v", ok, f)
	}

	// The same prompt shows again: a new raising, a new key, a new op.
	h.clk.Advance(time.Second)
	second := h.raise(t, "tmux-abc")
	if second.Interaction != "tmux-abc#2" {
		t.Fatalf("second raising is %q", second.Interaction)
	}
	req := laneRespond(t, second, "operator", RespondPayload{Action: "deny"})
	b, created, err := h.store.Propose(req)
	if err != nil || !created || b.ID == a.ID {
		t.Fatalf("second answer = %s, created %v, %v (first %s)", b.ID, created, err, a.ID)
	}
	if res := mustPass(t, l, view(), exec); res.Done != DoneAccepted || len(exec.responds) != 2 {
		t.Fatalf("second pass %+v", res)
	}
	c := exec.responds[1]
	if c.Interaction != "tmux-abc#2" || c.RequestID != "tmux-abc" || c.Response.Action != "deny" || c.Op.ID != b.ID {
		t.Fatalf("second respond call %+v", c)
	}
	if got := h.get(t, b.ID); got.Outcome.Arm != ArmSucceeded {
		t.Fatalf("second op %+v", got.Outcome)
	}
}

// The lane records the raising a probe shows, so the occurrence exists for a
// producer to answer even before any respond op does.
func TestLanePassRecordsTheRaisingItSees(t *testing.T) {
	h := newLaneHarness(t)
	msg := h.propose(t, laneMsg("m", RolePrompt, "hello"))
	exec := respondExec(h, "tmux-abc")
	res := mustPass(t, h.lane(t, "ctl"), view(), exec)
	if waitsOf(res.Waits)[msg.ID] != WaitPendingInteraction {
		t.Fatalf("message waits %v", res.Waits)
	}
	got, found, err := h.store.LatestRaising("gc-1", 1, "tmux-abc")
	if err != nil || !found || got.Raised.Interaction != "tmux-abc#1" || got.Resolved || got.Raised.Basis != FactBasisObserved {
		t.Fatalf("LatestRaising = %+v, found %v, %v", got, found, err)
	}
	// Another look at the same prompt is the same raising.
	mustPass(t, h.lane(t, "ctl"), view(), exec)
	if again, _, _ := h.store.LatestRaising("gc-1", 1, "tmux-abc"); again.Raised.ID != got.Raised.ID {
		t.Fatalf("a second look raised %+v", again.Raised)
	}
	// A look that failed, or a probe of another incarnation, records
	// nothing.
	for _, mod := range []func(*Probe){
		func(p *Probe) { p.Pending = PendingUnknown },
		func(p *Probe) { p.Pending, p.Fence = "tmux-def", Fence{Conversation: "gc-1", Generation: 2, Epoch: 1} },
		func(p *Probe) { p.Pending, p.Running = "tmux-def", false },
		func(p *Probe) { p.Pending, p.Available = "tmux-def", false },
		func(p *Probe) { p.Pending, p.Caps.Live = "tmux-def", false },
	} {
		e := respondExec(h, "")
		mod(&e.probe)
		mustPass(t, h.lane(t, "ctl"), view(), e)
		for _, req := range []string{PendingUnknown, "tmux-def"} {
			if _, found, _ := h.store.LatestRaising("gc-1", 1, req); found {
				t.Fatalf("probe %+v raised %q", e.probe, req)
			}
		}
	}
}

// A respond whose occurrence is over is stale: skipped{stale}, with nothing
// sent. A raising the probe shows gone gets a cleared interaction.resolved,
// so the next identical prompt is a new occurrence.
func TestLaneRespondStaleCases(t *testing.T) {
	cases := []struct {
		name    string
		pending string
		// setup runs after the op is proposed.
		setup   func(t *testing.T, h *laneHarness, raised Fact)
		cleared bool
	}{
		{"prompt gone", "", nil, true},
		{"another prompt shows", "tmux-other", nil, true},
		{"already resolved", "tmux-abc", func(t *testing.T, h *laneHarness, raised Fact) {
			if _, err := h.store.RecordFact(Fact{Kind: FactInteractionResolved, Conversation: "gc-1", Generation: 1, Interaction: raised.Interaction, Basis: FactBasisLane, At: t0}); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"re-raised since", "tmux-abc", func(t *testing.T, h *laneHarness, raised Fact) {
			if _, err := h.store.RecordFact(Fact{Kind: FactInteractionResolved, Conversation: "gc-1", Generation: 1, Interaction: raised.Interaction, Basis: FactBasisObserved, At: t0}); err != nil {
				t.Fatal(err)
			}
			h.raise(t, "tmux-abc")
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newLaneHarness(t)
			raised := h.raise(t, "tmux-abc")
			op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
			if c.setup != nil {
				c.setup(t, h, raised)
			}
			exec := respondExec(h, c.pending)
			res := mustPass(t, h.lane(t, "ctl"), view(), exec)
			if res.Took != TakeNone || len(exec.responds) != 0 {
				t.Fatalf("pass %+v sent %d responds", res, len(exec.responds))
			}
			if got := h.get(t, op.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
				t.Fatalf("op %+v", got.Outcome)
			}
			f, ok := resolvedFact(t, h, raised.Interaction)
			if !ok {
				t.Fatal("the stale occurrence is not resolved")
			}
			if cleared := f.Reason == ResolvedCleared && f.Basis == FactBasisObserved; cleared != c.cleared {
				t.Fatalf("resolved %+v, want cleared %v", f, c.cleared)
			}
			if c.pending == "tmux-abc" || c.pending == "" {
				return
			}
			if _, found, _ := h.store.LatestRaising("gc-1", 1, c.pending); !found {
				t.Fatalf("the prompt the probe shows (%s) was not raised", c.pending)
			}
		})
	}
}

// A respond naming an occurrence that was never raised is stale, even while
// its request id shows: a later raising number, or a request never seen.
func TestLaneRespondToAnUnraisedOccurrenceIsStale(t *testing.T) {
	h := newLaneHarness(t)
	ahead := h.propose(t, respondReq("tmux-abc#3", "gc", RespondPayload{Action: "approve"}))
	unseen := h.propose(t, respondReq("tmux-def#1", "gc", RespondPayload{Action: "approve"}))
	h.raise(t, "tmux-abc")
	exec := respondExec(h, "tmux-abc")
	mustPass(t, h.lane(t, "ctl"), view(), exec)
	for _, id := range []string{ahead.ID, unseen.ID} {
		if got := h.get(t, id); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
			t.Fatalf("op %s %+v", id, got.Outcome)
		}
	}
	if len(exec.responds) != 0 {
		t.Fatalf("%d responds sent", len(exec.responds))
	}
	// Ending an occurrence never raised would resolve its raising before
	// it happens.
	for _, occ := range []string{"tmux-abc#3", "tmux-def#1"} {
		if f, ok := resolvedFact(t, h, occ); ok {
			t.Fatalf("the unraised occurrence %s was resolved: %+v", occ, f)
		}
	}
}

// Two principals answer one occurrence: two ops, since op ids hash the
// principal. The first answer wins; the second finds the occurrence resolved
// and closes skipped{stale} without reaching the runtime.
func TestLaneRespondFirstAnswerWinsAcrossPrincipals(t *testing.T) {
	h := newLaneHarness(t)
	raised := h.raise(t, "tmux-abc")
	first := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	h.clk.Advance(time.Millisecond)
	second := h.propose(t, laneRespond(t, raised, "conversation:gc-9", RespondPayload{Action: "deny"}))
	exec := respondExec(h, "tmux-abc")
	l := h.lane(t, "ctl")
	if res := mustPass(t, l, view(), exec); res.Done != DoneAccepted || res.Taken[0] != first.ID || !res.More {
		t.Fatalf("first pass %+v", res)
	}
	mustPass(t, l, view(), exec)
	if len(exec.responds) != 1 {
		t.Fatalf("%d responds sent", len(exec.responds))
	}
	if got := h.get(t, second.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
		t.Fatalf("second answer %+v", got.Outcome)
	}
}

// A look that failed, or an occurrence the store could not read, leaves the
// respond waiting on a timer: nothing closes on an observation that was not
// made.
func TestLaneRespondWaitsWhenItCannotLook(t *testing.T) {
	t.Run("pending unknown", func(t *testing.T) {
		h := newLaneHarness(t)
		op := h.propose(t, laneRespond(t, h.raise(t, "tmux-abc"), "operator", RespondPayload{Action: "approve"}))
		exec := respondExec(h, PendingUnknown)
		res := mustPass(t, h.lane(t, "ctl"), view(), exec)
		if waitsOf(res.Waits)[op.ID] != WaitInteractionUnknown || res.Next.IsZero() || !res.Next.After(h.clk.Now()) {
			t.Fatalf("pass %+v", res)
		}
		if got := h.get(t, op.ID); got.Status == StatusClosed {
			t.Fatalf("op closed %+v", got.Outcome)
		}
	})
	t.Run("occurrence unreadable", func(t *testing.T) {
		// The pass that cannot read the raisings fails; nothing closes and
		// nothing is sent.
		h, fs := newLaneHarnessFlaky(t)
		raised := h.raise(t, "tmux-abc")
		op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		resolvedID, _ := FactID("gc", interactionFact(FactInteractionResolved, "gc-1", 1, raised.Interaction))
		fs.setFail(func(id string) bool { return id == resolvedID })
		exec := respondExec(h, "tmux-abc")
		l := h.lane(t, "ctl")
		if _, err := l.Pass(context.Background(), view(), exec); !errors.Is(err, errFlakyRead) {
			t.Fatalf("pass = %v, want the read error", err)
		}
		if got := h.get(t, op.ID); got.Status == StatusClosed || len(exec.responds) != 0 {
			t.Fatalf("op %s %+v after %d responds", got.Status, got.Outcome, len(exec.responds))
		}
		fs.setFail(nil)
		if res := mustPass(t, l, view(), exec); res.Done != DoneAccepted {
			t.Fatalf("pass after the store recovered %+v", res)
		}
	})
	t.Run("plan with an unreadable occurrence", func(t *testing.T) {
		p := newPlanner(t)
		op := p.op(respondReq("tmux-abc#1", "gc", RespondPayload{Action: "approve"}))
		now := t0.Add(time.Minute)
		rp := planRules(snapOf(op), view(), now, "lane:ctl")
		rp.raisings = map[string]raisingState{op.ID: {request: "tmux-abc", err: errFlakyRead}}
		probe := idleProbe()
		probe.Caps.Respond, probe.Pending = true, "tmux-abc"
		tp, err := planTake(rp, probe, view(), now)
		if err != nil {
			t.Fatal(err)
		}
		// The op is a minute old: its next look is an eighth of that away.
		if tp.take != nil || len(tp.closes) != 0 || waitsOf(tp.waits)[op.ID] != WaitInteractionUnknown || !tp.next.Equal(now.Add(time.Minute/interactionWaitDivisor)) {
			t.Fatalf("plan took %v, closes %v, waits %v, next %s", takenOf(tp), tp.closes, tp.waits, tp.next)
		}
	})
}

// The executor found the raising gone at the runtime (the probe raced the
// prompt): the op closes skipped{stale}, and Done records the cleared
// resolution in its batch.
func TestLaneRespondExecutorStaleResolvesTheOccurrence(t *testing.T) {
	h := newLaneHarness(t)
	raised := h.raise(t, "tmux-abc")
	op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	exec := respondExec(h, "tmux-abc")
	exec.respond = func(RespondCall) ControlResult { return Rejected{Arm: ArmSkipped, Reason: ReasonStale} }
	if res := mustPass(t, h.lane(t, "ctl"), view(), exec); res.Done != DoneRejected {
		t.Fatalf("pass %+v", res)
	}
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
		t.Fatalf("op %+v", got.Outcome)
	}
	if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedCleared {
		t.Fatalf("resolved %v %+v", ok, f)
	}
	if next := h.raise(t, "tmux-abc"); next.Interaction != "tmux-abc#2" {
		t.Fatalf("the next identical prompt is %q", next.Interaction)
	}
}

// A respond is at most once (finding #1 of the OJ-16 review): an attempt
// that may have answered is never sent again, since an identical prompt
// raised right after the answer has the same request id. It closes
// failed{ambiguous_delivery} and ends its occurrence, so the prompt still
// showing is the next occurrence and gets an answer of its own.
func TestLaneRespondIsNeverReplayed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// attempt runs the first attempt, which may have answered.
		attempt func(t *testing.T, h *laneHarness, exec *fakeExec)
	}{
		{"unknown", func(t *testing.T, h *laneHarness, exec *fakeExec) {
			exec.respond = func(RespondCall) ControlResult {
				return Unknown{Cause: errors.New("approval prompt did not clear within 10s of the answer")}
			}
			if res := mustPass(t, h.lane(t, "ctl"), view(), exec); res.Done != DoneReleased {
				t.Fatalf("first pass %+v", res)
			}
			exec.respond = nil
		}},
		{"lease expired", func(t *testing.T, h *laneHarness, exec *fakeExec) {
			a := h.lane(t, "ctl")
			a.hooks.at = crashAt(stageAfterEffect)
			if _, err := a.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
				t.Fatalf("pass = %v", err)
			}
			h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			first := h.raise(t, "tmux-abc")
			a := h.propose(t, laneRespond(t, first, "operator", RespondPayload{Action: "approve"}))
			h.clk.Advance(time.Millisecond)
			other := h.propose(t, laneRespond(t, first, "conversation:gc-9", RespondPayload{Action: "deny"}))
			// Two identical prompts back to back: the probe shows the same
			// request id throughout.
			exec := respondExec(h, "tmux-abc")
			tc.attempt(t, h, exec)
			l := h.lane(t, "cli")
			for range 3 {
				h.clk.Advance(ExecutorErrorBackoff + time.Second)
				mustPass(t, l, view(), exec)
			}
			if len(exec.responds) != 1 {
				t.Fatalf("%d responds sent; the first answer was replayed", len(exec.responds))
			}
			if got := h.get(t, a.ID); got.Outcome.Arm != ArmFailed || got.Outcome.Reason != ReasonAmbiguousDelivery || got.Attempt != 1 {
				t.Fatalf("first answer attempt %d %+v", got.Attempt, got.Outcome)
			}
			if got := h.get(t, other.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
				t.Fatalf("the other principal's answer to the ended occurrence %+v", got.Outcome)
			}
			f, ok := resolvedFact(t, h, first.Interaction)
			if !ok || f.Reason != ResolvedAmbiguous || f.Basis != FactBasisLane || !slices.Equal(f.CauseOps, []string{a.ID}) {
				t.Fatalf("first occurrence resolved %v %+v", ok, f)
			}
			// The pass recorded the prompt still showing as the next
			// occurrence; an answer to it is a new op, sent once.
			latest, found, err := h.store.LatestRaising("gc-1", 1, "tmux-abc")
			if err != nil || !found || latest.Raised.Interaction != "tmux-abc#2" || latest.Resolved {
				t.Fatalf("LatestRaising = %+v, %v, %v", latest, found, err)
			}
			b := h.propose(t, laneRespond(t, latest.Raised, "operator", RespondPayload{Action: "approve"}))
			if res := mustPass(t, l, view(), exec); res.Done != DoneAccepted || len(exec.responds) != 2 || exec.responds[1].Op.ID != b.ID || exec.responds[1].Interaction != "tmux-abc#2" {
				t.Fatalf("answer to the second prompt: pass %+v, responds %+v", res, exec.responds)
			}
		})
	}
}

// A respond that closes with nothing sent ends its occurrence too (finding
// #3: the first effective answer wins). A requester whose answer was
// refused, canceled, expired or unsupported can answer the prompt still
// showing, now the next occurrence, under a fresh key. A transient refusal
// keeps the occurrence: the op retries.
func TestLaneRespondWithNothingSentEndsTheOccurrence(t *testing.T) {
	cases := []struct {
		name string
		// run closes the op with nothing sent, or (ended false) releases it.
		run   func(t *testing.T, h *laneHarness, exec *fakeExec, op Op)
		want  Outcome
		ended bool
	}{
		{"refused action", func(t *testing.T, h *laneHarness, exec *fakeExec, _ Op) {
			exec.respond = func(RespondCall) ControlResult {
				return Rejected{Arm: ArmFailed, Reason: RefusedReasonPrefix + "action"}
			}
			mustPass(t, h.lane(t, "ctl"), view(), exec)
		}, Outcome{Arm: ArmFailed, Reason: RefusedReasonPrefix + "action"}, true},
		{"unsupported", func(t *testing.T, h *laneHarness, exec *fakeExec, _ Op) {
			exec.probe.Caps.Respond = false
			mustPass(t, h.lane(t, "ctl"), view(), exec)
		}, Outcome{Arm: ArmFailed, Reason: ReasonUnsupported}, true},
		// The live executor's shape for a session that takes no responses:
		// no respond capability, and so nothing pending it can name. That
		// is a capability answer, not a look showing the prompt gone.
		{"unsupported, nothing pending", func(t *testing.T, h *laneHarness, exec *fakeExec, _ Op) {
			exec.probe.Caps.Respond, exec.probe.Pending = false, ""
			mustPass(t, h.lane(t, "ctl"), view(), exec)
			exec.probe.Pending = "tmux-abc"
		}, Outcome{Arm: ArmFailed, Reason: ReasonUnsupported}, true},
		{"expired before start", func(t *testing.T, h *laneHarness, exec *fakeExec, op Op) {
			h.clk.Advance(op.StartBy.Sub(h.clk.Now()) + time.Second)
			mustPass(t, h.lane(t, "ctl"), view(), exec)
		}, Outcome{Arm: ArmFailed, Reason: ReasonExpiredBeforeStart}, true},
		{"canceled", func(t *testing.T, h *laneHarness, _ *fakeExec, op Op) {
			if res, _, err := h.store.Cancel(op.ID, "operator", "changed my mind"); err != nil || res != CancelClosed {
				t.Fatalf("Cancel = %s, %v", res, err)
			}
		}, Outcome{Arm: ArmCanceled}, true},
		{"poison", func(t *testing.T, h *laneHarness, exec *fakeExec, _ Op) {
			exec.respond = func(RespondCall) ControlResult { return NotDelivered{Cause: errors.New("refused")} }
			for range MaxFailures {
				mustPass(t, h.lane(t, "ctl"), view(), exec)
				h.clk.Advance(ExecutorErrorBackoff + time.Second)
			}
		}, Outcome{Arm: ArmFailed, Reason: ReasonPoison}, true},
		{"transient", func(t *testing.T, h *laneHarness, exec *fakeExec, _ Op) {
			exec.respond = func(RespondCall) ControlResult { return NotDelivered{Transient: true, Cause: errors.New("busy")} }
			mustPass(t, h.lane(t, "ctl"), view(), exec)
		}, Outcome{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newLaneHarness(t)
			raised := h.raise(t, "tmux-abc")
			op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
			exec := respondExec(h, "tmux-abc")
			c.run(t, h, exec, op)
			got := h.get(t, op.ID)
			f, ended := resolvedFact(t, h, raised.Interaction)
			if !c.ended {
				if got.Status == StatusClosed || ended {
					t.Fatalf("a transient refusal closed %+v or ended the occurrence %+v", got.Outcome, f)
				}
				return
			}
			if !got.Outcome.Equal(c.want) {
				t.Fatalf("op %+v, want %+v", got.Outcome, c.want)
			}
			if !ended || f.Reason != ResolvedUnanswered || f.Basis != FactBasisLane || !slices.Equal(f.CauseOps, []string{op.ID}) {
				t.Fatalf("occurrence ended %v %+v", ended, f)
			}
			// The same principal answers the prompt still showing.
			exec.probe.Caps.Respond, exec.respond = true, nil
			again := h.raise(t, "tmux-abc")
			if again.Interaction != "tmux-abc#2" {
				t.Fatalf("the prompt still showing is %s", again.Interaction)
			}
			fix, created, err := h.store.Propose(laneRespond(t, again, "operator", RespondPayload{Action: "deny"}))
			if err != nil || !created {
				t.Fatalf("corrected answer: created %v, %v", created, err)
			}
			sent := len(exec.responds)
			if res := mustPass(t, h.lane(t, "ctl"), view(), exec); res.Done != DoneAccepted || len(exec.responds) != sent+1 || exec.responds[sent].Op.ID != fix.ID {
				t.Fatalf("corrected answer pass %+v", res)
			}
		})
	}
}

// The end of an occurrence lands in the batch that closes its op: when the
// occurrence cannot be read, nothing closes, and the next pass closes the op
// with its end. Written after the close, a crash between would leave the
// occurrence open with its answers spent; written before, the end would
// stand for a close that lost its batch.
func TestLaneRespondEndLandsWithTheClose(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	raised := h.raise(t, "tmux-abc")
	op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	resolvedID, _ := FactID("gc", interactionFact(FactInteractionResolved, "gc-1", 1, raised.Interaction))
	fs.setFail(func(id string) bool { return id == resolvedID })
	h.clk.Advance(op.StartBy.Sub(h.clk.Now()) + time.Second)
	exec := respondExec(h, "tmux-abc")
	l := h.lane(t, "ctl")
	if _, err := l.Pass(context.Background(), view(), exec); !errors.Is(err, errFlakyRead) {
		t.Fatalf("pass = %v, want the occurrence's read error", err)
	}
	if got := h.get(t, op.ID); got.Status == StatusClosed {
		t.Fatalf("the op closed %+v before its occurrence ended", got.Outcome)
	}
	fs.setFail(nil)
	mustPass(t, l, view(), exec)
	if got := h.get(t, op.ID); got.Outcome.Reason != ReasonExpiredBeforeStart {
		t.Fatalf("op %+v", got.Outcome)
	}
	if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedUnanswered {
		t.Fatalf("occurrence ended %v %+v", ok, f)
	}
	if !slices.ContainsFunc(h.batcher.batches(), func(b beads.GuardedBatch) bool { return closesOp(b, op.ID) && createsRow(b, resolvedID) }) {
		t.Fatal("no batch carries both the close and the occurrence's end")
	}
}

// A request id the journal cannot store is no occurrence anyone can answer:
// the pass reads it as unknown instead of failing for every op of the
// conversation (finding #10). The look still showed another request than
// the respond's, so that respond closes skipped{stale}, while the message
// waits on the prompt showing.
func TestLaneUnrecordableRequestIDReadsUnknown(t *testing.T) {
	for _, req := range []string{strings.Repeat("x", maxClientKeyLen), "tmux-\x01", " tmux-abc"} {
		t.Run(fmt.Sprintf("%.12q", req), func(t *testing.T) {
			h := newLaneHarness(t)
			op := h.propose(t, laneRespond(t, h.raise(t, "tmux-abc"), "operator", RespondPayload{Action: "approve"}))
			msg := h.propose(t, laneMsg("m", RolePrompt, "hello"))
			exec := respondExec(h, req)
			res := mustPass(t, h.lane(t, "ctl"), view(), exec)
			waits := waitsOf(res.Waits)
			if waits[msg.ID] != WaitPendingInteraction || len(exec.responds) != 0 {
				t.Fatalf("pass %+v", res)
			}
			if got := h.get(t, op.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
				t.Fatalf("op %s %+v, want skipped{stale}", got.Status, got.Outcome)
			}
		})
	}
	// A store error recording the raising still fails the pass.
	h, fs := newLaneHarnessFlaky(t)
	h.propose(t, laneMsg("m", RolePrompt, "hello"))
	raisedID, _ := FactID("gc", interactionFact(FactInteractionRaised, "gc-1", 1, "tmux-abc#1"))
	fs.setFail(func(id string) bool { return id == raisedID })
	if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), respondExec(h, "tmux-abc")); !errors.Is(err, errFlakyRead) {
		t.Fatalf("pass = %v, want the store error", err)
	}
}

// A respond that cannot see its occurrence backs off with its age, capped,
// instead of a full pass every TransientBackoffBase for its hour (finding #9).
func TestInteractionRetryAtBacksOffWithAge(t *testing.T) {
	op := Op{CreatedAt: t0}
	for _, c := range []struct {
		age, want time.Duration
	}{
		{0, TransientBackoffBase},
		{TransientBackoffBase * interactionWaitDivisor, TransientBackoffBase},
		{10 * time.Minute, 10 * time.Minute / interactionWaitDivisor},
		{time.Hour, TransientBackoffMax},
	} {
		now := t0.Add(c.age)
		if got := interactionRetryAt(op, now).Sub(now); got != c.want {
			t.Errorf("age %s: retry in %s, want %s", c.age, got, c.want)
		}
	}
	// The age counts from not_before when that is later.
	late := Op{CreatedAt: t0, NotBefore: t0.Add(time.Hour)}
	if got := interactionRetryAt(late, t0.Add(time.Hour)).Sub(t0.Add(time.Hour)); got != TransientBackoffBase {
		t.Errorf("retry %s after not_before, want %s", got, TransientBackoffBase)
	}
}

// A taken respond whose payload or occurrence does not decode is refused
// before the executor runs. DecodeOp quarantines such a row, so only an op
// that skipped it reaches the effect.
func TestRespondEffectRefusesAnUndecodableOp(t *testing.T) {
	h := newLaneHarness(t)
	good := h.propose(t, laneRespond(t, h.raise(t, "tmux-abc"), "operator", RespondPayload{Action: "approve"}))
	for _, c := range []struct {
		name string
		mod  func(*Op)
		want string
	}{
		{"payload", func(op *Op) { op.Payload = `{"action":"approve","later":1}` }, ReasonRefusedPayload},
		{"interaction", func(op *Op) { op.Scope.Interaction = "tmux-abc" }, ReasonRefusedInteraction},
	} {
		t.Run(c.name, func(t *testing.T) {
			op := good
			c.mod(&op)
			if _, rej := respondCall(Fence{}, op); rej == nil || rej.Arm != ArmFailed || rej.Reason != c.want {
				t.Fatalf("respondCall = %+v, want failed{%s}", rej, c.want)
			}
			exec := respondExec(h, "tmux-abc")
			l := h.lane(t, "ctl")
			tb := &takenBatch{spec: &takeSpec{kind: TakeControl}, lease: Lease{Deadline: h.clk.Now().Add(time.Hour)}, ops: []Op{op}}
			er, _ := l.effect(context.Background(), exec, tb)
			if er.kind != resultRejected || er.rejected.Reason != c.want || len(exec.responds) != 0 {
				t.Fatalf("effect = %+v after %d responds", er, len(exec.responds))
			}
		})
	}
	call, rej := respondCall(Fence{Conversation: "gc-1"}, good)
	if rej != nil || call.RequestID != "tmux-abc" || call.Interaction != "tmux-abc#1" || call.Response.Action != "approve" || call.Op.ID != good.ID {
		t.Fatalf("respondCall(good) = %+v, %+v", call, rej)
	}
}

// A respond canceled while its attempt ran, whose lease then expired,
// closes canceled; the attempt may still have answered, so its occurrence
// ends ambiguous, not unanswered.
func TestLaneRespondCanceledAfterAnUnknownAttemptEndsAmbiguous(t *testing.T) {
	h := newLaneHarness(t)
	raised := h.raise(t, "tmux-abc")
	op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	exec := respondExec(h, "tmux-abc")
	a := h.lane(t, "ctl")
	a.hooks.at = crashAt(stageAfterEffect)
	if _, err := a.Pass(context.Background(), view(), exec); !errors.Is(err, errCrash) {
		t.Fatalf("pass = %v", err)
	}
	if res, _, err := h.store.Cancel(op.ID, "operator", ""); err != nil || res != CancelRequested {
		t.Fatalf("Cancel in flight = %s, %v", res, err)
	}
	h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
	mustPass(t, h.lane(t, "cli"), view(), exec)
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmCanceled || got.MaybeDelivered != 1 {
		t.Fatalf("op maybe %d %+v", got.MaybeDelivered, got.Outcome)
	}
	if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedAmbiguous {
		t.Fatalf("occurrence ended %v %+v", ok, f)
	}
	if len(exec.responds) != 1 {
		t.Fatalf("%d responds", len(exec.responds))
	}
}

// A look that shows a respond's prompt gone ends its occurrence, whatever
// the respond is doing (finding #1 of the second OJ-16 review): a runtime
// seen not running shows no prompt, and a respond in retry backoff is not a
// candidate, yet an identical prompt raised after either is another
// occurrence and must not get the old answer.
func TestLaneRespondPromptSeenGoneEndsTheOccurrence(t *testing.T) {
	t.Run("runtime not running", func(t *testing.T) {
		h := newLaneHarness(t)
		first := h.raise(t, "tmux-abc")
		op := h.propose(t, laneRespond(t, first, "operator", RespondPayload{Action: "approve"}))
		exec := respondExec(h, "")
		exec.probe.Running = false
		l := h.lane(t, "ctl")
		res := mustPass(t, l, view(), exec)
		if res.Took != TakeNone || len(exec.responds) != 0 {
			t.Fatalf("pass over a stopped runtime %+v", res)
		}
		if got := h.get(t, op.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
			t.Fatalf("op %+v, want skipped{stale}", got.Outcome)
		}
		if f, ok := resolvedFact(t, h, first.Interaction); !ok || f.Reason != ResolvedCleared || f.Basis != FactBasisObserved {
			t.Fatalf("occurrence ended %v %+v", ok, f)
		}
		// The runtime restarts in the same generation and re-raises the
		// identical prompt: another occurrence, which the old answer never
		// reaches.
		h.clk.Advance(5 * time.Second)
		exec.probe.Running, exec.probe.Pending = true, "tmux-abc"
		mustPass(t, l, view(), exec)
		if len(exec.responds) != 0 {
			t.Fatalf("the answer to %s reached the re-raised prompt: %+v", first.Interaction, exec.responds)
		}
		if next := h.raise(t, "tmux-abc"); next.Interaction != "tmux-abc#2" {
			t.Fatalf("the re-raised prompt is %s", next.Interaction)
		}
	})
	t.Run("respond in retry backoff", func(t *testing.T) {
		h := newLaneHarness(t)
		first := h.raise(t, "tmux-abc")
		op := h.propose(t, laneRespond(t, first, "operator", RespondPayload{Action: "approve"}))
		exec := respondExec(h, "tmux-abc")
		exec.respond = func(RespondCall) ControlResult {
			return NotDelivered{Transient: true, Cause: errors.New("fence miss before any key")}
		}
		l := h.lane(t, "ctl")
		if res := mustPass(t, l, view(), exec); res.Done != DoneReleased {
			t.Fatalf("first pass %+v", res)
		}
		exec.respond = nil
		// A message makes the next pass probe while the respond backs off:
		// the prompt is gone (a human answered it at the terminal).
		h.clk.Advance(500 * time.Millisecond)
		msg := h.propose(t, laneMsg("m", RolePrompt, "hello"))
		exec.probe.Pending = ""
		res := mustPass(t, l, view(), exec)
		if waitsOf(res.Waits)[op.ID] != WaitRetryBackoff || !slices.Equal(res.Taken, []string{msg.ID}) {
			t.Fatalf("pass with the prompt gone %+v", res)
		}
		if f, ok := resolvedFact(t, h, first.Interaction); !ok || f.Reason != ResolvedCleared {
			t.Fatalf("occurrence ended %v %+v", ok, f)
		}
		// The identical prompt shows again and the backoff passes.
		h.clk.Advance(5 * time.Second)
		exec.probe.Pending = "tmux-abc"
		mustPass(t, l, view(), exec)
		if len(exec.responds) != 1 {
			t.Fatalf("%d responds sent; the old answer reached the re-raised prompt", len(exec.responds))
		}
		if got := h.get(t, op.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
			t.Fatalf("op %+v, want skipped{stale}", got.Outcome)
		}
	})
	t.Run("prompt still showing", func(t *testing.T) {
		// The same look with the respond's request still pending ends
		// nothing: the backing-off respond answers it after the backoff.
		h := newLaneHarness(t)
		first := h.raise(t, "tmux-abc")
		op := h.propose(t, laneRespond(t, first, "operator", RespondPayload{Action: "approve"}))
		exec := respondExec(h, "tmux-abc")
		exec.respond = func(RespondCall) ControlResult { return NotDelivered{Transient: true, Cause: errors.New("busy")} }
		l := h.lane(t, "ctl")
		mustPass(t, l, view(), exec)
		exec.respond = nil
		h.propose(t, laneMsg("m", RolePrompt, "hello"))
		mustPass(t, l, view(), exec)
		if _, ok := resolvedFact(t, h, first.Interaction); ok {
			t.Fatal("a look that shows the prompt ended its occurrence")
		}
		h.clk.Advance(5 * time.Second)
		mustPass(t, l, view(), exec)
		if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded || len(exec.responds) != 2 {
			t.Fatalf("op %+v after %d responds", got.Outcome, len(exec.responds))
		}
	})
	t.Run("a look that is not definitive", func(t *testing.T) {
		for _, mod := range []func(*Probe){
			func(p *Probe) { p.Pending = PendingUnknown },
			func(p *Probe) { p.Fence = Fence{Conversation: "gc-1", Generation: 2, Epoch: 1} },
			func(p *Probe) { p.Available = false },
			func(p *Probe) { p.Caps.Live = false },
		} {
			h := newLaneHarness(t)
			first := h.raise(t, "tmux-abc")
			h.propose(t, laneRespond(t, first, "operator", RespondPayload{Action: "approve"}))
			exec := respondExec(h, "")
			mod(&exec.probe)
			if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec); err != nil {
				t.Fatalf("probe %+v: %v", exec.probe, err)
			}
			if f, ok := resolvedFact(t, h, first.Interaction); ok {
				t.Fatalf("probe %+v ended the occurrence %+v", exec.probe, f)
			}
		}
	})
}

// An answer that sends nothing ends its occurrence only when no other
// principal's answer to it is open (finding #2 of the second review): a
// refused or canceled answer leaves the prompt showing for the next one.
func TestLaneRespondNothingSentLeavesTheOccurrenceToAnotherAnswer(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		bad := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "bogus"}))
		h.clk.Advance(time.Millisecond)
		good := h.propose(t, laneRespond(t, raised, "conversation:gc-9", RespondPayload{Action: "approve"}))
		exec := respondExec(h, "tmux-abc")
		exec.respond = func(c RespondCall) ControlResult {
			if c.Response.Action == "bogus" {
				return Rejected{Arm: ArmFailed, Reason: RefusedReasonPrefix + "action"}
			}
			return Settled{Via: "live"}
		}
		l := h.lane(t, "ctl")
		mustPass(t, l, view(), exec)
		if got := h.get(t, bad.ID); got.Outcome.Reason != RefusedReasonPrefix+"action" {
			t.Fatalf("refused answer %+v", got.Outcome)
		}
		if _, ok := resolvedFact(t, h, raised.Interaction); ok {
			t.Fatal("the refused answer ended the occurrence another answer still names")
		}
		mustPass(t, l, view(), exec)
		if got := h.get(t, good.ID); got.Outcome.Arm != ArmSucceeded || len(exec.responds) != 2 {
			t.Fatalf("the other principal's answer %+v after %d responds", got.Outcome, len(exec.responds))
		}
		if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != "" || !slices.Equal(f.CauseOps, []string{good.ID}) {
			t.Fatalf("occurrence ended %v %+v, want the accepted answer's", ok, f)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		a := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		b := h.propose(t, laneRespond(t, raised, "conversation:gc-9", RespondPayload{Action: "deny"}))
		if res, _, err := h.store.Cancel(a.ID, "operator", ""); err != nil || res != CancelClosed {
			t.Fatalf("Cancel = %s, %v", res, err)
		}
		if _, ok := resolvedFact(t, h, raised.Interaction); ok {
			t.Fatal("canceling one answer ended the occurrence another names")
		}
		// The last answer to go with nothing sent ends it.
		if res, _, err := h.store.Cancel(b.ID, "conversation:gc-9", ""); err != nil || res != CancelClosed {
			t.Fatalf("Cancel = %s, %v", res, err)
		}
		if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedUnanswered || !slices.Equal(f.CauseOps, []string{b.ID}) {
			t.Fatalf("occurrence ended %v %+v", ok, f)
		}
	})
	t.Run("an answer to another occurrence does not hold it", func(t *testing.T) {
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		a := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		h.propose(t, laneRespond(t, h.raise(t, "tmux-xyz"), "conversation:gc-9", RespondPayload{Action: "deny"}))
		if res, _, err := h.store.Cancel(a.ID, "operator", ""); err != nil || res != CancelClosed {
			t.Fatalf("Cancel = %s, %v", res, err)
		}
		if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedUnanswered || !slices.Equal(f.CauseOps, []string{a.ID}) {
			t.Fatalf("occurrence ended %v %+v", ok, f)
		}
	})
	t.Run("both close in one pass", func(t *testing.T) {
		// Two answers expire together: neither is left open, so the
		// occurrence ends.
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		a := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		h.propose(t, laneRespond(t, raised, "conversation:gc-9", RespondPayload{Action: "deny"}))
		h.clk.Advance(a.StartBy.Sub(h.clk.Now()) + time.Second)
		mustPass(t, h.lane(t, "ctl"), view(), respondExec(h, "tmux-abc"))
		if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedUnanswered {
			t.Fatalf("occurrence ended %v %+v", ok, f)
		}
	})
}

// raceWriter runs race once, just before the op store's first fenced write
// to op id: between the read that write decides on and the write.
type raceWriter struct {
	beads.ConditionalWriter
	id   string
	race func()
}

func (w *raceWriter) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if race := w.race; race != nil && id == w.id {
		w.race = nil
		race()
	}
	return w.ConditionalWriter.UpdateIfMatch(id, rev, opts)
}

// closesOp reports whether batch closes op id.
func closesOp(batch beads.GuardedBatch, id string) bool {
	return slices.ContainsFunc(batch.Items, func(it beads.GuardedItem) bool {
		u := it.Update
		return u != nil && u.ID == id && u.Opts.Status != nil && *u.Opts.Status == beadStatusClosed
	})
}

// Two answers that send nothing and close at once each read the other open
// before their own close lands, so each withholds the occurrence's end for
// the other. Each re-checks once its close has landed: the later reads the
// other closed and ends the occurrence, and the prompt still showing is the
// next occurrence, answerable under fresh keys.
func TestNothingSentAnswersClosingAtOnceEndTheOccurrence(t *testing.T) {
	cancel := func(t *testing.T, h *laneHarness, op Op, principal string) {
		t.Helper()
		if res, _, err := h.store.Cancel(op.ID, principal, ""); err != nil || res != CancelClosed {
			t.Errorf("Cancel(%s) = %s, %v", op.ID, res, err)
		}
	}
	// cancelFirst cancels b just before the lane's batch closing a lands.
	cancelFirst := func(t *testing.T, h *laneHarness, a, b Op) {
		fired := false
		h.batcher.setFault(func(_ int, batch beads.GuardedBatch) (bool, error) {
			if !fired && closesOp(batch, a.ID) {
				fired = true
				cancel(t, h, b, "conversation:gc-9")
			}
			return false, nil
		})
	}
	ended := func(t *testing.T, h *laneHarness, raised Fact, ops ...Op) {
		t.Helper()
		for _, op := range ops {
			if got := h.get(t, op.ID); got.Status != StatusClosed {
				t.Fatalf("op %s %s", op.ID, got.Status)
			}
		}
		if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedUnanswered {
			t.Fatalf("occurrence ended %v %+v, want unanswered", ok, f)
		}
		if next := h.raise(t, "tmux-abc"); next.Interaction != "tmux-abc#2" {
			t.Fatalf("the prompt still showing raised %s, want tmux-abc#2", next.Interaction)
		}
	}
	t.Run("cancel and cancel", func(t *testing.T) {
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		a := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		b := h.propose(t, laneRespond(t, raised, "conversation:gc-9", RespondPayload{Action: "deny"}))
		h.store.writer = &raceWriter{ConditionalWriter: h.store.writer, id: a.ID, race: func() {
			cancel(t, h, b, "conversation:gc-9")
		}}
		cancel(t, h, a, "operator")
		ended(t, h, raised, a, b)
	})
	t.Run("lane rule close and cancel", func(t *testing.T) {
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		a := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		h.clk.Advance(2 * time.Second)
		b := h.propose(t, laneRespond(t, raised, "conversation:gc-9", RespondPayload{Action: "deny"}))
		// a's start_by passes; b's has not.
		h.clk.Advance(a.StartBy.Sub(h.clk.Now()) + time.Second)
		cancelFirst(t, h, a, b)
		exec := respondExec(h, "tmux-abc")
		mustPass(t, h.lane(t, "ctl"), view(), exec)
		if got := h.get(t, a.ID); got.Outcome.Reason != ReasonExpiredBeforeStart || len(exec.responds) != 0 {
			t.Fatalf("op a %+v after %d responds", got.Outcome, len(exec.responds))
		}
		ended(t, h, raised, a, b)
	})
	t.Run("lane done and cancel", func(t *testing.T) {
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		a := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "bogus"}))
		h.clk.Advance(time.Millisecond)
		b := h.propose(t, laneRespond(t, raised, "conversation:gc-9", RespondPayload{Action: "approve"}))
		exec := respondExec(h, "tmux-abc")
		exec.respond = func(RespondCall) ControlResult {
			return Rejected{Arm: ArmFailed, Reason: RefusedReasonPrefix + "action"}
		}
		cancelFirst(t, h, a, b)
		mustPass(t, h.lane(t, "ctl"), view(), exec)
		if got := h.get(t, a.ID); got.Outcome.Reason != RefusedReasonPrefix+"action" || len(exec.responds) != 1 {
			t.Fatalf("op a %+v after %d responds", got.Outcome, len(exec.responds))
		}
		ended(t, h, raised, a, b)
	})
}

// When the other answers cannot be read once its close has landed, a
// nothing-sent close ends the occurrence anyway and says so: left open, the
// occurrence's answers would be spent and the prompt unanswerable.
func TestNothingSentCloseEndsTheOccurrenceWhenTheOtherAnswersCannotBeRead(t *testing.T) {
	h := newLaneHarness(t)
	raised := h.raise(t, "tmux-abc")
	a := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	h.propose(t, laneRespond(t, raised, "conversation:gc-9", RespondPayload{Action: "deny"}))
	// The read before the close sees the other answer open; the one after
	// fails.
	h.store.writer = &raceWriter{ConditionalWriter: h.store.writer, id: a.ID, race: func() {
		h.store.store.Store = failingListStore{h.store.store.Store}
	}}
	if _, _, err := h.store.Cancel(a.ID, "operator", ""); err == nil || !strings.Contains(err.Error(), "dolt down") {
		t.Fatalf("Cancel = %v, want the failed read", err)
	}
	if got := h.get(t, a.ID); got.Status != StatusClosed || got.Outcome.Arm != ArmCanceled {
		t.Fatalf("op %s %s %+v", a.ID, got.Status, got.Outcome)
	}
	if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedUnanswered || !slices.Equal(f.CauseOps, []string{a.ID}) {
		t.Fatalf("occurrence ended %v %+v, want unanswered by %s", ok, f, a.ID)
	}
}

// A cancel whose bead repair (Store.closeBead) fails once its close has
// landed still re-checks its occurrence's end, and says why it failed: two
// cancels closing at once each withhold the end for the other, and the later
// of them ends the occurrence.
func TestCancelRechecksTheOccurrenceEndWhenItsBeadRepairFails(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	raised := h.raise(t, "tmux-abc")
	a := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	b := h.propose(t, laneRespond(t, raised, "conversation:gc-9", RespondPayload{Action: "deny"}))
	// b's cancel closes between a's read of b and a's close, and a's bead
	// then cannot be read back.
	h.store.writer = &raceWriter{ConditionalWriter: h.store.writer, id: a.ID, race: func() {
		if res, _, err := h.store.Cancel(b.ID, "conversation:gc-9", ""); err != nil || res != CancelClosed {
			t.Errorf("Cancel(%s) = %s, %v", b.ID, res, err)
		}
		fs.setFail(func(id string) bool { return id == a.ID })
	}}
	if _, _, err := h.store.Cancel(a.ID, "operator", ""); !errors.Is(err, errFlakyRead) {
		t.Fatalf("Cancel = %v, want the failed repair read", err)
	}
	fs.setFail(nil)
	if got := h.get(t, a.ID); got.Status != StatusClosed || got.Outcome.Arm != ArmCanceled {
		t.Fatalf("op %s %s %+v", a.ID, got.Status, got.Outcome)
	}
	if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedUnanswered || !slices.Equal(f.CauseOps, []string{a.ID}) {
		t.Fatalf("occurrence ended %v %+v, want unanswered by %s", ok, f, a.ID)
	}
}

// A respond closed outside the lane gets its occurrence's end from the next
// Cancel (CancelAlreadyTerminal) as a close of its own gets one: an answer's
// end as its outcome gives it, and a nothing-sent end even when the other
// answers cannot be read, the error saying so (Store.endClosed).
func TestCancelOfARespondClosedOutsideTheLaneEndsItsOccurrence(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		if _, err := h.store.Close(op.ID, Outcome{Arm: ArmSucceeded, Result: Result{Via: "live"}}, "operator"); err != nil {
			t.Fatal(err)
		}
		if res, _, err := h.store.Cancel(op.ID, "operator", ""); err != nil || res != CancelAlreadyTerminal {
			t.Fatalf("Cancel = %s, %v", res, err)
		}
		if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != "" || !slices.Equal(f.CauseOps, []string{op.ID}) {
			t.Fatalf("occurrence ended %v %+v, want answered by %s", ok, f, op.ID)
		}
	})
	t.Run("closed externally, the other answers unreadable", func(t *testing.T) {
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		if err := h.mem.Close(op.ID); err != nil {
			t.Fatal(err)
		}
		h.store.store.Store = failingListStore{h.store.store.Store}
		if _, _, err := h.store.Cancel(op.ID, "operator", ""); err == nil || !strings.Contains(err.Error(), "dolt down") {
			t.Fatalf("Cancel = %v, want the failed read", err)
		}
		if got := h.get(t, op.ID); got.Status != StatusClosed || got.Outcome.Reason != ReasonClosedExternally {
			t.Fatalf("op %s %s %+v", op.ID, got.Status, got.Outcome)
		}
		if f, ok := resolvedFact(t, h, raised.Interaction); !ok || f.Reason != ResolvedUnanswered || !slices.Equal(f.CauseOps, []string{op.ID}) {
			t.Fatalf("occurrence ended %v %+v, want unanswered by %s", ok, f, op.ID)
		}
	})
}

// Cancel writes a lane respond's end in the batch that closes it: when the
// occurrence cannot be read the op stays open and Cancel fails, so a retry,
// or the lane, closes it with its end.
func TestCancelRespondEndsTheOccurrenceWithTheClose(t *testing.T) {
	h, fs := newLaneHarnessFlaky(t)
	raised := h.raise(t, "tmux-abc")
	op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	fs.setFail(func(id string) bool { return id == raised.ID })
	if _, _, err := h.store.Cancel(op.ID, "operator", ""); !errors.Is(err, errFlakyRead) {
		t.Fatalf("Cancel = %v, want the read error", err)
	}
	fs.setFail(nil)
	if got := h.get(t, op.ID); got.Status == StatusClosed {
		t.Fatalf("op closed %+v while its occurrence is open", got.Outcome)
	}
	if res, _, err := h.store.Cancel(op.ID, "operator", ""); err != nil || res != CancelClosed {
		t.Fatalf("retried Cancel = %s, %v", res, err)
	}
	f, ok := resolvedFact(t, h, raised.Interaction)
	if !ok || f.Reason != ResolvedUnanswered {
		t.Fatalf("occurrence ended %v %+v", ok, f)
	}
	if !slices.ContainsFunc(h.batcher.batches(), func(b beads.GuardedBatch) bool { return closesOp(b, op.ID) && createsRow(b, f.ID) }) {
		t.Fatal("no batch carries both the cancel's close and the occurrence's end")
	}
	// The same principal's corrected answer is the next occurrence's.
	again := h.raise(t, "tmux-abc")
	if _, created, err := h.store.Propose(laneRespond(t, again, "operator", RespondPayload{Action: "deny"})); err != nil || !created {
		t.Fatalf("corrected answer: created %v, %v", created, err)
	}
}

// One undecodable interaction fact does not stall its conversation
// (finding #7 of the second review, DES-O1 §4.1): the respond that names it
// closes without an end, the other ops close, and so does the runtime row.
// A raising the lane cannot read is an unknown pending interaction.
func TestLaneCorruptInteractionFactDoesNotStallTheConversation(t *testing.T) {
	t.Run("closed conversation", func(t *testing.T) {
		h := newLaneHarness(t)
		// A Take stamps the runtime row, which the closed pass must close.
		h.propose(t, laneMsg("first", RolePrompt, "earlier"))
		if res := mustPass(t, h.lane(t, "ctl"), view(), respondExec(h, "")); res.Took != TakeContent {
			t.Fatalf("first pass %+v", res)
		}
		raised := h.raise(t, "tmux-abc")
		op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		msg := h.propose(t, laneMsg("m", RolePrompt, "hello"))
		if err := h.mem.SetMetadata(raised.ID, keyFactBasis, "bad basis!"); err != nil {
			t.Fatal(err)
		}
		v := view()
		v.Closed = true
		mustPass(t, h.lane(t, "ctl"), v, respondExec(h, ""))
		for _, id := range []string{op.ID, msg.ID} {
			if got := h.get(t, id); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonConversationClosed {
				t.Fatalf("op %s %s %+v", id, got.Status, got.Outcome)
			}
		}
		if !h.runtime(t).Closed {
			t.Fatal("the runtime row stayed open")
		}
	})
	t.Run("raising unreadable", func(t *testing.T) {
		h := newLaneHarness(t)
		raised := h.raise(t, "tmux-abc")
		op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
		msg := h.propose(t, laneMsg("m", RolePrompt, "hello"))
		if err := h.mem.SetMetadata(raised.ID, keyFactBasis, "bad basis!"); err != nil {
			t.Fatal(err)
		}
		exec := respondExec(h, "tmux-abc")
		res := mustPass(t, h.lane(t, "ctl"), view(), exec)
		waits := waitsOf(res.Waits)
		if waits[op.ID] != WaitInteractionUnknown || waits[msg.ID] != WaitPendingInteraction || len(exec.responds) != 0 {
			t.Fatalf("pass %+v", res)
		}
	})
}

// A look that shows another prompt ends the occurrence of a waiting respond
// even when the journal cannot record that prompt: its request id cannot be
// stored, or its raisings do not decode. The look was definitive, so the
// identical prompt raised later is another occurrence the old answer must
// not reach.
func TestLaneRespondUnrecordedOtherPromptEndsTheOccurrence(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, h *laneHarness) string{
		"unstorable request id": func(*testing.T, *laneHarness) string { return "tmux-\x01other" },
		"undecodable raising": func(t *testing.T, h *laneHarness) string {
			other := h.raise(t, "tmux-other")
			if err := h.mem.SetMetadata(other.ID, keyFactBasis, "bad basis!"); err != nil {
				t.Fatal(err)
			}
			return "tmux-other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLaneHarness(t)
			first := h.raise(t, "tmux-abc")
			shown := setup(t, h)
			op := h.propose(t, laneRespond(t, first, "operator", RespondPayload{Action: "approve"}))
			exec := respondExec(h, shown)
			l := h.lane(t, "ctl")
			mustPass(t, l, view(), exec)
			f, ended := resolvedFact(t, h, first.Interaction)
			if !ended || f.Reason != ResolvedCleared || f.Basis != FactBasisObserved {
				t.Fatalf("occurrence %s ended %v %+v, want cleared", first.Interaction, ended, f)
			}
			// The agent raises the identical first prompt again.
			h.clk.Advance(5 * time.Second)
			exec.probe.Pending = "tmux-abc"
			mustPass(t, l, view(), exec)
			if len(exec.responds) != 0 {
				t.Fatalf("the answer to %s reached the re-raised prompt", first.Interaction)
			}
			if got := h.get(t, op.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
				t.Fatalf("op %s %+v, want skipped{stale}", got.Status, got.Outcome)
			}
		})
	}
}

// A cancel recorded while a respond's effect runs turns Done's release (a
// transient refusal, nothing sent) into a canceled close on reconcile. That
// close ends the occurrence like any other, so the principal answers the
// prompt still showing under a fresh key.
func TestLaneRespondCanceledDuringTheEffectEndsTheOccurrence(t *testing.T) {
	h := newLaneHarness(t)
	raised := h.raise(t, "tmux-abc")
	op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	exec := respondExec(h, "tmux-abc")
	exec.onEffect = func() {
		if res, _, err := h.store.Cancel(op.ID, "operator", ""); err != nil || res != CancelRequested {
			t.Errorf("Cancel during the effect = %s, %v", res, err)
		}
	}
	exec.respond = func(RespondCall) ControlResult {
		return NotDelivered{Transient: true, Cause: errors.New("fence miss before any key")}
	}
	mustPass(t, h.lane(t, "ctl"), view(), exec)
	if got := h.get(t, op.ID); got.Status != StatusClosed || got.Outcome.Arm != ArmCanceled {
		t.Fatalf("op %s %+v, want closed canceled", got.Status, got.Outcome)
	}
	f, ended := resolvedFact(t, h, raised.Interaction)
	if !ended || f.Reason != ResolvedUnanswered || f.Basis != FactBasisLane || !slices.Equal(f.CauseOps, []string{op.ID}) {
		t.Fatalf("occurrence ended %v %+v", ended, f)
	}
	again := h.raise(t, "tmux-abc")
	if _, created, err := h.store.Propose(laneRespond(t, again, "operator", RespondPayload{Action: "deny"})); err != nil || !created {
		t.Fatalf("corrected answer to %s: created %v, %v", again.Interaction, created, err)
	}
}

// A respond whose raising does not decode, seen gone, does not stall its
// conversation: clearGone leaves that occurrence alone, the pass goes on,
// and an unrelated message is delivered.
func TestLaneCorruptRaisingSeenGoneDoesNotStall(t *testing.T) {
	h := newLaneHarness(t)
	raised := h.raise(t, "tmux-abc")
	h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	msg := h.propose(t, laneMsg("m", RolePrompt, "hello"))
	if err := h.mem.SetMetadata(raised.ID, keyFactBasis, "bad basis!"); err != nil {
		t.Fatal(err)
	}
	exec := respondExec(h, "")
	l := h.lane(t, "ctl")
	for i := range 3 {
		if _, err := l.Pass(context.Background(), view(), exec); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := h.get(t, msg.ID); got.Status != StatusClosed {
		t.Fatalf("the unrelated message op stays %s", got.Status)
	}
}

// createsRow reports whether batch creates row id.
func createsRow(batch beads.GuardedBatch, id string) bool {
	return slices.ContainsFunc(batch.Items, func(it beads.GuardedItem) bool { return it.Create != nil && it.Create.ID == id })
}

// takesOp reports whether batch takes op id: moves it to executing.
func takesOp(batch beads.GuardedBatch, id string) bool {
	return slices.ContainsFunc(batch.Items, func(it beads.GuardedItem) bool {
		u := it.Update
		return u != nil && u.ID == id && u.Opts.Metadata[keyStatus] == string(StatusExecuting)
	})
}

// Cancel decides on a respond as submitted, and the lane's Take lands before
// Cancel's write (finding #1 of the third OJ-16 review). The cancel then only
// records its request, and no end lands for the close that lost: the lane
// answers the occurrence still open, its end is the answer's, and a producer
// recording the prompt while the answer is in flight is handed that
// occurrence, not the next.
func TestCancelLosingItsWriteToTheTakeEndsNothing(t *testing.T) {
	h := newLaneHarness(t)
	raised := h.raise(t, "tmux-abc")
	op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	exec := respondExec(h, "tmux-abc")
	var during InteractionRaising
	exec.onEffect = func() {
		var err error
		if during, _, err = h.store.RecordRaising("gc-1", 1, "tmux-abc", FactBasisObserved, h.clk.Now()); err != nil {
			t.Errorf("RecordRaising while the answer is in flight: %v", err)
		}
	}
	// landTake applies the lane's Take, held at the batcher, just before
	// Cancel's write lands: its fenced update, or the batch that carries it.
	var take *beads.GuardedBatch
	landTake := func() {
		if take == nil {
			return
		}
		b := *take
		take = nil
		if _, err := h.mem.ApplyGuarded(context.Background(), b); err != nil {
			t.Errorf("landing the Take: %v", err)
		}
	}
	h.store.writer = &raceWriter{ConditionalWriter: h.store.writer, id: op.ID, race: landTake}
	var canceled CancelResult
	var cancelErr error
	fired := false
	h.batcher.setFault(func(_ int, b beads.GuardedBatch) (bool, error) {
		switch {
		case !fired && takesOp(b, op.ID):
			fired = true
			take = &b
			canceled, _, cancelErr = h.store.Cancel(op.ID, "operator", "")
			// The Take landed inside Cancel: the lane reads its batch as
			// unknown and resolves it by re-reading the lease.
			return false, errLinkDropped
		case take != nil && closesOp(b, op.ID):
			landTake()
		}
		return false, nil
	})
	res := mustPass(t, h.lane(t, "ctl"), view(), exec)
	if cancelErr != nil || canceled != CancelRequested {
		t.Fatalf("Cancel racing the Take = %s, %v; want requested", canceled, cancelErr)
	}
	if res.Done != DoneAccepted || len(exec.responds) != 1 {
		t.Fatalf("pass %+v after %d responds; want the answer sent once", res, len(exec.responds))
	}
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded {
		t.Fatalf("op %s %+v; want the answer's close", got.Status, got.Outcome)
	}
	f, ended := resolvedFact(t, h, raised.Interaction)
	if !ended || f.Reason != "" || f.Basis != FactBasisLane || !slices.Equal(f.CauseOps, []string{op.ID}) {
		t.Fatalf("occurrence ended %v %+v; want the answer's end", ended, f)
	}
	if during.Raised.Interaction != raised.Interaction || during.Resolved {
		t.Fatalf("a producer recording the prompt during the answer was handed %+v; want the open %s", during, raised.Interaction)
	}
}

// A lane's close of a respond loses its batch to another host's Take of it:
// the end that close gives lands with it or not at all, so the other host
// answers the occurrence still open, and its end is the answer's.
func TestLaneCloseLosingItsBatchToATakeEndsNothing(t *testing.T) {
	h := newLaneHarness(t)
	raised := h.raise(t, "tmux-abc")
	op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	// ctl's runtime says it takes no responses, so ctl closes the respond
	// failed{unsupported}; cli's shows the prompt and answers it.
	ctlExec := respondExec(h, "tmux-abc")
	ctlExec.probe.Caps.Respond = false
	cliExec := respondExec(h, "tmux-abc")
	cli := h.lane(t, "cli")
	fired := false
	h.batcher.setFault(func(_ int, b beads.GuardedBatch) (bool, error) {
		if !fired && closesOp(b, op.ID) {
			fired = true
			if res, err := cli.Pass(context.Background(), view(), cliExec); err != nil || res.Done != DoneAccepted {
				t.Errorf("cli's pass under ctl's close = %+v, %v; want the answer accepted", res, err)
			}
		}
		return false, nil
	})
	mustPass(t, h.lane(t, "ctl"), view(), ctlExec)
	if len(cliExec.responds) != 1 || len(ctlExec.responds) != 0 {
		t.Fatalf("cli sent %d responds, ctl %d; want cli's answer only", len(cliExec.responds), len(ctlExec.responds))
	}
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmSucceeded {
		t.Fatalf("op %s %+v; want the answer's close", got.Status, got.Outcome)
	}
	if f, ended := resolvedFact(t, h, raised.Interaction); !ended || f.Reason != "" || !slices.Equal(f.CauseOps, []string{op.ID}) {
		t.Fatalf("occurrence ended %v %+v; want the answer's end", ended, f)
	}
}

// A raising the lane saw ends once a later definitive look shows its prompt
// gone, even while no respond names it (finding #2 of the third OJ-16
// review): the requester shown the prompt answers after think time, keyed
// to that raising, as RespondKey prescribes. The identical prompt raised
// again is the next occurrence, and the late answer closes skipped{stale}
// instead of reaching it.
func TestLaneRaisingSeenGoneEndsWithNoRespondOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gone makes the probe show the prompt gone.
		gone func(p *Probe)
		// taken: a Take stamped the runtime row before the prompt showed.
		taken bool
	}{
		{"prompt gone", func(p *Probe) { p.Pending = "" }, false},
		{"another prompt", func(p *Probe) { p.Pending = "tmux-def" }, false},
		{"an unrecordable prompt", func(p *Probe) { p.Pending = "tmux-\x01def" }, false},
		{"runtime stopped", func(p *Probe) { p.Pending, p.Running = "", false }, false},
		{"prompt gone after a Take", func(p *Probe) { p.Pending = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			l := h.lane(t, "ctl")
			exec := respondExec(h, "")
			if tc.taken {
				h.propose(t, laneMsg("first", RoleNotice, "earlier"))
				if res := mustPass(t, l, view(), exec); res.Took != TakeContent {
					t.Fatalf("first pass %+v", res)
				}
			}
			// A message waiting on a busy runtime keeps the lane looking.
			msg := h.propose(t, laneMsg("m", RolePrompt, "hello"))
			shows := func(pending string) Probe {
				p := respondExec(h, pending).probe
				p.Boundary = TriNo
				return p
			}
			exec.probe = shows("tmux-abc")
			mustPass(t, l, view(), exec)
			first, found, err := h.store.LatestRaising("gc-1", 1, "tmux-abc")
			if err != nil || !found || first.Raised.Interaction != "tmux-abc#1" || first.Resolved {
				t.Fatalf("LatestRaising = %+v, found %v, %v", first, found, err)
			}
			// The prompt clears while the requester thinks.
			tc.gone(&exec.probe)
			mustPass(t, l, view(), exec)
			f, ended := resolvedFact(t, h, first.Raised.Interaction)
			if !ended || f.Reason != ResolvedCleared || f.Basis != FactBasisObserved {
				t.Fatalf("occurrence %s ended %v %+v; want cleared", first.Raised.Interaction, ended, f)
			}
			// The late answer to the first raising arrives as the
			// identical prompt shows again.
			late := h.propose(t, laneRespond(t, first.Raised, "operator", RespondPayload{Action: "approve"}))
			exec.probe = shows("tmux-abc")
			mustPass(t, l, view(), exec)
			if len(exec.responds) != 0 {
				t.Fatalf("the late answer to %s reached the re-raised prompt: %+v", first.Raised.Interaction, exec.responds)
			}
			if got := h.get(t, late.ID); got.Outcome.Arm != ArmSkipped || got.Outcome.Reason != ReasonStale {
				t.Fatalf("late answer %s %+v; want skipped{stale}", got.Status, got.Outcome)
			}
			latest, found, err := h.store.LatestRaising("gc-1", 1, "tmux-abc")
			if err != nil || !found || latest.Raised.Interaction != "tmux-abc#2" || latest.Resolved {
				t.Fatalf("the re-raised prompt is %+v, found %v, %v; want the open tmux-abc#2", latest, found, err)
			}
			if got := h.get(t, msg.ID); got.Status == StatusClosed {
				t.Fatalf("the message closed %+v while the runtime was busy", got.Outcome)
			}
		})
	}
}

// The lane keeps the occurrence its last definitive look saw pending in the
// runtime row's pending cell, and writes the row only when that changes: a
// look at the same prompt writes nothing, and a Take after a look that wrote
// the row guards on the row as written, so the pass probes once. A look that
// is not definitive (an unknown request, another incarnation, an unreachable
// runtime, no live control, no respond capability) leaves the cell and the
// raising as they are.
func TestLanePendingCellChangesOnlyOnADefinitiveLook(t *testing.T) {
	h := newLaneHarness(t)
	l := h.lane(t, "ctl")
	msg := h.propose(t, laneMsg("m", RolePrompt, "hello"))
	exec := respondExec(h, "tmux-abc")
	mustPass(t, l, view(), exec)
	if rt := h.runtime(t); rt.Pending != "tmux-abc#1" || rt.PendingGen != 1 {
		t.Fatalf("pending cell %q of generation %d; want tmux-abc#1 of 1", rt.Pending, rt.PendingGen)
	}
	_, rev, _, err := h.store.Runtime("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	mustPass(t, l, view(), exec)
	if _, again, _, _ := h.store.Runtime("gc-1"); again != rev {
		t.Fatal("a look at the same prompt rewrote the runtime row")
	}
	for _, mod := range []func(*Probe){
		func(p *Probe) { p.Pending = PendingUnknown },
		func(p *Probe) { p.Pending, p.Fence = "", Fence{Conversation: "gc-1", Generation: 2, Epoch: 1} },
		func(p *Probe) { p.Pending, p.Available = "", false },
		func(p *Probe) { p.Pending, p.Caps.Live = "", false },
		func(p *Probe) { p.Pending, p.Caps.Respond = "", false },
	} {
		// A busy runtime holds the message wherever nothing is pending.
		e := respondExec(h, "tmux-abc")
		e.probe.Boundary = TriNo
		mod(&e.probe)
		mustPass(t, l, view(), e)
		if f, ok := resolvedFact(t, h, "tmux-abc#1"); ok {
			t.Fatalf("probe %+v ended the raising: %+v", e.probe, f)
		}
		if rt := h.runtime(t); rt.Pending != "tmux-abc#1" {
			t.Fatalf("probe %+v moved the pending cell to %q", e.probe, rt.Pending)
		}
	}
	// The prompt clears: the look ends its raising and clears the cell, and
	// the message goes in the same pass.
	exec.probe.Pending = ""
	probes := exec.probes
	if res := mustPass(t, l, view(), exec); res.Done != DoneAccepted || !slices.Equal(res.Taken, []string{msg.ID}) {
		t.Fatalf("pass after the prompt cleared %+v", res)
	}
	if n := exec.probes - probes; n != 1 {
		t.Fatalf("the pass probed %d times; the Take after the look's write was re-planned", n)
	}
	if rt := h.runtime(t); rt.Pending != "" || rt.PendingGen != 0 {
		t.Fatalf("pending cell %q of generation %d after a look that saw none", rt.Pending, rt.PendingGen)
	}
	if f, ok := resolvedFact(t, h, "tmux-abc#1"); !ok || f.Reason != ResolvedCleared {
		t.Fatalf("raising ended %v %+v; want cleared", ok, f)
	}
}
