package opjournal

import (
	"context"
	"errors"
	"testing"
	"time"
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
		if tp.take != nil || len(tp.closes) != 0 || waitsOf(tp.waits)[op.ID] != WaitInteractionUnknown || !tp.next.Equal(now.Add(TransientBackoffBase)) {
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
	// A failed answer resolves nothing: the prompt may still show.
	h2 := newLaneHarness(t)
	raised2 := h2.raise(t, "tmux-abc")
	h2.propose(t, laneRespond(t, raised2, "operator", RespondPayload{Action: "approve"}))
	exec2 := respondExec(h2, "tmux-abc")
	exec2.respond = func(RespondCall) ControlResult {
		return Rejected{Arm: ArmFailed, Reason: RefusedReasonPrefix + "action"}
	}
	mustPass(t, h2.lane(t, "ctl"), view(), exec2)
	if _, ok := resolvedFact(t, h2, raised2.Interaction); ok {
		t.Fatal("a refused answer resolved the occurrence")
	}
}

// An attempt that may have answered (Unknown) and then finds the prompt gone
// cannot tell whether its answer cleared it: failed{ambiguous_delivery},
// never skipped{stale}, which would record that it never ran.
func TestLaneMaybeDeliveredRespondWithThePromptGoneIsAmbiguous(t *testing.T) {
	h := newLaneHarness(t)
	raised := h.raise(t, "tmux-abc")
	op := h.propose(t, laneRespond(t, raised, "operator", RespondPayload{Action: "approve"}))
	exec := respondExec(h, "tmux-abc")
	exec.respond = func(RespondCall) ControlResult { return Unknown{Cause: errors.New("verify timed out")} }
	l := h.lane(t, "ctl")
	mustPass(t, l, view(), exec)
	h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	exec.probe.Pending = ""
	mustPass(t, l, view(), exec)
	if got := h.get(t, op.ID); got.Outcome.Arm != ArmFailed || got.Outcome.Reason != ReasonAmbiguousDelivery {
		t.Fatalf("op %+v", got.Outcome)
	}
	if len(exec.responds) != 1 {
		t.Fatalf("%d responds sent", len(exec.responds))
	}
	if _, ok := resolvedFact(t, h, raised.Interaction); !ok {
		t.Fatal("the cleared raising is not resolved")
	}
}
