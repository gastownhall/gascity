package opjournal

// Deliver-now (OJ-17; DES-O1 §10.3, D-DN): the conversation lane lowers a
// deliver-now message M (when: now, or the legacy interrupt) into a derived
// interrupt I in one guarded batch, then takes [I, M] as one unit under one
// lease and runs one effect: the session's replacement step for I, then M
// once the step reached the session's boundary. No after link joins them.
// A unit whose interrupt does not end at a boundary leaves M open as urgent
// content, delivered alone at the next boundary and marked with why it was
// late.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Deliver-now vocabulary.
const (
	// DegradationInterruptFailed marks a deliver-now message delivered at a
	// boundary after its derived interrupt failed, or after an interrupt
	// that cannot be read back here: among them a closed one a lane of a
	// third namespace pinned, whatever its outcome.
	DegradationInterruptFailed = "interrupt_failed"
	// DegradationInterruptUnsupported marks a deliver-now message delivered
	// at a boundary because its session cannot interrupt the turn
	// (failed{unsupported}).
	DegradationInterruptUnsupported = "interrupt_unsupported"
)

const (
	// nowInterruptKeySuffix makes a derived interrupt's client key from
	// its message's id.
	nowInterruptKeySuffix = ":interrupt"
	// nowDeliveryMinBudget is the least time a unit's message needs left in
	// the effect after the replacement step; with less it waits for the
	// boundary.
	nowDeliveryMinBudget = 10 * time.Second
	// lastErrorNotAttempted is the last_error token of a unit's message
	// released without being sent.
	lastErrorNotAttempted = "not_attempted"
	// nowCascadeBy is the cancel author of a derived interrupt canceled
	// with its message.
	nowCascadeBy = "lane:cascade"
	// Refused reasons of a deliver-now message the lane cannot lower.
	refusedNowInterruptTaken   = RefusedReasonPrefix + "now_interrupt_taken"
	refusedNowInterruptInvalid = RefusedReasonPrefix + "now_interrupt_invalid"
)

// NowInterruptID returns the pinned id of deliver-now message m's derived
// interrupt in namespace prefix: client key <m.ID>:interrupt under m's
// conversation and principal, so a producer can watch it.
func NowInterruptID(prefix string, m Op) (string, error) {
	return DeriveID(prefix, m.Conversation, m.Producer.Principal, m.ID+nowInterruptKeySuffix)
}

// nowInterruptIDs are the ids deliver-now message m's derived interrupt may
// have in a store of namespace prefix: pinned in prefix, the id a lowering
// here creates, then in m's own namespace, where a lane of that namespace
// lowered m before it moved here with its id.
func nowInterruptIDs(prefix string, m Op) ([]string, error) {
	id, err := NowInterruptID(prefix, m)
	if err != nil {
		return nil, err
	}
	own := opIDPrefix(m.ID)
	if own == "" || own == prefix {
		return []string{id}, nil
	}
	ownID, err := NowInterruptID(own, m)
	if err != nil {
		return nil, err
	}
	return []string{id, ownID}, nil
}

// opIDPrefix is the namespace prefix an op id was minted in: what precedes
// its last -op- infix, since the hash after it holds no '-'; "" for an id
// that is no op id.
func opIDPrefix(id string) string {
	if !validOpID(id) {
		return ""
	}
	return id[:strings.LastIndex(id, opIDInfix)]
}

// TurnAfter is a session's turns as a deliver-now unit's replacement step
// left them. The zero value says nothing is known.
type TurnAfter struct {
	// Boundary yes: the message may go now; no: another turn runs; unknown:
	// the step cannot tell.
	Boundary Tri
	// Live and Turn are the message's link evidence's LiveAtProbe and
	// LiveTurn.
	Live Tri
	Turn string
	// Known are the turn ids the step's last read returned and the replaced
	// turn, which is known whether or not that read succeeded.
	Known []string
}

// FusedNowExecutor runs a deliver-now unit as one runtime command (T3
// protocol 2's restart_active). An executor whose probe sets Caps.FusedNow
// must implement it.
type FusedNowExecutor interface {
	DeliverNow(ctx context.Context, c NowCall) NowResult
}

// NowCall is one fused deliver-now effect: the unit's interrupt step and its
// message, as the unfused path would send them.
type NowCall struct {
	// Interrupt has ForReplacement set.
	Interrupt InterruptCall
	// Delivery has Replacement set.
	Delivery Delivery
}

// NowResult is a fused deliver-now effect's result.
type NowResult struct {
	// Interrupt is the replacement step's result.
	Interrupt ControlResult
	// Delivery is the message's result; nil when the command never reached
	// it, which only an Interrupt that sent nothing can say (NotDelivered,
	// skipped{stale}, failed{unsupported}). A nil Delivery after any other
	// Interrupt breaks the contract: the message is then recorded Unknown.
	Delivery DeliveryResult
}

// isNowMessage reports a deliver-now lane message.
func isNowMessage(op Op) bool {
	if op.Kind != KindMessage || op.Executor != ExecutorLane {
		return false
	}
	w := op.Policy.Message.When
	return w == WhenNow || w == WhenInterrupt
}

// nowKeyShape reports a lane interrupt keyed as a derived interrupt: client
// key <causation>:interrupt.
func nowKeyShape(op Op) bool {
	if op.Kind != KindInterrupt || op.Executor != ExecutorLane || op.Causation == "" {
		return false
	}
	head, ok := strings.CutSuffix(op.ClientKey, nowInterruptKeySuffix)
	return ok && head == op.Causation
}

// hasNowShape reports a lane interrupt that may be a derived interrupt, its
// deliver-now partner to be read: keyed as one (nowKeyShape), with a
// causation that is an op id of any namespace, since a message that moved
// here with its id pairs by identity (isDerivedOf). Any other causation (a
// mail or session bead, a producer's own id) names no deliver-now message,
// and the interrupt is plain.
func hasNowShape(op Op) bool {
	return nowKeyShape(op) && validOpID(op.Causation)
}

// isDerivedOf reports whether i is deliver-now message m's derived
// interrupt: its id is pinned by m's conversation, principal and id
// (checkOpID ties the id to those), and it names m as its causation.
func isDerivedOf(i, m Op) bool {
	return nowKeyShape(i) && isNowMessage(m) && i.Causation == m.ID &&
		i.Conversation == m.Conversation && i.Producer.Principal == m.Producer.Principal
}

// nowRow is a row a pass read by id for deliver-now: a derived interrupt, or
// its message, that is not open in the pass's read.
type nowRow struct {
	// state is afterClosed for a row found (open or not: see op.Status),
	// afterMissing or afterUnreadable. A row found that is no op, or an op
	// of a kind this build does not know, is afterClosed with a zero op: it
	// is neither a deliver-now message nor a derived interrupt.
	state afterState
	op    Op
	// err is an unreadable row's read or decode error; undecodable says it
	// will not decode on a retry either.
	err         error
	undecodable bool
}

// detail is the wait detail of an unreadable row.
func (r nowRow) detail() string {
	if r.err == nil {
		return ""
	}
	return r.err.Error()
}

// readNow reads, by id, each deliver-now partner that is not open: the
// derived interrupt of each deliver-now message, at each id it may have (is
// its id free, or how did it close?), and the message of each derived-shape
// interrupt (how did it close?). Gets, not lists.
func (l *ConversationLane) readNow(snap *laneSnapshot) {
	snap.prefix = l.store.prefix
	snap.nowRows = map[string]nowRow{}
	read := func(id string) {
		if id == "" || snap.openIDs[id] {
			return
		}
		if _, seen := snap.nowRows[id]; seen {
			return
		}
		snap.nowRows[id] = l.readNowRow(id)
	}
	for _, op := range snap.ops {
		switch {
		case isNowMessage(op):
			if ids, err := nowInterruptIDs(snap.prefix, op); err == nil {
				for _, id := range ids {
					read(id)
				}
			}
		case hasNowShape(op):
			read(op.Causation)
		}
	}
}

func (l *ConversationLane) readNowRow(id string) nowRow {
	b, err := l.store.store.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return nowRow{state: afterMissing}
	}
	if err != nil {
		return nowRow{state: afterUnreadable, err: err}
	}
	if !isOpBead(b) {
		return nowRow{state: afterClosed}
	}
	op, err := DecodeOp(b)
	switch {
	case errors.Is(err, ErrUnsupportedKind):
		// Deliver-now messages and interrupts are kinds this build knows.
		return nowRow{state: afterClosed}
	case err != nil:
		undecodable := errors.Is(err, ErrUnsupportedSchema) || errors.Is(err, ErrUnsupportedValue) || errors.Is(err, ErrCorruptOp)
		return nowRow{state: afterUnreadable, err: err, undecodable: undecodable}
	}
	return nowRow{state: afterClosed, op: op}
}

// nowOpState is a deliver-now op's phase-1 decision (planRules), which
// planNow finishes: a close stands (closing), while a wait or a class
// placement waits for the op's partner.
type nowOpState struct {
	e       entry
	closing *Outcome
	// wait, detail and until are the wait phase 1 decided; "" for none.
	wait   WaitReason
	detail string
	until  time.Time
	// placed says phase 1 reached class placement: a candidate, or (wait
	// set, blocks) a blocking waiter.
	placed bool
	blocks bool
}

// nowUnit is a unit entry's derived interrupt, as read (op) and with the
// rules' bookkeeping (cur).
type nowUnit struct {
	op  Op
	cur Op
}

// nowLowering is one lowering: create i, and move m (as read) to mNext.
type nowLowering struct {
	m, mNext, i Op
}

// nowPath is how a unit's effect runs.
type nowPath int

const (
	// nowDirect: no turn to replace; the message goes at once.
	nowDirect nowPath = iota + 1
	// nowReplace: the interrupt's replacement step, then the message.
	nowReplace
	// nowFused: one runtime command (FusedNowExecutor).
	nowFused
)

// nowOp returns op's deliver-now state, made on first use, or nil for an op
// that is neither a deliver-now message nor a derived-shape interrupt.
func (rp *rulesPlan) nowOp(op Op) *nowOpState {
	if !isNowMessage(op) && !hasNowShape(op) {
		return nil
	}
	st := &nowOpState{e: entry{op: op, cur: op}}
	rp.nowState[op.ID] = st
	rp.nowOrder = append(rp.nowOrder, op.ID)
	return st
}

// emitWait reports st's phase-1 wait for each of ids.
func (rp *rulesPlan) emitWait(st *nowOpState, ids ...string) {
	for _, id := range ids {
		rp.waitOpDetail(Op{ID: id}, st.wait, st.detail, st.until)
	}
}

// emitPlain finishes st as phase 1 would have for any op: its wait, then
// its class placement.
func (rp *rulesPlan) emitPlain(st *nowOpState, class *[]entry) {
	if st.wait != "" {
		rp.emitWait(st, st.e.op.ID)
	}
	if !st.placed {
		return
	}
	e := st.e
	e.wait, e.blocks = "", false
	if st.wait != "" {
		e.wait, e.blocks = st.wait, st.blocks
	} else {
		rp.remaining++
	}
	*class = append(*class, e)
}

// planNow is the rule phase's second step (pure): it pairs every
// deliver-now message with its derived interrupt and emits their final
// closes, waits and entries: a lowering for a message not yet lowered, a
// unit entry while both are open, a post-unit content entry once the
// interrupt is closed, and a close for an orphan interrupt. Pairing goes by
// identity (isDerivedOf), never by namespace: a message that moved here
// with its id pairs with its open derived interrupt wherever a lane pinned
// it.
func (rp *rulesPlan) planNow(snap laneSnapshot, conv ConversationView) {
	open := make(map[string]Op, len(snap.ops))
	for _, op := range snap.ops {
		open[op.ID] = op
	}
	quarantined := make(map[string]bool, len(snap.quarantined))
	for _, q := range snap.quarantined {
		quarantined[q.ID] = true
	}
	// partner is each open message's open derived interrupt, the first in
	// read order.
	partner := map[string]*nowOpState{}
	for _, id := range rp.nowOrder {
		st := rp.nowState[id]
		if m, ok := open[st.e.op.Causation]; ok && isDerivedOf(st.e.op, m) && partner[m.ID] == nil {
			partner[m.ID] = st
		}
	}
	for _, id := range rp.nowOrder {
		st := rp.nowState[id]
		if isNowMessage(st.e.op) {
			rp.planNowMessage(snap, conv, st, partner[id], quarantined)
		} else {
			rp.planNowInterrupt(snap, st, open, quarantined)
		}
	}
}

// planNowMessage finishes deliver-now message st, whose open derived
// interrupt is ist (nil when none is open in the read). A lowered message
// whose derived interrupt is open, by the read or by its own row, or whose
// interrupt's read failed transiently, holds the content (openUnits). A
// derived row this build cannot decode (quarantined, or undecodable at its
// id) holds nothing: this build cannot tell whether the unit is open, and
// holding every content op behind a row it will never read would stall the
// conversation until the message's start_by. The newer build that wrote
// the row orders the unit when it serves the lane.
func (rp *rulesPlan) planNowMessage(snap laneSnapshot, conv ConversationView, st, ist *nowOpState, quarantined map[string]bool) {
	m := st.e.op
	if st.closing != nil {
		return
	}
	ids, err := nowInterruptIDs(snap.prefix, m)
	if err != nil {
		rp.closeOp(m, st.e.cur, Outcome{Arm: ArmFailed, Reason: refusedNowInterruptInvalid})
		return
	}
	switch {
	case m.Status == StatusSubmitted:
		rp.planLowering(snap, conv, st, ist, ids)
	case ist != nil && ist.closing != nil:
		rp.postUnit(st, nowDegradation(*ist.closing, true))
	case ist != nil:
		rp.planUnitEntry(st, ist)
	default:
		for _, iid := range ids {
			if rp.planNowRow(snap, st, iid, quarantined) {
				return
			}
		}
		// Missing, or no row that is this message's interrupt.
		rp.postUnit(st, nowDegradation(Outcome{}, false))
	}
}

// planNowRow finishes lowered message st, whose derived interrupt is not
// open in the read, by the row at iid, one of the ids that interrupt may
// have, and reports whether that row decided it: false when the row is not
// st's interrupt.
func (rp *rulesPlan) planNowRow(snap laneSnapshot, st *nowOpState, iid string, quarantined map[string]bool) bool {
	m := st.e.op
	row, read := snap.nowRows[iid]
	switch {
	case quarantined[iid]:
		// A row this build cannot decode: a newer host may serve the unit.
		rp.nowWait(st, WaitNowUnreadable, "", time.Time{}, m.ID)
	case !read:
		// Open, but not this message's lane interrupt (planNow pairs that).
		return false
	case row.state == afterUnreadable:
		until := time.Time{}
		if !row.undecodable {
			until = rp.now.Add(TransientBackoffBase)
			rp.openUnits = append(rp.openUnits, m.ID)
		}
		rp.nowWait(st, WaitNowUnreadable, row.detail(), until, m.ID)
	case row.state != afterClosed || !isDerivedOf(row.op, m) || isForwardPointer(row.op):
		// Missing, a row that is not this message's interrupt, or a forward
		// pointer a re-lowering left: never the unit's outcome.
		return false
	case row.op.Status != StatusClosed:
		// Open by its own row, though the open list missed it: read again
		// shortly.
		rp.openUnits = append(rp.openUnits, m.ID)
		rp.nowWait(st, WaitNowUnreadable, "the derived interrupt is open but was not listed", rp.now.Add(TransientBackoffBase), m.ID)
	default:
		rp.postUnit(st, nowDegradation(row.op.Outcome, true))
	}
	return true
}

// nowWait reports a deliver-now op as waiting r for each of ids: the wait
// replaces whatever phase 1 decided.
func (rp *rulesPlan) nowWait(st *nowOpState, r WaitReason, detail string, until time.Time, ids ...string) {
	st.wait, st.detail, st.until = r, detail, until
	rp.emitWait(st, ids...)
}

// planLowering finishes an unlowered deliver-now message (L1 to L6), whose
// derived interrupt may have ids here and is ist when one is open in the
// read. A derived interrupt that already exists, open anywhere (ist) or
// found at any of ids, refuses it, whatever it waits on: only a lowering
// mints that interrupt's key, and a second interrupt minted beside one that
// may have been sent could cut a turn twice. The interrupt there is an
// orphan (planNowInterrupt): it closes, or waits while it is in flight.
// Otherwise the message waits as any op does, then until each of ids can be
// read and the conversation has a session awake to run the unit, and is
// then lowered. While it waits only on a transient read (of its after
// target or of one of ids) it holds the content (openUnits).
func (rp *rulesPlan) planLowering(snap laneSnapshot, conv ConversationView, st, ist *nowOpState, ids []string) {
	m := st.e.op
	taken := func(iid string) bool {
		row, read := snap.nowRows[iid]
		return snap.openIDs[iid] || read && row.state != afterMissing && row.state != afterUnreadable
	}
	if ist != nil || slices.ContainsFunc(ids, taken) {
		// Only m's own principal can mint the key: a key misuse, refused as
		// a key reuse is.
		rp.closeOp(m, st.e.cur, Outcome{Arm: ArmFailed, Reason: refusedNowInterruptTaken})
		return
	}
	if st.wait != "" {
		if st.wait == WaitAfterUnreadable && !st.until.IsZero() && !m.NotBefore.After(rp.now) {
			// Due, and lowered once the after read succeeds, by this lane or
			// by another host that reads it now: content waits, so none is
			// taken while a unit it forms may be open (U-ORDER). A target
			// that will never decode holds nothing.
			rp.openUnits = append(rp.openUnits, m.ID)
		}
		rp.emitWait(st, m.ID)
		return
	}
	for _, iid := range ids {
		row, read := snap.nowRows[iid]
		switch {
		case !read:
			// readNow reads every free id; a snapshot without the read is
			// a planner bug, not an id to take.
			rp.nowWait(st, WaitNowUnreadable, "the derived interrupt's id was not read", rp.now.Add(TransientBackoffBase), m.ID)
			return
		case row.state == afterUnreadable:
			until := time.Time{}
			if !row.undecodable {
				// Due, and lowered once the read succeeds: content waits as
				// it does for a lowered unit, so none goes ahead of the unit
				// it forms (U-ORDER). A row that will never decode holds
				// nothing.
				until = rp.now.Add(TransientBackoffBase)
				rp.openUnits = append(rp.openUnits, m.ID)
			}
			rp.nowWait(st, WaitNowUnreadable, row.detail(), until, m.ID)
			return
		}
	}
	switch {
	case conv.Generation < 1:
		rp.nowWait(st, WaitNoSession, "", time.Time{}, m.ID)
		rp.wake = rp.wake || wantsWake(m)
	case conv.Dormant:
		rp.nowWait(st, WaitDormant, "", time.Time{}, m.ID)
		rp.wake = rp.wake || wantsWake(m)
	default:
		i, err := deriveNowInterrupt(snap.prefix, st.e.cur, rp.nowGeneration(m), rp.now, rp.by)
		if err != nil {
			rp.closeOp(m, st.e.cur, Outcome{Arm: ArmFailed, Reason: refusedNowInterruptInvalid})
			return
		}
		next := st.e.cur
		next.Status = StatusAccepted
		next.Transitions = appendTransition(next.Transitions, Transition{Status: StatusAccepted, At: rp.now, By: rp.by})
		rp.lowerings = append(rp.lowerings, nowLowering{m: m, mNext: next, i: i})
		rp.remaining++
	}
}

// nowGeneration is the generation m's derived interrupt is scoped to: the
// one m names, else the effective incarnation's at lowering.
func (rp *rulesPlan) nowGeneration(m Op) int64 {
	if g := m.Policy.Message.NowGeneration; g > 0 {
		return g
	}
	return rp.inc.gen
}

// planUnitEntry finishes a lowered message st whose derived interrupt ist is
// open and not closing: the unit waits as its first waiting op does, for
// both ids, or is one control entry. Either way the unit is open
// (openUnits), unless it may have run.
func (rp *rulesPlan) planUnitEntry(st, ist *nowOpState) {
	m, i := st.e.op, ist.e.op
	if st.e.cur.MaybeDelivered == 0 && ist.e.cur.MaybeDelivered == 0 {
		rp.openUnits = append(rp.openUnits, m.ID)
	}
	switch {
	case ist.wait == WaitInFlight, st.wait == WaitInFlight:
		w := ist
		if w.wait != WaitInFlight {
			w = st
		}
		rp.emitWait(w, i.ID, m.ID)
		return
	case st.e.cur.MaybeDelivered > 0 || ist.e.cur.MaybeDelivered > 0:
		// Unreachable (N4): a unit that may have run closes its at most
		// once interrupt; defensive.
		rp.closeOp(i, ist.e.cur, Outcome{Arm: ArmFailed, Reason: ReasonAmbiguousDelivery})
		rp.postUnit(st, DegradationInterruptFailed)
		return
	}
	w := ist
	if w.wait == "" {
		w = st
	}
	e := entry{op: m, cur: st.e.cur, retake: st.e.retake, unit: &nowUnit{op: i, cur: ist.e.cur}}
	if w.wait != "" {
		rp.emitWait(w, i.ID, m.ID)
		if !w.placed || !w.blocks || !st.placed || !ist.placed {
			return
		}
		// A blocking waiter holds the control behind it, as either op
		// would alone.
		e.wait, e.blocks = w.wait, true
		rp.control = append(rp.control, e)
		return
	}
	rp.remaining += 2
	rp.control = append(rp.control, e)
}

// postUnit finishes a lowered message whose unit is over as urgent content
// that goes alone at a boundary, marked degradation.
func (rp *rulesPlan) postUnit(st *nowOpState, degradation string) {
	if st.wait != "" {
		rp.emitWait(st, st.e.op.ID)
	}
	if !st.placed {
		return
	}
	e := st.e
	e.urgent, e.postUnit, e.nowDegradation = true, true, degradation
	e.wait, e.blocks = "", false
	if st.wait != "" {
		e.wait, e.blocks = st.wait, st.blocks
	} else {
		rp.remaining++
	}
	rp.content = append(rp.content, e)
}

// planNowInterrupt finishes derived-shape interrupt st: a plain interrupt
// when its causation is no deliver-now message of its principal (another
// namespace's op id with no row here among them), nothing while it is in a
// unit, and otherwise an orphan, which closes (canceled with its canceled
// message, the cascade; not_needed otherwise) unless it is in flight. The
// cascade replaces a rule close of an interrupt that never ran: the
// message's cancel, not the rule, is why it ends.
func (rp *rulesPlan) planNowInterrupt(snap laneSnapshot, st *nowOpState, open map[string]Op, quarantined map[string]bool) {
	x := st.e.op
	mid := x.Causation
	orphan := func(m Op, found bool) {
		mOutcome := m.Outcome
		if ms := rp.nowState[mid]; found && ms != nil && ms.closing != nil {
			mOutcome = *ms.closing
		}
		mCanceled := found && mOutcome.Arm == ArmCanceled && (m.Status == StatusClosed || rp.nowState[mid] != nil)
		cascade := func() Op {
			cur := st.e.cur
			cur.Cancel = Cancel{By: nowCascadeBy, At: rp.now, Reason: "deliver-now message " + mid + " canceled"}
			return cur
		}
		switch {
		case st.closing != nil:
			if mCanceled && x.Status != StatusExecuting && st.closing.Arm != ArmCanceled {
				rp.replaceClose(x, cascade(), Outcome{Arm: ArmCanceled})
			}
		case st.wait == WaitInFlight:
			rp.emitWait(st, x.ID)
		case mCanceled:
			rp.closeOp(x, cascade(), Outcome{Arm: ArmCanceled})
		default:
			rp.closeOp(x, st.e.cur, Outcome{Arm: ArmSkipped, Reason: ReasonNotNeeded})
		}
	}
	// plain finishes st as any op, unless a rule closes it.
	plain := func() {
		if st.closing == nil {
			rp.emitPlain(st, &rp.control)
		}
	}
	if m, ok := open[mid]; ok {
		if !isDerivedOf(x, m) {
			plain()
			return
		}
		ms := rp.nowState[mid]
		if m.Status != StatusSubmitted && ms.closing == nil {
			return // a unit: its message finishes it
		}
		orphan(m, true)
		return
	}
	row := snap.nowRows[mid]
	switch {
	case st.closing != nil && row.state != afterClosed:
	case quarantined[mid]:
		rp.unreadablePartner(st, "", time.Time{})
	case snap.openIDs[mid]:
		// Open, and no lane conversation op: not a deliver-now message.
		plain()
	case row.state == afterUnreadable:
		until := time.Time{}
		if !row.undecodable {
			until = rp.now.Add(TransientBackoffBase)
		}
		rp.unreadablePartner(st, row.detail(), until)
	case row.state == afterMissing && opIDPrefix(mid) != snap.prefix:
		// Another namespace's op id with no row here is no message this
		// lane serves: a producer's own interrupt, caused by an op of
		// another store.
		plain()
	case row.state == afterMissing:
		orphan(Op{}, false)
	case !isDerivedOf(x, row.op):
		plain()
	case row.op.Status != StatusClosed:
		if st.closing == nil {
			rp.unreadablePartner(st, "the deliver-now message is open but was not listed", rp.now.Add(TransientBackoffBase))
		}
	default:
		orphan(row.op, true)
	}
}

// replaceClose makes the rule close of read close cur with o instead.
func (rp *rulesPlan) replaceClose(read, cur Op, o Outcome) {
	for i := range rp.closes {
		if rp.closes[i].cur.ID == read.ID {
			rp.closes[i].next = closedAt(cur, o, rp.by, rp.now)
			return
		}
	}
}

// unreadablePartner reports derived-shape interrupt st, whose message could
// not be read, as waiting now_unreadable, or in flight while it is.
func (rp *rulesPlan) unreadablePartner(st *nowOpState, detail string, until time.Time) {
	if st.wait == WaitInFlight {
		rp.emitWait(st, st.e.op.ID)
		return
	}
	rp.nowWait(st, WaitNowUnreadable, detail, until, st.e.op.ID)
}

// nowDegradation is the mark a post-unit message carries for its derived
// interrupt's outcome o (found false: no derived interrupt to read):
// interrupt_unsupported for failed{unsupported}, interrupt_failed for any
// other failure or a missing interrupt, none otherwise.
func nowDegradation(o Outcome, found bool) string {
	switch {
	case !found:
		return DegradationInterruptFailed
	case o.Arm != ArmFailed:
		return ""
	case o.Reason == ReasonUnsupported:
		return DegradationInterruptUnsupported
	default:
		return DegradationInterruptFailed
	}
}

// deriveNowInterrupt is deliver-now message m's derived interrupt, as the
// lane lowers it at now (pure; Store.Preview with an explicit clock): an at
// most once, on_failure fail lane interrupt of m's producer, keyed
// <m.ID>:interrupt, caused by m, scoped to generation gen and m's target
// turn, with the interrupt kind's deadlines clamped to m's, and no after.
func deriveNowInterrupt(prefix string, m Op, gen int64, now time.Time, by string) (Op, error) {
	spec, err := Lookup(KindInterrupt)
	if err != nil {
		return Op{}, err
	}
	req := Request{
		Conversation: m.Conversation,
		Kind:         KindInterrupt,
		Executor:     ExecutorLane,
		ClientKey:    m.ID + nowInterruptKeySuffix,
		Producer:     m.Producer,
		// At most once: an interrupt sent again after an attempt that may
		// have run can cut the turn its own message started (T5).
		Policy:      Policy{Guarantee: GuaranteeAtMostOnce, Interrupt: InterruptPolicy{OnFailure: OnFailureFail}},
		Scope:       Scope{Kind: ScopeSession, Generation: gen, Turn: m.Policy.Message.NowTurn},
		StartBy:     earliest(now.Add(spec.StartWithin), m.StartBy),
		FinishBy:    earliest(now.Add(spec.FinishWithin), m.FinishBy),
		Causation:   m.ID,
		Correlation: m.Correlation,
	}
	op, err := normalizeRequest(req, spec, now)
	if err != nil {
		return Op{}, fmt.Errorf("deriving the interrupt of deliver-now message %s: %w", m.ID, err)
	}
	if op.ID, err = NowInterruptID(prefix, m); err != nil {
		return Op{}, fmt.Errorf("deriving the interrupt of deliver-now message %s: %w", m.ID, err)
	}
	op.PayloadDigest = RequestDigest(op)
	op.Status = StatusSubmitted
	op.Transitions = []Transition{{Status: StatusSubmitted, At: op.CreatedAt, By: by}}
	if _, err := EncodeOp(op); err != nil {
		return Op{}, fmt.Errorf("deriving the interrupt of deliver-now message %s: %w", m.ID, err)
	}
	return op, nil
}

// earliest is the earlier of two times, either of which may be zero (none).
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero() || a.Before(b):
		return a
	default:
		return b
	}
}

// writeLowerings writes each lowering as one guarded batch: the derived
// interrupt created at its pinned id, and its message moved to accepted
// under its revision. done is false when a batch was refused or ended
// unknown: the pass re-reads and re-plans.
func (l *ConversationLane) writeLowerings(ctx context.Context, lowerings []nowLowering, res *PassResult) (bool, error) {
	for _, lw := range lowerings {
		b, err := EncodeOp(lw.i)
		if err != nil {
			return false, err
		}
		update, err := opUpdateItem(lw.m, lw.mNext)
		if err != nil {
			return false, err
		}
		create := b
		_, kind, err := l.store.apply(ctx, "opjournal: lane lower deliver-now", []beads.GuardedItem{{Create: &create}, update})
		switch kind {
		case batchOK:
			res.Lowered = append(res.Lowered, lw.m.ID)
		case batchInvalid:
			return false, fmt.Errorf("lowering deliver-now message %s: %w", lw.m.ID, err)
		default:
			return false, nil
		}
	}
	return true, nil
}

// planUnit decides a unit entry e against the probe (DES-O1 §10.3): it
// waits (for both ids) while the runtime cannot run it, closes its
// interrupt skipped{stale} when no turn it may target runs (the message
// then waits for the boundary, post-unit), and otherwise is taken, on the
// path the probe allows. A unit never reports Busy: a host's busy re-probe
// waits for a boundary, which a unit does not need.
func (tp *takePlan) planUnit(rp rulesPlan, e entry, probe Probe, now time.Time) (nowPath, bool) {
	i, m := e.unit.op, e.op
	caps := probe.Caps
	stale := func() {
		tp.close(rp, entry{op: i, cur: e.unit.cur}, Outcome{Arm: ArmSkipped, Reason: ReasonStale}, now)
		tp.more = true
	}
	target := i.Scope.Turn
	switch {
	case caps.ContentOnly:
		// This host runs no control: the unit waits, undecided, for one
		// that does, and content waits behind it.
		tp.waitEntry(e, WaitNotLive, "", time.Time{})
		tp.controlLeft = true
	case !caps.Live:
		tp.waitEntry(e, WaitNotLive, "", time.Time{})
	case caps.MaxText > 0 && !(textBudget{max: caps.MaxText, style: caps.Text}).fits([]Op{m}):
		tp.waitEntry(e, WaitTextTooLong, "", time.Time{})
	case !probe.Running:
		// No turn runs in a runtime that is not running.
		stale()
	case probe.ContentBlock != "":
		tp.waitEntry(e, WaitContentBlock, probe.ContentBlock, time.Time{})
		if tp.unitBlocked == nil {
			tp.unitBlocked = map[string]bool{}
		}
		tp.unitBlocked[m.ID] = true
	case probe.Pending != "":
		tp.waitEntry(e, WaitPendingInteraction, "", time.Time{})
	case probe.Boundary != TriYes && probe.Boundary != TriNo:
		until := now.Add(TransientBackoffBase)
		tp.waitEntry(e, WaitBoundaryUnknown, "", until)
		tp.next = minTime(tp.next, until)
	case probe.Boundary == TriYes && probe.TurnLive != TriYes:
		return nowDirect, true
	case probe.TurnLive == TriNo,
		target != "" && probe.TurnLive == TriYes && probe.Turn != "" && probe.Turn != target:
		// The target is not running: a newer turn is never interrupted
		// (T5).
		stale()
	case caps.FusedNow:
		return nowFused, true
	default:
		return nowReplace, true
	}
	return 0, false
}

// nowEffect is a unit's effect: each op's result, and how the message's
// step went.
type nowEffect struct {
	i        effectResult
	iPreCall time.Time
	m        effectResult
	mSent    bool
	mPreCall time.Time
	// after is the replacement step's read, when the message was sent
	// after one.
	after *TurnAfter
	// unsentWhy is why the message was not sent: its last_error text.
	unsentWhy string
}

// unitOps reports whether ops are a whole unit: its interrupt, then its
// message.
func unitOps(ops []Op) bool {
	return len(ops) == 2 && ops[0].Kind == KindInterrupt && ops[1].Kind == KindMessage
}

// runNow runs a unit's effect inside its lease, then settles both ops in one
// Done. An abandoned Take, a unit that lost an op, or no time left releases
// every op still under the lease as transient, with nothing run.
func (l *ConversationLane) runNow(ctx context.Context, exec Executor, tb *takenBatch, res *PassResult) error {
	now := l.now()
	cause := tb.abandon
	if cause == nil && !unitOps(tb.ops) {
		cause = errors.New("opjournal: a deliver-now unit lost an op between its Take and its effect")
	}
	budget := tb.lease.Deadline.Sub(now) - ExecutorMargin
	if cause == nil && budget <= 0 {
		cause = errLeaseTooShort
	}
	if cause != nil {
		if err := l.hooks.run(stageAfterEffect); err != nil {
			return err
		}
		return l.done(ctx, tb, effectResult{kind: resultNotDelivered, notDelivered: NotDelivered{Transient: true, Cause: cause}}, now, res)
	}
	ectx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ne := l.unitEffect(ectx, exec, tb)
	if err := l.hooks.run(stageAfterEffect); err != nil {
		return err
	}
	return l.doneNow(ctx, tb, ne, res)
}

// unitEffect runs the unit's path: direct (no interrupt), replace (the
// interrupt's replacement step, then the message only when the step settled
// at a boundary with time left), or fused (one command).
func (l *ConversationLane) unitEffect(ctx context.Context, exec Executor, tb *takenBatch) nowEffect {
	spec := tb.spec
	i, m := tb.ops[0], tb.ops[1]
	call := InterruptCall{Fence: spec.fence, Op: i, Turn: i.Scope.Turn, Seq: i.LaneSeq, ForReplacement: true}
	delivery := Delivery{Fence: spec.fence, Key: spec.key, Seq: m.LaneSeq, Mode: spec.mode, Ops: []Op{m}, Text: spec.text}
	ne := nowEffect{iPreCall: l.now()}
	switch spec.now {
	case nowDirect:
		ne.i = effectResult{kind: resultRejected, rejected: Rejected{Arm: ArmSkipped, Reason: ReasonStale}}
		ne.mPreCall, ne.mSent = l.now(), true
		ne.m = normalizeDelivery(exec.Deliver(ctx, delivery))
	case nowFused:
		delivery.Replacement = true
		fused, ok := exec.(FusedNowExecutor)
		if !ok {
			// Pass refuses FusedNow without DeliverNow before any Take.
			ne.i = effectResult{kind: resultNotDelivered, notDelivered: NotDelivered{Transient: true, Cause: errors.New("opjournal: fused_now without DeliverNow")}}
			ne.unsentWhy = "the executor cannot run the unit fused"
			return ne
		}
		nr := fused.DeliverNow(ctx, NowCall{Interrupt: call, Delivery: delivery})
		ne.i = normalizeControl(nr.Interrupt)
		switch {
		case nr.Delivery != nil:
			ne.m, ne.mSent, ne.mPreCall = normalizeDelivery(nr.Delivery), true, ne.iPreCall
			// Only a settled interrupt says no turn was live when the
			// message went; otherwise the probe's evidence stands (D23).
			if ne.i.kind == resultSettled {
				after := TurnAfter{Boundary: TriYes, Live: TriNo}
				if ne.i.settled.Turn != "" {
					after.Known = []string{ne.i.settled.Turn}
				}
				ne.after = &after
			}
		case !sentNothing(ne.i):
			// The command may have carried the message.
			ne.m = unknownResult(errors.New("fused executor returned no delivery result after an interrupt step that may have sent the command"))
			ne.mSent, ne.mPreCall = true, ne.iPreCall
		default:
			ne.unsentWhy = "the interrupt step ended " + stepEnd(ne.i)
		}
	default:
		ne.i = normalizeControl(exec.Interrupt(ctx, call))
		after := ne.i.settled.After
		switch {
		case ne.i.kind != resultSettled:
			ne.unsentWhy = "the interrupt step ended " + stepEnd(ne.i)
		case after.Boundary == TriNo:
			ne.unsentWhy = "the replaced turn ended but another turn runs"
		case after.Boundary != TriYes:
			ne.unsentWhy = "the session could not tell its boundary after the replacement"
		case tb.lease.Deadline.Sub(l.now())-ExecutorMargin < nowDeliveryMinBudget:
			ne.unsentWhy = "no time left in the lease to deliver"
		default:
			delivery.Replacement = true
			ne.after = &after
			ne.mPreCall, ne.mSent = l.now(), true
			ne.m = normalizeDelivery(exec.Deliver(ctx, delivery))
		}
	}
	return ne
}

// sentNothing reports an interrupt step's result that says its command sent
// nothing: not delivered, skipped{stale} or failed{unsupported}. Any other
// result may have reached the runtime.
func sentNothing(er effectResult) bool {
	switch er.kind {
	case resultNotDelivered:
		return true
	case resultRejected:
		r := er.rejected
		return r.Arm == ArmSkipped && r.Reason == ReasonStale || r.Arm == ArmFailed && r.Reason == ReasonUnsupported
	default:
		return false
	}
}

// stepEnd names how an interrupt step that did not settle ended.
func stepEnd(er effectResult) string {
	switch er.kind {
	case resultRejected:
		if er.rejected.KeyRejected {
			return "key_rejected"
		}
		return fmt.Sprintf("%s{%s}", er.rejected.Arm, er.rejected.Reason)
	case resultNotDelivered:
		return lastErrorNotDelivered
	default:
		return lastErrorUnknown
	}
}

// unitView is tb as one op of its unit sees it: kind kind, op alone, and,
// for a message sent after a replacement step, the link evidence the step's
// read left (after).
func unitView(tb *takenBatch, kind TakeKind, op Op, after *TurnAfter) *takenBatch {
	spec := *tb.spec
	spec.kind = kind
	if after != nil {
		spec.probe.TurnLive, spec.probe.Turn = after.Live, after.Turn
		known := slices.Clone(spec.probe.KnownTurns)
		for _, t := range after.Known {
			if t != "" && !slices.Contains(known, t) {
				known = append(known, t)
			}
		}
		spec.probe.KnownTurns = known
	}
	v := *tb
	v.spec = &spec
	v.ops = []Op{op}
	return &v
}

// doneNow settles a unit with one guarded batch: the runtime row's lease
// cleared, the interrupt under its own result, and the message under its
// own, or, when it was not sent, released with no gate and no failure
// counted (releaseUnattempted).
func (l *ConversationLane) doneNow(ctx context.Context, tb *takenBatch, ne nowEffect, res *PassResult) error {
	iOp, mOp := tb.ops[0], tb.ops[1]
	tbI := unitView(tb, TakeControl, iOp, nil)
	tbM := unitView(tb, TakeContent, mOp, ne.after)
	erI, successI := l.settle(tbI, ne.i, ne.iPreCall)
	var erM effectResult
	var successM Outcome
	if ne.mSent {
		erM, successM = l.settle(tbM, ne.m, ne.mPreCall)
	}
	nextI := func(cur Op, now time.Time) Op { return l.doneNext(tbI, erI, successI, cur, now) }
	nextM := func(cur Op, now time.Time) Op {
		if ne.mSent {
			return l.doneNext(tbM, erM, successM, cur, now)
		}
		return releaseUnattempted(tb, cur, now, ne.unsentWhy)
	}
	now := l.now()
	itemI := &doneItem{kind: doneOp, id: iOp.ID, cur: iOp, next: nextI(iOp, now), redo: nextI}
	itemM := &doneItem{kind: doneOp, id: mOp.ID, cur: mOp, next: nextM(mOp, now), redo: nextM}
	items := []*doneItem{{kind: doneRow, id: tb.row.ID, row: tb.row}, itemI, itemM}
	settleBy := erI.settleBy()
	if ne.mSent {
		settleBy = minTime(settleBy, erM.settleBy())
	}
	dctx, cancel := doneContext(ctx, tb, settleBy, now)
	defer cancel()
	endErr, err := l.writeDone(dctx, tb, erI, successI, items, now)
	if err == nil {
		switch {
		case itemM.next.Status != StatusClosed:
			res.Done = DoneReleased
		case itemM.next.Outcome.Arm == ArmSucceeded:
			res.Done = DoneAccepted
		default:
			res.Done = DoneRejected
		}
		// The message waits on its own, post-unit: pass again at once.
		if itemI.written && itemI.next.Status == StatusClosed && itemM.written && itemM.next.Status != StatusClosed && itemM.next.RetryAt.IsZero() {
			res.More = true
		}
	}
	endErr = errors.Join(endErr, l.reportDone(res, items, err, now))
	return errors.Join(err, erI.contractErr, erM.contractErr, endErr)
}

// releaseUnattempted releases a unit's message the effect did not send:
// accepted, no lease, no retry gate and no failure counted, the Take's
// attempt undone (an attempt counts a send, so the message's own transient
// backoff never grows across units), maybe_delivered kept, last_error
// not_attempted with why. A cancel recorded while it was in flight closes
// it canceled instead.
func releaseUnattempted(tb *takenBatch, cur Op, now time.Time, why string) Op {
	by := "lane:" + tb.lease.Nonce
	if cur.Cancel.Recorded() {
		return closedAt(cur, Outcome{Arm: ArmCanceled}, by, now)
	}
	next := cur
	next.Status = StatusAccepted
	next.Lease = Lease{}
	next.RetryAt = time.Time{}
	if next.Attempt > next.MaybeDelivered {
		next.Attempt--
	}
	next.LastError = lastError(lastErrorNotAttempted, errors.New(why))
	next.Transitions = appendTransition(cur.Transitions, Transition{Status: StatusAccepted, At: now, By: by})
	return next
}
