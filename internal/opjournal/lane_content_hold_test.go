package opjournal

import (
	"slices"
	"testing"
	"time"
)

// The content hold (DES-O1 D21) as the planner honors it: OJ-19 sets
// ConversationView.ContentHold from the row; these tests pin what a held
// conversation then does.

func heldView() ConversationView {
	v := view()
	v.ContentHold = true
	return v
}

func TestLaneContentHoldHoldsEveryNormalWhen(t *testing.T) {
	now := t0.Add(time.Minute)
	for _, when := range []When{"", WhenBoundary, WhenAfterTurn, WhenIfIdle} {
		for _, role := range []Role{RoleNotice, RolePrompt} {
			p := newPlanner(t)
			op := p.op(laneMsg("a", role, "x", withWhen(when)))
			held := planRules(snapOf(op), heldView(), now, "lane:ctl")
			if w := waitsOf(held.waits)[op.ID]; w != WaitContentHold || len(candidatesOf(held.content)) != 0 {
				t.Errorf("%s %q under a hold: wait %q, candidates %v; want content_hold", role, when, w, candidatesOf(held.content))
			}
			free := planRules(snapOf(op), view(), now, "lane:ctl")
			if w := waitsOf(free.waits)[op.ID]; w != "" || len(candidatesOf(free.content)) != 1 {
				t.Errorf("%s %q without a hold: wait %q, candidates %v; want a candidate", role, when, w, candidatesOf(free.content))
			}
		}
	}
}

func TestLaneContentHoldPassesControl(t *testing.T) {
	h := newLaneHarness(t)
	i := h.propose(t, interruptReq())
	exec := newFakeExec(h.clk)
	res := mustPass(t, h.lane(t, "ctl"), heldView(), exec)
	if res.Took != TakeControl || !slices.Equal(res.Taken, []string{i.ID}) || len(exec.interrupts) != 1 {
		t.Fatalf("held pass = %+v, %d interrupts; want the interrupt taken", res, len(exec.interrupts))
	}
}

func TestLaneContentHoldNeverHoldsNow(t *testing.T) {
	now := t0.Add(time.Minute)
	for _, when := range []When{WhenNow, WhenInterrupt} {
		p := newPlanner(t)
		op := p.op(laneMsg("a", RolePrompt, "x", withWhen(when)))
		rp := planRules(snapOf(op), heldView(), now, "lane:ctl")
		if w := waitsOf(rp.waits)[op.ID]; w == WaitContentHold {
			t.Errorf("when %q was held; urgent content passes the hold", when)
		}
	}
}

func TestLaneContentHoldLetsALaterSteerPass(t *testing.T) {
	h := newLaneHarness(t)
	n := h.propose(t, laneMsg("n", RoleNotice, "fyi"))
	h.clk.Advance(time.Millisecond)
	s := h.propose(t, laneMsg("s", RolePrompt, "steer", withWhen(WhenSteer)))
	exec := newFakeExec(h.clk)
	res := mustPass(t, h.lane(t, "ctl"), heldView(), exec)
	if !slices.Equal(res.Taken, []string{s.ID}) {
		t.Fatalf("held pass took %v, want only the steer", res.Taken)
	}
	if w := waitsOf(res.Waits)[n.ID]; w != WaitContentHold {
		t.Fatalf("held notice wait %q, want content_hold", w)
	}
}

func TestLaneContentHoldAsksNoWake(t *testing.T) {
	for _, held := range []bool{true, false} {
		h := newLaneHarness(t)
		h.propose(t, laneMsg("a", RolePrompt, "x"))
		exec := newFakeExec(h.clk)
		v := view()
		v.Dormant, v.ContentHold = true, held
		res := mustPass(t, h.lane(t, "ctl"), v, exec)
		if res.Wake == held || res.Took != TakeNone {
			t.Errorf("dormant, held %v: wake %v took %v; want a wake only without the hold", held, res.Wake, res.Took)
		}
		if held && exec.probes != 0 {
			t.Errorf("a held dormant conversation was probed %d times", exec.probes)
		}
	}
}

func TestLaneContentHoldNeverProbes(t *testing.T) {
	h := newLaneHarness(t)
	h.propose(t, laneMsg("a", RoleNotice, "x"))
	h.propose(t, laneMsg("b", RolePrompt, "y"))
	exec := newFakeExec(h.clk)
	res := mustPass(t, h.lane(t, "ctl"), heldView(), exec)
	if exec.probes != 0 || res.Took != TakeNone || len(exec.delivered()) != 0 {
		t.Fatalf("held pass probed %d times and took %v", exec.probes, res.Took)
	}
}

func TestLaneContentHoldKeepsDeadlines(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("a", RoleNotice, "x", func(r *Request) { r.StartBy = t0.Add(time.Minute) }))
	h.clk.Advance(2 * time.Minute)
	mustPass(t, h.lane(t, "ctl"), heldView(), newFakeExec(h.clk))
	got := h.get(t, op.ID)
	if got.Status != StatusClosed || got.Outcome.Arm != ArmFailed || got.Outcome.Reason != ReasonExpiredBeforeStart {
		t.Fatalf("held op past start_by = %s %+v, want failed{expired_before_start}", got.Status, got.Outcome)
	}
}

func TestLaneContentHoldDoesNotAdmitTheDrainNotice(t *testing.T) {
	drain := ContentClass{Role: RoleNotice, When: WhenBoundary, Principal: "gc:mail"}
	for _, held := range []bool{true, false} {
		h := newLaneHarness(t)
		op := h.propose(t, laneMsg("d", RoleNotice, "drain", withWhen(WhenBoundary)))
		exec := newFakeExec(h.clk)
		exec.probe.ContentBlock, exec.probe.ContentAdmit = "draining", []ContentClass{drain}
		v := view()
		v.ContentHold = held
		res := mustPass(t, h.lane(t, "ctl"), v, exec)
		switch {
		case held && (res.Took != TakeNone || waitsOf(res.Waits)[op.ID] != WaitContentHold):
			t.Errorf("held drain notice: took %v, waits %v; want content_hold", res.Took, res.Waits)
		case !held && !slices.Equal(res.Taken, []string{op.ID}):
			t.Errorf("admitted drain notice without a hold: took %v, waits %v", res.Taken, res.Waits)
		}
	}
}

// TestLaneContentHoldEditedMessageGoesToTheBack characterizes T3 decision 2:
// a queued message edited under a hold (Cancel, then a new message) loses
// its place unless the edit reuses the original's client_seq, which
// orderEntries sorts one principal's ops by.
func TestLaneContentHoldEditedMessageGoesToTheBack(t *testing.T) {
	seq := func(n uint64) func(*Request) { return func(r *Request) { r.ClientSeq = n } }
	none := func(*Request) {}
	for _, tc := range []struct {
		name           string
		m1, m2, edit   func(*Request)
		editKeepsPlace bool
	}{
		{"no client_seq", none, none, none, false},
		{"fresh client_seq", seq(1), seq(2), seq(3), false},
		{"reused client_seq", seq(1), seq(2), seq(1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			l := h.lane(t, "ctl")
			exec := newFakeExec(h.clk)
			m1 := h.propose(t, laneMsg("m1", RoleNotice, "one", tc.m1))
			h.clk.Advance(time.Millisecond)
			m2 := h.propose(t, laneMsg("m2", RoleNotice, "two", tc.m2))
			mustPass(t, l, heldView(), exec)
			if res, _, err := h.store.Cancel(m1.ID, "operator", "edited"); err != nil || res != CancelClosed {
				t.Fatalf("Cancel = %v, %v", res, err)
			}
			h.clk.Advance(time.Millisecond)
			edited := h.propose(t, laneMsg("m1-edit", RoleNotice, "one, edited", tc.edit))
			if res := mustPass(t, l, heldView(), exec); res.Took != TakeNone {
				t.Fatalf("held pass took %v", res.Taken)
			}
			want := []string{m2.ID, edited.ID}
			if tc.editKeepsPlace {
				want = []string{edited.ID, m2.ID}
			}
			if res := mustPass(t, l, view(), exec); !slices.Equal(res.Taken, want) {
				t.Fatalf("resumed pass took %v, want %v", res.Taken, want)
			}
		})
	}
}
