package opjournal

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// incarnation is a (generation, epoch) pair, ordered lexicographically.
type incarnation struct{ gen, epoch int64 }

func (a incarnation) cmp(b incarnation) int {
	if a.gen != b.gen {
		if a.gen < b.gen {
			return -1
		}
		return 1
	}
	switch {
	case a.epoch < b.epoch:
		return -1
	case a.epoch > b.epoch:
		return 1
	default:
		return 0
	}
}

// Last-error tokens the lane writes.
const (
	lastErrorLeaseExpired = "lease_expired"
	lastErrorTransient    = "transient"
	lastErrorUnavailable  = "unavailable"
	lastErrorNotDelivered = "not_delivered"
	lastErrorUnknown      = "unknown"
	lastErrorKeyRejected  = "key_rejected"
)

// entry is one open op in a class's order: a candidate (wait == "") or a
// blocking waiter, which stops every later op of its class.
type entry struct {
	op     Op
	urgent bool
	// retake is an executing op whose lease expired, taken again.
	retake bool
	// cur is op with the expiry bookkeeping a re-take records (failures+1,
	// maybe_delivered, last_error lease_expired); op otherwise. Every write
	// for this entry starts from cur, guarded by op's revision.
	cur    Op
	wait   WaitReason
	blocks bool
	// unit, on a control entry, is a deliver-now unit: op is its message
	// and unit its derived interrupt, taken together (DES-O1 §10.3).
	unit *nowUnit
	// postUnit is a deliver-now message whose unit is over: urgent content
	// that goes alone at a boundary, marked nowDegradation (its interrupt's
	// failure).
	postUnit       bool
	nowDegradation string
}

func (e entry) candidate() bool { return e.wait == "" }

// entryIDs are the ids of the ops an entry stands for: a unit's interrupt
// and message, else its op.
func entryIDs(e entry) []string {
	if e.unit != nil {
		return []string{e.unit.op.ID, e.op.ID}
	}
	return []string{e.op.ID}
}

// maybeDelivered reports whether an earlier attempt may have reached the
// executor: a re-take, or an op released after an Unknown result. The probe
// closes of a fresh op would record that it never ran, so it waits instead,
// and it goes only as a replay head, never in a fresh batch.
func (e entry) maybeDelivered() bool { return e.retake || e.cur.MaybeDelivered > 0 }

// ruleClose is one close the planner decided: cur as read (its revision
// guards the write), next the closed op.
type ruleClose struct {
	cur  Op
	next Op
}

// rulesPlan is planRules' answer.
type rulesPlan struct {
	// inc is the effective incarnation: the larger of the view's and the
	// runtime row's.
	inc incarnation
	// baseSeq is the runtime row's seq as read.
	baseSeq uint64
	closes  []ruleClose
	// closeRow closes the runtime row with the last close batch.
	closeRow bool
	// control and content are each class's candidates and blocking waiters,
	// in order.
	control, content []entry
	waits            []OpWait
	takePossible     bool
	wake             bool
	next             time.Time
	// rowErr stops any Take: the runtime row is undecodable, or closed
	// under an open view.
	rowErr error
	by     string
	// groups maps each delivery key to the maybe-delivered ops under it
	// that the rules leave open and not in flight, in a class or not: a
	// member waiting on its after target is still in its replay group.
	groups map[string][]Op
	// raisings holds, by op id, the occurrence of each respond candidate as
	// the pass read it after its probe (noteRaisings).
	raisings map[string]raisingState
	// responds are the open respond ops the rules leave open and not in
	// flight, candidates or waiting: a look that shows a prompt gone ends
	// their occurrences (clearGone).
	responds []Op
	now      time.Time
	// remaining counts the ops the rules leave open, for closeRow.
	remaining int
	// nowState holds each deliver-now message's and derived-shape
	// interrupt's phase-1 decision, which planNow finishes; nowOrder is
	// their read order.
	nowState map[string]*nowOpState
	nowOrder []string
	// lowerings are the deliver-now messages this pass lowers.
	lowerings []nowLowering
	// openUnits are the lowered deliver-now messages whose derived
	// interrupt is open, whatever either op waits on, and the due ones not
	// yet lowered only because a read (of their after target or of their
	// derived id) failed transiently:
	// while any is, no content is taken (U-ORDER), unless planTake admits it
	// past the content block the unit waits on.
	openUnits []string
}

func (rp *rulesPlan) waitOpDetail(op Op, r WaitReason, detail string, until time.Time) {
	rp.waits = append(rp.waits, OpWait{ID: op.ID, Reason: r, Detail: detail, Until: until})
	rp.next = minTime(rp.next, until)
	rp.remaining++
}

// closeOp closes read, the op as read (its revision guards the write), from
// cur, the op with any bookkeeping the rules record. A stale close of an op
// whose scope the incarnation has moved past, the scope rule's, records that
// incarnation (FencedOff): an op closed so never ran, unlike one the
// executor found nothing to do for.
func (rp *rulesPlan) closeOp(read, cur Op, o Outcome) {
	if o.Arm == ArmSkipped && o.Reason == ReasonStale && scopeCmp(read.Scope, rp.inc) < 0 {
		o.Result = Result{Generation: rp.inc.gen, ContextEpoch: rp.inc.epoch}
	}
	rp.closes = append(rp.closes, ruleClose{cur: read, next: closedAt(cur, o, rp.by, rp.now)})
}

// isUrgent reports urgent content precedence: steer, and a deliver-now
// message (now, or the legacy interrupt).
func isUrgent(op Op) bool {
	if op.Kind != KindMessage {
		return false
	}
	switch op.Policy.Message.When {
	case WhenSteer, WhenNow, WhenInterrupt:
		return true
	}
	return false
}

// scopeCmp compares an op's scope with the effective incarnation: -1 older,
// 0 current, 1 newer.
func scopeCmp(s Scope, inc incarnation) int {
	switch s.Kind {
	case ScopeSession:
		return incarnation{gen: s.Generation}.cmp(incarnation{gen: inc.gen})
	case ScopeContext:
		return incarnation{epoch: s.Epoch}.cmp(incarnation{epoch: inc.epoch})
	default:
		return 0
	}
}

// planRules applies the rules that need no probe to one snapshot (DES-O1
// §4.4 as refined by OJ-10's design): the closes they decide, each class's
// order of candidates and blocking waiters, whether a Take is possible at
// all, the wake need and the next timer. It is pure.
func planRules(snap laneSnapshot, conv ConversationView, now time.Time, by string) rulesPlan {
	rp := rulesPlan{by: by, now: now, inc: incarnation{conv.Generation, conv.Epoch}, nowState: map[string]*nowOpState{}}
	rt := snap.rt
	rowOK := rt.found && rt.err == nil
	switch {
	case rt.err != nil:
		rp.rowErr = fmt.Errorf("conversation %s: %w", conv.ID, rt.err)
	case rowOK:
		rp.baseSeq = rt.row.Seq
		if row := (incarnation{rt.row.Generation, rt.row.Epoch}); rp.inc.cmp(row) < 0 {
			rp.inc = row
		}
		if rt.row.Closed && !conv.Closed {
			rp.rowErr = fmt.Errorf("%w: conversation %s: the runtime row is closed but the view is open", ErrIllegalTransition, conv.ID)
		}
	}
	// inFlight is the earliest time a live lease can end, zero when no op
	// is in flight.
	var inFlight time.Time
	// past holds every op past the in-flight and expiry rules, as the rules
	// see it; the ones a later rule closes are dropped from groups.
	var past []Op
	for _, op := range snap.ops {
		spec, err := Lookup(op.Kind)
		if err != nil {
			continue
		}
		// A deliver-now op's waits and placement are left to planNow
		// (phase 2), which pairs it with its partner; its closes stand.
		ns := rp.nowOp(op)
		waitOpDetail := func(op Op, r WaitReason, detail string, until time.Time) {
			if ns != nil {
				ns.wait, ns.detail, ns.until = r, detail, until
				return
			}
			rp.waitOpDetail(op, r, detail, until)
		}
		waitOp := func(op Op, r WaitReason, until time.Time) { waitOpDetail(op, r, "", until) }
		closeOp := func(read, cur Op, o Outcome) {
			rp.closeOp(read, cur, o)
			if ns != nil {
				ns.closing = &o
			}
		}
		if op.Status == StatusSettling {
			waitOp(op, WaitSettling, time.Time{})
			continue
		}
		executing := op.Status == StatusExecuting
		live := executing && !op.Lease.IsZero() && !now.After(op.Lease.Deadline.Add(LeaseSkew))
		expired := executing && !live
		cur := op
		if expired {
			cur.Failures++
			cur.MaybeDelivered = cur.Attempt
			cur.LastError = lastErrorLeaseExpired
		}
		if !live && !now.After(op.StartBy) {
			// start_by closes an op once now is past it.
			rp.next = minTime(rp.next, timerAt(op.StartBy, now))
		}
		canceled := Outcome{Arm: ArmCanceled}
		switch {
		case live:
			end := timerAt(op.Lease.Deadline.Add(LeaseSkew), now)
			inFlight = minTime(inFlight, end)
			waitOp(op, WaitInFlight, end)
			continue
		case conv.Closed:
			closeOp(op, cur, Outcome{Arm: ArmSkipped, Reason: ReasonConversationClosed})
			continue
		case expired && op.Cancel.Recorded():
			closeOp(op, cur, canceled)
			continue
		case expired && now.After(op.FinishBy):
			closeOp(op, cur, Outcome{Arm: ArmFailed, Reason: ReasonDeadlineInFlight})
			continue
		case expired && op.Policy.Guarantee == GuaranteeAtMostOnce:
			closeOp(op, cur, Outcome{Arm: ArmFailed, Reason: ReasonAmbiguousDelivery})
			continue
		case expired && now.After(op.StartBy):
			closeOp(op, cur, Outcome{Arm: ArmFailed, Reason: ReasonDeadlineInFlight})
			continue
		case expired && cur.Failures >= MaxFailures:
			cur.Failures = MaxFailures
			closeOp(op, cur, Outcome{Arm: ArmFailed, Reason: ReasonPoison})
			continue
		case !executing && op.Cancel.Recorded():
			closeOp(op, cur, canceled)
			continue
		case !executing && now.After(op.StartBy):
			closeOp(op, cur, Outcome{Arm: ArmFailed, Reason: startByReason(op)})
			continue
		}
		past = append(past, cur)
		// An expired op that survived the rules above is a re-take
		// candidate; it still passes the scope and state rules.
		e := entry{op: op, urgent: isUrgent(op), retake: expired, cur: cur}
		if ns != nil {
			ns.e = e
		}
		scope := scopeCmp(op.Scope, rp.inc)
		switch {
		case scope < 0:
			closeOp(op, cur, Outcome{Arm: ArmSkipped, Reason: ReasonStale})
			continue
		case spec.Class == ClassControl && conv.Dormant:
			closeOp(op, cur, Outcome{Arm: ArmSkipped, Reason: ReasonStale})
			continue
		case scope > 0:
			waitOp(op, WaitScopeFuture, time.Time{})
			continue
		}
		// A lowered deliver-now message passed its after at the lowering,
		// and the target, closed, is immutable: it is not read again.
		loweredNow := isNowMessage(op) && op.Status != StatusSubmitted
		if op.After != "" && !loweredNow {
			if snap.openIDs[op.After] {
				waitOp(op, WaitAfterPending, time.Time{})
				continue
			}
			t := snap.after[op.After]
			switch {
			case t.state == afterClosed && t.arm != ArmSucceeded:
				closeOp(op, cur, Outcome{Arm: ArmSkipped, Reason: ReasonDependencyFailed})
				continue
			case (t.state == afterClosed || t.state == afterOpen) && t.conversation != op.Conversation:
				closeOp(op, cur, Outcome{Arm: ArmFailed, Reason: RefusedReasonPrefix + "after_scope"})
				continue
			case t.state == afterUnreadable && t.undecodable:
				// A row this build cannot decode will not decode on a retry:
				// wait, as after_pending does, until a change or start_by.
				waitOpDetail(op, WaitAfterUnreadable, t.detail(), time.Time{})
				continue
			case t.state == afterUnreadable:
				// Nothing rings when a store read recovers: retry on a timer.
				waitOpDetail(op, WaitAfterUnreadable, t.detail(), now.Add(TransientBackoffBase))
				continue
			case t.state != afterClosed:
				waitOp(op, WaitAfterPending, time.Time{})
				continue
			}
		}
		class := &rp.content
		if spec.Class == ClassControl {
			class = &rp.control
		}
		switch {
		case op.NotBefore.After(now):
			waitOp(op, WaitNotBefore, op.NotBefore)
			continue
		case op.RetryAt.After(now):
			waitOp(op, WaitRetryBackoff, op.RetryAt)
			e.wait, e.blocks = WaitRetryBackoff, true
		case spec.Class == ClassContent && !e.urgent && conv.ContentHold:
			waitOp(op, WaitContentHold, time.Time{})
			e.wait, e.blocks = WaitContentHold, true
		case ns == nil:
			rp.remaining++
		}
		if ns != nil {
			ns.e, ns.placed, ns.blocks = e, true, e.blocks
			continue
		}
		*class = append(*class, e)
	}
	rp.planNow(snap, conv)
	orderEntries(rp.control)
	orderEntries(rp.content)
	rp.groups = replayGroups(past, rp.closes)
	rp.responds = openResponds(past, rp.closes)
	rp.closeRow = conv.Closed && rp.remaining == 0 && rowOK && !rt.row.Closed
	// A lowering is written and the conversation read again before any
	// Take, so no content goes ahead of the unit it forms.
	rp.takePossible = rp.decideTake(conv, rt, rowOK, inFlight, now) && len(rp.lowerings) == 0
	return rp
}

// timerAt is the pass timer of a rule that applies once now is past at, for
// an at not before now: at, or the next instant when now is at. A host sleeps
// until the timer, so a timer at now would make it spin.
func timerAt(at, now time.Time) time.Time {
	if !at.After(now) {
		return now.Add(time.Nanosecond)
	}
	return at
}

// replayGroups maps each delivery key to the maybe-delivered ops under it
// among ops, less the ones closes close.
func replayGroups(ops []Op, closes []ruleClose) map[string][]Op {
	closed := make(map[string]bool, len(closes))
	for _, c := range closes {
		closed[c.cur.ID] = true
	}
	groups := map[string][]Op{}
	for _, op := range ops {
		if op.MaybeDelivered > 0 && op.DeliveryKey != "" && !closed[op.ID] {
			groups[op.DeliveryKey] = append(groups[op.DeliveryKey], op)
		}
	}
	return groups
}

// openResponds returns the respond ops among ops, less the ones closes
// close.
func openResponds(ops []Op, closes []ruleClose) []Op {
	var out []Op
	for _, op := range ops {
		if op.Kind == KindRespond && !slices.ContainsFunc(closes, func(c ruleClose) bool { return c.cur.ID == op.ID }) {
			out = append(out, op)
		}
	}
	return out
}

// decideTake reports whether a Take is possible and, when it is not because
// of the conversation's state, turns every candidate into a wait. inFlight
// is the earliest end of a live op lease, zero when none is in flight.
func (rp *rulesPlan) decideTake(conv ConversationView, rt runtimeSnap, rowOK bool, inFlight, now time.Time) bool {
	candidates := 0
	for _, e := range append(slices.Clone(rp.control), rp.content...) {
		if e.candidate() {
			candidates++
		}
	}
	if conv.Closed || candidates == 0 || rp.rowErr != nil {
		return false
	}
	if !inFlight.IsZero() {
		rp.waitCandidates(WaitInFlight, inFlight, false)
		return false
	}
	view := incarnation{conv.Generation, conv.Epoch}
	cellHeld := rowOK && !rt.row.Lease.IsZero() && !now.After(rt.row.Lease.Deadline.Add(LeaseSkew))
	switch {
	case cellHeld:
		until := timerAt(rt.row.Lease.Deadline.Add(LeaseSkew), now)
		rp.waitCandidates(WaitInFlight, until, false)
		rp.next = minTime(rp.next, until)
	case view.cmp(rp.inc) != 0:
		// The view is behind the row, and nothing rings when the host's
		// view catches up: retry on a timer.
		until := now.Add(TransientBackoffBase)
		rp.waitCandidates(WaitIncarnation, until, false)
		rp.next = minTime(rp.next, until)
	case conv.Generation < 1:
		rp.waitCandidates(WaitNoSession, time.Time{}, true)
	case conv.Dormant:
		rp.waitCandidates(WaitDormant, time.Time{}, true)
	default:
		return true
	}
	return false
}

// waitCandidates reports every candidate as waiting r. wake sets the wake
// need for an if_needed content candidate.
func (rp *rulesPlan) waitCandidates(r WaitReason, until time.Time, wake bool) {
	for _, list := range [][]entry{rp.control, rp.content} {
		for _, e := range list {
			if !e.candidate() {
				continue
			}
			for _, id := range entryIDs(e) {
				rp.waits = append(rp.waits, OpWait{ID: id, Reason: r, Until: until})
			}
			if wake && wantsWake(e.op) {
				rp.wake = true
			}
		}
	}
}

// wantsWake reports a content op that may wake its conversation.
func wantsWake(op Op) bool {
	return op.Kind == KindMessage && op.Policy.Message.Wake == WakeIfNeeded
}

// orderEntries sorts one class: urgent before normal, then CompareOrder; then
// the slots each (precedence, principal) group's client_seq ops hold are
// refilled with those ops sorted by (client_seq, CompareOrder). Ops without a
// client_seq keep their slots.
func orderEntries(es []entry) {
	slices.SortStableFunc(es, func(a, b entry) int {
		if a.urgent != b.urgent {
			if a.urgent {
				return -1
			}
			return 1
		}
		return CompareOrder(a.op, b.op)
	})
	type group struct {
		urgent    bool
		principal string
	}
	slots := map[group][]int{}
	var order []group
	for i, e := range es {
		if e.op.ClientSeq == 0 {
			continue
		}
		g := group{e.urgent, e.op.Producer.Principal}
		if _, ok := slots[g]; !ok {
			order = append(order, g)
		}
		slots[g] = append(slots[g], i)
	}
	for _, g := range order {
		idx := slots[g]
		members := make([]entry, len(idx))
		for j, i := range idx {
			members[j] = es[i]
		}
		slices.SortStableFunc(members, func(a, b entry) int {
			if a.op.ClientSeq != b.op.ClientSeq {
				if a.op.ClientSeq < b.op.ClientSeq {
					return -1
				}
				return 1
			}
			return CompareOrder(a.op, b.op)
		})
		for j, i := range idx {
			es[i] = members[j]
		}
	}
}

// takePlan is planTake's answer.
type takePlan struct {
	closes []ruleClose
	take   *takeSpec
	waits  []OpWait
	wake   bool
	busy   bool
	more   bool
	next   time.Time
	// unitBlocked are the open units (by message id) that wait on the
	// probe's content block this pass.
	unitBlocked map[string]bool
	// controlLeft says a content-only pass (Caps.ContentOnly) left a
	// takeable control op to a lane host that runs control: content waits
	// behind it.
	controlLeft bool
}

// takeSpec is the batch a Take writes.
type takeSpec struct {
	kind TakeKind
	// ops are the entries taken, in delivery order.
	ops        []entry
	key        string
	mode       LinkMode
	steer      bool
	redelivery bool
	text       string
	fence      Fence
	inc        incarnation
	baseSeq    uint64
	probe      Probe
	view       ConversationView
	// now is a deliver-now Take's path (TakeNow).
	now nowPath
	// degradation marks a delivery that neither the executor nor the
	// unfenced default marks: a post-unit deliver-now message's.
	degradation string
}

// contentItem is a content candidate as the probe sees it.
type contentItem struct {
	e        entry
	eligible bool
	starter  bool // starts a turn on its own
	rider    bool // after_turn: rides only with a starter
	stop     bool // a blocking waiter: nothing after it is taken
	closing  bool // this pass closes it
	solo     bool // goes alone: a post-unit deliver-now message
}

// planTake picks what one Take carries, given the probe (DES-O1 §4.4's when
// table and §4.5's batch formation): a control op first, else the content
// batch. It is pure.
func planTake(rp rulesPlan, probe Probe, conv ConversationView, now time.Time) (takePlan, error) {
	var tp takePlan
	if err := probe.Caps.validate(); err != nil {
		return tp, err
	}
	caps := probe.Caps
	if fenceDisagrees(probe, conv) {
		// Nothing rings when the runtime and the view agree again: retry on
		// a timer.
		tp.next = now.Add(TransientBackoffBase)
		tp.waitAll(rp, WaitIncarnation, tp.next)
		return tp, nil
	}
	if !probe.Available {
		tp.waitAll(rp, WaitUnavailable, time.Time{})
		return tp, nil
	}
	spec := takeSpec{
		fence:   Fence{Conversation: conv.ID, Generation: conv.Generation, Epoch: conv.Epoch, Token: conv.Token},
		inc:     rp.inc,
		baseSeq: rp.baseSeq,
		probe:   probe,
		view:    conv,
	}
	control, path := tp.planControl(rp, probe, now)
	// While a deliver-now unit is open no content is taken (U-ORDER): its
	// message heads the content, and its interrupt must not cut a turn
	// other content just started. Content the probe admits past a content
	// block (the 2026-10-19 ruling's drain reminder) still goes while every
	// open unit waits on that block: the block, not the unit, holds the
	// session, and the unit's interrupt runs only once it lifts.
	var content *contentBatch
	unitOpen := slices.ContainsFunc(rp.openUnits, func(id string) bool { return !tp.unitBlocked[id] })
	held := false
	if unitOpen || tp.controlLeft {
		for _, e := range rp.content {
			if e.candidate() {
				tp.wait(e.op, WaitBlocked, "")
				held = true
			}
		}
	} else {
		content = tp.planContent(rp, probe, now)
	}
	switch {
	case control != nil && control.unit != nil:
		m := control.op
		spec.kind = TakeNow
		spec.ops = []entry{{op: control.unit.op, cur: control.unit.cur}, {op: m, cur: control.cur}}
		// The message's lane_seq at this Take: the interrupt takes base+1.
		spec.key = m.ID + ":" + strconv.FormatUint(rp.baseSeq+2, 10)
		spec.mode = LinkStarts
		spec.now = path
		text, err := RenderDelivery([]Op{m}, caps.Text, false)
		if err != nil {
			return tp, err
		}
		spec.text = text
		tp.take = &spec
		tp.more = tp.more || held
	case control != nil:
		spec.kind = TakeControl
		spec.ops = []entry{*control}
		tp.take = &spec
		if content != nil {
			tp.more = true
			for _, e := range content.ops {
				tp.wait(e.op, WaitBlocked, "")
			}
		}
	case content != nil:
		spec.kind = TakeContent
		spec.ops = content.ops
		spec.key = content.key
		spec.mode = content.mode
		spec.steer = content.mode == LinkJoins && (!caps.InBand || caps.Live)
		spec.redelivery = content.redelivery
		ops := make([]Op, len(spec.ops))
		for i, e := range spec.ops {
			ops[i] = e.op
		}
		text, err := RenderDelivery(ops, caps.Text, spec.redelivery && !caps.Dedup)
		if err != nil {
			return tp, err
		}
		spec.text = text
		if head := spec.ops[0]; head.postUnit {
			// A post-unit message goes alone (contentItem.solo).
			spec.degradation = head.nowDegradation
		}
		tp.take = &spec
	}
	return tp, nil
}

// waitAll reports every candidate of both classes as waiting r, until until.
func (tp *takePlan) waitAll(rp rulesPlan, r WaitReason, until time.Time) {
	for _, list := range [][]entry{rp.control, rp.content} {
		for _, e := range list {
			if e.candidate() {
				tp.waitEntry(e, r, "", until)
			}
		}
	}
}

func (tp *takePlan) wait(op Op, r WaitReason, detail string) {
	tp.waits = append(tp.waits, OpWait{ID: op.ID, Reason: r, Detail: detail})
}

// waitUntil is wait with a timer: the wait ends by itself at until.
func (tp *takePlan) waitUntil(op Op, r WaitReason, detail string, until time.Time) {
	tp.waits = append(tp.waits, OpWait{ID: op.ID, Reason: r, Detail: detail, Until: until})
	tp.next = minTime(tp.next, until)
}

// waitEntry reports every op of e (entryIDs) as waiting r, until until.
func (tp *takePlan) waitEntry(e entry, r WaitReason, detail string, until time.Time) {
	for _, id := range entryIDs(e) {
		tp.waits = append(tp.waits, OpWait{ID: id, Reason: r, Detail: detail, Until: until})
	}
}

func (tp *takePlan) close(rp rulesPlan, e entry, o Outcome, now time.Time) {
	tp.closes = append(tp.closes, ruleClose{cur: e.op, next: closedAt(e.cur, o, rp.by, now)})
}

// planControl picks the first takeable control op (I10), closing a respond
// the runtime cannot perform. Control takes one op, or one deliver-now unit
// (planUnit), whose path it returns. Under Caps.ContentOnly it takes none:
// each takeable control op waits not_live (controlLeft).
func (tp *takePlan) planControl(rp rulesPlan, probe Probe, now time.Time) (*entry, nowPath) {
	var picked *entry
	var path nowPath
	blocked := false
	for i := range rp.control {
		e := rp.control[i]
		switch {
		case e.blocks:
			blocked = true
			continue
		case !e.candidate():
			continue
		case blocked:
			tp.waitEntry(e, WaitBlocked, "", time.Time{})
		case e.unit != nil && picked != nil:
			tp.more = true
		case e.unit != nil:
			if p, ok := tp.planUnit(rp, e, probe, now); ok {
				picked, path = &e, p
			}
		case e.op.Kind == KindRespond && rp.raisings[e.op.ID].over():
			// Its occurrence ended (another answer, or a look that saw its
			// prompt gone): whatever the runtime shows, it answers nothing.
			tp.close(rp, e, Outcome{Arm: ArmSkipped, Reason: ReasonStale}, now)
		case probe.Caps.ContentOnly:
			// This host runs no control: the op waits for one that does,
			// and content waits behind it (I10).
			tp.wait(e.op, WaitNotLive, "")
			tp.controlLeft = true
		case !probe.Caps.Live:
			tp.wait(e.op, WaitNotLive, "")
		case !probe.Running:
			tp.wait(e.op, WaitNotRunning, "")
		case e.op.Kind == KindRespond && !probe.Caps.Respond:
			tp.close(rp, e, Outcome{Arm: ArmFailed, Reason: ReasonUnsupported}, now)
		case e.op.Kind == KindRespond && !tp.respondReady(rp, e, probe, now):
			// respondReady recorded its wait or close.
		case picked == nil:
			picked = &e
		default:
			tp.more = true
		}
	}
	return picked, path
}

// contentBatch is the content batch planContent formed.
type contentBatch struct {
	ops        []entry
	key        string
	mode       LinkMode
	redelivery bool
}

// planContent classifies every content candidate against the probe and forms
// the batch (DES-O1 §4.5): a replay of a maybe-delivered head's group under
// its key, or else the longest fresh prefix within the limits.
func (tp *takePlan) planContent(rp rulesPlan, probe Probe, now time.Time) *contentBatch {
	caps := probe.Caps
	inBand := caps.InBand && !caps.Live
	boundary := probe.Boundary
	if inBand {
		boundary = TriYes
	}
	// blockOf is why op waits on the runtime's state before anything else:
	// the content block, unless op is of a class the probe admits past it,
	// then a pending interaction.
	blockOf := func(op Op) (WaitReason, string) {
		switch {
		case probe.ContentBlock != "" && !probe.admits(op):
			return WaitContentBlock, probe.ContentBlock
		case probe.Pending != "":
			return WaitPendingInteraction, ""
		}
		return "", ""
	}
	items := make([]contentItem, 0, len(rp.content))
	for _, e := range rp.content {
		it := contentItem{e: e, solo: e.postUnit}
		if !e.candidate() {
			it.stop = e.blocks
			items = append(items, it)
			continue
		}
		op := e.op
		m := op.Policy.Message
		blockReason, blockDetail := blockOf(op)
		switch {
		case blockReason != "":
			tp.wait(op, blockReason, blockDetail)
		case !caps.Live && !caps.InBand:
			tp.wait(op, WaitNotLive, "")
		case caps.Live && !probe.Running:
			tp.wait(op, WaitNotRunning, "")
			// An op admitted past a content block never wakes the runtime:
			// the block, not the op, decides whether the row may start.
			if wantsWake(op) && probe.ContentBlock == "" {
				tp.wake = true
			}
		case inBand:
			if m.Role == RoleNotice && (m.When == WhenBoundary || m.When == WhenAfterTurn) {
				it.eligible = true
			} else {
				tp.wait(op, WaitNotLive, "")
			}
		case boundary == TriUnknown:
			tp.wait(op, WaitBoundaryUnknown, "")
			tp.busy = true
		case m.When == WhenBoundary, e.postUnit:
			// A post-unit deliver-now message waits for the boundary its
			// interrupt did not reach.
			if boundary == TriYes {
				it.eligible, it.starter = true, true
			} else {
				tp.wait(op, WaitBusy, "")
				tp.busy = true
			}
		case m.When == WhenAfterTurn:
			if boundary == TriYes {
				it.eligible, it.rider = true, true
			} else {
				tp.wait(op, WaitBusy, "")
				tp.busy = true
			}
		case m.When == WhenIfIdle:
			switch {
			case boundary == TriYes:
				it.eligible, it.starter = true, true
			case e.maybeDelivered():
				// Its own delivery may have started the busy turn.
				tp.wait(op, WaitBusy, "")
				tp.busy = true
			default:
				tp.close(rp, e, Outcome{Arm: ArmSkipped, Reason: ReasonBusy}, now)
				it.closing = true
			}
		case m.When == WhenSteer:
			switch {
			case boundary == TriYes:
				it.eligible, it.starter = true, true
			case caps.Steer:
				it.eligible = true
			case m.OnUnsupported == FallbackFail && !e.maybeDelivered():
				tp.close(rp, e, Outcome{Arm: ArmFailed, Reason: ReasonUnsupported}, now)
				it.closing = true
			default:
				tp.wait(op, WaitBusy, "")
				tp.busy = true
			}
		default:
			// Every when is handled above; a deliver-now message reaches
			// content only post-unit.
			tp.wait(op, WaitBlocked, "")
		}
		items = append(items, it)
	}
	mode := LinkStarts
	if inBand || boundary == TriNo {
		mode = LinkJoins
	}
	batch := tp.formBatch(items, mode, inBand, rp.baseSeq, caps, rp.groups)
	if batch != nil {
		tp.busy = false
	}
	// A busy boundary that names its deadline wakes the pass there; a
	// deadline at or before now is stale, and the host's backoff finds it.
	if tp.busy && boundary == TriNo && probe.BoundaryAt.After(now) {
		tp.next = minTime(tp.next, probe.BoundaryAt)
	}
	return batch
}

// formBatch picks the batch from the classified items under the runtime's
// caps. A runtime that deduplicates by delivery key gets a replay group whole
// or not at all; groups holds every replay group's open members, in the class
// or not. A head whose text alone is over caps.MaxText waits text_too_long and
// holds the items behind it.
func (tp *takePlan) formBatch(items []contentItem, mode LinkMode, ridersFree bool, baseSeq uint64, caps Caps, groups map[string][]Op) *contentBatch {
	whole := caps.Dedup
	for {
		head := -1
		for i, it := range items {
			if it.stop {
				break
			}
			if it.eligible {
				head = i
				break
			}
		}
		if head < 0 {
			tp.reportLeft(items, nil, false)
			return nil
		}
		var picked []int
		full := false
		replay := items[head].e.maybeDelivered()
		group := groups[items[head].e.op.DeliveryKey]
		text := textBudget{max: caps.MaxText, style: caps.Text, note: replay && !whole}
		if !text.fits([]Op{items[head].e.op}) {
			tp.wait(items[head].e.op, WaitTextTooLong, "")
			items[head].eligible, items[head].stop = false, true
			continue
		}
		if replay {
			var split bool
			picked, full, split = pickReplay(items, head, whole, group, text)
			if split {
				// The head waits for its group and, as a blocking waiter,
				// holds everything after it (I8).
				tp.wait(items[head].e.op, WaitReplayGroup, "")
				items[head].eligible, items[head].stop = false, true
				continue
			}
		} else {
			picked, full = pickFresh(items, head, text)
		}
		if !replay || groupHasNonRider(items, head, group) {
			// A replay group of riders only lost its starter to a close;
			// it may already have reached the agent with that turn, so it
			// goes without one rather than wait until start_by.
			picked = dropLoneRiders(items, picked, ridersFree)
		}
		if len(picked) == 0 {
			// Only riders were picked and nothing starts a turn for them.
			for i := head; i < len(items) && !items[i].stop; i++ {
				if items[i].eligible && items[i].rider {
					tp.wait(items[i].e.op, WaitAfterTurn, "")
					items[i].eligible = false
				}
			}
			continue
		}
		b := &contentBatch{mode: mode, redelivery: replay}
		for _, i := range deliveryOrder(items, picked) {
			b.ops = append(b.ops, items[i].e)
		}
		headOp := b.ops[0].op
		b.key = headOp.DeliveryKey
		// A released head whose last attempt had its key refused, or a
		// replay head with no stored key (a re-take that never had an
		// attempt), gets a new key. A re-take's last attempt is the expired
		// one, under the stored key, whatever an earlier attempt's last_error
		// says (L3).
		keyRefused := !b.ops[0].retake && strings.HasPrefix(headOp.LastError, lastErrorKeyRejected)
		if !replay || b.key == "" || keyRefused {
			b.key = headOp.ID + ":" + strconv.FormatUint(baseSeq+1, 10)
		}
		tp.reportLeft(items, picked, full)
		return b
	}
}

// pickReplay picks the head's replay group: the open items that share its
// delivery key (an item this pass closes has left the group), within the
// limits and the text budget. A member that is not eligible, sits behind a
// stop, or is in group but not among the items (the rules kept it out of the
// class) is left for a later Take; when whole is set that would split the
// group, so split is reported and nothing is picked.
func pickReplay(items []contentItem, head int, whole bool, group []Op, text textBudget) (picked []int, full, split bool) {
	lim := batchLimits{text: text}
	stopped := false
	for i, it := range items {
		if it.stop && i != head {
			stopped = true
		}
		if !inReplayGroup(items, head, i) {
			continue
		}
		if !it.eligible || stopped || full || !lim.admit(it.e.op) {
			if whole {
				return nil, false, true
			}
			if it.eligible && !stopped && i > head {
				full = true
			}
			continue
		}
		picked = append(picked, i)
	}
	if whole {
		for _, op := range group {
			if !slices.ContainsFunc(items, func(it contentItem) bool { return it.e.op.ID == op.ID }) {
				return nil, false, true
			}
		}
	}
	return picked, full, false
}

// groupHasNonRider reports whether the head's replay group still holds an
// open member, eligible or not and in the class or not, that is not an
// after_turn rider.
func groupHasNonRider(items []contentItem, head int, group []Op) bool {
	for i, it := range items {
		if inReplayGroup(items, head, i) && it.e.op.Policy.Message.When != WhenAfterTurn {
			return true
		}
	}
	for _, op := range group {
		inClass := slices.ContainsFunc(items, func(it contentItem) bool { return it.e.op.ID == op.ID })
		if !inClass && op.Policy.Message.When != WhenAfterTurn {
			return true
		}
	}
	return false
}

// inReplayGroup reports whether item i is in the replay group of the
// maybe-delivered head: the head, or a maybe-delivered item under the head's
// delivery key, that this pass does not close.
func inReplayGroup(items []contentItem, head, i int) bool {
	it, key := items[i], items[head].e.op.DeliveryKey
	member := i == head || (key != "" && it.e.op.DeliveryKey == key && it.e.cur.MaybeDelivered > 0)
	return member && !it.closing
}

// pickFresh picks the longest fresh prefix from head: it stops at a stop, at
// a maybe-delivered op, at a solo op (a solo head goes alone) and at the
// first op the limits or the text budget refuse.
func pickFresh(items []contentItem, head int, text textBudget) ([]int, bool) {
	lim := batchLimits{text: text}
	var picked []int
	for i := head; i < len(items); i++ {
		it := items[i]
		if it.stop {
			break
		}
		if !it.eligible {
			continue
		}
		if it.e.maybeDelivered() || it.solo && i != head {
			break
		}
		if !lim.admit(it.e.op) {
			return picked, true
		}
		picked = append(picked, i)
		if it.solo {
			break
		}
	}
	return picked, false
}

// batchLimits admits ops up to one prompt, MaxBatchOps ops, MaxBatchPayload
// bytes and the text budget.
type batchLimits struct {
	ops, payload int
	prompt       bool
	text         textBudget
	// admitted are the ops admitted, kept only under a text budget.
	admitted []Op
}

func (l *batchLimits) admit(op Op) bool {
	prompt := op.Policy.Message.Role == RolePrompt
	if l.ops+1 > MaxBatchOps || l.payload+len(op.Payload) > MaxBatchPayload || (prompt && l.prompt) {
		return false
	}
	if l.text.max > 0 {
		l.admitted = append(l.admitted, op)
		if !l.text.fits(l.admitted) {
			l.admitted = l.admitted[:len(l.admitted)-1]
			return false
		}
	}
	l.ops++
	l.payload += len(op.Payload)
	l.prompt = l.prompt || prompt
	return true
}

// textBudget is the most bytes of text one delivery may carry (Caps.MaxText;
// 0 is no cap), rendered in style, ending in the RedeliveryNote when note is
// set.
type textBudget struct {
	max   int
	style TextStyle
	note  bool
}

// fits reports whether the delivery of ops, a batch the limits admit, renders
// within the budget.
func (b textBudget) fits(ops []Op) bool {
	return b.max == 0 || len(renderDelivery(ops, b.style, b.note)) <= b.max
}

// dropLoneRiders drops after_turn riders from a batch that holds no
// turn-starting op, unless riders may go alone (an in-band pass).
func dropLoneRiders(items []contentItem, picked []int, ridersFree bool) []int {
	if ridersFree {
		return picked
	}
	for _, i := range picked {
		if items[i].starter {
			return picked
		}
	}
	var out []int
	for _, i := range picked {
		if !items[i].rider {
			out = append(out, i)
		}
	}
	return out
}

// deliveryOrder puts the batch's notices first, in order, then its prompt.
func deliveryOrder(items []contentItem, picked []int) []int {
	out := make([]int, 0, len(picked))
	var prompt []int
	for _, i := range picked {
		if items[i].e.op.Policy.Message.Role == RolePrompt {
			prompt = append(prompt, i)
			continue
		}
		out = append(out, i)
	}
	return append(out, prompt...)
}

// reportLeft reports each eligible item outside the batch: batch_full when
// the limits cut the batch, blocked otherwise. Eligible items before the
// first stop make the next pass due at once; items behind a stop wait.
func (tp *takePlan) reportLeft(items []contentItem, picked []int, full bool) {
	stopped := false
	for i, it := range items {
		if it.stop {
			stopped = true
			continue
		}
		if !it.eligible || slices.Contains(picked, i) {
			continue
		}
		if stopped {
			tp.wait(it.e.op, WaitBlocked, "")
			continue
		}
		if full {
			tp.wait(it.e.op, WaitBatchFull, "")
		} else {
			tp.wait(it.e.op, WaitBlocked, "")
		}
		if picked != nil {
			tp.more = true
		}
	}
}
