package opjournal

import (
	"errors"
	"fmt"
	"time"
)

// Respond outcome reasons the lane writes.
const (
	// ReasonRefusedPayload closes a taken respond whose payload is not a
	// RespondPayload. DecodeOp quarantines such a row, so only an executor
	// handed an op that skipped it sees one.
	ReasonRefusedPayload = RefusedReasonPrefix + "payload"
	// ReasonRefusedInteraction closes a taken respond whose scope names no
	// interaction occurrence; like ReasonRefusedPayload, DecodeOp
	// quarantines such a row first.
	ReasonRefusedInteraction = RefusedReasonPrefix + "interaction"
)

// interactionWaitDivisor sets how a respond that cannot see its occurrence
// backs off: its next look comes after its age over this divisor, between
// TransientBackoffBase and TransientBackoffMax, so a long blind wait costs a
// logarithmic number of passes rather than one every TransientBackoffBase.
const interactionWaitDivisor = 8

// raisingState is the occurrence a respond candidate answers, as the pass
// read it after its probe.
type raisingState struct {
	// request is the occurrence's runtime request id.
	request string
	// raised: its interaction.raised fact exists; resolved: so does its
	// interaction.resolved.
	raised, resolved bool
	// err is why the occurrence could not be read.
	err error
}

// fenceDisagrees reports a probe whose fence names another incarnation than
// the view.
func fenceDisagrees(probe Probe, conv ConversationView) bool {
	f := probe.Fence
	return f != (Fence{}) && (f.Conversation != conv.ID || f.Generation != conv.Generation || f.Epoch != conv.Epoch)
}

// pendingRequest reports whether the probe looked at the runtime of the view
// and saw request id Pending: a raising to record (DES-O1 §5.3).
func pendingRequest(probe Probe, conv ConversationView) bool {
	return probe.Available && probe.Running && probe.Caps.Live && !fenceDisagrees(probe, conv) &&
		probe.Pending != "" && probe.Pending != PendingUnknown
}

// noteRaisings records the raising the probe shows, if any, then reads the
// occurrence each respond candidate answers. A probe sees one prompt where a
// runtime's request ids repeat; RecordRaising numbers it the next occurrence
// once the last is resolved, so it is read after the record. A request id the
// journal cannot store (too long, control characters, padding) is no
// occurrence anyone can answer: the probe returned reads it as
// PendingUnknown, and only a store error fails the pass.
func (l *ConversationLane) noteRaisings(rp rulesPlan, probe Probe, conv ConversationView) (Probe, map[string]raisingState, error) {
	if pendingRequest(probe, conv) {
		_, _, err := l.store.RecordRaising(conv.ID, conv.Generation, probe.Pending, FactBasisObserved, l.now())
		switch {
		case errors.Is(err, ErrInvalidRequest):
			probe.Pending = PendingUnknown
		case err != nil:
			return probe, nil, err
		}
	}
	var out map[string]raisingState
	for _, e := range rp.control {
		if !e.candidate() || e.op.Kind != KindRespond {
			continue
		}
		if out == nil {
			out = map[string]raisingState{}
		}
		var st raisingState
		occ := e.op.Scope.Interaction
		if st.request, _, st.err = ParseInteractionOccurrence(occ); st.err == nil {
			st.raised, st.resolved, st.err = l.store.raisingOf(conv.ID, e.op.Scope.Generation, occ)
		}
		if st.err != nil {
			st.err = fmt.Errorf("reading interaction %s of op %s: %w", occ, e.op.ID, st.err)
		}
		out[e.op.ID] = st
	}
	return probe, out, nil
}

// respondReady decides a respond candidate against its occurrence and the
// probe (DES-O1 §10.2): ready when the probe shows the occurrence's request
// and the occurrence is raised and unresolved. It waits when the look or the
// read failed, and otherwise closes skipped{stale}: the occurrence is over,
// or its prompt is gone (Pass then records it cleared, respondEnd). A respond
// is at most once, so a candidate is never one that may have answered.
func (tp *takePlan) respondReady(rp rulesPlan, e entry, probe Probe, now time.Time) bool {
	st, read := rp.raisings[e.op.ID]
	switch {
	case !read:
		tp.waitUntil(e.op, WaitInteractionUnknown, "occurrence not read", interactionRetryAt(e.op, now))
	case st.err != nil:
		tp.waitUntil(e.op, WaitInteractionUnknown, st.err.Error(), interactionRetryAt(e.op, now))
	case probe.Pending == PendingUnknown:
		tp.waitUntil(e.op, WaitInteractionUnknown, "the runtime's pending interaction is unknown", interactionRetryAt(e.op, now))
	case st.raised && !st.resolved && probe.Pending == st.request:
		return true
	default:
		tp.close(rp, e, Outcome{Arm: ArmSkipped, Reason: ReasonStale}, now)
	}
	return false
}

// interactionRetryAt is when a respond that could not see its occurrence
// looks again: after its age over interactionWaitDivisor, within
// [TransientBackoffBase, TransientBackoffMax].
func interactionRetryAt(op Op, now time.Time) time.Time {
	since := op.CreatedAt
	if op.NotBefore.After(since) {
		since = op.NotBefore
	}
	d := now.Sub(since) / interactionWaitDivisor
	return now.Add(min(max(d, TransientBackoffBase), TransientBackoffMax))
}

// respondEnd is the interaction.resolved that ends the occurrence of next,
// a lane respond as it closes (DES-O1 §10.2, first effective answer wins):
// its answer's, basis lane, caused by it, when it succeeded; cleared,
// observed, when it closed skipped{stale} (its prompt gone, its incarnation
// over, or its occurrence already ended); ambiguous when an attempt may have
// answered (maybe_delivered); unanswered when it closes with nothing sent. ok is false for any
// other op, and for one still open.
//
// The lane is not the only writer of these facts: two closes of respond ops
// to one occurrence (two principals' answers, or a host whose probe saw the
// prompt gone after another host's answer cleared it) race, and the first
// fact stands. A cleared fact may so stand for a prompt another op answered;
// that op's own outcome still records the answer.
func respondEnd(next Op, now time.Time) (Fact, bool) {
	if next.Kind != KindRespond || next.Executor != ExecutorLane || next.Status != StatusClosed {
		return Fact{}, false
	}
	f := interactionFact(FactInteractionResolved, next.Conversation, next.Scope.Generation, next.Scope.Interaction)
	f.At, f.Basis, f.CauseOps = now, FactBasisLane, []string{next.ID}
	o := next.Outcome
	switch {
	case o.Arm == ArmSucceeded:
	case o.Arm == ArmSkipped && o.Reason == ReasonStale:
		f.Reason, f.Basis, f.CauseOps = ResolvedCleared, FactBasisObserved, nil
	case next.MaybeDelivered > 0:
		f.Reason = ResolvedAmbiguous
	default:
		f.Reason = ResolvedUnanswered
	}
	return f, true
}

// endOccurrences records the end of the occurrence of every lane respond
// closes closes (respondEnd), before the closes are written: a crash in
// between leaves the ops open, and the next pass closes them again on the
// recorded facts. Written after, a crash would leave an occurrence open
// whose answers are spent, so the prompt still showing could not be
// answered under a fresh key.
func (l *ConversationLane) endOccurrences(closes []ruleClose) error {
	for _, c := range closes {
		if f, ok := respondEnd(c.next, l.now()); ok {
			if err := l.store.endRaising(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// respondCall is the effect call of a taken respond op, or the rejection of
// one whose payload or occurrence does not decode. DecodeOp refuses such a
// row, so the rejection is reached only by an op that never went through it.
func respondCall(fence Fence, op Op) (RespondCall, *Rejected) {
	resp, err := DecodeRespondPayload(op.Payload)
	if err != nil {
		return RespondCall{}, &Rejected{Arm: ArmFailed, Reason: ReasonRefusedPayload}
	}
	req, _, err := ParseInteractionOccurrence(op.Scope.Interaction)
	if err != nil {
		return RespondCall{}, &Rejected{Arm: ArmFailed, Reason: ReasonRefusedInteraction}
	}
	return RespondCall{Fence: fence, Op: op, Interaction: op.Scope.Interaction, RequestID: req, Response: resp, Seq: op.LaneSeq}, nil
}
