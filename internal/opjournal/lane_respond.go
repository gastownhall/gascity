package opjournal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

// noteRaisings notes what a definitive look shows (noteLook), then reads the
// occurrence each respond candidate answers. A probe sees one prompt where a
// runtime's request ids repeat; RecordRaising numbers it the next occurrence
// once the last is resolved, so it is read after the record. moved says the
// look's write to the runtime row lost to another writer: the pass re-reads.
func (l *ConversationLane) noteRaisings(ctx context.Context, snap *laneSnapshot, rp rulesPlan, probe Probe, conv ConversationView) (Probe, map[string]raisingState, bool, error) {
	probe, moved, err := l.noteLook(ctx, &snap.rt, rp, probe, conv)
	if err != nil || moved {
		return probe, nil, moved, err
	}
	return probe, l.readRaisings(rp, conv), false, nil
}

// noteLook records what a definitive look shows (definitiveLook): the
// raising of the request it shows pending, if any; the end of every
// occurrence it shows gone (clearGone); and, in the runtime row's pending
// cell, the occurrence it shows (notePending), so that a later look that
// shows the prompt gone ends that occurrence even while no respond names it
// yet. A request id the journal cannot store (too long, control characters,
// padding), or whose raisings do not decode, is no occurrence anyone can
// answer: the probe returned reads it as PendingUnknown, and only a store
// error fails the pass. The look still showed that request, so the
// occurrences it shows gone end all the same, and a cell naming an earlier
// raising of the same request keeps it.
func (l *ConversationLane) noteLook(ctx context.Context, rt *runtimeSnap, rp rulesPlan, probe Probe, conv ConversationView) (Probe, bool, error) {
	pending, looked := definitiveLook(probe, conv)
	if !looked {
		return probe, false, nil
	}
	last := pendingCell(*rt, conv)
	shown := ""
	if pending != "" {
		r, _, err := l.store.RecordRaising(conv.ID, conv.Generation, pending, FactBasisObserved, l.now())
		switch {
		case errors.Is(err, ErrInvalidRequest), undecodableRow(err):
			probe.Pending = PendingUnknown
			if req, _, perr := ParseInteractionOccurrence(last); perr == nil && req == pending {
				shown = last
			}
		case err != nil:
			return probe, false, err
		default:
			shown = r.Raised.Interaction
		}
	}
	if err := l.clearGone(goneCandidates(rp.responds, conv, last), conv, pending); err != nil {
		return probe, false, err
	}
	moved, err := l.notePending(ctx, rt, rp.inc, conv, shown)
	return probe, moved, err
}

// readRaisings reads the occurrence each respond candidate answers.
func (l *ConversationLane) readRaisings(rp rulesPlan, conv ConversationView) map[string]raisingState {
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
	return out
}

// goneCandidates are the occurrences of the view's generation a look can show
// gone: the one the lane's last look saw pending (last, the runtime row's
// pending cell), which no respond may name yet while its requester chooses
// an answer, and every open respond's, including one the pass would not take
// now, waiting on a runtime that is not running or backing off after a
// transient refusal. responds are the open respond ops (rulesPlan.responds).
func goneCandidates(responds []Op, conv ConversationView, last string) []string {
	var out []string
	if last != "" {
		out = append(out, last)
	}
	for _, op := range responds {
		if op.Scope.Generation == conv.Generation && !slices.Contains(out, op.Scope.Interaction) {
			out = append(out, op.Scope.Interaction)
		}
	}
	return out
}

// clearGone ends, cleared, each occurrence of occs (all of the view's
// generation) whose request the look shows gone: another request, or none,
// is pending (DES-O1 §10.2, "still raised"). An identical prompt raised
// later is another occurrence, which an answer to the old one must not
// reach. An end already recorded stands (endRaising), and an occurrence
// whose facts do not decode is left alone: a respond to it waits on the read.
func (l *ConversationLane) clearGone(occs []string, conv ConversationView, pending string) error {
	for _, occ := range occs {
		req, _, err := ParseInteractionOccurrence(occ)
		if err != nil || req == pending {
			continue
		}
		f := interactionFact(FactInteractionResolved, conv.ID, conv.Generation, occ)
		f.At, f.Basis, f.Reason = l.now(), FactBasisObserved, ResolvedCleared
		if err := l.store.endRaising(f); err != nil && !undecodableRow(err) {
			return fmt.Errorf("ending interaction %s of conversation %s: %w", occ, conv.ID, err)
		}
	}
	return nil
}

// pendingCell is the occurrence rt's pending cell holds for the view's
// generation: "" when the row holds none, or one of another generation,
// whose runtime the look did not see.
func pendingCell(rt runtimeSnap, conv ConversationView) string {
	if !rt.found || rt.row.PendingGen != conv.Generation {
		return ""
	}
	return rt.row.Pending
}

// notePending sets the runtime row's pending cell to shown, the occurrence a
// definitive look shows pending ("" for none), creating the row when no Take
// has. It writes only when the cell changes, so a look at the same prompt
// writes nothing, and it leaves rt as written, so the Take that follows
// guards on the row it wrote and is not re-planned. moved says the write
// lost to another writer of the row, or ended unknown: the pass re-reads.
func (l *ConversationLane) notePending(ctx context.Context, rt *runtimeSnap, inc incarnation, conv ConversationView, shown string) (moved bool, err error) {
	if pendingCell(*rt, conv) == shown {
		return false, nil
	}
	next := RuntimeRow{Conversation: conv.ID, Generation: inc.gen, Epoch: inc.epoch}
	if rt.found {
		next = rt.row
	}
	next.Pending, next.PendingGen = shown, 0
	if shown != "" {
		next.PendingGen = conv.Generation
	}
	item, b, err := l.rowWrite(*rt, next)
	if err != nil {
		return false, err
	}
	res, kind, err := l.store.apply(ctx, "opjournal: lane look", []beads.GuardedItem{item})
	switch kind {
	case batchOK:
	case batchInvalid:
		return false, fmt.Errorf("noting the pending interaction of conversation %s: %w", conv.ID, err)
	default:
		return true, nil
	}
	b.Revision = res.Revisions[b.ID]
	row, err := DecodeRuntimeRow(b)
	if err != nil {
		return false, err
	}
	*rt = runtimeSnap{read: true, found: true, bead: b, row: row}
	return false, nil
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

// endItem is the guarded create of the end the close of next gives its
// occurrence (occurrenceEnd), for the batch that writes that close, so the
// end lands exactly when the close does. An end written before the close
// would stand for a close that lost its write to a Take of the op: the
// answer that Take sends would land under an ended occurrence, and the
// prompt still showing would be recorded as the next one. An end written
// after would leave the occurrence open, its answers spent, on a crash in
// between. ok is false when the close gives no end, when the occurrence was
// never raised (an end would resolve that raising before it happens) or has
// ended, and when its facts do not decode: no one can read or answer that
// occurrence, and one undecodable row must not stall the conversation
// (DES-O1 §4.1). A writer that ends the occurrence first refuses the batch
// (beads.ErrAlreadyExists), and the re-read finds it ended.
func (s *Store) endItem(next Op, closing map[string]bool, now time.Time) (beads.GuardedItem, bool, error) {
	f, ok, err := s.occurrenceEnd(next, closing, now)
	if err == nil && ok {
		var raised, resolved bool
		raised, resolved, err = s.raisingOf(f.Conversation, f.Generation, f.Interaction)
		ok = raised && !resolved
	}
	switch {
	case err != nil && undecodableRow(err):
		return beads.GuardedItem{}, false, nil
	case err != nil || !ok:
		return beads.GuardedItem{}, false, err
	}
	item, err := FactCreateItem(s.prefix, f)
	if err != nil {
		return beads.GuardedItem{}, false, err
	}
	return item, true, nil
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
// (endRaising), and every other end lands with the close (endItem). When the
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
// other answers cannot be read; any other end lands as it is, unless the
// occurrence's facts do not decode.
func (s *Store) endTerminal(next Op, now time.Time) error {
	f, ok := respondEnd(next, now)
	switch {
	case !ok:
		return nil
	case f.Reason == ResolvedUnanswered:
		return s.endClosed(next, now)
	}
	if err := s.endRaising(f); err != nil && !undecodableRow(err) {
		return err
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
