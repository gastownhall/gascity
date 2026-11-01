package opjournal

import (
	"slices"
	"testing"
	"time"
)

// nowReq is a deliver-now prompt on conversation gc-1. mods adjust the
// request.
func nowReq(key string, mods ...func(*Request)) Request {
	return laneMsg(key, RolePrompt, "stop and do this", append([]func(*Request){withWhen(WhenNow)}, mods...)...)
}

// nowTarget names the session generation and turn a deliver-now targets.
func nowTarget(gen int64, turn string) func(*Request) {
	return func(r *Request) { r.Policy.Message.NowGeneration, r.Policy.Message.NowTurn = gen, turn }
}

// nowID is m's derived interrupt id in namespace gc.
func nowID(t *testing.T, m Op) string {
	t.Helper()
	id, err := NowInterruptID("gc", m)
	if err != nil {
		t.Fatalf("NowInterruptID: %v", err)
	}
	return id
}

// lowered is deliver-now message m and its derived interrupt as a lowering
// at at in generation 1 leaves them: m accepted, the interrupt submitted.
func lowered(t *testing.T, m Op, at time.Time) (Op, Op) {
	t.Helper()
	return loweredIn(t, "gc", m, at)
}

// loweredIn is lowered by a lane of namespace prefix: the derived interrupt
// is pinned in prefix.
func loweredIn(t *testing.T, prefix string, m Op, at time.Time) (Op, Op) {
	t.Helper()
	i, err := deriveNowInterrupt(prefix, m, 1, at, "lane:ctl")
	if err != nil {
		t.Fatalf("deriveNowInterrupt: %v", err)
	}
	m.Status = StatusAccepted
	m.Transitions = appendTransition(m.Transitions, Transition{Status: StatusAccepted, At: at, By: "lane:ctl"})
	return m, i
}

// mintedIn is deliver-now message nowReq("a"), not yet lowered, with its id
// minted in namespace prefix, as it reads here once it moved with its id.
func mintedIn(t *testing.T, p *planner, prefix string) Op {
	t.Helper()
	m := p.op(nowReq("a"))
	id, err := DeriveID(prefix, m.Conversation, m.Producer.Principal, m.ClientKey)
	if err != nil {
		t.Fatalf("DeriveID: %v", err)
	}
	m.ID = id
	return m
}

// waitOf is op id's wait in ws, the zero wait when it has none.
func waitOf(ws []OpWait, id string) OpWait {
	i := slices.IndexFunc(ws, func(w OpWait) bool { return w.ID == id })
	if i < 0 {
		return OpWait{}
	}
	return ws[i]
}

// The lowering (L1 to L6): a deliver-now message waits as any op does, then
// until its derived id reads free and the conversation has a session awake,
// and is then lowered into exactly one derived interrupt (N1): M's producer,
// at most once, on_failure fail, scoped to M's target (or the effective
// incarnation's generation), no after, deadlines clamped to M's, written by
// the lane, with M moved to accepted and its own after untouched.
func TestPlanNowLowering(t *testing.T) {
	now := t0.Add(time.Second)
	for _, tc := range []struct {
		name    string
		req     Request
		view    func(*ConversationView)
		gen     int64
		turn    string
		startBy time.Time
	}{
		{name: "untargeted", req: nowReq("a"), gen: 1},
		{name: "untargeted on a newer row", req: nowReq("a"), view: func(v *ConversationView) { v.Generation = 4 }, gen: 4},
		{name: "targeted", req: nowReq("a", nowTarget(1, "t1")), gen: 1, turn: "t1"},
		{name: "target generation only", req: nowReq("a", nowTarget(3, "")), view: func(v *ConversationView) { v.Generation = 2 }, gen: 3},
		{name: "legacy interrupt", req: laneMsg("a", RolePrompt, "x", withWhen(WhenInterrupt)), gen: 1},
		{name: "start_by clamped to the message's", req: nowReq("a", func(r *Request) { r.StartBy = now.Add(30 * time.Second) }), gen: 1, startBy: now.Add(30 * time.Second)},
		{name: "with its own after", req: nowReq("a", func(r *Request) { r.After = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa" }), gen: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(t)
			m := p.op(tc.req, func(op *Op) { op.Correlation = "corr-1"; op.Producer.OwnerEpoch = 7 })
			snap := snapOf(m)
			if m.After != "" {
				snap.after[m.After] = afterTarget{state: afterClosed, arm: ArmSucceeded, conversation: "gc-1"}
			}
			v := view()
			if tc.view != nil {
				tc.view(&v)
			}
			rp := planRules(snap, v, now, "lane:ctl")
			if len(rp.lowerings) != 1 || rp.takePossible || len(rp.closes) != 0 || len(rp.waits) != 0 {
				t.Fatalf("lowerings %+v takePossible %v closes %v waits %v", rp.lowerings, rp.takePossible, closesOf(rp), rp.waits)
			}
			lw := rp.lowerings[0]
			i := lw.i
			if i.ID != nowID(t, m) || i.ClientKey != m.ID+":interrupt" || i.Kind != KindInterrupt || i.Executor != ExecutorLane {
				t.Fatalf("interrupt id/key/kind %s %s %s %s", i.ID, i.ClientKey, i.Kind, i.Executor)
			}
			if i.Producer != m.Producer || i.Causation != m.ID || i.Correlation != "corr-1" {
				t.Fatalf("interrupt producer %+v causation %s correlation %s", i.Producer, i.Causation, i.Correlation)
			}
			if i.Policy.Guarantee != GuaranteeAtMostOnce || i.Policy.Interrupt.OnFailure != OnFailureFail {
				t.Fatalf("interrupt policy %+v", i.Policy)
			}
			if i.Scope != (Scope{Kind: ScopeSession, Generation: tc.gen, Turn: tc.turn}) {
				t.Fatalf("interrupt scope %+v, want session(%d, %q)", i.Scope, tc.gen, tc.turn)
			}
			if i.After != "" || i.Payload != "" || i.ClientSeq != 0 || !i.NotBefore.IsZero() {
				t.Fatalf("interrupt after %q payload %q seq %d not_before %s", i.After, i.Payload, i.ClientSeq, i.NotBefore)
			}
			wantStart := tc.startBy
			if wantStart.IsZero() {
				wantStart = now.Add(time.Minute)
			}
			if !i.StartBy.Equal(wantStart) || !i.FinishBy.Equal(now.Add(3*time.Minute)) || !i.CreatedAt.Equal(now) {
				t.Fatalf("interrupt start_by %s finish_by %s created %s", i.StartBy, i.FinishBy, i.CreatedAt)
			}
			if i.Status != StatusSubmitted || len(i.Transitions) != 1 || i.Transitions[0] != (Transition{Status: StatusSubmitted, At: now, By: "lane:ctl"}) {
				t.Fatalf("interrupt status %s transitions %+v", i.Status, i.Transitions)
			}
			if i.PayloadDigest != RequestDigest(i) {
				t.Fatal("interrupt digest is not its request's")
			}
			if _, err := EncodeOp(i); err != nil {
				t.Fatalf("interrupt does not encode: %v", err)
			}
			if lw.mNext.Status != StatusAccepted || lw.mNext.After != m.After || lw.mNext.Transitions[len(lw.mNext.Transitions)-1] != (Transition{Status: StatusAccepted, At: now, By: "lane:ctl"}) {
				t.Fatalf("message next %+v", lw.mNext)
			}
			if _, err := opUpdateItem(lw.m, lw.mNext); err != nil {
				t.Fatalf("message patch: %v", err)
			}
		})
	}

	// L1 to L4: no lowering, and the waits and refusals.
	for _, tc := range []struct {
		name  string
		req   Request
		view  func(*ConversationView)
		row   *nowRow
		taken bool
		close string
		wait  WaitReason
		until bool
		wake  bool
	}{
		{name: "L1 unreadable", req: nowReq("a"), row: &nowRow{state: afterUnreadable, err: errFlakyRead}, wait: WaitNowUnreadable, until: true},
		{name: "L1 undecodable", req: nowReq("a"), row: &nowRow{state: afterUnreadable, err: ErrUnsupportedSchema, undecodable: true}, wait: WaitNowUnreadable},
		{name: "L2 found", req: nowReq("a"), row: &nowRow{state: afterClosed, op: Op{Status: StatusClosed}}, close: "failed{refused:now_interrupt_taken}"},
		{name: "L2 open", req: nowReq("a"), taken: true, close: "failed{refused:now_interrupt_taken}"},
		{name: "L3 no session", req: nowReq("a"), view: func(v *ConversationView) { v.Generation = 0 }, wait: WaitNoSession, wake: true},
		{name: "L3 no session, live only", req: nowReq("a", liveOnly), view: func(v *ConversationView) { v.Generation = 0 }, wait: WaitNoSession},
		{name: "L4 dormant", req: nowReq("a"), view: func(v *ConversationView) { v.Dormant = true }, wait: WaitDormant, wake: true},
		{name: "L4 dormant, live only", req: nowReq("a", liveOnly), view: func(v *ConversationView) { v.Dormant = true }, wait: WaitDormant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(t)
			m := p.op(tc.req)
			snap := snapOf(m)
			id := nowID(t, m)
			if tc.row != nil {
				snap.nowRows[id] = *tc.row
			}
			if tc.taken {
				delete(snap.nowRows, id)
				snap.quarantined = append(snap.quarantined, QuarantinedRow{ID: id})
				snap.openIDs[id] = true
			}
			v := view()
			if tc.view != nil {
				tc.view(&v)
			}
			rp := planRules(snap, v, now, "lane:ctl")
			if len(rp.lowerings) != 0 {
				t.Fatalf("lowered %+v", rp.lowerings)
			}
			if got := closesOf(rp)[m.ID]; got != tc.close {
				t.Fatalf("close %q, want %q", got, tc.close)
			}
			w := waitOf(rp.waits, m.ID)
			if w.Reason != tc.wait || w.Until.IsZero() == tc.until || rp.wake != tc.wake {
				t.Fatalf("wait %+v wake %v, want %s (timer %v) wake %v", w, rp.wake, tc.wait, tc.until, tc.wake)
			}
			if tc.until && !w.Until.Equal(now.Add(TransientBackoffBase)) {
				t.Fatalf("until %s", w.Until)
			}
		})
	}
}

// Pairing: a lowered message and its open derived interrupt are one control
// entry (a unit); once the interrupt is closed the message is urgent content
// marked by the interrupt's outcome; an interrupt whose message is gone
// closes (canceled with it, the cascade); and an interrupt with the derived
// key shape that is no deliver-now message's stays a plain interrupt.
func TestPlanNowPairing(t *testing.T) {
	now := t0.Add(time.Second)
	t.Run("unit", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		rp := planRules(snapOf(m, i), view(), now, "lane:ctl")
		if len(rp.control) != 1 || rp.control[0].unit == nil || rp.control[0].op.ID != m.ID || rp.control[0].unit.op.ID != i.ID || len(rp.content) != 0 {
			t.Fatalf("control %+v content %+v", rp.control, rp.content)
		}
		if rp.control[0].urgent || !rp.takePossible || len(rp.waits) != 0 || len(rp.closes) != 0 {
			t.Fatalf("unit entry %+v takePossible %v waits %v closes %v", rp.control[0], rp.takePossible, rp.waits, closesOf(rp))
		}
	})
	for _, tc := range []struct {
		name    string
		outcome *Outcome // nil: missing
		want    string
	}{
		{"succeeded", &Outcome{Arm: ArmSucceeded}, ""},
		{"stale", &Outcome{Arm: ArmSkipped, Reason: ReasonStale}, ""},
		{"canceled", &Outcome{Arm: ArmCanceled}, ""},
		{"unsupported", &Outcome{Arm: ArmFailed, Reason: ReasonUnsupported}, DegradationInterruptUnsupported},
		{"no boundary", &Outcome{Arm: ArmFailed, Reason: ReasonNoBoundary}, DegradationInterruptFailed},
		{"not prepared", &Outcome{Arm: ArmFailed, Reason: ReasonReplacementNotPrepared}, DegradationInterruptFailed},
		{"ambiguous", &Outcome{Arm: ArmFailed, Reason: ReasonAmbiguousDelivery}, DegradationInterruptFailed},
		{"expired before start", &Outcome{Arm: ArmFailed, Reason: ReasonExpiredBeforeStart}, DegradationInterruptFailed},
		{"provider unavailable", &Outcome{Arm: ArmFailed, Reason: ReasonProviderUnavailable}, DegradationInterruptFailed},
		{"missing", nil, DegradationInterruptFailed},
	} {
		t.Run("post-unit "+tc.name, func(t *testing.T) {
			p := newPlanner(t)
			m, i := lowered(t, p.op(nowReq("a")), now)
			snap := snapOf(m)
			if tc.outcome != nil {
				i.Status, i.Outcome = StatusClosed, *tc.outcome
				snap.nowRows[i.ID] = nowRow{state: afterClosed, op: i}
			}
			rp := planRules(snap, view(), now, "lane:ctl")
			if len(rp.content) != 1 || len(rp.control) != 0 {
				t.Fatalf("control %+v content %+v", rp.control, rp.content)
			}
			e := rp.content[0]
			if e.op.ID != m.ID || !e.urgent || !e.postUnit || e.nowDegradation != tc.want || !e.candidate() {
				t.Fatalf("post-unit entry %+v, want degradation %q", e, tc.want)
			}
		})
	}
	t.Run("interrupt quarantined or unreadable", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		snap := snapOf(m)
		snap.openIDs[i.ID] = true
		snap.quarantined = []QuarantinedRow{{ID: i.ID}}
		rp := planRules(snap, view(), now, "lane:ctl")
		if w := waitOf(rp.waits, m.ID); w.Reason != WaitNowUnreadable || !w.Until.IsZero() || len(rp.content)+len(rp.control) != 0 {
			t.Fatalf("quarantined: wait %+v control %v content %v", w, rp.control, rp.content)
		}
		snap = snapOf(m)
		snap.nowRows[i.ID] = nowRow{state: afterUnreadable, err: errFlakyRead}
		rp = planRules(snap, view(), now, "lane:ctl")
		if w := waitOf(rp.waits, m.ID); w.Reason != WaitNowUnreadable || !w.Until.Equal(now.Add(TransientBackoffBase)) || w.Detail != errFlakyRead.Error() {
			t.Fatalf("unreadable: wait %+v", w)
		}
	})
	for _, tc := range []struct {
		name    string
		m       func(Op) *nowRow // nil row: m open in the read
		want    string
		cascade bool
	}{
		{name: "message canceled", m: func(m Op) *nowRow {
			m.Status, m.Outcome, m.Cancel = StatusClosed, Outcome{Arm: ArmCanceled}, Cancel{By: "operator", At: t0}
			return &nowRow{state: afterClosed, op: m}
		}, want: "canceled{}", cascade: true},
		{name: "message succeeded", m: func(m Op) *nowRow {
			m.Status, m.Outcome = StatusClosed, Outcome{Arm: ArmSucceeded}
			return &nowRow{state: afterClosed, op: m}
		}, want: "skipped{not_needed}"},
		{name: "message missing", m: func(Op) *nowRow { return &nowRow{state: afterMissing} }, want: "skipped{not_needed}"},
	} {
		t.Run("orphan "+tc.name, func(t *testing.T) {
			p := newPlanner(t)
			m, i := lowered(t, p.op(nowReq("a")), now)
			snap := snapOf(i)
			snap.nowRows[m.ID] = *tc.m(m)
			rp := planRules(snap, view(), now, "lane:ctl")
			if got := closesOf(rp)[i.ID]; got != tc.want {
				t.Fatalf("close %q, want %q", got, tc.want)
			}
			if c := rp.closes[0].next; tc.cascade && (c.Cancel.By != "lane:cascade" || !c.Cancel.At.Equal(now) || c.Cancel.Reason == "") {
				t.Fatalf("cascade cancel %+v", c.Cancel)
			}
			if _, err := EncodeOp(rp.closes[0].next); err != nil {
				t.Fatalf("orphan close does not encode: %v", err)
			}
		})
	}
	t.Run("the cascade replaces a rule close", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		i.StartBy = now.Add(-time.Millisecond)
		m.Status, m.Outcome, m.Cancel = StatusClosed, Outcome{Arm: ArmCanceled}, Cancel{By: "operator", At: now}
		snap := snapOf(i)
		snap.nowRows[m.ID] = nowRow{state: afterClosed, op: m}
		rp := planRules(snap, view(), now, "lane:ctl")
		if got := closesOf(rp)[i.ID]; got != "canceled{}" || len(rp.closes) != 1 || rp.closes[0].next.Cancel.By != "lane:cascade" {
			t.Fatalf("closes %v, want the cascade over expired_before_start", closesOf(rp))
		}
	})
	t.Run("an interrupt that may have run keeps its close", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		lease := Lease{Lane: "conversation", Conversation: "gc-1", Generation: 1, Nonce: "x.1", Deadline: now.Add(-LeaseSkew - time.Second)}
		i.Status, i.Attempt, i.LaneSeq, i.Lease = StatusExecuting, 1, 1, lease
		m.Status, m.Attempt, m.LaneSeq, m.Lease, m.DeliveryKey = StatusExecuting, 1, 2, lease, m.ID+":2"
		m.Cancel = Cancel{By: "operator", At: now}
		rp := planRules(snapOf(m, i), view(), now, "lane:ctl")
		if c := closesOf(rp); c[m.ID] != "canceled{}" || c[i.ID] != "failed{ambiguous_delivery}" {
			t.Fatalf("closes %v", c)
		}
	})
	t.Run("orphan message unreadable", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		snap := snapOf(i)
		snap.nowRows[m.ID] = nowRow{state: afterUnreadable, err: errFlakyRead}
		rp := planRules(snap, view(), now, "lane:ctl")
		if w := waitOf(rp.waits, i.ID); w.Reason != WaitNowUnreadable || w.Until.IsZero() || len(rp.closes) != 0 {
			t.Fatalf("wait %+v closes %v", w, closesOf(rp))
		}
	})
	t.Run("plain interrupt with the derived key shape", func(t *testing.T) {
		p := newPlanner(t)
		boundary := p.op(laneMsg("b", RolePrompt, "x"))
		plain := p.op(Request{
			Conversation: "gc-1", Kind: KindInterrupt, Executor: ExecutorLane, ClientKey: boundary.ID + ":interrupt",
			Producer: boundary.Producer, Scope: Scope{Kind: ScopeSession, Generation: 1}, Causation: boundary.ID,
		})
		rp := planRules(snapOf(boundary, plain), view(), now, "lane:ctl")
		if len(rp.control) != 1 || rp.control[0].op.ID != plain.ID || rp.control[0].unit != nil || len(rp.closes) != 0 {
			t.Fatalf("control %+v closes %v", rp.control, closesOf(rp))
		}
		// Another principal's interrupt under a deliver-now message's key
		// shape is not its derived interrupt.
		m := p.op(nowReq("c"))
		foreign := p.op(Request{
			Conversation: "gc-1", Kind: KindInterrupt, Executor: ExecutorLane, ClientKey: m.ID + ":interrupt",
			Producer: Producer{Principal: "operator", Actor: ActorOperator}, Scope: Scope{Kind: ScopeSession, Generation: 1}, Causation: m.ID,
		})
		m.Status = StatusAccepted
		snap := snapOf(m, foreign)
		snap.nowRows[nowID(t, m)] = nowRow{state: afterMissing}
		rp = planRules(snap, view(), now, "lane:ctl")
		if len(rp.control) != 1 || rp.control[0].op.ID != foreign.ID || rp.control[0].unit != nil {
			t.Fatalf("control %+v", rp.control)
		}
		if len(rp.content) != 1 || !rp.content[0].postUnit {
			t.Fatalf("content %+v", rp.content)
		}
	})
	t.Run("unlowered message beside an open derived row", func(t *testing.T) {
		p := newPlanner(t)
		m := p.op(nowReq("a"))
		_, i := lowered(t, m, now)
		rp := planRules(snapOf(m, i), view(), now, "lane:ctl")
		closes := closesOf(rp)
		if closes[m.ID] != "failed{refused:now_interrupt_taken}" || closes[i.ID] != "skipped{not_needed}" || len(rp.lowerings) != 0 {
			t.Fatalf("closes %v lowerings %v", closes, rp.lowerings)
		}
	})
	t.Run("closing together", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		closed := view()
		closed.Closed = true
		rp := planRules(snapOf(m, i), closed, now, "lane:ctl")
		if c := closesOf(rp); c[m.ID] != "skipped{conversation_closed}" || c[i.ID] != "skipped{conversation_closed}" || len(rp.closes) != 2 {
			t.Fatalf("closed conversation: closes %v", c)
		}
		// The generation moved: the interrupt is stale and the message,
		// unscoped, goes post-unit at once.
		moved := view()
		moved.Generation = 2
		rp = planRules(snapOf(m, i), moved, now, "lane:ctl")
		if c := closesOf(rp); c[i.ID] != "skipped{stale}" || len(rp.closes) != 1 {
			t.Fatalf("moved generation: closes %v", c)
		}
		if len(rp.content) != 1 || !rp.content[0].postUnit || rp.content[0].nowDegradation != "" {
			t.Fatalf("moved generation: content %+v", rp.content)
		}
	})
	t.Run("expired unit", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		lease := Lease{Lane: "conversation", Conversation: "gc-1", Generation: 1, Nonce: "x.1", Deadline: now.Add(-LeaseSkew - time.Second)}
		i.Status, i.Attempt, i.LaneSeq, i.Lease = StatusExecuting, 1, 1, lease
		m.Status, m.Attempt, m.LaneSeq, m.Lease, m.DeliveryKey = StatusExecuting, 1, 2, lease, m.ID+":2"
		rp := planRules(snapOf(m, i), view(), now, "lane:ctl")
		if c := closesOf(rp); c[i.ID] != "failed{ambiguous_delivery}" || len(rp.closes) != 1 {
			t.Fatalf("closes %v", c)
		}
		if len(rp.content) != 1 {
			t.Fatalf("content %+v", rp.content)
		}
		e := rp.content[0]
		if !e.postUnit || !e.retake || e.cur.MaybeDelivered != 1 || e.nowDegradation != DegradationInterruptFailed {
			t.Fatalf("post-unit replay %+v", e)
		}
		if g := rp.groups[m.ID+":2"]; len(g) != 1 || g[0].ID != m.ID {
			t.Fatalf("replay group %+v", rp.groups)
		}
	})
	t.Run("maybe delivered message never forms a unit", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		m.Attempt, m.MaybeDelivered, m.DeliveryKey = 1, 1, m.ID+":2"
		rp := planRules(snapOf(m, i), view(), now, "lane:ctl")
		if c := closesOf(rp); c[i.ID] != "failed{ambiguous_delivery}" {
			t.Fatalf("closes %v", c)
		}
		if len(rp.control) != 0 || len(rp.content) != 1 || !rp.content[0].postUnit || rp.content[0].nowDegradation != DegradationInterruptFailed {
			t.Fatalf("control %+v content %+v", rp.control, rp.content)
		}
	})
	t.Run("waits are the unit's", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		i.Scope.Generation = 5
		rp := planRules(snapOf(m, i), view(), now, "lane:ctl")
		wi := waitOf(rp.waits, i.ID)
		wm := waitOf(rp.waits, m.ID)
		if wi.Reason != WaitScopeFuture || wm.Reason != WaitScopeFuture || len(rp.control) != 0 {
			t.Fatalf("scope future: waits %v control %v", rp.waits, rp.control)
		}
		// A unit released transient waits its backoff as a blocking waiter.
		m2, i2 := lowered(t, p.op(nowReq("b")), now)
		i2.Status, i2.Attempt, i2.RetryAt = StatusAccepted, 1, now.Add(5*time.Second)
		rp = planRules(snapOf(m2, i2), view(), now, "lane:ctl")
		if len(rp.control) != 1 || !rp.control[0].blocks || rp.control[0].wait != WaitRetryBackoff || !rp.next.Equal(now.Add(5*time.Second)) {
			t.Fatalf("backoff: control %+v next %s", rp.control, rp.next)
		}
		if wm := waitOf(rp.waits, m2.ID); wm.Reason != WaitRetryBackoff {
			t.Fatalf("backoff: message wait %+v", wm)
		}
	})
}

// The unit's plan (U1 to U9) against each probe: the waits name both ops,
// the plan-time closes close only the interrupt, a unit never reports Busy,
// and a takeable unit is one Take of [I, M] with M's key at its own lane_seq
// and M's text alone.
func TestPlanUnitTable(t *testing.T) {
	now := t0.Add(time.Second)
	busy := func(turn string) func(*Probe) {
		return func(p *Probe) { p.Boundary, p.TurnLive, p.Turn = TriNo, TriYes, turn }
	}
	for _, tc := range []struct {
		name   string
		target string
		probe  func(*Probe)
		wait   WaitReason
		until  bool
		stale  bool
		path   nowPath
	}{
		{name: "U1 not live", probe: func(p *Probe) { p.Caps.Live = false }, wait: WaitNotLive},
		{name: "U1b text too long", probe: func(p *Probe) { p.Caps.MaxText = 3 }, wait: WaitTextTooLong},
		{name: "U2 not running", probe: func(p *Probe) { p.Running = false }, stale: true},
		{name: "U3 content block", probe: func(p *Probe) {
			p.ContentBlock = "draining"
			p.ContentAdmit = []ContentClass{{Role: RoleNotice, When: WhenBoundary, Principal: "gc:mail"}}
		}, wait: WaitContentBlock},
		{name: "U4 pending", probe: func(p *Probe) { p.Pending = "req-1" }, wait: WaitPendingInteraction},
		{name: "U5 boundary unknown", probe: func(p *Probe) { p.Boundary = TriUnknown }, wait: WaitBoundaryUnknown, until: true},
		{name: "U6 idle", path: nowDirect},
		{name: "U6 idle, turn unknown", probe: func(p *Probe) { p.TurnLive = TriUnknown }, path: nowDirect},
		{name: "U7 busy, no turn live", probe: func(p *Probe) { p.Boundary, p.TurnLive = TriNo, TriNo }, stale: true},
		{name: "U7 a newer turn runs", target: "t1", probe: busy("t2"), stale: true},
		{name: "U8 fused", probe: func(p *Probe) { busy("t1")(p); p.Caps.FusedNow = true }, path: nowFused},
		{name: "U9 busy", probe: busy("t1"), path: nowReplace},
		{name: "U9 the target runs", target: "t1", probe: busy("t1"), path: nowReplace},
		{name: "U9 idle but a turn is live", probe: func(p *Probe) { p.TurnLive, p.Turn = TriYes, "t1" }, path: nowReplace},
		{name: "U9 busy, turn unknown", probe: func(p *Probe) { p.Boundary, p.TurnLive = TriNo, TriUnknown }, path: nowReplace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(t)
			req := nowReq("a")
			if tc.target != "" {
				req = nowReq("a", nowTarget(1, tc.target))
			}
			m, i := lowered(t, p.op(req), now)
			rp := planRules(snapOf(m, i), view(), now, "lane:ctl")
			rp.baseSeq = 7
			probe := idleProbe()
			if tc.probe != nil {
				tc.probe(&probe)
			}
			tp, err := planTake(rp, probe, view(), now)
			if err != nil {
				t.Fatal(err)
			}
			if tp.busy {
				t.Fatal("a unit reported busy")
			}
			if tc.wait != "" {
				wi := waitOf(tp.waits, i.ID)
				wm := waitOf(tp.waits, m.ID)
				if tp.take != nil || wi.Reason != tc.wait || wm.Reason != tc.wait || wi.Until.IsZero() == tc.until {
					t.Fatalf("take %v waits %+v, want %s for both", tp.take, tp.waits, tc.wait)
				}
				if tc.until && !tp.next.Equal(now.Add(TransientBackoffBase)) {
					t.Fatalf("next %s", tp.next)
				}
				return
			}
			if tc.stale {
				if tp.take != nil || len(tp.closes) != 1 || tp.closes[0].cur.ID != i.ID || tp.closes[0].next.Outcome != (Outcome{Arm: ArmSkipped, Reason: ReasonStale}) || !tp.more {
					t.Fatalf("take %v closes %+v more %v", tp.take, tp.closes, tp.more)
				}
				return
			}
			s := tp.take
			if s == nil || s.kind != TakeNow || s.now != tc.path || len(s.ops) != 2 || s.ops[0].op.ID != i.ID || s.ops[1].op.ID != m.ID {
				t.Fatalf("take %+v, want a %v unit", s, tc.path)
			}
			text, _ := RenderDelivery([]Op{m}, probe.Caps.Text, false)
			if s.key != m.ID+":9" || s.mode != LinkStarts || s.redelivery || s.steer || s.text != text || len(tp.closes) != 0 {
				t.Fatalf("take key %s mode %s text %q", s.key, s.mode, s.text)
			}
		})
	}
}

// Ordering: a unit is a control Take in its message's arrival order among
// the control ops, and while one is open no content is taken (U-ORDER), a
// steer included; a unit waiting out its backoff holds later control, while
// one waiting on a pending interaction lets a later respond go.
func TestPlanNowOrdering(t *testing.T) {
	now := t0.Add(time.Second)
	p := newPlanner(t)
	before := p.op(laneInterrupt("before", ""))
	m, i := lowered(t, p.op(nowReq("a")), now)
	after := p.op(laneInterrupt("after", ""))
	steer := p.op(laneMsg("s", RolePrompt, "x", withWhen(WhenSteer)))
	normal := p.op(laneMsg("n", RoleNotice, "x"))
	rp := planRules(snapOf(after, m, i, before, steer, normal), view(), now, "lane:ctl")
	if got := []string{rp.control[0].op.ID, rp.control[1].op.ID, rp.control[2].op.ID}; !slices.Equal(got, []string{before.ID, m.ID, after.ID}) {
		t.Fatalf("control order %v, want before, the unit, after", got)
	}
	tp, err := planTake(rp, idleProbe(), view(), now)
	if err != nil {
		t.Fatal(err)
	}
	if tp.take == nil || tp.take.kind != TakeControl || tp.take.ops[0].op.ID != before.ID || !tp.more {
		t.Fatalf("take %+v more %v, want the earlier interrupt", tp.take, tp.more)
	}
	for _, id := range []string{steer.ID, normal.ID} {
		if w := waitOf(tp.waits, id); w.Reason != WaitBlocked {
			t.Fatalf("content %s wait %+v, want blocked by the open unit", id, w)
		}
	}
	// Without the earlier interrupt the unit goes before the steer and the
	// normal content.
	rp = planRules(snapOf(after, m, i, steer, normal), view(), now, "lane:ctl")
	tp, _ = planTake(rp, idleProbe(), view(), now)
	if tp.take == nil || tp.take.kind != TakeNow || !tp.more {
		t.Fatalf("take %+v more %v, want the unit", tp.take, tp.more)
	}
	// A unit waiting on a pending interaction lets a later respond go, its
	// occurrence read raised and unresolved.
	respond := p.op(respondReq("req-1#1", "gc", RespondPayload{Action: "approve"}))
	rp = planRules(snapOf(m, i, respond), view(), now, "lane:ctl")
	rp.raisings = map[string]raisingState{respond.ID: {request: "req-1", raised: true}}
	pending := idleProbe()
	pending.Pending, pending.Caps.Respond = "req-1", true
	tp, _ = planTake(rp, pending, view(), now)
	if tp.take == nil || tp.take.ops[0].op.ID != respond.ID {
		t.Fatalf("take %+v, want the respond past the waiting unit", tp.take)
	}
	// A unit waiting out its backoff blocks the control after it.
	m2, i2 := lowered(t, p.op(nowReq("b")), now)
	i2.Status, i2.Attempt, i2.RetryAt = StatusAccepted, 1, now.Add(time.Second)
	later := p.op(laneInterrupt("later", ""))
	rp = planRules(snapOf(m2, i2, later), view(), now, "lane:ctl")
	tp, _ = planTake(rp, idleProbe(), view(), now)
	if tp.take != nil {
		t.Fatalf("took %+v past a unit's backoff", tp.take.ops)
	}
	if w := waitOf(tp.waits, later.ID); w.Reason != WaitBlocked {
		t.Fatalf("later interrupt wait %+v", w)
	}
}

// A post-unit message goes alone, at a boundary only, ahead of normal
// content, and carries its interrupt's mark unless its delivery has its own.
func TestPlanNowPostUnitContent(t *testing.T) {
	now := t0.Add(time.Second)
	p := newPlanner(t)
	notice := p.op(laneMsg("n", RoleNotice, "x"))
	m, i := lowered(t, p.op(nowReq("a")), now)
	i.Status, i.Outcome = StatusClosed, Outcome{Arm: ArmFailed, Reason: ReasonUnsupported}
	snap := snapOf(notice, m)
	snap.nowRows[i.ID] = nowRow{state: afterClosed, op: i}
	rp := planRules(snap, view(), now, "lane:ctl")
	tp, err := planTake(rp, idleProbe(), view(), now)
	if err != nil {
		t.Fatal(err)
	}
	if tp.take == nil || tp.take.kind != TakeContent || len(tp.take.ops) != 1 || tp.take.ops[0].op.ID != m.ID || tp.take.degradation != DegradationInterruptUnsupported || !tp.more {
		t.Fatalf("take %+v more %v, want the message alone", tp.take, tp.more)
	}
	busy := idleProbe()
	busy.Boundary, busy.TurnLive = TriNo, TriYes
	tp, _ = planTake(rp, busy, view(), now)
	if w := waitOf(tp.waits, m.ID); tp.take != nil || w.Reason != WaitBusy || !tp.busy {
		t.Fatalf("busy: take %v wait %+v busy %v", tp.take, w, tp.busy)
	}
	inBand := idleProbe()
	inBand.Caps.Live, inBand.Caps.InBand = false, true
	tp, _ = planTake(rp, inBand, view(), now)
	if w := waitOf(tp.waits, m.ID); w.Reason != WaitNotLive {
		t.Fatalf("in band: wait %+v", w)
	}
	// A fresh batch stops before a solo message that is not its head.
	steer := p.op(laneMsg("s", RoleNotice, "x", withWhen(WhenSteer)))
	m2, i2 := lowered(t, p.op(nowReq("b")), now)
	i2.Status, i2.Outcome = StatusClosed, Outcome{Arm: ArmSucceeded}
	steer2 := p.op(laneMsg("s2", RoleNotice, "y", withWhen(WhenSteer)))
	snap = snapOf(steer, m2, steer2)
	snap.nowRows[i2.ID] = nowRow{state: afterClosed, op: i2}
	rp = planRules(snap, view(), now, "lane:ctl")
	tp, _ = planTake(rp, idleProbe(), view(), now)
	if tp.take == nil || len(tp.take.ops) != 1 || tp.take.ops[0].op.ID != steer.ID || tp.take.degradation != "" {
		t.Fatalf("take %+v, want the earlier steer alone", tp.take)
	}
}

// U-ORDER (N3) holds every content candidate while a lowered message's
// derived interrupt is open, whatever either op waits on, so no content
// starts a turn the unit's interrupt would then cut; a lowered message's
// own after is never read again (its target closed succeeded before the
// lowering, and a closed op is immutable). An interrupt this build cannot
// decode (quarantined) holds nothing: this build cannot tell whether the
// unit is open, and holding the lane behind a row it cannot read would stall
// all content until the message's start_by.
func TestPlanNowUOrderHolds(t *testing.T) {
	now := t0.Add(time.Second)
	take := func(t *testing.T, snap laneSnapshot) takePlan {
		t.Helper()
		rp := planRules(snap, view(), now, "lane:ctl")
		tp, err := planTake(rp, idleProbe(), view(), now)
		if err != nil {
			t.Fatal(err)
		}
		return tp
	}
	held := func(t *testing.T, tp takePlan, n Op) {
		t.Helper()
		if tp.take != nil && tp.take.kind == TakeContent {
			t.Fatalf("took content %+v while the unit is open", tp.take.ops)
		}
		if w := waitOf(tp.waits, n.ID); w.Reason != WaitBlocked {
			t.Fatalf("content %s wait %+v, want blocked by the open unit", n.ID, w)
		}
	}
	t.Run("a lowered message's after is not read again", func(t *testing.T) {
		p := newPlanner(t)
		x := p.op(laneMsg("x", RoleNotice, "earlier"))
		m, i := lowered(t, p.op(nowReq("a", func(r *Request) { r.After = x.ID })), now)
		n := p.op(laneMsg("n", RolePrompt, "later"))
		snap := snapOf(m, i, n)
		snap.after[x.ID] = afterTarget{state: afterUnreadable, err: errFlakyRead}
		tp := take(t, snap)
		if tp.take == nil || tp.take.kind != TakeNow {
			t.Fatalf("take %+v, want the unit: its after was satisfied at the lowering", tp.take)
		}
		held(t, tp, n)
		// An unlowered message still waits on its after.
		u := p.op(nowReq("b", func(r *Request) { r.After = x.ID }))
		snap = snapOf(u)
		snap.after[x.ID] = afterTarget{state: afterUnreadable, err: errFlakyRead}
		rp := planRules(snap, view(), now, "lane:ctl")
		if w := waitOf(rp.waits, u.ID); w.Reason != WaitAfterUnreadable || len(rp.lowerings) != 0 {
			t.Fatalf("unlowered: wait %+v lowerings %v", w, rp.lowerings)
		}
	})
	t.Run("the interrupt waits for a future scope", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		i.Scope.Generation = 5
		n := p.op(laneMsg("n", RolePrompt, "later"))
		tp := take(t, snapOf(m, i, n))
		if tp.take != nil {
			t.Fatalf("take %+v", tp.take)
		}
		held(t, tp, n)
	})
	t.Run("the interrupt is open but not listed", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		n := p.op(laneMsg("n", RolePrompt, "later"))
		snap := snapOf(m, n)
		snap.nowRows[i.ID] = nowRow{state: afterClosed, op: i}
		rp := planRules(snap, view(), now, "lane:ctl")
		// The message waits to read again shortly, never post-unit while its
		// interrupt is open (N2).
		if w := waitOf(rp.waits, m.ID); w.Reason != WaitNowUnreadable || !w.Until.Equal(now.Add(TransientBackoffBase)) {
			t.Fatalf("message wait %+v", w)
		}
		if slices.ContainsFunc(rp.content, func(e entry) bool { return e.op.ID == m.ID }) {
			t.Fatalf("message is content %+v while its interrupt is open", rp.content)
		}
		tp := take(t, snap)
		if tp.take != nil {
			t.Fatalf("take %+v", tp.take)
		}
		held(t, tp, n)
	})
	t.Run("the interrupt's read failed", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		n := p.op(laneMsg("n", RolePrompt, "later"))
		snap := snapOf(m, n)
		snap.nowRows[i.ID] = nowRow{state: afterUnreadable, err: errFlakyRead}
		held(t, take(t, snap), n)
	})
	t.Run("a quarantined interrupt holds nothing", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		n := p.op(laneMsg("n", RolePrompt, "later"))
		snap := snapOf(m, n)
		snap.openIDs[i.ID] = true
		snap.quarantined = []QuarantinedRow{{ID: i.ID}}
		tp := take(t, snap)
		if tp.take == nil || tp.take.kind != TakeContent || tp.take.ops[0].op.ID != n.ID {
			t.Fatalf("take %+v, want the content", tp.take)
		}
	})
	t.Run("an interrupt closing this pass holds nothing", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		i.StartBy = now.Add(-time.Millisecond)
		n := p.op(laneMsg("n", RolePrompt, "later"))
		rp := planRules(snapOf(m, i, n), view(), now, "lane:ctl")
		if len(rp.openUnits) != 0 {
			t.Fatalf("open units %v with the interrupt closing", rp.openUnits)
		}
	})
}

// The partner reads (mutation guards): an interrupt open by its own row but
// not listed keeps its message off the content (above); a message open by
// its row but not listed keeps its interrupt open, waiting to read again;
// a quarantined message keeps its interrupt waiting, never plain; and a
// foreign op open at the derived id sends the message post-unit, marked.
func TestPlanNowPartnerReads(t *testing.T) {
	now := t0.Add(time.Second)
	t.Run("the message is open but not listed", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		snap := snapOf(i)
		snap.nowRows[m.ID] = nowRow{state: afterClosed, op: m}
		rp := planRules(snap, view(), now, "lane:ctl")
		if w := waitOf(rp.waits, i.ID); w.Reason != WaitNowUnreadable || !w.Until.Equal(now.Add(TransientBackoffBase)) || len(rp.closes) != 0 || len(rp.control) != 0 {
			t.Fatalf("wait %+v closes %v control %+v", w, closesOf(rp), rp.control)
		}
	})
	t.Run("the message is quarantined", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		snap := snapOf(i)
		snap.openIDs[m.ID] = true
		snap.quarantined = []QuarantinedRow{{ID: m.ID}}
		rp := planRules(snap, view(), now, "lane:ctl")
		if w := waitOf(rp.waits, i.ID); w.Reason != WaitNowUnreadable || !w.Until.IsZero() || len(rp.closes) != 0 || len(rp.control) != 0 {
			t.Fatalf("wait %+v closes %v control %+v", w, closesOf(rp), rp.control)
		}
	})
	t.Run("a foreign op at the derived id", func(t *testing.T) {
		p := newPlanner(t)
		m, i := lowered(t, p.op(nowReq("a")), now)
		foreign := i
		foreign.Causation = ""
		rp := planRules(snapOf(m, foreign), view(), now, "lane:ctl")
		if len(rp.content) != 1 || rp.content[0].op.ID != m.ID || !rp.content[0].postUnit || rp.content[0].nowDegradation != DegradationInterruptFailed || !rp.content[0].candidate() {
			t.Fatalf("content %+v, want the message post-unit, marked", rp.content)
		}
		if len(rp.openUnits) != 0 {
			t.Fatalf("open units %v", rp.openUnits)
		}
	})
}

// A lane interrupt may be a derived interrupt, its causation read as its
// deliver-now message, only when that causation is an op id, of any
// namespace: pairing goes by identity, not by this store's prefix. Any other
// causation makes it plain.
func TestPlanNowShape(t *testing.T) {
	for _, tc := range []struct {
		causation string
		want      bool
	}{
		{"gc-op-abcdefghijklmnopqrstuvwxyz", true},
		{"gc-op-abcdefghijklmnopqrstuvwxy2", true},
		{"other-op-abcdefghijklmnopqrstuvwxyz", true},
		{"gc-12", false},
		{"t3-msg-42", false},
		{"gc-op-short", false},
	} {
		op := Op{Kind: KindInterrupt, Executor: ExecutorLane, Causation: tc.causation, ClientKey: tc.causation + ":interrupt"}
		if got := hasNowShape(op); got != tc.want {
			t.Errorf("hasNowShape(causation %q) = %v, want %v", tc.causation, got, tc.want)
		}
	}
}

// The drain reminder the 2026-10-19 ruling admits past a content block is
// taken while an open unit waits on that block; content the block holds
// still waits, and the unit stays open.
func TestPlanNowUOrderAdmitsPastTheBlock(t *testing.T) {
	now := t0.Add(time.Second)
	p := newPlanner(t)
	m, i := lowered(t, p.op(nowReq("a")), now)
	drain := p.op(laneMsg("drain", RoleNotice, "please run drain-ack", func(r *Request) { r.Producer.Principal = "gc:planner/drain" }))
	other := p.op(laneMsg("other", RoleNotice, "x"))
	rp := planRules(snapOf(m, i, drain, other), view(), now, "lane:ctl")
	probe := idleProbe()
	probe.ContentBlock = "draining"
	probe.ContentAdmit = []ContentClass{{Role: RoleNotice, When: WhenBoundary, Principal: "gc:planner/drain"}}
	tp, err := planTake(rp, probe, view(), now)
	if err != nil {
		t.Fatal(err)
	}
	if tp.take == nil || tp.take.kind != TakeContent || len(tp.take.ops) != 1 || tp.take.ops[0].op.ID != drain.ID {
		t.Fatalf("take %+v, want the admitted drain notice alone", tp.take)
	}
	for _, id := range []string{m.ID, i.ID, other.ID} {
		if w := waitOf(tp.waits, id); w.Reason != WaitContentBlock {
			t.Fatalf("%s wait %+v, want content_block", id, w)
		}
	}
	// A unit waiting on anything else (here its backoff) still holds the
	// admitted notice.
	i.Status, i.Attempt, i.RetryAt = StatusAccepted, 1, now.Add(time.Second)
	rp = planRules(snapOf(m, i, drain), view(), now, "lane:ctl")
	tp, _ = planTake(rp, probe, view(), now)
	if w := waitOf(tp.waits, drain.ID); tp.take != nil || w.Reason != WaitBlocked {
		t.Fatalf("take %+v drain wait %+v with the unit in its backoff", tp.take, w)
	}
}

// A deliver-now message and its derived interrupt pair by identity, not by
// this store's namespace. A message that moved here with its id from
// another namespace forms its unit whether this lane lowered it (its
// interrupt pinned in this namespace) or a lane of its own namespace did
// (pinned in that one), and an open interrupt pinned in a third namespace
// pairs too. While the interrupt is open, whatever the unit waits on, it
// holds later content (U-ORDER) and the message never goes ahead of it; a
// cancel of the message cascades to the interrupt; once the interrupt is
// closed, by a rule in this pass or by its own row, the message is
// post-unit, marked with the interrupt's own outcome.
func TestPlanNowForeignPrefixUnit(t *testing.T) {
	now := t0.Add(time.Second)
	// moved is deliver-now message m with its id in namespace msgNS,
	// lowered by a lane of namespace laneNS.
	moved := func(t *testing.T, p *planner, msgNS, laneNS string) (Op, Op) {
		t.Helper()
		m, i := loweredIn(t, laneNS, mintedIn(t, p, msgNS), now)
		if !hasNowShape(i) || !isDerivedOf(i, m) {
			t.Fatalf("hasNowShape %v isDerivedOf %v, want %s the derived interrupt of %s", hasNowShape(i), isDerivedOf(i, m), i.ID, m.ID)
		}
		return m, i
	}
	type shape struct{ name, msgNS, laneNS string }
	lowered := []shape{{"lowered here", "old", "gc"}, {"lowered in its namespace", "old", "old"}}
	for _, s := range append(lowered, shape{"lowered in a third namespace", "gc", "old"}) {
		for _, tc := range []struct {
			name string
			mod  func(*Op)
			// wait is the unit's wait, on both ops; "" for a candidate.
			wait WaitReason
		}{
			{"a candidate", func(*Op) {}, ""},
			{"in its backoff", func(i *Op) { i.Status, i.Attempt, i.RetryAt = StatusAccepted, 1, now.Add(time.Second) }, WaitRetryBackoff},
			{"for a future scope", func(i *Op) { i.Scope.Generation = 5 }, WaitScopeFuture},
			{"in flight", executingWith(now.Add(time.Minute)), WaitInFlight},
		} {
			t.Run(s.name+": the interrupt is open: "+tc.name, func(t *testing.T) {
				p := newPlanner(t)
				m, i := moved(t, p, s.msgNS, s.laneNS)
				tc.mod(&i)
				n := p.op(laneMsg("n", RolePrompt, "later"))
				rp := planRules(snapOf(m, i, n), view(), now, "lane:ctl")
				if cs := closesOf(rp); len(cs) != 0 {
					t.Fatalf("closes %v, want none", cs)
				}
				if !slices.Contains(rp.openUnits, m.ID) || slices.ContainsFunc(rp.content, func(e entry) bool { return e.op.ID == m.ID }) {
					t.Fatalf("open units %v content %+v, want the unit open and the message no content", rp.openUnits, rp.content)
				}
				if wi, wm := waitOf(rp.waits, i.ID), waitOf(rp.waits, m.ID); wi.Reason != tc.wait || wm.Reason != tc.wait {
					t.Fatalf("waits %+v and %+v, want the unit's %q on both", wi, wm, tc.wait)
				}
				if !rp.takePossible {
					if tc.wait != WaitInFlight {
						t.Fatal("no Take possible")
					}
					return
				}
				tp, err := planTake(rp, idleProbe(), view(), now)
				if err != nil {
					t.Fatal(err)
				}
				if w := waitOf(tp.waits, n.ID); w.Reason != WaitBlocked {
					t.Fatalf("later content wait %+v, want blocked by the open unit", w)
				}
				switch {
				case tc.wait == "":
					if tp.take == nil || tp.take.kind != TakeNow || !slices.Equal(candidatesOf(tp.take.ops), []string{i.ID, m.ID}) {
						t.Fatalf("take %+v, want the unit [%s %s]", tp.take, i.ID, m.ID)
					}
				case tp.take != nil:
					t.Fatalf("take %+v while the unit waits %s", tp.take, tc.wait)
				}
			})
		}
		for _, tc := range []struct {
			name string
			// snap is the read once the message is canceled: closed by its
			// producer (Store.Cancel closes an accepted lane op), or closing
			// by a rule this pass.
			snap func(m, i Op) laneSnapshot
		}{
			{"by its producer", func(m, i Op) laneSnapshot {
				m.Status, m.Outcome, m.Cancel = StatusClosed, Outcome{Arm: ArmCanceled}, Cancel{By: "operator", At: now}
				snap := snapOf(i)
				snap.nowRows[m.ID] = nowRow{state: afterClosed, op: m}
				return snap
			}},
			{"by a rule this pass", func(m, i Op) laneSnapshot {
				m.Cancel = Cancel{By: "operator", At: now}
				return snapOf(m, i)
			}},
		} {
			t.Run(s.name+": the message is canceled "+tc.name, func(t *testing.T) {
				p := newPlanner(t)
				m, i := moved(t, p, s.msgNS, s.laneNS)
				i.Status, i.Attempt, i.RetryAt = StatusAccepted, 1, now.Add(time.Second)
				rp := planRules(tc.snap(m, i), view(), now, "lane:ctl")
				if got := closesOf(rp)[i.ID]; got != "canceled{}" {
					t.Fatalf("interrupt close %q, want the cascade (closes %v)", got, closesOf(rp))
				}
				for _, c := range rp.closes {
					if c.cur.ID == i.ID && c.next.Cancel.By != nowCascadeBy {
						t.Fatalf("interrupt cancel %+v, want %s", c.next.Cancel, nowCascadeBy)
					}
				}
				if slices.ContainsFunc(rp.control, func(e entry) bool { return slices.Contains(entryIDs(e), i.ID) }) {
					t.Fatalf("control %+v, want the canceled message's interrupt never run", rp.control)
				}
			})
		}
	}
	// A closed interrupt is read where this lane or the message's own
	// namespace pinned it.
	for _, s := range lowered {
		for _, tc := range []struct {
			name string
			// snap is the read: the interrupt closing by a rule this pass,
			// or closed by its row.
			snap        func(m, i Op) laneSnapshot
			degradation string
		}{
			{"closes at its start_by", func(m, i Op) laneSnapshot {
				i.StartBy = now.Add(-time.Millisecond)
				return snapOf(m, i)
			}, DegradationInterruptFailed},
			{"closes canceled", func(m, i Op) laneSnapshot {
				i.Cancel = Cancel{By: "operator", At: now}
				return snapOf(m, i)
			}, ""},
			{"closed unsupported", func(m, i Op) laneSnapshot {
				snap := snapOf(m)
				snap.nowRows[i.ID] = nowRow{state: afterClosed, op: closedAt(i, Outcome{Arm: ArmFailed, Reason: ReasonUnsupported}, "lane:ctl", now)}
				return snap
			}, DegradationInterruptUnsupported},
			{"closed succeeded", func(m, i Op) laneSnapshot {
				snap := snapOf(m)
				snap.nowRows[i.ID] = nowRow{state: afterClosed, op: closedAt(i, Outcome{Arm: ArmSucceeded}, "lane:ctl", now)}
				return snap
			}, ""},
		} {
			t.Run(s.name+": the interrupt "+tc.name, func(t *testing.T) {
				p := newPlanner(t)
				m, i := moved(t, p, s.msgNS, s.laneNS)
				rp := planRules(tc.snap(m, i), view(), now, "lane:ctl")
				if len(rp.content) != 1 || rp.content[0].op.ID != m.ID || !rp.content[0].postUnit || !rp.content[0].candidate() || rp.content[0].nowDegradation != tc.degradation {
					t.Fatalf("content %+v, want the message post-unit marked %q", rp.content, tc.degradation)
				}
				if w := waitOf(rp.waits, m.ID); w.Reason != "" || slices.Contains(rp.openUnits, m.ID) {
					t.Fatalf("message wait %+v open units %v, want neither", w, rp.openUnits)
				}
			})
		}
	}
}

// A deliver-now message not yet lowered is refused (now_interrupt_taken),
// and never lowered, while its derived interrupt's key is taken: by a row at
// either id the interrupt may have here (this namespace's, the message's
// own) or by an open derived interrupt pinned anywhere, whatever the message
// waits on. Only a lowering mints that key, and a second interrupt minted
// beside one that may have been sent could cut a turn twice. The interrupt
// already there closes as an orphan, or waits while it is in flight. A read
// that failed at either id holds the lowering, as this namespace's does
// (L1).
func TestPlanNowUnloweredDerivedKeyTaken(t *testing.T) {
	now := t0.Add(time.Second)
	type pin struct{ name, msgNS, pinNS string }
	here := pin{"pinned here", "old", "gc"}
	own := pin{"pinned in its own namespace", "old", "old"}
	third := pin{"pinned in a third namespace", "gc", "old"}
	keep := func(*Op) {}
	inFlight := executingWith(now.Add(time.Minute))
	notDue := func(m *Op) { m.NotBefore = now.Add(time.Minute) }
	for _, s := range []pin{here, own, third} {
		for _, tc := range []struct {
			name string
			// i and m adjust the interrupt and the message.
			i, m func(*Op)
			// close is the interrupt's close; "" while it is in flight.
			close string
		}{
			{"open", keep, keep, "skipped{not_needed}"},
			{"in flight", inFlight, keep, ""},
			{"open, the message not yet due", keep, notDue, "skipped{not_needed}"},
			{"in flight, the message not yet due", inFlight, notDue, ""},
		} {
			t.Run(s.name+": "+tc.name, func(t *testing.T) {
				p := newPlanner(t)
				m := mintedIn(t, p, s.msgNS)
				_, i := loweredIn(t, s.pinNS, m, now)
				tc.i(&i)
				tc.m(&m)
				rp := planRules(snapOf(m, i), view(), now, "lane:ctl")
				cs := closesOf(rp)
				if len(rp.lowerings) != 0 || cs[m.ID] != "failed{refused:now_interrupt_taken}" {
					t.Fatalf("lowerings %+v closes %v, want %s refused, never lowered beside %s", rp.lowerings, cs, m.ID, i.ID)
				}
				if cs[i.ID] != tc.close {
					t.Fatalf("interrupt close %q, want %q", cs[i.ID], tc.close)
				}
				if w := waitOf(rp.waits, i.ID); tc.close == "" && w.Reason != WaitInFlight {
					t.Fatalf("interrupt wait %+v, want in flight", w)
				}
			})
		}
	}
	for _, s := range []pin{here, own} {
		t.Run(s.name+": closed", func(t *testing.T) {
			p := newPlanner(t)
			m := mintedIn(t, p, s.msgNS)
			_, i := loweredIn(t, s.pinNS, m, now)
			snap := snapOf(m)
			snap.nowRows[i.ID] = nowRow{state: afterClosed, op: closedAt(i, Outcome{Arm: ArmSucceeded}, "lane:ctl", now)}
			rp := planRules(snap, view(), now, "lane:ctl")
			if cs := closesOf(rp); len(rp.lowerings) != 0 || cs[m.ID] != "failed{refused:now_interrupt_taken}" {
				t.Fatalf("lowerings %+v closes %v, want %s refused", rp.lowerings, cs, m.ID)
			}
		})
		for _, tc := range []struct {
			name        string
			undecodable bool
		}{
			{"unreadable", false},
			{"undecodable", true},
		} {
			t.Run(s.name+": "+tc.name, func(t *testing.T) {
				p := newPlanner(t)
				m := mintedIn(t, p, s.msgNS)
				_, i := loweredIn(t, s.pinNS, m, now)
				snap := snapOf(m)
				snap.nowRows[i.ID] = nowRow{state: afterUnreadable, err: errFlakyRead, undecodable: tc.undecodable}
				rp := planRules(snap, view(), now, "lane:ctl")
				if len(rp.lowerings) != 0 || len(rp.closes) != 0 {
					t.Fatalf("lowerings %+v closes %v, want %s waiting its read", rp.lowerings, closesOf(rp), m.ID)
				}
				w := waitOf(rp.waits, m.ID)
				if w.Reason != WaitNowUnreadable || w.Until.IsZero() != tc.undecodable || slices.Contains(rp.openUnits, m.ID) == tc.undecodable {
					t.Fatalf("wait %+v open units %v, want now_unreadable, held and on a timer only while the read may recover", w, rp.openUnits)
				}
			})
		}
	}
}

// Content the probe admits past a content block goes only while every open
// unit waits on that block: one unit waiting on a future scope still holds
// it.
func TestPlanNowAdmitNeedsEveryUnitBlocked(t *testing.T) {
	now := t0.Add(time.Second)
	p := newPlanner(t)
	m1, i1 := lowered(t, p.op(nowReq("a")), now)
	m2, i2 := lowered(t, p.op(nowReq("b")), now)
	i2.Scope.Generation = 5
	drain := p.op(laneMsg("drain", RoleNotice, "please run drain-ack", func(r *Request) { r.Producer.Principal = "gc:planner/drain" }))
	rp := planRules(snapOf(m1, i1, m2, i2, drain), view(), now, "lane:ctl")
	probe := idleProbe()
	probe.ContentBlock = "draining"
	probe.ContentAdmit = []ContentClass{{Role: RoleNotice, When: WhenBoundary, Principal: "gc:planner/drain"}}
	tp, err := planTake(rp, probe, view(), now)
	if err != nil {
		t.Fatal(err)
	}
	if tp.take != nil && slices.ContainsFunc(tp.take.ops, func(e entry) bool { return e.op.ID == drain.ID }) {
		t.Fatalf("admitted notice taken while unit %s waits a future scope", m2.ID)
	}
	if w := waitOf(tp.waits, drain.ID); w.Reason != WaitBlocked {
		t.Fatalf("notice wait %+v, want blocked", w)
	}
}

// A due deliver-now message not yet lowered because a read failed
// transiently (its derived id's, or its after target's) holds content as a
// lowered unit does (U-ORDER); one whose row will never decode holds
// nothing.
func TestPlanNowUnloweredUnreadableHoldsContent(t *testing.T) {
	now := t0.Add(time.Second)
	for _, tc := range []struct {
		name        string
		after       bool
		undecodable bool
		holds       bool
	}{
		{"transient read", false, false, true},
		{"undecodable row", false, true, false},
		{"transient after read", true, false, true},
		{"undecodable after target", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(t)
			var m, n Op
			var snap laneSnapshot
			if tc.after {
				x := p.op(laneMsg("x", RoleNotice, "earlier"))
				m = p.op(nowReq("a", func(r *Request) { r.After = x.ID }))
				n = p.op(laneMsg("n", RolePrompt, "later"))
				snap = snapOf(m, n)
				snap.after[x.ID] = afterTarget{state: afterUnreadable, err: errFlakyRead, undecodable: tc.undecodable}
			} else {
				m = p.op(nowReq("a"))
				n = p.op(laneMsg("n", RolePrompt, "later"))
				snap = snapOf(m, n)
				snap.nowRows[nowID(t, m)] = nowRow{state: afterUnreadable, err: errFlakyRead, undecodable: tc.undecodable}
			}
			rp := planRules(snap, view(), now, "lane:ctl")
			if w := waitOf(rp.waits, m.ID); tc.after && w.Reason != WaitAfterUnreadable {
				t.Fatalf("message wait %+v, want after_unreadable", w)
			}
			if got := slices.Contains(rp.openUnits, m.ID); got != tc.holds {
				t.Fatalf("open units %v, want %s held %v", rp.openUnits, m.ID, tc.holds)
			}
			tp, err := planTake(rp, idleProbe(), view(), now)
			if err != nil {
				t.Fatal(err)
			}
			taken := tp.take != nil && slices.ContainsFunc(tp.take.ops, func(e entry) bool { return e.op.ID == n.ID })
			if taken == tc.holds {
				t.Fatalf("content %s taken %v, want %v (take %+v waits %+v)", n.ID, taken, !tc.holds, tp.take, tp.waits)
			}
		})
	}
}

// A forward pointer (skipped{coalesced_into}, a re-lowering's close) is
// never a unit's outcome: read at a message's own pin with nothing at the
// store's pin, the message goes post-unit marked interrupt_failed, as with a
// missing interrupt; with the interrupt closed succeeded at the store's pin,
// that row decides and the message goes unmarked (RL7).
func TestPlanNowForwardPointerIsNoOutcome(t *testing.T) {
	now := t0.Add(time.Second)
	for _, tc := range []struct {
		name string
		// home is the row at the store's pin; nil reads missing.
		home        func(i Op, home string) *Op
		degradation string
	}{
		{"the home row is missing", func(Op, string) *Op { return nil }, DegradationInterruptFailed},
		{"the home row closed succeeded", func(i Op, home string) *Op {
			c := i
			c.ID = home
			c = closedAt(c, Outcome{Arm: ArmSucceeded}, "lane:ctl", now)
			return &c
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(t)
			m, i := loweredIn(t, "old", mintedIn(t, p, "old"), now)
			home := nowID(t, m)
			snap := snapOf(m)
			snap.nowRows[i.ID] = nowRow{state: afterClosed, op: closedAt(i, Outcome{Arm: ArmSkipped, Reason: ReasonCoalescedInto, Result: Result{CoalescedInto: home}}, "gc:storage-migrate", now)}
			if r := tc.home(i, home); r != nil {
				snap.nowRows[home] = nowRow{state: afterClosed, op: *r}
			}
			rp := planRules(snap, view(), now, "lane:ctl")
			if len(rp.content) != 1 || rp.content[0].op.ID != m.ID || !rp.content[0].postUnit || rp.content[0].nowDegradation != tc.degradation {
				t.Fatalf("content %+v, want the message post-unit marked %q", rp.content, tc.degradation)
			}
		})
	}
}
