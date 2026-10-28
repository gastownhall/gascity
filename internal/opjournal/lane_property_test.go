package opjournal

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// propSeeds is the default seed count; GC_LANE_PROPERTY_SEEDS raises it
	// for a soak run (1024 seeds take about 15 CPU-minutes).
	propSeeds = 64
	propSteps = 400
)

// propSeedCount is propSeeds, or GC_LANE_PROPERTY_SEEDS when set.
func propSeedCount(t *testing.T) uint64 {
	v := os.Getenv("GC_LANE_PROPERTY_SEEDS")
	if v == "" {
		return propSeeds
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 {
		t.Fatalf("GC_LANE_PROPERTY_SEEDS=%q is not a positive count", v)
	}
	return n
}

var propConversations = []string{"gc-1", "gc-2"}

// propPrefixes are the namespaces the world's ops are minted in: the lanes'
// own, then the one some ops moved here from with their ids.
var propPrefixes = []string{"gc", "old"}

// propConv is one conversation's world: its view and the agent's side.
type propConv struct {
	view  ConversationView
	dedup bool
	// transcript counts how often each op's content reached the agent.
	transcript map[string]int
	// receipts are the keys a deduplicating runtime holds.
	receipts map[string]bool
}

// propWorld drives two lanes over one store with a scripted runtime and
// checks the lane's invariants after every step.
type propWorld struct {
	t     *testing.T
	r     *rand.Rand
	h     *laneHarness
	lanes []*ConversationLane
	// old is a store of namespace propPrefixes[1] over the same beads, and
	// oldLane a lane of it, which never passes: it only lowers.
	old        *Store
	oldLane    *ConversationLane
	convs      map[string]*propConv
	restricted bool
	keys       int
	ids        []string

	// The runtime's script for the current pass.
	probe       Probe
	contentRes  func(d Delivery) (DeliveryResult, bool)
	controlRes  func() ControlResult
	pendingHook func()

	// Invariant bookkeeping.
	outcomes  map[string]Outcome
	seqs      map[string]map[uint64]string // conv → lane_seq → op:attempt
	maxSeq    map[string]uint64
	seenTake  map[string]bool // op:attempt
	rowSeen   map[string]RuntimeRow
	ambiguous map[string]bool // at_most_once ops whose effect may have happened
	// lastSend is the op ids of the last send under each key.
	lastSend map[string][]string
	convOf   map[string]string
	decoded  map[string]Op
	cov      *propCoverage
	// keyOfAttempt is the key each op's attempt was delivered under.
	keyOfAttempt map[string]map[int]string
	// rejectedKeys are the keys a runtime refused.
	rejectedKeys map[string]bool
	// lost are the ops already reported as lost.
	lost       map[string]bool
	violations []string
	// answers counts the respond effects that may have answered, by
	// occurrence (conversation/generation/occurrence): at most one each.
	answers map[string]int

	// Flaky reads: while flaky is set, a store Get fails transiently, most
	// often on an after target; while afterDown is set, every read of an
	// after target fails. fr draws the failures.
	flaky        bool
	afterDown    bool
	afterTargets map[string]bool
	fr           *rand.Rand

	// Deliver-now: nowMsgs maps each deliver-now message to its
	// conversation; sentInterrupt holds the derived interrupts an attempt
	// may have run; lastStep is each conversation's last replacement
	// step's result.
	nowMsgs       map[string]string
	nowIDs        []string // nowMsgs' keys, in proposal order
	sentInterrupt map[string]bool
	lastStep      map[string]ControlResult
	// lineage maps each interrupt a re-lowering created to the row it
	// copied.
	lineage map[string]string
}

func (w *propWorld) fail(format string, args ...any) {
	w.violations = append(w.violations, fmt.Sprintf(format, args...))
}

func newPropWorld(t *testing.T, seed uint64) *propWorld {
	h, fs := newLaneHarnessFlaky(t)
	w := &propWorld{
		t: t, r: rand.New(rand.NewPCG(seed, seed*0x9e3779b97f4a7c15+1)), h: h,
		convs: map[string]*propConv{}, restricted: seed%2 == 1,
		outcomes: map[string]Outcome{}, seqs: map[string]map[uint64]string{}, maxSeq: map[string]uint64{},
		seenTake: map[string]bool{}, rowSeen: map[string]RuntimeRow{}, ambiguous: map[string]bool{},
		lastSend: map[string][]string{}, convOf: map[string]string{}, decoded: map[string]Op{},
		keyOfAttempt: map[string]map[int]string{}, rejectedKeys: map[string]bool{}, lost: map[string]bool{},
		afterTargets: map[string]bool{}, fr: rand.New(rand.NewPCG(seed+77, seed)),
		answers: map[string]int{},
		nowMsgs: map[string]string{}, sentInterrupt: map[string]bool{}, lastStep: map[string]ControlResult{},
		lineage: map[string]string{},
	}
	fs.setFail(func(id string) bool {
		if w.afterDown && w.afterTargets[id] {
			return true
		}
		return w.flaky && (w.afterTargets[id] && w.fr.IntN(2) == 0 || w.fr.IntN(25) == 0)
	})
	w.lanes = []*ConversationLane{h.lane(t, "ctl"), h.lane(t, "cli")}
	old, err := NewLaneStore(beads.SessionStore{Store: fs}, h.clk, WithIDPrefix(propPrefixes[1]))
	if err != nil {
		t.Fatalf("NewLaneStore: %v", err)
	}
	if w.oldLane, err = NewLane(old, propPrefixes[1], h.clk); err != nil {
		t.Fatalf("NewLane: %v", err)
	}
	w.old = old
	for i, id := range propConversations {
		w.convs[id] = &propConv{
			view:  ConversationView{ID: id, Generation: 1, Epoch: 1, Token: "tok"},
			dedup: i == 0, transcript: map[string]int{}, receipts: map[string]bool{},
		}
		w.seqs[id] = map[uint64]string{}
	}
	return w
}

// executor is the scripted runtime as an Executor.
type propExec struct{ w *propWorld }

func (e propExec) Probe(_ context.Context, v ConversationView) (Probe, error) {
	p := e.w.probe
	p.Caps.Dedup = e.w.convs[v.ID].dedup
	return p, nil
}

func (e propExec) Deliver(_ context.Context, d Delivery) DeliveryResult {
	w := e.w
	conv := w.convs[d.Fence.Conversation]
	w.checkInFlight(d.Fence.Conversation, d.Ops)
	w.checkNowDelivery(d)
	assertNoIdentifiersQuiet(w, d)
	wantNote := d.Redelivery && !conv.dedup
	if n := strings.Count(d.Text, RedeliveryNote); (wantNote && n != 1) || (!wantNote && n != 0) {
		w.fail("delivery %s: note appears %d times (redelivery %v dedup %v)", d.Key, n, d.Redelivery, conv.dedup)
	}
	ids := make([]string, len(d.Ops))
	for i, op := range d.Ops {
		ids[i] = op.ID
		if op.Policy.Guarantee == GuaranteeAtMostOnce && w.ambiguous[op.ID] {
			w.fail("at_most_once op %s delivered again after an ambiguous attempt", op.ID)
		}
		// L3: a redelivery reuses the key of the attempt that may have
		// delivered, unless the runtime refused that key.
		if m := op.MaybeDelivered; m > 0 {
			if k := w.keyOfAttempt[op.ID][m]; k != "" && !w.rejectedKeys[k] && k != d.Key {
				w.fail("op %s redelivered under %s; its maybe-delivered attempt %d used %s (L3)", op.ID, d.Key, m, k)
			}
		}
		if w.keyOfAttempt[op.ID] == nil {
			w.keyOfAttempt[op.ID] = map[int]string{}
		}
		w.keyOfAttempt[op.ID][op.Attempt] = d.Key
	}
	// L2, on a deduplicating runtime: every send under a key is a subset of
	// every earlier send under it, else the runtime drops the new content.
	if prev, ok := w.lastSend[d.Key]; ok && conv.dedup {
		for _, id := range ids {
			if !slices.Contains(prev, id) {
				w.fail("key %s carries op %s outside its earlier send %v (L2)", d.Key, id, prev)
			}
		}
	}
	w.lastSend[d.Key] = ids
	w.cov.add("delivery")
	if d.Redelivery {
		w.cov.add("redelivery")
		if conv.dedup && conv.receipts[d.Key] {
			w.cov.add("replay_dedup")
		}
	}
	if strings.Contains(d.Text, RedeliveryNote) {
		w.cov.add("note")
	}
	res, landed := w.contentRes(d)
	// The executor contract (Rejected): a runtime refuses only a key that
	// never landed, and answers one it already applied as Delivered.
	if rej, ok := res.(Rejected); ok && rej.KeyRejected && conv.receipts[d.Key] {
		res = Delivered{Record: DeliveryRecord{At: w.h.clk.Now(), Ack: AckDelivered}}
		landed = true
	}
	if landed {
		if !conv.dedup || !conv.receipts[d.Key] {
			for _, id := range ids {
				conv.transcript[id]++
			}
		}
		conv.receipts[d.Key] = true
	}
	rej, isRej := res.(Rejected)
	if isRej && rej.KeyRejected {
		w.rejectedKeys[d.Key] = true
		w.cov.add("key_rejected")
	}
	for _, op := range d.Ops {
		_, nd := res.(NotDelivered)
		if !nd && (!isRej || !rej.KeyRejected) && op.Policy.Guarantee == GuaranteeAtMostOnce {
			w.ambiguous[op.ID] = true
		}
	}
	if w.pendingHook != nil {
		w.pendingHook()
	}
	return res
}

func (e propExec) Interrupt(_ context.Context, c InterruptCall) ControlResult {
	w := e.w
	if !c.ForReplacement {
		return w.controlRes()
	}
	w.cov.add("now_replace")
	w.checkInFlight(c.Fence.Conversation, []Op{c.Op})
	if w.sentInterrupt[c.Op.ID] || w.sentInterrupt[w.lineage[c.Op.ID]] {
		w.fail("derived interrupt %s sent again after an attempt that may have run (N5)", c.Op.ID)
	}
	if target := c.Op.Scope.Turn; target != "" && c.Turn != target {
		w.fail("derived interrupt %s aimed at %q, not its target %q", c.Op.ID, c.Turn, target)
	}
	if target := c.Op.Scope.Turn; target != "" && w.probe.TurnLive == TriYes && w.probe.Turn != "" && w.probe.Turn != target {
		w.fail("derived interrupt %s for turn %s sent while %s runs (T5)", c.Op.ID, target, w.probe.Turn)
	}
	res := w.controlRes()
	if _, nd := res.(NotDelivered); !nd {
		w.sentInterrupt[c.Op.ID] = true
	}
	w.lastStep[c.Fence.Conversation] = res
	return res
}

// Respond answers as scripted. A respond is at most once and the first
// effective answer wins, so an occurrence gets at most one effect that may
// have answered it: a second would answer a prompt it was not meant for.
func (e propExec) Respond(_ context.Context, c RespondCall) ControlResult {
	r := e.w.controlRes()
	switch r.(type) {
	case Settled, Unknown:
		e.w.cov.add("respond_answered")
		occ := fmt.Sprintf("%s/%d/%s", c.Op.Conversation, c.Op.Scope.Generation, c.Interaction)
		if e.w.answers[occ]++; e.w.answers[occ] > 1 {
			e.w.fail("occurrence %s answered %d times (op %s attempt %d)", occ, e.w.answers[occ], c.Op.ID, c.Op.Attempt)
		}
	}
	return r
}

// unflaky runs fn with the store's reads healthy.
func (w *propWorld) unflaky(fn func()) {
	flaky, down := w.flaky, w.afterDown
	w.flaky, w.afterDown = false, false
	defer func() { w.flaky, w.afterDown = flaky, down }()
	fn()
}

// checkNowDelivery is N2, N3 and N4 at a delivery: a deliver-now message
// goes in its unit (alone, its interrupt under the same lease, never maybe
// delivered, with no turn at the probe or after a step that settled at a
// boundary) or post-unit (alone, at a boundary, its interrupt closed); and
// no other content goes while a unit of the conversation is open, but what
// the probe admits past its content block.
func (w *propWorld) checkNowDelivery(d Delivery) {
	conv := d.Fence.Conversation
	w.unflaky(func() {
		unit := false
		for _, op := range d.Ops {
			if !isNowMessage(op) {
				continue
			}
			i, err := w.nowInterruptOf(op)
			iid := i.ID
			inUnit := err == nil && i.Status == StatusExecuting && i.Lease == op.Lease
			if len(d.Ops) != 1 {
				w.fail("deliver-now message %s delivered with %d other ops", op.ID, len(d.Ops)-1)
			}
			switch {
			case inUnit:
				unit = true
				w.cov.add("now_unit")
				if opIDPrefix(op.ID) != propPrefixes[0] {
					w.cov.add("now_unit_foreign_message")
				}
				if opIDPrefix(iid) != propPrefixes[0] {
					w.cov.add("now_unit_foreign_interrupt")
				}
				if _, moved := w.lineage[iid]; moved {
					w.cov.add("now_relowered_unit")
				}
				if d.Redelivery || op.MaybeDelivered > 0 {
					w.fail("maybe-delivered message %s in a unit (N4)", op.ID)
				}
				if i.LaneSeq+1 != op.LaneSeq {
					w.fail("unit %s/%s lane_seqs %d/%d (N2)", iid, op.ID, i.LaneSeq, op.LaneSeq)
				}
				if d.Replacement {
					if s, ok := w.lastStep[conv].(Settled); !ok || s.After.Boundary != TriYes {
						w.fail("message %s sent after a step that did not settle at a boundary (N4): %+v", op.ID, w.lastStep[conv])
					}
				} else if w.probe.Boundary != TriYes || w.probe.TurnLive == TriYes {
					w.fail("message %s sent directly while a turn may run (N4): %+v", op.ID, w.probe)
				}
			case err == nil && i.Status != StatusClosed:
				w.fail("message %s typed while its interrupt is %s outside its unit (N2)", op.ID, i.Status)
			default:
				w.cov.add("now_post_unit")
				if w.probe.Boundary != TriYes || !w.probe.Caps.Live {
					w.fail("post-unit message %s typed off a boundary (N4): %+v", op.ID, w.probe)
				}
			}
		}
		admitted := w.probe.ContentBlock != "" && !slices.ContainsFunc(d.Ops, func(op Op) bool { return !w.probe.admits(op) })
		if unit || admitted {
			return
		}
		for mid, c := range w.nowMsgs {
			if c != conv {
				continue
			}
			m, err := w.h.store.Get(mid)
			if err != nil || m.Status == StatusSubmitted || m.Status == StatusClosed {
				continue
			}
			if i, err := w.nowInterruptOf(m); err == nil && i.Status != StatusClosed {
				w.fail("content %v delivered while unit %s/%s is open (N3)", takenOpIDs(d.Ops), i.ID, mid)
			}
		}
	})
}

// nowInterruptOf reads deliver-now message m's derived interrupt by its
// identity in each of the world's namespaces, wherever the lane that
// lowered m pinned it: beads.ErrNotFound when m has none, and an N1 failure
// when it has two. A forward pointer a re-lowering left is checked and set
// aside: it names m's one interrupt, at m's home id here (N8).
func (w *propWorld) nowInterruptOf(m Op) (Op, error) {
	var found, pointers []Op
	for _, p := range propPrefixes {
		id, err := NowInterruptID(p, m)
		if err != nil {
			return Op{}, err
		}
		i, err := w.h.store.Get(id)
		switch {
		case errors.Is(err, beads.ErrNotFound):
			continue
		case err != nil:
			return Op{}, err
		}
		if isForwardPointer(i) {
			pointers = append(pointers, i)
			continue
		}
		found = append(found, i)
	}
	home, err := NowInterruptID(propPrefixes[0], m)
	if err != nil {
		return Op{}, err
	}
	for _, p := range pointers {
		if p.Outcome.Result.CoalescedInto != home || len(found) != 1 || found[0].ID != home {
			w.fail("forward pointer %s of %s names %s, want its one interrupt at %s (N8)", p.ID, m.ID, p.Outcome.Result.CoalescedInto, home)
		}
	}
	if len(found) == 0 {
		return Op{}, fmt.Errorf("deliver-now message %s: %w", m.ID, beads.ErrNotFound)
	}
	if len(found) > 1 {
		w.fail("deliver-now message %s has %d derived interrupts (N1)", m.ID, len(found))
	}
	return found[0], nil
}

func takenOpIDs(ops []Op) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.ID
	}
	return out
}

func assertNoIdentifiersQuiet(w *propWorld, d Delivery) {
	for _, op := range d.Ops {
		forbidden := []string{op.ID, op.ClientKey, op.Lease.String(), d.Key}
		if op.Producer.Principal != SourceLabel(op.Producer.Principal) {
			forbidden = append(forbidden, op.Producer.Principal)
		}
		for _, f := range forbidden {
			if strings.Contains(d.Text, f) {
				w.fail("delivered text carries %q (I14)", f)
			}
		}
	}
}

// checkInFlight is I5: every op the effect carries is executing under its
// lease, durably, and the runtime row holds that lease.
func (w *propWorld) checkInFlight(conv string, ops []Op) {
	flaky, down := w.flaky, w.afterDown
	w.flaky, w.afterDown = false, false
	defer func() { w.flaky, w.afterDown = flaky, down }()
	for _, op := range ops {
		got, err := w.h.store.Get(op.ID)
		if err != nil || got.Status != StatusExecuting || got.Lease != op.Lease {
			w.fail("effect ran on op %s not durably executing under its lease: %+v, %v", op.ID, got.Lease, err)
		}
	}
	rt, _, _, err := w.h.store.Runtime(conv)
	if err != nil || rt.Lease != ops[0].Lease {
		w.fail("effect ran while the runtime row's cell holds %v, %v", rt.Lease, err)
	}
}

func (w *propWorld) randomProbe() Probe {
	r := w.r
	p := idleProbe()
	p.Available = r.IntN(10) != 0
	p.Running = r.IntN(8) != 0
	p.Boundary = []Tri{TriYes, TriYes, TriYes, TriNo, TriUnknown}[r.IntN(5)]
	p.Caps.Steer = r.IntN(2) == 0
	p.Caps.Respond = r.IntN(2) == 0
	p.Caps.Text = []TextStyle{TextReminder, TextPlain}[r.IntN(2)]
	p.Caps.Input = []FenceMode{FenceAtomic, FenceChecked, FenceNone}[r.IntN(3)]
	// The session's turns as the probe read them.
	p.TurnLive = []Tri{TriYes, TriNo, TriUnknown}[r.IntN(3)]
	if p.TurnLive == TriYes {
		p.Turn = fmt.Sprintf("t%d", r.IntN(3))
		p.KnownTurns = []string{p.Turn}
	}
	if r.IntN(6) == 0 {
		p.Caps.Live, p.Caps.InBand = false, true
	}
	if r.IntN(10) == 0 {
		p.ContentBlock = "draining"
	}
	if r.IntN(6) == 0 {
		p.Pending = []string{propRequests[0], propRequests[1], PendingUnknown}[r.IntN(3)]
	}
	return p
}

// propRequests are the runtime request ids the scripted runtime raises.
var propRequests = []string{"tmux-a", "tmux-b"}

// respondRequest is a lane respond to the latest raising of a request id in
// conv's current generation, recorded now as a producer would.
func (w *propWorld) respondRequest(conv, key string) Request {
	gen := max(1, w.convs[conv].view.Generation)
	raised, _, err := w.h.store.RecordRaising(conv, gen, propRequests[w.r.IntN(len(propRequests))], FactBasisObserved, w.h.clk.Now())
	if err != nil {
		w.t.Fatalf("RecordRaising: %v", err)
	}
	if k, err := RespondKey(raised.Raised); err == nil {
		key = k
	}
	payload, _ := EncodeRespondPayload(RespondPayload{Action: []string{"approve", "deny"}[w.r.IntN(2)]})
	return Request{
		Conversation: conv, Kind: KindRespond, Executor: ExecutorLane, ClientKey: key,
		Producer: Producer{Principal: []string{"operator", "conversation:gc-9"}[w.r.IntN(2)], Actor: ActorOperator},
		Payload:  payload,
		Scope:    Scope{Kind: ScopeSession, Generation: gen, Interaction: raised.Raised.Interaction},
	}
}

func (w *propWorld) randomContent() func(Delivery) (DeliveryResult, bool) {
	r := w.r
	clk := w.h.clk
	switch r.IntN(10) {
	case 0:
		return func(Delivery) (DeliveryResult, bool) { return NotDelivered{Transient: true}, false }
	case 1:
		return func(Delivery) (DeliveryResult, bool) { return NotDelivered{Cause: errors.New("x")}, false }
	case 2:
		landed := r.IntN(2) == 0
		return func(Delivery) (DeliveryResult, bool) { return Unknown{Cause: errors.New("?")}, landed }
	case 3:
		return func(Delivery) (DeliveryResult, bool) {
			return Rejected{KeyRejected: true, Reason: "nudge_rejected"}, false
		}
	case 4:
		return func(Delivery) (DeliveryResult, bool) {
			return Rejected{Arm: ArmSkipped, Reason: ReasonNotNeeded}, false
		}
	default:
		return func(Delivery) (DeliveryResult, bool) {
			return Delivered{
				Record:   DeliveryRecord{At: clk.Now(), Ack: AckDelivered},
				Evidence: TurnEvidence{HasSource: true, Seen: []SeenTurn{{ID: "t", Generation: 1}}, FirstRead: FirstTurnRead{Done: true}},
			}, true
		}
	}
}

func (w *propWorld) randomControl() func() ControlResult {
	switch w.r.IntN(8) {
	case 0:
		return func() ControlResult { return NotDelivered{Transient: true} }
	case 1:
		return func() ControlResult { return Unknown{} }
	case 2:
		return func() ControlResult { return Rejected{Arm: ArmFailed, Reason: ReasonNoBoundary} }
	case 3:
		return func() ControlResult { return Rejected{Arm: ArmSkipped, Reason: ReasonStale} }
	case 4:
		// A replacement step that settled, then another turn ran.
		return func() ControlResult {
			return Settled{Turn: "t", After: TurnAfter{Boundary: TriNo, Live: TriYes, Turn: "t9", Known: []string{"t"}}}
		}
	case 5, 6:
		// A replacement step that settled at the boundary.
		return func() ControlResult {
			return Settled{Turn: "t", After: TurnAfter{Boundary: TriYes, Live: TriNo, Known: []string{"t"}}}
		}
	default:
		return func() ControlResult { return Settled{Turn: "t"} }
	}
}

func (w *propWorld) propose() {
	r := w.r
	conv := propConversations[r.IntN(len(propConversations))]
	w.keys++
	key := fmt.Sprintf("k%d", w.keys)
	principal := []string{"gc:mail", "gc:planner/drain", "operator", "t3:env-1"}[r.IntN(4)]
	req := Request{
		Conversation: conv, Kind: KindMessage, Executor: ExecutorLane, ClientKey: key,
		Producer: Producer{Principal: principal, Actor: ActorBackground},
		Payload:  fmt.Sprintf("payload %d", r.IntN(1000)),
		Policy:   Policy{Message: MessagePolicy{Role: RoleNotice}},
	}
	if !w.restricted {
		m := &req.Policy.Message
		m.Role = []Role{RoleNotice, RoleNotice, RolePrompt}[r.IntN(3)]
		m.When = []When{WhenBoundary, WhenBoundary, WhenAfterTurn, WhenIfIdle, WhenSteer}[r.IntN(5)]
		m.Wake = []Wake{WakeIfNeeded, WakeLiveOnly}[r.IntN(2)]
		m.OnUnsupported = []Fallback{FallbackBoundary, FallbackFail}[r.IntN(2)]
		if r.IntN(4) == 0 {
			req.Policy.Guarantee = GuaranteeAtMostOnce
		}
		if r.IntN(4) == 0 {
			req.Scope = Scope{Kind: ScopeSession, Generation: max(1, w.convs[conv].view.Generation-int64(r.IntN(2)))}
		}
		if r.IntN(4) == 0 && len(w.ids) > 0 {
			req.After = w.ids[r.IntN(len(w.ids))]
			w.afterTargets[req.After] = true
		}
		if r.IntN(6) == 0 {
			req.NotBefore = w.h.clk.Now().Add(time.Duration(r.IntN(120)) * time.Second)
		}
		if r.IntN(6) == 0 {
			req.ClientSeq = uint64(1 + r.IntN(5))
		}
		switch r.IntN(8) {
		case 0:
			req = Request{
				Conversation: conv, Kind: KindInterrupt, Executor: ExecutorLane, ClientKey: key,
				Producer: Producer{Principal: "operator", Actor: ActorOperator},
				Scope:    Scope{Kind: ScopeSession, Generation: max(1, w.convs[conv].view.Generation)},
			}
		case 1:
			req = w.respondRequest(conv, key)
		default:
			if r.IntN(5) != 0 {
				break
			}
			// A deliver-now message, untargeted or aimed at a turn of the
			// current session, keeping any after or not_before drawn above:
			// N3 holds whatever its unit waits on.
			req.Policy.Message.When = []When{WhenNow, WhenNow, WhenInterrupt}[r.IntN(3)]
			if req.Policy.Message.When == WhenNow && r.IntN(2) == 0 {
				req.Policy.Message.NowGeneration = max(1, w.convs[conv].view.Generation)
				if r.IntN(2) == 0 {
					req.Policy.Message.NowTurn = fmt.Sprintf("t%d", r.IntN(3))
				}
			}
		}
	}
	// A quarter of the ops moved here with their ids from another
	// namespace; a deliver-now message among them may have been lowered
	// there before it moved. A respond stays in the lanes' namespace: the
	// raising it answers, recorded there, keys it (validateRespondKey).
	s := w.h.store
	if r.IntN(4) == 0 && req.Kind != KindRespond {
		s = w.old
	}
	// Ops arrive at distinct instants: the interim order key (OJ-10a)
	// breaks same-instant ties by id, not by arrival.
	w.h.clk.Advance(time.Millisecond)
	op, created, err := s.Propose(req)
	if req.Kind == KindRespond && (errors.Is(err, ErrKeyReuse) || err == nil && !created) {
		return // the same principal answering one occurrence again: the first answer wins
	}
	if err != nil {
		w.t.Fatalf("Propose: %v", err)
	}
	w.ids = append(w.ids, op.ID)
	w.convOf[op.ID] = conv
	if isNowMessage(op) {
		w.nowMsgs[op.ID] = conv
		w.nowIDs = append(w.nowIDs, op.ID)
		if s == w.old && r.IntN(2) == 0 && w.lowerable(op) {
			w.lowerIn(w.oldLane, op)
			if r.IntN(2) == 0 {
				// The namespace move's migrate, right after the lowering.
				w.relower()
			}
		}
	}
}

// lowerable reports a deliver-now message the rules would lower at once:
// no after or not_before to wait for, its scope the current one, and a
// session awake.
func (w *propWorld) lowerable(m Op) bool {
	v := w.convs[m.Conversation].view
	return m.After == "" && !m.NotBefore.After(w.h.clk.Now()) && scopeCmp(m.Scope, incarnation{v.Generation, v.Epoch}) == 0 &&
		v.Generation >= 1 && !v.Dormant && !v.Closed
}

// lowerIn lowers deliver-now message m as a lane of l's namespace did
// before m moved here with its id: its derived interrupt pinned in that
// namespace and m accepted, in one guarded batch.
func (w *propWorld) lowerIn(l *ConversationLane, m Op) {
	now := w.h.clk.Now()
	gen := m.Policy.Message.NowGeneration
	if gen < 1 {
		gen = w.convs[m.Conversation].view.Generation
	}
	i, err := deriveNowInterrupt(l.store.prefix, m, gen, now, l.by())
	if err != nil {
		w.t.Fatalf("deriving the interrupt of %s: %v", m.ID, err)
	}
	next := m
	next.Status = StatusAccepted
	next.Transitions = appendTransition(m.Transitions, Transition{Status: StatusAccepted, At: now, By: l.by()})
	var res PassResult
	if done, err := l.writeLowerings(context.Background(), []nowLowering{{m: m, mNext: next, i: i}}, &res); err != nil || !done {
		w.t.Fatalf("lowering %s in namespace %s: done %v, %v", m.ID, l.store.prefix, done, err)
	}
}

func (w *propWorld) cancel() {
	if len(w.ids) == 0 {
		return
	}
	id := w.ids[w.r.IntN(len(w.ids))]
	if len(w.nowIDs) > 0 && w.r.IntN(2) == 0 {
		// A deliver-now message, so cancels reach lowered units (the
		// cascade).
		id = w.nowIDs[w.r.IntN(len(w.nowIDs))]
	}
	if _, _, err := w.h.store.Cancel(id, "operator", ""); err != nil {
		w.t.Fatalf("Cancel: %v", err)
	}
}

// pass runs one pass; a third of passes read through a flaky store, and a
// pass may then end with the read error.
func (w *propWorld) pass(lane int, conv string) {
	w.flaky = w.r.IntN(3) == 0
	res, err := w.lanes[lane].Pass(context.Background(), w.convs[conv].view, propExec{w})
	w.flaky = false
	if err != nil && !errors.Is(err, errCrash) && !errors.Is(err, errFlakyRead) {
		w.t.Fatalf("Pass: %v", err)
	}
	for _, wt := range res.Waits {
		switch wt.Reason {
		case WaitReplayGroup:
			w.cov.add("replay_group")
		case WaitAfterUnreadable:
			w.cov.add("after_unreadable")
		}
	}
}

func (w *propWorld) step() {
	r := w.r
	w.probe = w.randomProbe()
	w.contentRes = w.randomContent()
	w.controlRes = w.randomControl()
	w.pendingHook = nil
	conv := propConversations[r.IntN(len(propConversations))]
	lane := r.IntN(2)
	switch k := r.IntN(19); {
	case k < 4:
		w.propose()
	case k == 4:
		w.cancel()
	case k < 9:
		w.pass(lane, conv)
	case k == 9:
		stage := []laneStage{stageAfterTake, stageAfterEffect}[r.IntN(2)]
		w.lanes[lane].hooks.at = crashAt(stage)
		w.pass(lane, conv)
		w.lanes[lane].hooks.at = nil
	case k == 10:
		other := w.lanes[1-lane]
		ran := false
		w.lanes[lane].hooks.at = func(s laneStage) error {
			if s == stageAfterRead && !ran {
				ran = true
				_, err := other.Pass(context.Background(), w.convs[conv].view, propExec{w})
				if err != nil && !errors.Is(err, errFlakyRead) {
					w.t.Fatalf("inner Pass: %v", err)
				}
			}
			return nil
		}
		w.pass(lane, conv)
		w.lanes[lane].hooks.at = nil
	case k == 11:
		w.h.clk.Advance(time.Second + time.Duration(r.Int64N(int64(3*time.Minute))))
	case k == 12:
		v := &w.convs[conv].view
		if !w.restricted && r.IntN(2) == 0 {
			v.Generation++
		} else {
			v.Epoch++
		}
	case k == 13:
		w.ambiguousThenSplit(lane, conv)
	case k == 14:
		w.keyRejectedThenCrash(lane, conv)
	case k == 15:
		w.ambiguousThenAfterUnreadable(lane, conv)
	case k == 16:
		w.relower()
	case k == 17:
		w.migrateUnit(lane, conv)
	default:
		v := &w.convs[conv].view
		switch r.IntN(10) {
		case 0:
			v.Closed = true
		case 1, 2, 3:
			v.Dormant = !v.Dormant
		default:
			v.ContentHold = !v.ContentHold
		}
	}
}

// migrateUnit targets re-lowering: a deliver-now message lowered in the
// old namespace, then moved here by a migrate, at once or after a pass that
// took its unit and crashed (the step leaves that unit in flight).
func (w *propWorld) migrateUnit(lane int, conv string) {
	r := w.r
	if w.restricted {
		// The restricted generator proposes notices delivered in arrival
		// order (I8), which a deliver-now message would jump.
		return
	}
	w.keys++
	req := Request{
		Conversation: conv, Kind: KindMessage, Executor: ExecutorLane, ClientKey: fmt.Sprintf("k%d", w.keys),
		Producer: Producer{Principal: "operator", Actor: ActorBackground},
		Payload:  fmt.Sprintf("payload %d", r.IntN(1000)),
		Policy:   Policy{Message: MessagePolicy{Role: RolePrompt, When: WhenNow}},
	}
	w.h.clk.Advance(time.Millisecond)
	m, _, err := w.old.Propose(req)
	if err != nil {
		w.t.Fatalf("Propose: %v", err)
	}
	w.ids = append(w.ids, m.ID)
	w.convOf[m.ID] = conv
	w.nowMsgs[m.ID] = conv
	w.nowIDs = append(w.nowIDs, m.ID)
	if !w.lowerable(m) {
		return
	}
	w.lowerIn(w.oldLane, m)
	if r.IntN(2) == 0 {
		w.probe = idleProbe()
		w.probe.Boundary, w.probe.TurnLive, w.probe.Turn = TriNo, TriYes, "t"
		w.lanes[lane].hooks.at = crashAt(stageAfterTake)
		w.pass(lane, conv)
		w.lanes[lane].hooks.at = nil
	}
	w.relower()
}

// relower runs RelowerNowUnits between passes, as a fleet-stopped gc
// storage migrate does, leases left by crashed passes included. A third of
// runs read through a flaky store, and a quarter fault one batch, with or
// without its commit. It checks N9 (no interrupt an attempt may have run is
// copied) and N10 (a run writes only the rows of the units it moved).
func (w *propWorld) relower() {
	before := w.rowRevisions()
	w.flaky = w.r.IntN(3) == 0
	if w.r.IntN(4) == 0 {
		commit, first := w.r.IntN(2) == 0, len(w.h.batcher.batches())
		w.h.batcher.setFault(func(n int, _ beads.GuardedBatch) (bool, error) {
			if n == first {
				return commit, errLinkDropped
			}
			return false, nil
		})
	}
	rep, err := w.h.store.RelowerNowUnits(context.Background(), "gc:storage-migrate")
	w.flaky = false
	w.h.batcher.setFault(nil)
	if err != nil && !errors.Is(err, errFlakyRead) && !errors.Is(err, errLinkDropped) {
		w.t.Fatalf("RelowerNowUnits: %v", err)
	}
	for _, rl := range rep.Relowered {
		w.cov.add("now_relowered")
		w.lineage[rl.To] = rl.From
		if w.sentInterrupt[rl.From] {
			w.fail("re-lowering copied interrupt %s, which an attempt may have run (N9)", rl.From)
		}
	}
	if len(rep.Left) > 0 {
		w.cov.add("now_relower_left")
	}
	after := w.rowRevisions()
	// A move the run could not read back (a flaky read after a committed
	// batch) still shows as a new pointer and its copy.
	moved := map[string]bool{}
	for id, rev := range after {
		if old, ok := before[id]; ok && old == rev {
			continue
		}
		b, err := w.h.mem.Get(id)
		if err != nil {
			w.t.Fatal(err)
		}
		if op, err := DecodeOp(b); err == nil && isForwardPointer(op) {
			if _, existed := before[op.Outcome.Result.CoalescedInto]; !existed {
				moved[id], moved[op.Outcome.Result.CoalescedInto] = true, true
				w.lineage[op.Outcome.Result.CoalescedInto] = id
				if w.sentInterrupt[id] {
					w.fail("re-lowering copied interrupt %s, which an attempt may have run (N9)", id)
				}
			}
		}
	}
	for id, rev := range after {
		if old, ok := before[id]; (!ok || old != rev) && !moved[id] {
			w.fail("re-lowering wrote %s, which is no row of a unit it moved (N10)", id)
		}
	}
	for _, rl := range rep.Relowered {
		if !moved[rl.From] || !moved[rl.To] {
			w.fail("re-lowering reported %s -> %s, which the rows do not show (N10)", rl.From, rl.To)
		}
	}
}

// origin is the row id's lineage starts at: the interrupt a re-lowering
// copied, followed back, or id itself.
func (w *propWorld) origin(id string) string {
	for {
		from, ok := w.lineage[id]
		if !ok {
			return id
		}
		id = from
	}
}

// rowRevisions is every row's revision, read from the beads directly.
func (w *propWorld) rowRevisions() map[string]int64 {
	rows, err := w.h.mem.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		w.t.Fatal(err)
	}
	out := make(map[string]int64, len(rows))
	for _, b := range rows {
		out[b.ID] = b.Revision
	}
	return out
}

// delivered is a healthy runtime's answer: delivered, and shown.
func (w *propWorld) delivered() func(Delivery) (DeliveryResult, bool) {
	return func(Delivery) (DeliveryResult, bool) {
		return Delivered{Record: DeliveryRecord{At: w.h.clk.Now(), Ack: AckDelivered}}, true
	}
}

// ambiguousThenSplit targets replay groups: an attempt ends Unknown, then
// part of its group becomes eligible before the rest (a mid-turn steer, or
// T3's Stop-with-hold holding normal content while a steer bypasses it),
// then a boundary comes.
func (w *propWorld) ambiguousThenSplit(lane int, conv string) {
	landed := w.r.IntN(2) == 0
	w.contentRes = func(Delivery) (DeliveryResult, bool) { return Unknown{Cause: errors.New("?")}, landed }
	w.probe = idleProbe()
	w.pass(lane, conv)
	w.h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	w.contentRes = w.delivered()
	w.probe.Caps.Steer = true
	v := &w.convs[conv].view
	hold := v.ContentHold
	if w.r.IntN(2) == 0 {
		w.probe.Boundary = TriNo
	} else {
		v.ContentHold = true
	}
	w.pass(lane, conv)
	v.ContentHold = hold
	w.probe = idleProbe()
	w.pass(1-lane, conv)
}

// ambiguousThenAfterUnreadable targets replay groups whose members the rules
// keep out of the class: two fresh notices, one after an op that closed
// succeeded, go in one attempt that ends Unknown; the replay pass cannot read
// any after target; then the store recovers.
func (w *propWorld) ambiguousThenAfterUnreadable(lane int, conv string) {
	if w.restricted {
		// Restricted runs check arrival order, which after reorders.
		w.pass(lane, conv)
		return
	}
	var target string
	for id, o := range w.outcomes {
		if w.convOf[id] == conv && o.Arm == ArmSucceeded && (target == "" || id < target) {
			target = id
		}
	}
	for i, after := range []string{"", target} {
		w.keys++
		req := laneMsg(fmt.Sprintf("k%d", w.keys), RoleNotice, fmt.Sprintf("grouped %d", i))
		req.Conversation, req.After = conv, after
		w.h.clk.Advance(time.Millisecond)
		op, _, err := w.h.store.Propose(req)
		if err != nil {
			w.t.Fatalf("Propose: %v", err)
		}
		w.ids = append(w.ids, op.ID)
		w.convOf[op.ID] = conv
	}
	w.afterTargets[target] = target != ""
	landed := w.r.IntN(2) == 0
	w.probe = idleProbe()
	w.contentRes = func(Delivery) (DeliveryResult, bool) { return Unknown{Cause: errors.New("?")}, landed }
	w.pass(lane, conv)
	w.h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	w.contentRes = w.delivered()
	w.afterDown = true
	w.pass(lane, conv)
	w.afterDown = false
	w.pass(1-lane, conv)
}

// keyRejectedThenCrash targets L3: a maybe-delivered op has its key
// refused, its next attempt under a new key reaches the agent and the lane
// crashes before Done; the lease then expires.
func (w *propWorld) keyRejectedThenCrash(lane int, conv string) {
	landed := w.r.IntN(2) == 0
	w.probe = idleProbe()
	w.contentRes = func(Delivery) (DeliveryResult, bool) { return Unknown{Cause: errors.New("?")}, landed }
	w.pass(lane, conv)
	w.h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	w.contentRes = func(Delivery) (DeliveryResult, bool) {
		return Rejected{KeyRejected: true, Reason: "nudge_rejected"}, false
	}
	w.pass(lane, conv)
	w.h.clk.Advance(ExecutorErrorBackoff + time.Millisecond)
	w.contentRes = w.delivered()
	w.lanes[lane].hooks.at = crashAt(stageAfterEffect)
	w.pass(lane, conv)
	w.lanes[lane].hooks.at = nil
	w.h.clk.Advance(DefaultLeaseTTL + LeaseSkew + time.Second)
}

// check runs the per-step invariants over the store.
func (w *propWorld) check() {
	now := w.h.clk.Now()
	for _, conv := range propConversations {
		ops, _, err := w.h.store.ListOpenTolerant(conv)
		if err != nil {
			w.t.Fatal(err)
		}
		live := map[Lease]bool{}
		for _, op := range ops {
			if op.Status == StatusExecuting && !now.After(op.Lease.Deadline.Add(LeaseSkew)) {
				live[op.Lease] = true
			}
		}
		rt, _, found, err := w.h.store.Runtime(conv)
		if err != nil {
			w.t.Fatal(err)
		}
		if len(live) > 1 {
			w.fail("conversation %s has %d live leases (I3)", conv, len(live))
		}
		for l := range live {
			if rt.Lease != l {
				w.fail("conversation %s: live lease %s but the cell holds %s (I3)", conv, l, rt.Lease)
			}
		}
		if found {
			prev := w.rowSeen[conv]
			if (incarnation{rt.Generation, rt.Epoch}).cmp(incarnation{prev.Generation, prev.Epoch}) < 0 || rt.Seq < prev.Seq {
				w.fail("conversation %s runtime row went backwards: %+v after %+v (L1)", conv, rt, prev)
			}
			w.rowSeen[conv] = rt
		}
	}
	w.checkNowPairs()
	prevMax := maps.Clone(w.maxSeq)
	for _, id := range w.ids {
		b, err := w.h.mem.Get(id)
		if err != nil {
			w.t.Fatal(err)
		}
		if cached, ok := w.decoded[id]; ok && cached.Revision == b.Revision {
			continue // unchanged since the last check
		}
		op, err := DecodeOp(b)
		if err != nil {
			w.t.Fatal(err)
		}
		w.decoded[id] = op
		if op.Status == StatusClosed {
			if prev, ok := w.outcomes[id]; ok && !prev.Equal(op.Outcome) {
				w.fail("op %s outcome changed from %+v to %+v (I1)", id, prev, op.Outcome)
			}
			w.outcomes[id] = op.Outcome
			if op.Outcome.Reason == ReasonPoison && op.Failures != MaxFailures {
				w.fail("op %s poisoned with %d failures", id, op.Failures)
			}
			// A probe close records that the op never ran: never one that
			// may have reached the executor.
			if op.MaybeDelivered > 0 && (op.Outcome.Reason == ReasonBusy || op.Outcome.Reason == ReasonUnsupported) {
				w.fail("op %s closed %s{%s} after attempt %d may have been delivered", id, op.Outcome.Arm, op.Outcome.Reason, op.MaybeDelivered)
			}
		}
		if op.LaneSeq > 0 {
			// A re-lowered interrupt carries its row's attempts: a take
			// is keyed by the row the lineage started at.
			take := fmt.Sprintf("%s:%d", w.origin(id), op.Attempt)
			if !w.seenTake[take] {
				w.seenTake[take] = true
				if other, dup := w.seqs[op.Conversation][op.LaneSeq]; dup {
					w.fail("lane_seq %d reused by %s and %s (I4)", op.LaneSeq, other, take)
				}
				w.seqs[op.Conversation][op.LaneSeq] = take
				if op.LaneSeq <= prevMax[op.Conversation] {
					w.fail("lane_seq %d of %s is not above %d (I4)", op.LaneSeq, take, prevMax[op.Conversation])
				}
				w.maxSeq[op.Conversation] = max(w.maxSeq[op.Conversation], op.LaneSeq)
			}
		}
	}
	// One record and one link per delivery.
	records := map[string]Result{}
	for id, o := range w.outcomes {
		if o.Arm != ArmSucceeded || o.Result.Delivery.IsZero() {
			continue
		}
		k := fmt.Sprintf("%s#%d", w.convOf[id], o.Result.Delivery.Seq)
		if prev, ok := records[k]; ok && !prev.Equal(o.Result) {
			w.fail("delivery %s recorded twice differently: %+v vs %+v", k, prev, o.Result)
		}
		records[k] = o.Result
	}
	for _, conv := range propConversations {
		c := w.convs[conv]
		if !c.dedup {
			continue
		}
		for id, n := range c.transcript {
			if n > 1 {
				w.fail("deduplicating runtime showed op %s %d times", id, n)
			}
		}
	}
	// At least once: an op that closed succeeded with a delivery record
	// reached the agent.
	for id, o := range w.outcomes {
		if o.Arm == ArmSucceeded && !o.Result.Delivery.IsZero() && w.convs[w.convOf[id]].transcript[id] == 0 && !w.lost[id] {
			w.lost[id] = true
			w.fail("op %s closed succeeded but the agent never saw it (at least once)", id)
		}
	}
}

// checkNowPairs is N1 and N7: every lowered message has its one derived
// interrupt, shaped as the lowering writes it (from then on it is tracked
// as an op of the run), and an interrupt closes before or with its message,
// unless it closes after it as an orphan (the cascade, or not_needed) or
// its message was canceled in the store, outside the lane: the lane did not
// close that message, so it orders nothing against its interrupt's close.
func (w *propWorld) checkNowPairs() {
	for _, mid := range w.nowIDs {
		conv := w.nowMsgs[mid]
		m, err := w.h.store.Get(mid)
		if err != nil {
			w.t.Fatal(err)
		}
		i, err := w.nowInterruptOf(m)
		iid := i.ID
		switch {
		case errors.Is(err, beads.ErrNotFound) && slices.ContainsFunc(m.Transitions, func(t Transition) bool { return t.Status == StatusAccepted }):
			w.fail("lowered message %s (%s) has no derived interrupt (N1)", mid, m.Status)
			continue
		case err != nil:
			continue
		}
		if _, tracked := w.convOf[iid]; !tracked {
			w.ids = append(w.ids, iid)
			w.convOf[iid] = conv
		}
		if i.Kind != KindInterrupt || i.Executor != ExecutorLane || i.Causation != mid || i.Producer != m.Producer ||
			i.Policy.Guarantee != GuaranteeAtMostOnce || i.Policy.Interrupt.OnFailure != OnFailureFail || i.After != "" ||
			i.Scope.Turn != m.Policy.Message.NowTurn || m.After == iid {
			w.fail("derived interrupt %s of %s is misshapen (N1): %+v", iid, mid, i)
		}
		if i.Status == StatusClosed && i.Outcome.Arm == ArmCanceled && i.Cancel.By == nowCascadeBy {
			w.cov.add("now_cascade")
		}
		if i.Status != StatusClosed || m.Status != StatusClosed {
			continue
		}
		iAt, mAt := i.Transitions[len(i.Transitions)-1].At, m.Transitions[len(m.Transitions)-1].At
		orphan := i.Outcome.Arm == ArmCanceled && i.Cancel.By == nowCascadeBy ||
			i.Outcome.Arm == ArmSkipped && i.Outcome.Reason == ReasonNotNeeded
		direct := m.Outcome.Arm == ArmCanceled && m.Cancel.By != nowCascadeBy
		if iAt.After(mAt) && !orphan && !direct {
			w.fail("derived interrupt %s closed %s{%s} after its message %s (N7)", iid, i.Outcome.Arm, i.Outcome.Reason, mid)
		}
	}
}

// checkOrder is I8 for the restricted generator: among notices of one
// conversation that both succeeded, arrival order is delivery order.
func (w *propWorld) checkOrder() {
	var ops []Op
	for _, id := range w.ids {
		op, _ := w.h.store.Get(id)
		if op.Outcome.Arm == ArmSucceeded {
			ops = append(ops, op)
		}
	}
	for _, a := range ops {
		for _, b := range ops {
			if a.Conversation == b.Conversation && CompareOrder(a, b) < 0 &&
				a.Outcome.Result.Delivery.Seq > b.Outcome.Result.Delivery.Seq {
				w.fail("op %s arrived before %s but was delivered after it (I8)", a.ID, b.ID)
			}
		}
	}
}

// drainSlack bounds how long a healthy runtime may take to settle what the
// random steps left: leases in flight, backoffs and not_before gates.
const drainSlack = time.Hour

// drain runs a healthy idle runtime until nothing is left, advancing the
// clock only to the next time a waiting op can change by itself. Every op
// must close, and none whose start_by was more than drainSlack ahead when
// the drain began may close for a deadline (riders aside): that would be
// starvation.
func (w *propWorld) drain() {
	for _, c := range w.convs {
		c.view.Dormant, c.view.ContentHold = false, false
	}
	start := w.h.clk.Now()
	w.probe = idleProbe()
	w.probe.Caps.Respond = true
	w.probe.Caps.Steer = true
	w.contentRes = w.delivered()
	w.controlRes = func() ControlResult { return Settled{} }
	for range 500 {
		quiet, next := true, time.Time{}
		for _, conv := range propConversations {
			res, err := w.lanes[0].Pass(context.Background(), w.convs[conv].view, propExec{w})
			if err != nil {
				w.t.Fatalf("drain Pass: %v", err)
			}
			if res.Took != TakeNone || len(res.Closed) > 0 || res.More {
				quiet = false
			}
			next = minTime(next, res.Next)
		}
		w.check()
		switch {
		case !quiet:
		case next.IsZero():
			goto settled
		case next.After(w.h.clk.Now()):
			w.h.clk.Advance(next.Sub(w.h.clk.Now()))
		default:
			// A host sleeps until Next: a quiet round whose timer is not
			// ahead would make it spin.
			w.t.Fatalf("drain: a quiet round's next timer %s is not after now %s", next, w.h.clk.Now())
		}
	}
settled:
	for _, id := range w.ids {
		op, _ := w.h.store.Get(id)
		switch {
		case op.Status != StatusClosed:
			w.fail("op %s still %s after the drain", id, op.Status)
		case w.ridesOnRider(op):
			// A rider waits for a turn-starting op by design; with none
			// left it expires at start_by, and so does an op whose after
			// chain runs through one.
		case op.StartBy.After(start.Add(drainSlack)) && op.Outcome.Arm == ArmFailed &&
			(op.Outcome.Reason == ReasonExpiredBeforeStart || op.Outcome.Reason == ReasonDeadlineInFlight) &&
			op.Transitions[len(op.Transitions)-1].At.After(start):
			w.fail("op %s starved: it closed %s in the drain, though its start_by %s was more than %s ahead of the drain at %s",
				id, op.Outcome.Reason, op.StartBy, drainSlack, start)
		}
	}
}

// ridesOnRider reports whether op is a rider (when after_turn) or waits,
// through its after chain, on one.
func (w *propWorld) ridesOnRider(op Op) bool {
	rides := false
	w.unflaky(func() {
		seen := map[string]bool{}
		for {
			if op.Policy.Message.When == WhenAfterTurn {
				rides = true
				return
			}
			if op.After == "" || seen[op.After] {
				return
			}
			seen[op.After] = true
			next, err := w.h.store.Get(op.After)
			if err != nil {
				return
			}
			op = next
		}
	})
	return rides
}

// propCoverage counts what the property runs exercised, across seeds.
type propCoverage struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *propCoverage) add(k string) {
	c.mu.Lock()
	c.counts[k]++
	c.mu.Unlock()
}

func TestLaneProperties(t *testing.T) {
	cov := &propCoverage{counts: map[string]int{}}
	t.Cleanup(func() {
		for _, k := range []string{
			"delivery", "redelivery", "note", "replay_dedup", "closed:poison", "closed:ambiguous_delivery",
			"closed:stale", "closed:canceled", "closed:busy", "closed:dependency_failed", "closed:expired_before_start",
			"closed:deadline_in_flight", "closed:conversation_closed", "key_rejected", "replay_group", "after_unreadable",
			"respond_answered", "now_unit", "now_post_unit", "now_replace", "now_cascade", "now_unit_foreign_message", "now_unit_foreign_interrupt",
			"now_relowered", "now_relowered_unit", "now_relower_left",
		} {
			if cov.counts[k] == 0 {
				t.Errorf("the property runs never exercised %s (%v)", k, cov.counts)
			}
		}
	})
	for seed := range propSeedCount(t) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			w := newPropWorld(t, seed)
			w.cov = cov
			defer w.tally()
			for i := range propSteps {
				w.step()
				w.check()
				if len(w.violations) > 0 {
					t.Fatalf("seed %d step %d (restricted %v):\n%s", seed, i, w.restricted, strings.Join(w.violations, "\n"))
				}
			}
			if w.restricted {
				w.checkOrder()
			}
			w.drain()
			w.check()
			if len(w.violations) > 0 {
				t.Fatalf("seed %d after the drain:\n%s", seed, strings.Join(w.violations, "\n"))
			}
		})
	}
}

// tally records the outcomes the run reached.
func (w *propWorld) tally() {
	for _, o := range w.outcomes {
		r := o.Reason
		if o.Arm == ArmCanceled {
			r = "canceled"
		}
		w.cov.add("closed:" + r)
	}
}
