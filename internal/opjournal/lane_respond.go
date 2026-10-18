package opjournal

import (
	"fmt"
	"time"
)

// Respond outcome reasons the lane writes.
const (
	// ReasonRefusedPayload closes a respond whose payload is not a
	// RespondPayload.
	ReasonRefusedPayload = RefusedReasonPrefix + "payload"
	// ReasonRefusedInteraction closes a respond whose scope names no
	// interaction occurrence.
	ReasonRefusedInteraction = RefusedReasonPrefix + "interaction"
)

// raisingState is the occurrence a respond candidate answers, as the pass
// read it after its probe.
type raisingState struct {
	// request is the occurrence's runtime request id.
	request string
	// raised: its interaction.raised fact exists; resolved: so does its
	// interaction.resolved.
	raised, resolved bool
	// invalid says the scope names no occurrence; err says why.
	invalid bool
	// err is why the occurrence could not be read, or why it is invalid.
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
// once the last is resolved, so it is read after the record.
func (l *ConversationLane) noteRaisings(rp rulesPlan, probe Probe, conv ConversationView) (map[string]raisingState, error) {
	if pendingRequest(probe, conv) {
		if _, _, err := l.store.RecordRaising(conv.ID, conv.Generation, probe.Pending, FactBasisObserved, l.now()); err != nil {
			return nil, err
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
		if st.request, _, st.err = ParseInteractionOccurrence(occ); st.err != nil {
			st.invalid = true
		} else if st.raised, st.resolved, st.err = l.store.raisingOf(conv.ID, e.op.Scope.Generation, occ); st.err != nil {
			st.err = fmt.Errorf("reading interaction %s of op %s: %w", occ, e.op.ID, st.err)
		}
		out[e.op.ID] = st
	}
	return out, nil
}

// respondReady decides a respond candidate against its occurrence and the
// probe (DES-O1 §10.2): ready when the probe shows the occurrence's request
// and the occurrence is raised and unresolved. Otherwise it waits (the look
// or the read failed) or closes: skipped{stale} for an occurrence that is
// over, failed{ambiguous_delivery} for one an earlier attempt of this op may
// have answered. An occurrence the probe shows gone gets its
// interaction.resolved recorded, so the next raising of its request id is a
// new occurrence.
func (tp *takePlan) respondReady(rp rulesPlan, e entry, probe Probe, now time.Time) bool {
	st, read := rp.raisings[e.op.ID]
	retry := now.Add(TransientBackoffBase)
	_, payloadErr := DecodeRespondPayload(e.op.Payload)
	switch {
	case !read:
		tp.waitUntil(e.op, WaitInteractionUnknown, "occurrence not read", retry)
	case st.invalid:
		tp.close(rp, e, Outcome{Arm: ArmFailed, Reason: ReasonRefusedInteraction}, now)
	case payloadErr != nil:
		tp.close(rp, e, Outcome{Arm: ArmFailed, Reason: ReasonRefusedPayload}, now)
	case st.err != nil:
		tp.waitUntil(e.op, WaitInteractionUnknown, st.err.Error(), retry)
	case probe.Pending == PendingUnknown:
		tp.waitUntil(e.op, WaitInteractionUnknown, "the runtime's pending interaction is unknown", retry)
	case st.raised && !st.resolved && probe.Pending == st.request:
		return true
	default:
		if st.raised && !st.resolved {
			tp.resolves = append(tp.resolves, clearedFact(e.op, now))
		}
		o := Outcome{Arm: ArmSkipped, Reason: ReasonStale}
		if e.maybeDelivered() {
			o = Outcome{Arm: ArmFailed, Reason: ReasonAmbiguousDelivery}
		}
		tp.close(rp, e, o, now)
	}
	return false
}

// clearedFact is the interaction.resolved the lane records for the
// occurrence op answers when it sees the raising gone.
func clearedFact(op Op, now time.Time) Fact {
	f := interactionFact(FactInteractionResolved, op.Conversation, op.Scope.Generation, op.Scope.Interaction)
	f.Reason, f.Basis, f.At = ResolvedCleared, FactBasisObserved, now
	return f
}

// writeResolves records the facts of raisings a plan saw gone, before the
// closes of their ops: a crash in between leaves the ops open, and the next
// pass closes them on the recorded facts.
func (l *ConversationLane) writeResolves(facts []Fact) error {
	for _, f := range facts {
		if _, err := l.store.RecordFact(f); err != nil {
			return fmt.Errorf("recording that interaction %s of conversation %s cleared: %w", f.Interaction, f.Conversation, err)
		}
	}
	return nil
}

// respondCall is the effect call of a taken respond op, or the rejection of
// one the planner should have closed: a payload or occurrence that does not
// decode.
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

// respondDoneFact is the interaction.resolved a respond's Done writes in its
// batch: the answer's, when the effect settled, and a cleared one when the
// executor found the raising gone (Rejected skipped{stale}).
func respondDoneFact(tb *takenBatch, er effectResult, success Outcome, now time.Time) (Fact, bool) {
	if len(tb.ops) == 0 || tb.ops[0].Kind != KindRespond {
		return Fact{}, false
	}
	op := tb.ops[0]
	switch {
	case er.kind == resultSettled:
		f := interactionFact(FactInteractionResolved, op.Conversation, tb.spec.fence.Generation, op.Scope.Interaction)
		f.CauseOps, f.Basis, f.At = []string{op.ID}, FactBasisLane, now
		return f, true
	case er.kind == resultRejected && success.Arm == ArmSkipped && success.Reason == ReasonStale:
		return clearedFact(op, now), true
	}
	return Fact{}, false
}
