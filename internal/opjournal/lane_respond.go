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

// definitiveLook reports whether the probe looked at the runtime of the view,
// and what it saw pending there: Pending, or nothing when the runtime is not
// running, since a runtime that is not running shows no prompt. A runtime
// that takes no responses names no prompt either way: that is a capability
// answer (failed{unsupported}), not a look showing the prompt gone.
func definitiveLook(probe Probe, conv ConversationView) (pending string, looked bool) {
	switch {
	case !probe.Available || !probe.Caps.Live || !probe.Caps.Respond || fenceDisagrees(probe, conv):
		return "", false
	case !probe.Running:
		return "", true
	case probe.Pending == PendingUnknown:
		return "", false
	default:
		return probe.Pending, true
	}
}

// undecodableRow reports an error that a retry will not clear: a row this
// build cannot decode.
func undecodableRow(err error) bool {
	return errors.Is(err, ErrCorruptOp) || errors.Is(err, ErrUnsupportedSchema) ||
		errors.Is(err, ErrUnsupportedValue) || errors.Is(err, ErrUnsupportedKind)
}

// noteRaisings records the raising the probe shows, if any, ends the
// occurrence of every open respond whose prompt the probe shows gone
// (clearGone), then reads the occurrence each respond candidate answers. A
// probe sees one prompt where a runtime's request ids repeat; RecordRaising
// numbers it the next occurrence once the last is resolved, so it is read
// after the record. A request id the journal cannot store (too long, control
// characters, padding), or whose raisings do not decode, is no occurrence
// anyone can answer: the probe returned reads it as PendingUnknown, and only
// a store error fails the pass. The look still showed that request, so the
// occurrences it shows gone end all the same.
func (l *ConversationLane) noteRaisings(rp rulesPlan, probe Probe, conv ConversationView) (Probe, map[string]raisingState, error) {
	pending, looked := definitiveLook(probe, conv)
	if looked && pending != "" {
		_, _, err := l.store.RecordRaising(conv.ID, conv.Generation, pending, FactBasisObserved, l.now())
		switch {
		case errors.Is(err, ErrInvalidRequest), undecodableRow(err):
			probe.Pending = PendingUnknown
		case err != nil:
			return probe, nil, err
		}
	}
	if looked {
		if err := l.clearGone(rp.responds, conv, pending); err != nil {
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

// clearGone ends, cleared, the open occurrence of every open respond of the
// view's generation whose request the look shows gone: another request, or
// none, is pending (DES-O1 §10.2, "still raised"). That covers a respond the
// pass would not take now, waiting on a runtime that is not running or
// backing off after a transient refusal: an identical prompt raised later is
// another occurrence, which the old answer must not reach. An occurrence
// whose facts do not decode is left alone; its respond waits on the read.
func (l *ConversationLane) clearGone(responds []Op, conv ConversationView, pending string) error {
	for _, op := range responds {
		if op.Scope.Generation != conv.Generation {
			continue
		}
		req, _, err := ParseInteractionOccurrence(op.Scope.Interaction)
		if err != nil || req == pending {
			continue
		}
		if err := l.store.endRaising(clearedEnd(op, l.now())); err != nil && !undecodableRow(err) {
			return fmt.Errorf("ending interaction %s of op %s: %w", op.Scope.Interaction, op.ID, err)
		}
	}
	return nil
}

// clearedEnd is the interaction.resolved that ends op's occurrence as seen
// gone with no answer of the lane's: reason cleared, basis observed.
func clearedEnd(op Op, now time.Time) Fact {
	f := interactionFact(FactInteractionResolved, op.Conversation, op.Scope.Generation, op.Scope.Interaction)
	f.At, f.Basis, f.Reason = now, FactBasisObserved, ResolvedCleared
	return f
}

// over reports an occurrence read as raised and already resolved: a respond
// to it answers nothing, whatever the runtime shows now.
func (st raisingState) over() bool { return st.err == nil && st.raised && st.resolved }

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
// a lane respond as it closes (DES-O1 §10.2): its answer's, basis lane,
// caused by it, when it succeeded; cleared, observed, when it closed
// skipped{stale} (its prompt gone, its incarnation over, or its occurrence
// already ended); ambiguous when an attempt may have answered
// (maybe_delivered); unanswered when it closes with nothing sent, which
// occurrenceEnd drops while another answer is open. ok is false for any
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

// endOccurrences records the end of the occurrence of every lane respond it
// closes (Store.endOccurrence), before the closes are written: a crash in
// between leaves the ops open, and the next pass closes them again on the
// recorded facts. Written after, a crash would leave an occurrence open whose
// answers are spent, so the prompt still showing could not be answered under
// a fresh key. An end withheld for another open answer is re-checked once
// the closes land (endClosedOccurrences).
func (l *ConversationLane) endOccurrences(closes []ruleClose) error {
	closing := make(map[string]bool, len(closes))
	for _, c := range closes {
		closing[c.cur.ID] = true
	}
	for _, c := range closes {
		if err := l.store.endOccurrence(c.next, closing, l.now()); err != nil {
			return err
		}
	}
	return nil
}

// occurrenceEnd is the end the close of next records for its occurrence
// (respondEnd), less an unanswered one while another open lane respond,
// outside closing, still answers the occurrence: an answer that sent
// nothing leaves the prompt showing for that one (DES-O1 §4.7, §10.2: the
// first accepted answer wins). The last of them to close with nothing sent
// ends it; two closing at once can each read the other open, so each
// re-checks once its close lands (Store.endClosed). A respond proposed after
// the read races the end: it then closes skipped{stale}, and its requester
// answers the next occurrence.
func (s *Store) occurrenceEnd(next Op, closing map[string]bool, now time.Time) (Fact, bool, error) {
	f, ok := respondEnd(next, now)
	if !ok || f.Reason != ResolvedUnanswered {
		return f, ok, nil
	}
	open, _, err := s.ListOpenTolerant(next.Conversation)
	if err != nil {
		return Fact{}, false, fmt.Errorf("reading the other answers to interaction %s of op %s: %w", next.Scope.Interaction, next.ID, err)
	}
	for _, op := range open {
		if op.ID != next.ID && !closing[op.ID] && op.Kind == KindRespond && op.Executor == ExecutorLane &&
			op.Scope.Generation == next.Scope.Generation && op.Scope.Interaction == next.Scope.Interaction {
			return Fact{}, false, nil
		}
	}
	return f, true, nil
}

// endOccurrence records the end the close of next gives its occurrence
// (occurrenceEnd), if any, before that close is written. An occurrence whose
// facts do not decode gets none, and the close goes on: no one can read or
// answer that occurrence, and one undecodable row must not stall the
// conversation (DES-O1 §4.1).
func (s *Store) endOccurrence(next Op, closing map[string]bool, now time.Time) error {
	f, ok, err := s.occurrenceEnd(next, closing, now)
	if err == nil && ok {
		err = s.endRaising(f)
	}
	if err != nil && !undecodableRow(err) {
		return err
	}
	return nil
}

// endClosedOccurrences re-checks, once closes have landed, the end of the
// occurrence each one answers (Store.endClosed).
func (l *ConversationLane) endClosedOccurrences(closes []ruleClose) error {
	var errs error
	for _, c := range closes {
		errs = errors.Join(errs, l.store.endClosed(c.next, l.now()))
	}
	return errs
}

// endClosed records, once next's close has landed, the unanswered end that
// occurrenceEnd withheld before it for another open answer, when no other
// answer is open now. Two answers closing with nothing sent at once can each
// read the other open before either close lands, and each withholds the end
// for the other; re-read after its own close, the later of them reads both
// closed and ends the occurrence. An end already recorded stands
// (endRaising), and every other end is written before the close. When the
// other answers cannot be read the end lands anyway and the error says so,
// as in Done (doneEnds): the close has landed, and an occurrence left open
// with its answers spent could not be answered again.
func (s *Store) endClosed(next Op, now time.Time) error {
	f, ok := respondEnd(next, now)
	if !ok || f.Reason != ResolvedUnanswered {
		return nil
	}
	_, ok, readErr := s.occurrenceEnd(next, nil, now)
	if !ok && readErr == nil {
		return nil
	}
	if err := s.endRaising(f); err != nil && !undecodableRow(err) {
		return errors.Join(readErr, err)
	}
	return readErr
}

// endTerminal records the end of the occurrence of next, a lane respond
// Cancel found closed: by Store.Close or outside the journal, neither of
// which ends its occurrence, or by a close whose re-check (endClosed) did
// not land. An unanswered end lands as endClosed lands it, even when the
// other answers cannot be read; any other as endOccurrence writes it.
func (s *Store) endTerminal(next Op, now time.Time) error {
	if f, ok := respondEnd(next, now); ok && f.Reason == ResolvedUnanswered {
		return s.endClosed(next, now)
	}
	return s.endOccurrence(next, nil, now)
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
