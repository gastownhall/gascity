package opjournal

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

// planner builds ops and snapshots for the pure planner tests.
type planner struct {
	t     *testing.T
	store *Store
	clk   *clock.Fake
}

func newPlanner(t *testing.T) *planner {
	h := newLaneHarness(t)
	return &planner{t: t, store: h.store, clk: h.clk}
}

// op previews req as a submitted lane op created now; mods adjust the op.
func (p *planner) op(req Request, mods ...func(*Op)) Op {
	p.t.Helper()
	op, err := p.store.Preview(req)
	if err != nil {
		p.t.Fatalf("Preview: %v", err)
	}
	p.clk.Advance(time.Millisecond)
	for _, m := range mods {
		m(&op)
	}
	return op
}

// snapOf is a snapshot of ops in namespace gc, read over a store that holds
// nothing else: every deliver-now partner that is not among ops reads
// missing (readNow).
func snapOf(ops ...Op) laneSnapshot {
	s := laneSnapshot{openIDs: map[string]bool{}, after: map[string]afterTarget{}, prefix: "gc", nowRows: map[string]nowRow{}}
	for _, op := range ops {
		s.ops = append(s.ops, op)
		s.openIDs[op.ID] = true
	}
	slices.SortFunc(s.ops, CompareOrder)
	for _, op := range s.ops {
		ids := []string{op.Causation}
		if isNowMessage(op) {
			ids, _ = nowInterruptIDs(s.prefix, op)
		} else if !hasNowShape(op) {
			continue
		}
		for _, id := range ids {
			if !s.openIDs[id] {
				s.nowRows[id] = nowRow{state: afterMissing}
			}
		}
	}
	return s
}

func executingWith(deadline time.Time) func(*Op) {
	return func(op *Op) {
		op.Status = StatusExecuting
		op.Attempt = 1
		op.LaneSeq = 1
		op.DeliveryKey = op.ID + ":1"
		op.Lease = Lease{Lane: "conversation", Conversation: op.Conversation, Generation: 1, Nonce: "x.1", Deadline: deadline}
	}
}

func closesOf(rp rulesPlan) map[string]string {
	out := map[string]string{}
	for _, c := range rp.closes {
		out[c.cur.ID] = string(c.next.Outcome.Arm) + "{" + c.next.Outcome.Reason + "}"
	}
	return out
}

func waitsOf(ws []OpWait) map[string]WaitReason {
	out := map[string]WaitReason{}
	for _, w := range ws {
		out[w.ID] = w.Reason
	}
	return out
}

func candidatesOf(es []entry) []string {
	var out []string
	for _, e := range es {
		if e.candidate() {
			out = append(out, e.op.ID)
		}
	}
	return out
}

func TestPlanRulesRows(t *testing.T) {
	now := t0.Add(time.Hour)
	expired := executingWith(now.Add(-LeaseSkew - time.Second))
	live := executingWith(now.Add(time.Minute))
	canceled := func(op *Op) { op.Cancel = Cancel{By: "operator", At: t0} }
	cases := []struct {
		name  string
		req   Request
		mods  []func(*Op)
		conv  func(*ConversationView)
		after *afterTarget
		close string
		wait  WaitReason
		cand  bool
		retry bool
		// lower: the op is lowered (a deliver-now message).
		lower bool
	}{
		{name: "R1 closed", req: laneMsg("a", RolePrompt, "x"), conv: func(v *ConversationView) { v.Closed = true }, close: "skipped{conversation_closed}"},
		{name: "R1 closed in flight", req: laneMsg("a", RolePrompt, "x"), mods: []func(*Op){live}, conv: func(v *ConversationView) { v.Closed = true }, wait: WaitInFlight},
		{name: "R2 in flight", req: laneMsg("a", RolePrompt, "x"), mods: []func(*Op){live}, wait: WaitInFlight},
		{name: "R2 inside the skew", req: laneMsg("a", RolePrompt, "x"), mods: []func(*Op){executingWith(now.Add(-LeaseSkew))}, wait: WaitInFlight},
		{name: "R3 expired cancel", req: laneMsg("a", RolePrompt, "x"), mods: []func(*Op){expired, canceled}, close: "canceled{}"},
		{name: "R4 expired past finish_by", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.FinishBy = t0.Add(time.Minute) }), mods: []func(*Op){expired}, close: "failed{deadline_in_flight}"},
		{name: "R5 expired at most once", req: laneMsg("a", RolePrompt, "x", atMostOnce), mods: []func(*Op){expired}, close: "failed{ambiguous_delivery}"},
		{name: "R6 expired past start_by", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.StartBy = t0.Add(time.Minute) }), mods: []func(*Op){expired}, close: "failed{deadline_in_flight}"},
		{name: "R7 poison", req: laneMsg("a", RolePrompt, "x"), mods: []func(*Op){expired, func(op *Op) { op.Failures = 4 }}, close: "failed{poison}"},
		{name: "R8 retake", req: laneMsg("a", RolePrompt, "x"), mods: []func(*Op){expired}, cand: true, retry: true},
		{name: "R8 retake still checks scope", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.Scope = Scope{Kind: ScopeSession, Generation: 1} }), mods: []func(*Op){expired}, conv: func(v *ConversationView) { v.Generation = 2 }, close: "skipped{stale}"},
		{name: "R9 cancel", req: laneMsg("a", RolePrompt, "x"), mods: []func(*Op){canceled}, close: "canceled{}"},
		{name: "R10 expired before start", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.StartBy = t0.Add(time.Minute) }), close: "failed{expired_before_start}"},
		{name: "R10 maybe delivered", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.StartBy = t0.Add(time.Minute) }), mods: []func(*Op){func(op *Op) { op.Status, op.Attempt, op.MaybeDelivered = StatusAccepted, 1, 1 }}, close: "failed{deadline_in_flight}"},
		{name: "R11 stale session", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.Scope = Scope{Kind: ScopeSession, Generation: 1} }), conv: func(v *ConversationView) { v.Generation = 2 }, close: "skipped{stale}"},
		{name: "R11 stale context", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.Scope = Scope{Kind: ScopeContext, Epoch: 1} }), conv: func(v *ConversationView) { v.Epoch = 2 }, close: "skipped{stale}"},
		{name: "R12 control dormant", req: interruptReq(), conv: func(v *ConversationView) { v.Dormant = true }, close: "skipped{stale}"},
		{name: "R13 future scope", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.Scope = Scope{Kind: ScopeSession, Generation: 5} }), wait: WaitScopeFuture},
		{name: "R14 after failed", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.After = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa" }), after: &afterTarget{state: afterClosed, arm: ArmFailed, conversation: "gc-1"}, close: "skipped{dependency_failed}"},
		{name: "R14 after succeeded", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.After = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa" }), after: &afterTarget{state: afterClosed, arm: ArmSucceeded, conversation: "gc-1"}, cand: true},
		{name: "R15 after elsewhere", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.After = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa" }), after: &afterTarget{state: afterClosed, arm: ArmSucceeded, conversation: "gc-2"}, close: "failed{refused:after_scope}"},
		{name: "R16 after missing", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.After = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa" }), after: &afterTarget{state: afterMissing}, wait: WaitAfterPending},
		{name: "R16 after unreadable", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.After = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa" }), after: &afterTarget{state: afterUnreadable, err: errFlakyRead}, wait: WaitAfterUnreadable},
		{name: "R16 after undecodable", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.After = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa" }), after: &afterTarget{state: afterUnreadable, err: ErrUnsupportedSchema, undecodable: true}, wait: WaitAfterUnreadable},
		{name: "R17 not before", req: laneMsg("a", RolePrompt, "x", func(r *Request) { r.NotBefore = now.Add(time.Minute) }), wait: WaitNotBefore},
		{name: "R18 retry at", req: laneMsg("a", RolePrompt, "x"), mods: []func(*Op){func(op *Op) { op.Status, op.Attempt, op.RetryAt = StatusAccepted, 1, now.Add(time.Second) }}, wait: WaitRetryBackoff},
		{name: "R19 now", req: laneMsg("a", RolePrompt, "x", withWhen(WhenNow)), lower: true},
		{name: "R19 legacy interrupt", req: laneMsg("a", RolePrompt, "x", withWhen(WhenInterrupt)), lower: true},
		{name: "R19 now waits its not_before", req: laneMsg("a", RolePrompt, "x", withWhen(WhenNow), func(r *Request) { r.NotBefore = now.Add(time.Minute) }), wait: WaitNotBefore},
		{name: "R20 content hold", req: laneMsg("a", RolePrompt, "x"), conv: func(v *ConversationView) { v.ContentHold = true }, wait: WaitContentHold},
		{name: "R20 steer bypasses the hold", req: laneMsg("a", RolePrompt, "x", withWhen(WhenSteer)), conv: func(v *ConversationView) { v.ContentHold = true }, cand: true},
		{name: "settling", req: laneMsg("a", RolePrompt, "x"), mods: []func(*Op){func(op *Op) { op.Status = StatusSettling }}, wait: WaitSettling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(t)
			op := p.op(tc.req, tc.mods...)
			snap := snapOf(op)
			if tc.after != nil {
				snap.after[op.After] = *tc.after
			}
			v := view()
			if tc.conv != nil {
				tc.conv(&v)
			}
			rp := planRules(snap, v, now, "lane:ctl")
			if got := closesOf(rp)[op.ID]; got != tc.close {
				t.Fatalf("close %q, want %q (waits %v)", got, tc.close, rp.waits)
			}
			if got := waitsOf(rp.waits)[op.ID]; got != tc.wait {
				t.Fatalf("wait %q, want %q", got, tc.wait)
			}
			if lowered := len(rp.lowerings) == 1 && rp.lowerings[0].m.ID == op.ID; lowered != tc.lower || len(rp.lowerings) > 1 {
				t.Fatalf("lowerings %+v, want lowered=%v", rp.lowerings, tc.lower)
			}
			if tc.lower && rp.takePossible {
				t.Fatal("a pass that lowers takes nothing before it reads again")
			}
			all := append(candidatesOf(rp.control), candidatesOf(rp.content)...)
			if (len(all) == 1) != tc.cand {
				t.Fatalf("candidates %v, want candidate=%v", all, tc.cand)
			}
			if tc.cand {
				var e entry
				for _, x := range append(rp.control, rp.content...) {
					if x.candidate() {
						e = x
					}
				}
				if e.retake != tc.retry || (tc.retry && e.cur.MaybeDelivered != 1) {
					t.Fatalf("entry %+v", e)
				}
			}
			for _, c := range rp.closes {
				if c.cur.Status == StatusExecuting && c.next.MaybeDelivered != c.cur.Attempt {
					t.Fatalf("an expired op closed without maybe_delivered: %+v", c.next)
				}
				if _, err := EncodeOp(c.next); err != nil {
					t.Fatalf("close does not encode: %v", err)
				}
			}
		})
	}
}

func TestPlanRulesBlockingWaiters(t *testing.T) {
	now := t0.Add(time.Hour)
	p := newPlanner(t)
	retry := p.op(laneMsg("a", RoleNotice, "x"), func(op *Op) { op.Status, op.Attempt, op.RetryAt = StatusAccepted, 1, now.Add(time.Second) })
	later := p.op(laneMsg("b", RoleNotice, "y"))
	nowOp := p.op(laneMsg("c", RolePrompt, "z", withWhen(WhenNow)))
	rp := planRules(snapOf(retry, later, nowOp), view(), now, "lane:ctl")
	tp, err := planTake(rp, idleProbe(), view(), now)
	if err != nil {
		t.Fatal(err)
	}
	if tp.take != nil {
		t.Fatalf("took past a retry_backoff waiter: %+v", tp.take.ops)
	}
	if w := waitsOf(tp.waits)[later.ID]; w != WaitBlocked {
		t.Fatalf("later op wait %q", w)
	}
	// A pass that lowers a deliver-now message takes nothing: it reads
	// again first, so no content goes ahead of the unit.
	rp = planRules(snapOf(nowOp, later), view(), now, "lane:ctl")
	if rp.takePossible || len(rp.lowerings) != 1 || rp.lowerings[0].m.ID != nowOp.ID {
		t.Fatalf("lowering pass: takePossible %v lowerings %+v", rp.takePossible, rp.lowerings)
	}
	if !rp.next.IsZero() && rp.next.Before(now) {
		t.Fatalf("next %s", rp.next)
	}
}

func TestPlanRulesNext(t *testing.T) {
	now := t0.Add(time.Hour)
	p := newPlanner(t)
	nb := p.op(laneMsg("a", RoleNotice, "x", func(r *Request) { r.NotBefore = now.Add(time.Minute) }))
	inflight := p.op(laneMsg("b", RoleNotice, "x"), executingWith(now.Add(30*time.Second)))
	rp := planRules(snapOf(nb, inflight), view(), now, "lane:ctl")
	if want := now.Add(30*time.Second + LeaseSkew); !rp.next.Equal(want) {
		t.Fatalf("next %s, want %s", rp.next, want)
	}
	// At the lease's end (its deadline plus LeaseSkew) the op is still in
	// flight (it expires once now is past the end), so the timer is the
	// next instant, never now.
	edge := p.op(laneMsg("e", RoleNotice, "x"), executingWith(now.Add(-LeaseSkew)))
	rp = planRules(snapOf(edge), view(), now, "lane:ctl")
	if len(rp.waits) != 1 || rp.waits[0].Reason != WaitInFlight || !rp.waits[0].Until.Equal(now.Add(time.Nanosecond)) || !rp.next.Equal(rp.waits[0].Until) {
		t.Fatalf("waits %v next %s at the lease's end %s", rp.waits, rp.next, now)
	}
	if rp = planRules(snapOf(edge), view(), rp.next, "lane:ctl"); !rp.takePossible {
		t.Fatalf("at the timer: waits %v closes %v", rp.waits, closesOf(rp))
	}
	start := p.op(laneMsg("c", RoleNotice, "x", func(r *Request) { r.StartBy = now.Add(5 * time.Second) }))
	rp = planRules(snapOf(nb, start), view(), now, "lane:ctl")
	if want := now.Add(5 * time.Second); !rp.next.Equal(want) {
		t.Fatalf("next %s, want start_by %s", rp.next, want)
	}
	// At start_by itself the op is still open (it closes once now is past
	// start_by), so the timer is the next instant, never none.
	at := p.op(laneMsg("d", RoleNotice, "x", withWhen(WhenAfterTurn), func(r *Request) { r.StartBy = now }))
	rp = planRules(snapOf(at), view(), now, "lane:ctl")
	if len(rp.closes) != 0 || !rp.next.After(now) {
		t.Fatalf("closes %v next %s at start_by %s", closesOf(rp), rp.next, now)
	}
	if rp = planRules(snapOf(at), view(), rp.next, "lane:ctl"); closesOf(rp)[at.ID] != "failed{expired_before_start}" {
		t.Fatalf("at the timer: closes %v", closesOf(rp))
	}
}

func TestPlanRulesTakePossible(t *testing.T) {
	now := t0.Add(time.Hour)
	p := newPlanner(t)
	need := p.op(laneMsg("a", RoleNotice, "x"))
	live := p.op(laneMsg("b", RoleNotice, "x", liveOnly))
	cases := map[string]struct {
		conv   func(*ConversationView)
		row    *RuntimeRow
		ops    []Op
		ok     bool
		wait   WaitReason
		wake   bool
		nextIs time.Time
	}{
		"ok":                    {ops: []Op{need}, ok: true},
		"no session":            {conv: func(v *ConversationView) { v.Generation = 0 }, ops: []Op{need}, wait: WaitNoSession, wake: true},
		"no session, live only": {conv: func(v *ConversationView) { v.Generation = 0 }, ops: []Op{live}, wait: WaitNoSession},
		"dormant":               {conv: func(v *ConversationView) { v.Dormant = true }, ops: []Op{need}, wait: WaitDormant, wake: true},
		"stale view":            {row: &RuntimeRow{Conversation: "gc-1", Generation: 2, Epoch: 1}, ops: []Op{need}, wait: WaitIncarnation, nextIs: now.Add(TransientBackoffBase)},
		"cell held":             {row: &RuntimeRow{Conversation: "gc-1", Generation: 1, Epoch: 1, Lease: Lease{Lane: "conversation", Conversation: "gc-1", Generation: 1, Nonce: "x.1", Deadline: now}}, ops: []Op{need}, wait: WaitInFlight, nextIs: now.Add(LeaseSkew)},
		"cell at its end":       {row: &RuntimeRow{Conversation: "gc-1", Generation: 1, Epoch: 1, Lease: Lease{Lane: "conversation", Conversation: "gc-1", Generation: 1, Nonce: "x.1", Deadline: now.Add(-LeaseSkew)}}, ops: []Op{need}, wait: WaitInFlight, nextIs: now.Add(time.Nanosecond)},
		"cell expired":          {row: &RuntimeRow{Conversation: "gc-1", Generation: 1, Epoch: 1, Lease: Lease{Lane: "conversation", Conversation: "gc-1", Generation: 1, Nonce: "x.1", Deadline: now.Add(-LeaseSkew - time.Nanosecond)}}, ops: []Op{need}, ok: true},
		"newer view":            {row: &RuntimeRow{Conversation: "gc-1", Generation: 1, Epoch: 0}, ops: []Op{need}, ok: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v := view()
			if tc.conv != nil {
				tc.conv(&v)
			}
			snap := snapOf(tc.ops...)
			if tc.row != nil {
				b, err := EncodeRuntimeRow("gc", *tc.row)
				if err != nil {
					t.Fatal(err)
				}
				row, _ := DecodeRuntimeRow(b)
				snap.rt = runtimeSnap{read: true, found: true, bead: b, row: row}
			}
			rp := planRules(snap, v, now, "lane:ctl")
			if rp.takePossible != tc.ok || rp.wake != tc.wake {
				t.Fatalf("takePossible %v wake %v (waits %v)", rp.takePossible, rp.wake, rp.waits)
			}
			if tc.wait != "" && waitsOf(rp.waits)[tc.ops[0].ID] != tc.wait {
				t.Fatalf("waits %v, want %s", rp.waits, tc.wait)
			}
			if tc.nextIs.IsZero() {
				return
			}
			if !rp.next.Equal(tc.nextIs) {
				t.Fatalf("next %s, want %s", rp.next, tc.nextIs)
			}
			for _, w := range rp.waits {
				if !w.Until.Equal(tc.nextIs) {
					t.Fatalf("wait %+v, want until %s", w, tc.nextIs)
				}
			}
		})
	}
}

// takeFor plans a Take of ops under probe. Every respond op's occurrence
// reads raised and unresolved.
func takeFor(t *testing.T, ops []Op, probe Probe) takePlan {
	t.Helper()
	now := t0.Add(time.Hour)
	rp := planRules(snapOf(ops...), view(), now, "lane:ctl")
	rp.raisings = map[string]raisingState{}
	for _, op := range ops {
		if op.Kind == KindRespond {
			req, _, err := ParseInteractionOccurrence(op.Scope.Interaction)
			rp.raisings[op.ID] = raisingState{request: req, raised: err == nil, err: err}
		}
	}
	tp, err := planTake(rp, probe, view(), now)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func takenOf(tp takePlan) []string {
	if tp.take == nil {
		return nil
	}
	var out []string
	for _, e := range tp.take.ops {
		out = append(out, e.op.ID)
	}
	return out
}

func TestPlanTakeWhenTable(t *testing.T) {
	type outcome struct {
		take  bool
		mode  LinkMode
		wait  WaitReason
		close string
	}
	steerFail := func(r *Request) {
		r.Policy.Message.When = WhenSteer
		r.Policy.Message.OnUnsupported = FallbackFail
	}
	cases := []struct {
		name string
		mods []func(*Request)
		caps func(*Caps)
		yes  outcome
		no   outcome
		unk  outcome
	}{
		{name: "boundary", yes: outcome{take: true, mode: LinkStarts}, no: outcome{wait: WaitBusy}, unk: outcome{wait: WaitBoundaryUnknown}},
		{name: "after_turn alone", mods: []func(*Request){withWhen(WhenAfterTurn)}, yes: outcome{wait: WaitAfterTurn}, no: outcome{wait: WaitBusy}, unk: outcome{wait: WaitBoundaryUnknown}},
		{name: "if_idle", mods: []func(*Request){withWhen(WhenIfIdle)}, yes: outcome{take: true, mode: LinkStarts}, no: outcome{close: "skipped{busy}"}, unk: outcome{wait: WaitBoundaryUnknown}},
		{name: "steer capable", mods: []func(*Request){withWhen(WhenSteer)}, caps: func(c *Caps) { c.Steer = true }, yes: outcome{take: true, mode: LinkStarts}, no: outcome{take: true, mode: LinkJoins}, unk: outcome{wait: WaitBoundaryUnknown}},
		{name: "steer fallback boundary", mods: []func(*Request){withWhen(WhenSteer)}, yes: outcome{take: true, mode: LinkStarts}, no: outcome{wait: WaitBusy}, unk: outcome{wait: WaitBoundaryUnknown}},
		{name: "steer fallback fail", mods: []func(*Request){steerFail}, yes: outcome{take: true, mode: LinkStarts}, no: outcome{close: "failed{unsupported}"}, unk: outcome{wait: WaitBoundaryUnknown}},
	}
	for _, tc := range cases {
		for b, want := range map[Tri]outcome{TriYes: tc.yes, TriNo: tc.no, TriUnknown: tc.unk} {
			t.Run(tc.name+"/"+strconv.Itoa(int(b)), func(t *testing.T) {
				p := newPlanner(t)
				op := p.op(laneMsg("a", RoleNotice, "x", tc.mods...))
				probe := idleProbe()
				probe.Boundary = b
				if tc.caps != nil {
					tc.caps(&probe.Caps)
				}
				tp := takeFor(t, []Op{op}, probe)
				if (tp.take != nil) != want.take {
					t.Fatalf("take %v, want %v (waits %v closes %v)", takenOf(tp), want.take, tp.waits, tp.closes)
				}
				if want.take && tp.take.mode != want.mode {
					t.Fatalf("mode %s, want %s", tp.take.mode, want.mode)
				}
				if want.take && want.mode == LinkJoins && !tp.take.steer {
					t.Fatal("a mid-turn steer is not marked steer")
				}
				if w := waitsOf(tp.waits)[op.ID]; w != want.wait {
					t.Fatalf("wait %q, want %q", w, want.wait)
				}
				got := ""
				for _, c := range tp.closes {
					got = string(c.next.Outcome.Arm) + "{" + c.next.Outcome.Reason + "}"
				}
				if got != want.close {
					t.Fatalf("close %q, want %q", got, want.close)
				}
				if want.wait == WaitBusy || want.wait == WaitBoundaryUnknown {
					if !tp.busy {
						t.Fatal("busy not set")
					}
				}
			})
		}
	}
}

// A probe close records that the op never ran, so only a fresh op gets one:
// a re-take, or an op released after an Unknown result, may have reached the
// executor and waits instead. The probe that allows it takes it.
func TestPlanTakeMaybeDeliveredWaitsInsteadOfClosing(t *testing.T) {
	steerFail := func(r *Request) {
		r.Policy.Message.When = WhenSteer
		r.Policy.Message.OnUnsupported = FallbackFail
	}
	kinds := []struct {
		name  string
		req   Request
		deny  func(*Probe)
		close string
		wait  WaitReason
	}{
		{"if_idle", laneMsg("a", RoleNotice, "x", withWhen(WhenIfIdle)), func(p *Probe) { p.Boundary = TriNo }, "skipped{busy}", WaitBusy},
		{"steer fallback_fail", laneMsg("a", RoleNotice, "x", steerFail), func(p *Probe) { p.Boundary = TriNo }, "failed{unsupported}", WaitBusy},
	}
	states := []struct {
		name  string
		fresh bool
		mod   func(*Op)
	}{
		{"fresh", true, func(*Op) {}},
		{"re-take", false, executingWith(t0)},
		// Store.Transition can still move a lane op to executing: no
		// attempt, no lease, so its re-take records maybe_delivered 0.
		{"re-take with no attempt", false, func(op *Op) { op.Status = StatusExecuting }},
		{"released after unknown", false, func(op *Op) {
			op.Status, op.Attempt, op.MaybeDelivered, op.Failures = StatusAccepted, 1, 1, 1
			op.DeliveryKey = op.ID + ":1"
			op.LastError = lastErrorUnknown + ": bridge timeout"
		}},
	}
	for _, k := range kinds {
		for _, s := range states {
			t.Run(k.name+"/"+s.name, func(t *testing.T) {
				p := newPlanner(t)
				op := p.op(k.req, s.mod)
				allow := idleProbe()
				deny := allow
				k.deny(&deny)
				tp := takeFor(t, []Op{op}, deny)
				closed := ""
				for _, c := range tp.closes {
					closed = string(c.next.Outcome.Arm) + "{" + c.next.Outcome.Reason + "}"
				}
				wantClose, wantWait := k.close, WaitReason("")
				if !s.fresh {
					wantClose, wantWait = "", k.wait
				}
				if tp.take != nil || closed != wantClose || waitsOf(tp.waits)[op.ID] != wantWait {
					t.Fatalf("took %v, closed %q, waits %v; want close %q, wait %q", takenOf(tp), closed, tp.waits, wantClose, wantWait)
				}
				if tp.busy != (wantWait == WaitBusy) {
					t.Fatalf("busy %v with wait %q", tp.busy, wantWait)
				}
				tp = takeFor(t, []Op{op}, allow)
				if !slices.Equal(takenOf(tp), []string{op.ID}) {
					t.Fatalf("the probe that allows it took %v (waits %v, closes %v)", takenOf(tp), tp.waits, tp.closes)
				}
				// A maybe-delivered message goes as a replay, under its stored
				// key or, when it has none, a new one.
				wantKey, wantRedelivery := op.ID+":1", !s.fresh
				if tp.take.key != wantKey || tp.take.redelivery != wantRedelivery {
					t.Fatalf("took it under key %q, redelivery %v; want %q, %v", tp.take.key, tp.take.redelivery, wantKey, wantRedelivery)
				}
			})
		}
	}
}

func TestPlanTakeAfterTurnRidesWithAStarter(t *testing.T) {
	p := newPlanner(t)
	rider := p.op(laneMsg("a", RoleNotice, "x", withWhen(WhenAfterTurn)))
	starter := p.op(laneMsg("b", RolePrompt, "y"))
	tp := takeFor(t, []Op{rider, starter}, idleProbe())
	if got := takenOf(tp); !slices.Equal(got, []string{rider.ID, starter.ID}) {
		t.Fatalf("taken %v", got)
	}
}

func TestPlanTakeSteerJoinsOnlyWithSteer(t *testing.T) {
	p := newPlanner(t)
	normal := p.op(laneMsg("a", RoleNotice, "x"))
	s1 := p.op(laneMsg("b", RoleNotice, "y", withWhen(WhenSteer)))
	s2 := p.op(laneMsg("c", RoleNotice, "z", withWhen(WhenSteer)))
	probe := idleProbe()
	probe.Boundary = TriNo
	probe.Caps.Steer = true
	tp := takeFor(t, []Op{normal, s1, s2}, probe)
	if got := takenOf(tp); !slices.Equal(got, []string{s1.ID, s2.ID}) || tp.take.mode != LinkJoins {
		t.Fatalf("taken %v", got)
	}
	// Urgent before normal at a boundary.
	tp = takeFor(t, []Op{normal, s1}, idleProbe())
	if got := takenOf(tp); !slices.Equal(got, []string{s1.ID, normal.ID}) {
		t.Fatalf("taken %v", got)
	}
}

func TestPlanTakeInBand(t *testing.T) {
	p := newPlanner(t)
	notice := p.op(laneMsg("a", RoleNotice, "x"))
	prompt := p.op(laneMsg("b", RolePrompt, "y"))
	rider := p.op(laneMsg("c", RoleNotice, "z", withWhen(WhenAfterTurn)))
	probe := idleProbe()
	probe.Caps = Caps{InBand: true, Input: FenceNone}
	probe.Boundary = TriNo
	tp := takeFor(t, []Op{notice, prompt, rider}, probe)
	if got := takenOf(tp); !slices.Equal(got, []string{notice.ID, rider.ID}) || tp.take.mode != LinkJoins || tp.take.steer {
		t.Fatalf("taken %v mode %v", got, tp.take)
	}
	if w := waitsOf(tp.waits)[prompt.ID]; w != WaitNotLive {
		t.Fatalf("prompt wait %q", w)
	}
	// Neither live nor in-band: nothing.
	probe.Caps = Caps{Input: FenceNone}
	if tp := takeFor(t, []Op{notice}, probe); tp.take != nil {
		t.Fatal("took without a live or in-band executor")
	}
	// Live as well as in-band: the live rules apply, so the prompt goes too
	// and the batch starts a turn.
	probe.Caps = Caps{Live: true, InBand: true, Input: FenceNone}
	probe.Boundary = TriYes
	tp = takeFor(t, []Op{notice, prompt, rider}, probe)
	if got := takenOf(tp); !slices.Equal(got, []string{notice.ID, rider.ID, prompt.ID}) || tp.take.mode != LinkStarts {
		t.Fatalf("live and in-band: taken %v mode %v", got, tp.take)
	}
}

func TestPlanTakeControlFirst(t *testing.T) {
	p := newPlanner(t)
	msg := p.op(laneMsg("a", RoleNotice, "x", withWhen(WhenSteer)))
	stop := p.op(interruptReq())
	tp := takeFor(t, []Op{msg, stop}, idleProbe())
	if got := takenOf(tp); !slices.Equal(got, []string{stop.ID}) || tp.take.kind != TakeControl || !tp.more {
		t.Fatalf("taken %v", got)
	}
	probe := idleProbe()
	probe.Running = false
	tp = takeFor(t, []Op{stop}, probe)
	if tp.take != nil || waitsOf(tp.waits)[stop.ID] != WaitNotRunning {
		t.Fatalf("control without a running runtime: %+v", tp)
	}
}

func TestPlanTakeContentBlocks(t *testing.T) {
	p := newPlanner(t)
	msg := p.op(laneMsg("a", RoleNotice, "x"))
	stop := p.op(interruptReq())
	for name, probe := range map[string]Probe{
		"content block": func() Probe { p := idleProbe(); p.ContentBlock = "draining"; return p }(),
		"pending":       func() Probe { p := idleProbe(); p.Pending = "?"; return p }(),
	} {
		t.Run(name, func(t *testing.T) {
			tp := takeFor(t, []Op{msg}, probe)
			if tp.take != nil {
				t.Fatal("content taken")
			}
			w := tp.waits[0]
			if name == "content block" && (w.Reason != WaitContentBlock || w.Detail != "draining") {
				t.Fatalf("wait %+v", w)
			}
			if name == "pending" && w.Reason != WaitPendingInteraction {
				t.Fatalf("wait %+v", w)
			}
			if tp := takeFor(t, []Op{msg, stop}, probe); tp.take == nil || tp.take.kind != TakeControl {
				t.Fatal("the block stopped control")
			}
		})
	}
}

// TestPlanTakeContentAdmitPassesOnlyItsClass pins a host's narrow I7
// exemption: under a content block, only content of an admitted class
// (role, when and principal, all three) is taken; every neighbor waits on
// the block, and the admitted class still waits on a pending interaction.
func TestPlanTakeContentAdmitPassesOnlyItsClass(t *testing.T) {
	p := newPlanner(t)
	const admitted = "gc:planner/drain-reminder"
	from := func(principal string) func(*Request) {
		return func(r *Request) { r.Producer.Principal = principal }
	}
	notice := p.op(laneMsg("notice", RoleNotice, "x", withWhen(WhenBoundary), from(admitted)))
	probe := idleProbe()
	probe.ContentBlock = "draining"
	probe.ContentAdmit = []ContentClass{{Role: RoleNotice, When: WhenBoundary, Principal: admitted}}
	if tp := takeFor(t, []Op{notice}, probe); !slices.Equal(takenOf(tp), []string{notice.ID}) {
		t.Fatalf("admitted notice: taken %v, waits %+v", takenOf(tp), tp.waits)
	}
	for name, op := range map[string]Op{
		"other principal": p.op(laneMsg("other", RoleNotice, "x", withWhen(WhenBoundary), from("gc:planner/idle-claim-nudge"))),
		"prompt":          p.op(laneMsg("prompt", RolePrompt, "x", withWhen(WhenBoundary), from(admitted))),
		"steer":           p.op(laneMsg("steer", RoleNotice, "x", withWhen(WhenSteer), from(admitted))),
	} {
		t.Run(name, func(t *testing.T) {
			tp := takeFor(t, []Op{op}, probe)
			if tp.take != nil || len(tp.waits) != 1 || tp.waits[0].Reason != WaitContentBlock || tp.waits[0].Detail != "draining" {
				t.Fatalf("taken %v, waits %+v; want a content_block wait", takenOf(tp), tp.waits)
			}
			tp = takeFor(t, []Op{op, notice}, probe)
			if got := takenOf(tp); !slices.Equal(got, []string{notice.ID}) {
				t.Fatalf("beside the admitted notice: taken %v, want only it", got)
			}
		})
	}
	t.Run("pending interaction", func(t *testing.T) {
		pending := probe
		pending.Pending = "?"
		tp := takeFor(t, []Op{notice}, pending)
		if tp.take != nil || len(tp.waits) != 1 || tp.waits[0].Reason != WaitPendingInteraction {
			t.Fatalf("taken %v, waits %+v; want a pending_interaction wait", takenOf(tp), tp.waits)
		}
	})
	t.Run("admitted op raises no wake", func(t *testing.T) {
		stopped := probe
		stopped.Running = false
		wake := p.op(laneMsg("wake", RoleNotice, "x", withWhen(WhenBoundary), from(admitted)))
		tp := takeFor(t, []Op{wake}, stopped)
		if tp.take != nil || tp.wake || len(tp.waits) != 1 || tp.waits[0].Reason != WaitNotRunning {
			t.Fatalf("taken %v, wake %v, waits %+v; want a not_running wait and no wake past the block", takenOf(tp), tp.wake, tp.waits)
		}
	})
	t.Run("no block", func(t *testing.T) {
		open := idleProbe()
		open.ContentAdmit = probe.ContentAdmit
		other := p.op(laneMsg("free", RoleNotice, "x", withWhen(WhenBoundary)))
		if tp := takeFor(t, []Op{other}, open); tp.take == nil {
			t.Fatalf("an admit list without a block held content: waits %+v", tp.waits)
		}
	})
}

func TestPlanTakeWake(t *testing.T) {
	p := newPlanner(t)
	need := p.op(laneMsg("a", RoleNotice, "x"))
	live := p.op(laneMsg("b", RoleNotice, "x", liveOnly))
	probe := idleProbe()
	probe.Running = false
	if tp := takeFor(t, []Op{need}, probe); !tp.wake || tp.take != nil {
		t.Fatalf("if_needed on a stopped runtime: %+v", tp)
	}
	if tp := takeFor(t, []Op{live}, probe); tp.wake {
		t.Fatal("live_only asked for a wake")
	}
}

func TestPlanTakeLimits(t *testing.T) {
	t.Run("one prompt", func(t *testing.T) {
		p := newPlanner(t)
		a := p.op(laneMsg("a", RolePrompt, "x"))
		n := p.op(laneMsg("n", RoleNotice, "x"))
		b := p.op(laneMsg("b", RolePrompt, "y"))
		tp := takeFor(t, []Op{a, n, b}, idleProbe())
		if got := takenOf(tp); !slices.Equal(got, []string{n.ID, a.ID}) || !tp.more {
			t.Fatalf("taken %v more %v", got, tp.more)
		}
		if w := waitsOf(tp.waits)[b.ID]; w != WaitBatchFull {
			t.Fatalf("second prompt wait %q", w)
		}
	})
	t.Run("99 ops", func(t *testing.T) {
		p := newPlanner(t)
		var ops []Op
		for i := range 100 {
			ops = append(ops, p.op(laneMsg("k"+strconv.Itoa(i), RoleNotice, "x")))
		}
		tp := takeFor(t, ops, idleProbe())
		if n := len(takenOf(tp)); n != MaxBatchOps || !tp.more {
			t.Fatalf("took %d, more %v", n, tp.more)
		}
	})
	t.Run("64 KiB", func(t *testing.T) {
		p := newPlanner(t)
		big := strings.Repeat("x", 15<<10)
		var ops []Op
		for i := range 6 {
			ops = append(ops, p.op(laneMsg("k"+strconv.Itoa(i), RoleNotice, big)))
		}
		tp := takeFor(t, ops, idleProbe())
		if n := len(takenOf(tp)); n != 4 || !tp.more {
			t.Fatalf("took %d ops of 15 KiB", n)
		}
	})
}

// inBandProbe is an in-band executor's probe whose text may run to maxText
// bytes.
func inBandProbe(maxText int) Probe {
	p := idleProbe()
	p.Caps = Caps{InBand: true, Input: FenceNone, MaxText: maxText}
	return p
}

// renderedLen is the length of ops' delivery text in the reminder style.
func renderedLen(t *testing.T, note bool, ops ...Op) int {
	t.Helper()
	text, err := RenderDelivery(ops, TextReminder, note)
	if err != nil {
		t.Fatal(err)
	}
	return len(text)
}

// TestPlanTakeTextBudget pins Caps.MaxText: a batch takes notices only while
// its rendered text fits, to the byte; the rest wait batch_full for the next
// pass; and a head whose text alone does not fit, the redelivery note
// counted, waits text_too_long and holds the content behind it.
func TestPlanTakeTextBudget(t *testing.T) {
	p := newPlanner(t)
	a := p.op(laneMsg("a", RoleNotice, strings.Repeat("a", 300)))
	b := p.op(laneMsg("b", RoleNotice, strings.Repeat("b", 300)))
	c := p.op(laneMsg("c", RoleNotice, strings.Repeat("c", 300)))
	two := renderedLen(t, false, a, b)
	t.Run("a backlog goes out in batches that fit", func(t *testing.T) {
		tp := takeFor(t, []Op{a, b, c}, inBandProbe(two))
		if got := takenOf(tp); !slices.Equal(got, []string{a.ID, b.ID}) || !tp.more {
			t.Fatalf("taken %v more %v, want the two notices that fit and another pass", got, tp.more)
		}
		if len(tp.take.text) != two {
			t.Fatalf("text is %d bytes, want the %d of the two notices", len(tp.take.text), two)
		}
		if w := waitsOf(tp.waits)[c.ID]; w != WaitBatchFull {
			t.Fatalf("third notice wait %q, want %q", w, WaitBatchFull)
		}
	})
	t.Run("a byte short of two takes one", func(t *testing.T) {
		tp := takeFor(t, []Op{a, b, c}, inBandProbe(two-1))
		if got := takenOf(tp); !slices.Equal(got, []string{a.ID}) || !tp.more {
			t.Fatalf("taken %v more %v, want the first notice alone", got, tp.more)
		}
	})
	t.Run("a head too long alone holds the rest", func(t *testing.T) {
		tp := takeFor(t, []Op{a, b}, inBandProbe(renderedLen(t, false, a)-1))
		if tp.take != nil {
			t.Fatalf("took %v, want nothing", takenOf(tp))
		}
		if w := waitsOf(tp.waits); w[a.ID] != WaitTextTooLong || w[b.ID] != WaitBlocked {
			t.Fatalf("waits %v, want the head %q and the notice behind it %q", w, WaitTextTooLong, WaitBlocked)
		}
		if tp.more {
			t.Fatal("more set with nothing taken")
		}
	})
	t.Run("a replay counts the redelivery note", func(t *testing.T) {
		replay := p.op(laneMsg("r", RoleNotice, strings.Repeat("r", 300)), func(op *Op) {
			op.Status, op.Attempt, op.MaybeDelivered, op.DeliveryKey = StatusAccepted, 1, 1, op.ID+":1"
		})
		if tp := takeFor(t, []Op{replay}, inBandProbe(renderedLen(t, true, replay))); !slices.Equal(takenOf(tp), []string{replay.ID}) || !tp.take.redelivery {
			t.Fatalf("taken %v, want the replay within a budget that fits its note", takenOf(tp))
		}
		tp := takeFor(t, []Op{replay}, inBandProbe(renderedLen(t, true, replay)-1))
		if tp.take != nil || waitsOf(tp.waits)[replay.ID] != WaitTextTooLong {
			t.Fatalf("taken %v waits %v, want the replay to wait %q: its note does not fit", takenOf(tp), waitsOf(tp.waits), WaitTextTooLong)
		}
	})
}

func TestPlanTakeClientSeqFixup(t *testing.T) {
	p := newPlanner(t)
	seq := func(n uint64, principal string) func(*Request) {
		return func(r *Request) { r.ClientSeq, r.Producer.Principal = n, principal }
	}
	a3 := p.op(laneMsg("a", RoleNotice, "x", seq(3, "gc:a")))
	other := p.op(laneMsg("o", RoleNotice, "x", seq(1, "gc:b")))
	plain := p.op(laneMsg("p", RoleNotice, "x", func(r *Request) { r.Producer.Principal = "gc:a" }))
	a1 := p.op(laneMsg("b", RoleNotice, "x", seq(1, "gc:a")))
	a2 := p.op(laneMsg("c", RoleNotice, "x", seq(2, "gc:a")))
	steer := p.op(laneMsg("s", RoleNotice, "x", seq(0, "gc:a"), withWhen(WhenSteer), seq(9, "gc:a")))
	rp := planRules(snapOf(a3, other, plain, a1, a2, steer), view(), t0.Add(time.Hour), "lane:ctl")
	got := candidatesOf(rp.content)
	want := []string{steer.ID, a1.ID, other.ID, plain.ID, a2.ID, a3.ID}
	if !slices.Equal(got, want) {
		t.Fatalf("order\n got %v\nwant %v", got, want)
	}
}

func TestPlanTakeKeys(t *testing.T) {
	p := newPlanner(t)
	head := p.op(laneMsg("a", RoleNotice, "x"))
	rest := p.op(laneMsg("b", RoleNotice, "y"))
	now := t0.Add(time.Hour)
	snap := snapOf(head, rest)
	b, _ := EncodeRuntimeRow("gc", RuntimeRow{Conversation: "gc-1", Generation: 1, Epoch: 1, Seq: 41})
	row, _ := DecodeRuntimeRow(b)
	snap.rt = runtimeSnap{read: true, found: true, bead: b, row: row}
	rp := planRules(snap, view(), now, "lane:ctl")
	tp, _ := planTake(rp, idleProbe(), view(), now)
	if tp.take.key != head.ID+":42" || tp.take.redelivery {
		t.Fatalf("fresh key %s", tp.take.key)
	}
	// A head whose key names another op gets its own fresh key.
	released := func(key string) func(*Op) {
		return func(op *Op) { op.Status, op.Attempt, op.DeliveryKey = StatusAccepted, 1, key }
	}
	h2 := p.op(laneMsg("c", RoleNotice, "x"), released(rest.ID+":7"))
	rp = planRules(snapOf(h2), view(), now, "lane:ctl")
	tp, _ = planTake(rp, idleProbe(), view(), now)
	if tp.take.key != h2.ID+":1" {
		t.Fatalf("key %s", tp.take.key)
	}
	// Replay keeps the key and takes only the group; a fresh op behind it
	// waits; the fresh batch stops before a maybe op.
	m1 := p.op(laneMsg("d", RoleNotice, "x"), released("gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa:3"), func(op *Op) { op.MaybeDelivered = 1 })
	m2 := p.op(laneMsg("e", RoleNotice, "y"), released("gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa:3"), func(op *Op) { op.MaybeDelivered = 1 })
	fresh := p.op(laneMsg("f", RoleNotice, "z"))
	rp = planRules(snapOf(m1, fresh, m2), view(), now, "lane:ctl")
	tp, _ = planTake(rp, idleProbe(), view(), now)
	if got := takenOf(tp); !slices.Equal(got, []string{m1.ID, m2.ID}) || tp.take.key != "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa:3" || !tp.take.redelivery || !tp.more {
		t.Fatalf("replay %v key %s", got, tp.take.key)
	}
	rp = planRules(snapOf(head, m1, fresh), view(), now, "lane:ctl")
	tp, _ = planTake(rp, idleProbe(), view(), now)
	if got := takenOf(tp); !slices.Equal(got, []string{head.ID}) {
		t.Fatalf("fresh batch %v", got)
	}
	// A re-take that never had an attempt may have been delivered too: the
	// fresh batch stops before it.
	ra := p.op(laneMsg("i", RoleNotice, "x"), func(op *Op) { op.Status = StatusExecuting })
	rp = planRules(snapOf(head, ra), view(), now, "lane:ctl")
	tp, _ = planTake(rp, idleProbe(), view(), now)
	if got := takenOf(tp); !slices.Equal(got, []string{head.ID}) || !tp.more {
		t.Fatalf("fresh batch %v, more %v", got, tp.more)
	}
	// After a key rejection the replay group gets a new key.
	kr := p.op(laneMsg("g", RoleNotice, "x"), released("gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa:3"), func(op *Op) {
		op.MaybeDelivered = 1
		op.LastError = lastErrorKeyRejected + ": x"
	})
	rp = planRules(snapOf(kr), view(), now, "lane:ctl")
	tp, _ = planTake(rp, idleProbe(), view(), now)
	if tp.take.key != kr.ID+":1" || !tp.take.redelivery {
		t.Fatalf("re-keyed replay %s", tp.take.key)
	}
	// A re-take keeps its key even when an earlier attempt's key was
	// refused: the expired attempt used the stored key (L3). The stored
	// key names lane_seq 3, which a fresh mint here (seq 1) cannot produce.
	rk := p.op(laneMsg("h", RoleNotice, "x"), executingWith(t0), func(op *Op) {
		op.Attempt, op.MaybeDelivered, op.LaneSeq = 2, 1, 3
		op.DeliveryKey = op.ID + ":3"
		op.LastError = lastErrorKeyRejected + ": x"
	})
	rp = planRules(snapOf(rk), view(), now, "lane:ctl")
	tp, _ = planTake(rp, idleProbe(), view(), now)
	if tp.take.key != rk.DeliveryKey || !tp.take.redelivery {
		t.Fatalf("re-take key %s, want %s", tp.take.key, rk.DeliveryKey)
	}
}

func TestPlanTakeFenceMismatchWaits(t *testing.T) {
	p := newPlanner(t)
	op := p.op(laneMsg("a", RoleNotice, "x"))
	probe := idleProbe()
	probe.Fence = Fence{Conversation: "gc-1", Generation: 2, Epoch: 1}
	tp := takeFor(t, []Op{op}, probe)
	// takeFor plans an hour after t0; a re-pass cannot clear the mismatch,
	// so it retries on a timer.
	want := t0.Add(time.Hour + TransientBackoffBase)
	if tp.take != nil || tp.waits[0].Reason != WaitIncarnation || !tp.waits[0].Until.Equal(want) || !tp.next.Equal(want) {
		t.Fatalf("plan %+v, want a wait until %s", tp, want)
	}
}

func TestPlanTakeRefusesBadCaps(t *testing.T) {
	for name, c := range map[string]Caps{
		"fence":     {Live: true, Input: "maybe"},
		"text":      {Live: true, Input: FenceAtomic, Text: "html"},
		"short ttl": {Live: true, Input: FenceAtomic, LeaseTTL: time.Second},
		"max text":  {Live: true, Input: FenceAtomic, MaxText: -1},
	} {
		probe := idleProbe()
		probe.Caps = c
		if _, err := planTake(rulesPlan{}, probe, view(), t0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTransientBackoff(t *testing.T) {
	for k, want := range map[int]time.Duration{0: 2 * time.Second, 1: 2 * time.Second, 2: 4 * time.Second, 8: 256 * time.Second, 9: 5 * time.Minute, 1000: 5 * time.Minute} {
		if got := transientBackoff(k); got != want {
			t.Errorf("transientBackoff(%d) = %s, want %s", k, got, want)
		}
	}
}

func TestLastErrorIsBounded(t *testing.T) {
	got := lastError("unknown", errorString(strings.Repeat("日\n", 400)))
	if err := validateLastError(got); err != nil {
		t.Fatalf("lastError produced an invalid value: %v", err)
	}
	if !strings.HasPrefix(got, "unknown: 日 ") {
		t.Fatalf("lastError %q", got)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// interruptReq is a lane interrupt of generation 1 that is still due an hour
// after t0.
func interruptReq() Request {
	return Request{
		Conversation: "gc-1", Kind: KindInterrupt, Executor: ExecutorLane, ClientKey: "i",
		Producer: Producer{Principal: "operator", Actor: "operator"},
		Scope:    Scope{Kind: ScopeSession, Generation: 1},
		StartBy:  t0.Add(2 * time.Hour), FinishBy: t0.Add(3 * time.Hour),
	}
}
