package api

// Session commands under lane authority (DES-O1 §7.5, §10.1): when the
// city's running conversation lane host executes a kind ([ops] authoritative
// listed it at the controller's start), the request surface that produces
// that kind proposes a lane op and answers from its outcome instead of
// running the legacy path inline. The ops.* gates do not govern a listed
// kind: a request whose op cannot be written is refused, 503
// op_journal_unavailable, nothing done.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/lanesubmit"
)

// laneControlAnswerBound is how long a control command under lane authority
// awaits its op's outcome before it answers in_flight: under the generated
// Go client's 60 s timeout, and above an interrupt's settle. A var so tests
// can shorten it.
var laneControlAnswerBound = 45 * time.Second

// laneExecutes reports whether the city's running lane host executes kind.
func (s *Server) laneExecutes(kind opjournal.Kind) bool {
	return slices.Contains(s.conversationLaneKinds(), string(kind))
}

// proposeLaneOp proposes req, pokes the lane when it created the op, and maps
// a refusal onto the wire (proposeError).
func (s *Server) proposeLaneOp(st *opjournal.Store, req opjournal.Request) (opjournal.Op, bool, error) {
	op, created, err := st.Propose(req)
	if err != nil {
		return opjournal.Op{}, false, proposeError(err)
	}
	if created {
		s.pokeConversationLane(req.Conversation)
	}
	return op, created, nil
}

// awaitLaneOp waits for op id of conv to close until ctx, which the caller
// bounds (laneControlAnswerBound), ends, and returns it as last read: closed,
// or still open when ctx ended. It waits through the lane host's
// AwaitConversationOp, which wakes at the end of each pass over conv. A read
// that fails is 503 op_journal_unavailable, as is a State with no lane host,
// which executes no kind (laneExecutes).
func (s *Server) awaitLaneOp(ctx context.Context, conv, id string) (opjournal.Op, error) {
	h, ok := s.state.(ConversationLaneHost)
	if !ok {
		return opjournal.Op{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: awaiting op %s: no conversation lane host runs", id)
	}
	op, err := h.AwaitConversationOp(ctx, conv, id)
	if err != nil {
		return op, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: awaiting op %s: %v", id, err)
	}
	return op, nil
}

// keyedLaneOp reads the op the operator's client key names on conv, kind
// kind: found false when key is empty or names no op. A row of another kind
// is idempotency_mismatch, and a read that fails 503
// op_journal_unavailable.
func keyedLaneOp(st *opjournal.Store, kind opjournal.Kind, conv, key string) (opjournal.Op, bool, error) {
	if key == "" {
		return opjournal.Op{}, false, nil
	}
	id, err := opjournal.DeriveID(st.Prefix(), conv, legacyop.PrincipalOperator, key)
	if err != nil {
		return opjournal.Op{}, false, apierr.InvalidIdempotencyKey.Msg("invalid_idempotency_key: " + err.Error())
	}
	op, err := st.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return opjournal.Op{}, false, nil
	case err != nil:
		return opjournal.Op{}, false, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading op %s: %v", id, err)
	case op.Kind != kind:
		return opjournal.Op{}, false, apierr.IdempotencyMismatch.Msgf("idempotency_mismatch: Idempotency-Key reused with a different request: it names a %s op", op.Kind)
	}
	return op, true, nil
}

// followCoalesced is the op op answers through: the op it coalesced into
// (the row a named stop leaves under its own key), else op itself.
func followCoalesced(st *opjournal.Store, op opjournal.Op) (opjournal.Op, error) {
	into := op.Outcome.Result.CoalescedInto
	if op.Status != opjournal.StatusClosed || into == "" {
		return op, nil
	}
	target, err := st.Get(into)
	if err != nil {
		return opjournal.Op{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading op %s, which op %s coalesced into, so nothing was done: %v", into, op.ID, err)
	}
	return target, nil
}

// laneOpInFlight is the answer of a command whose lane op is still open at
// laneControlAnswerBound: 409 in_flight naming the op and its key, under
// which a retry adopts it.
func laneOpInFlight(op opjournal.Op) error {
	return apierr.IdempotencyInFlight.Msgf("in_flight: op %s is still running; retry with Idempotency-Key %q to await it", op.ID, op.ClientKey)
}

// --- /stop and run cancel ---

// stopSessionLane is /stop under lane authority (interrupt listed): it
// proposes an interrupt op the lane host runs, awaits its outcome at most
// laneControlAnswerBound, and answers as OJ-4 answers that outcome. key is
// the Idempotency-Key header and named the turn the request names, "" for
// none.
//
// A key that already names a stop is that stop's retry (retryLaneStop). A
// named turn keeps OJ-4's front-door checks (a refusal writes nothing). A
// session with no turn to stop, as the lane's probe reads it
// (sessionHasNoTurn), gets no lane op, which would wait out its start_by: it
// is answered at once, as OJ-4's StopTurn guard answers it, a named turn
// turn_not_current with nothing written and a stop naming none with success
// (stopFoundNoTurn). Otherwise a named turn is stopped by its turn's op
// (stopTurnLane), and without one the op is session(generation)-scoped
// under the request's key, or a minted one, and the executor reads the turn
// itself.
func (s *Server) stopSessionLane(ctx context.Context, store beads.Store, id, key, named string) (syncAnswer, error) {
	st, err := s.conversationOpStore()
	if err != nil {
		return syncAnswer{}, err
	}
	prior, retry, err := keyedLaneOp(st, opjournal.KindInterrupt, id, key)
	if err != nil {
		return syncAnswer{}, err
	}
	if retry {
		return s.retryLaneStop(ctx, st, store, id, prior, named)
	}
	if named != "" {
		if _, err := s.namedStopPlan(ctx, store, id, named); err != nil {
			return syncAnswer{}, err
		}
	}
	req, err := laneStopRequest(store, id, key)
	if err != nil {
		return syncAnswer{}, err
	}
	noTurn, err := s.sessionHasNoTurn(ctx, store, id)
	switch {
	case err != nil:
		return syncAnswer{}, humaSessionManagerError(err)
	case noTurn && named != "":
		return syncAnswer{}, humaStopError(fmt.Errorf("session %s runs no turn, so turn %q is not current: %w", id, named, runtime.ErrTurnNotCurrent))
	case noTurn:
		return s.stopFoundNoTurn(ctx, st, store, id, req)
	case named != "":
		return s.stopTurnLane(ctx, st, store, id, req, named)
	}
	op, created, err := s.proposeLaneOp(st, req)
	if err != nil {
		return syncAnswer{}, err
	}
	return s.answerLaneStop(ctx, st, store, id, op, !created)
}

// sessionHasNoTurn reports whether session id has no turn to interrupt, as
// the lane's probe reads it (conversationLaneExec.look): closed, suspended,
// or its runtime not both running and alive, read through the session's
// worker handle, which passes the agent's process names, with the error the
// read can give. A runtime that cannot be observed
// (runtime.ErrRuntimeUnavailable) is not known to run no turn: the lane's
// probe holds an op proposed for it until the runtime answers, or start_by
// closes the op provider_unavailable. A runtime session that is gone runs
// none. Run cancel's legacy guard, holderHasNoTurn, reads the provider's
// IsRunning instead, which answers false when the runtime cannot be read.
func (s *Server) sessionHasNoTurn(ctx context.Context, store beads.Store, id string) (bool, error) {
	info, err := s.sessionManager(store).Get(id)
	if err != nil {
		return false, err
	}
	if info.Closed || info.State == session.StateSuspended {
		return true, nil
	}
	h, err := s.workerHandleForSession(store, id)
	if err != nil {
		return false, err
	}
	obs, err := h.LiveObservation(ctx)
	switch {
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		return false, nil
	case errors.Is(err, runtime.ErrSessionNotFound), errors.Is(err, session.ErrSessionNotFound):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("observing session %s's runtime: %w", id, err)
	}
	return !obs.Running || !obs.Alive, nil
}

// liveRunHolders is the sessions among ids with a turn to interrupt, or that
// may have one, as the lane's probe reads them (sessionHasNoTurn); one that
// cannot be read is a failure.
func (s *Server) liveRunHolders(ctx context.Context, store beads.Store, ids []string, fail func(string, error)) []string {
	live := make([]string, 0, len(ids))
	for _, id := range ids {
		noTurn, err := s.sessionHasNoTurn(ctx, store, id)
		switch {
		case err != nil:
			fail(id, err)
		case !noTurn:
			live = append(live, id)
		}
	}
	return live
}

// stopFoundNoTurn answers a /stop naming no turn of session id, req its
// request (laneStopRequest), when the session has no turn to stop: 200, as
// OJ-4 answers a stop that finds none, recorded under req's key
// (recordNoTurnStop), which a same-key retry replays. A row the key came to
// name meanwhile answers as a retry's would.
func (s *Server) stopFoundNoTurn(ctx context.Context, st *opjournal.Store, store beads.Store, id string, req opjournal.Request) (syncAnswer, error) {
	row, created, err := recordNoTurnStop(st, req)
	if err != nil {
		return syncAnswer{}, err
	}
	return s.answerLaneStop(ctx, st, store, id, row, !created)
}

// noTurnStopCausation is the causation of the row a /stop under lane
// authority records when it finds no turn to stop (recordNoTurnStop). The
// stop decides before it writes the row, so a request that finds the row
// still open closes it as that stop does (finishNoTurnStopRecord). The
// request digest does not cover causation, so the row and a lane op under
// the same key dedup as one request, and an OJ-4 stop left open at the
// flip, which carries none, is not taken for such a row.
const noTurnStopCausation = "stop-found-no-turn"

// recordNoTurnStop records, under req's key, a stop of req's session that
// found no turn to stop: an API-executed interrupt row, its causation
// noTurnStopCausation, closed skipped{not_needed}
// (finishNoTurnStopRecord). created says this call wrote it; an op the key
// already names is returned as it is, closed first when it is such a row
// still open.
func recordNoTurnStop(st *opjournal.Store, req opjournal.Request) (opjournal.Op, bool, error) {
	req.Executor = opjournal.ExecutorLegacyAPI
	req.Causation = noTurnStopCausation
	row, created, err := st.Propose(req)
	if err != nil {
		return opjournal.Op{}, false, proposeError(err)
	}
	row, err = finishNoTurnStopRecord(st, row)
	return row, created, err
}

// noTurnStopRow reports whether op is the row of a stop that found no turn
// to stop (recordNoTurnStop).
func noTurnStopRow(op opjournal.Op) bool {
	return op.Executor == opjournal.ExecutorLegacyAPI && op.Causation == noTurnStopCausation
}

// finishNoTurnStopRecord closes row skipped{not_needed} when it is the row of
// a stop that found no turn (noTurnStopRow) still open: its writer has not
// closed it yet, or failed or died before it did. A close that loses to
// another writer's (the sweep's) answers as that close. Any other op is
// returned as it is.
func finishNoTurnStopRecord(st *opjournal.Store, row opjournal.Op) (opjournal.Op, error) {
	if !noTurnStopRow(row) || row.Status == opjournal.StatusClosed {
		return row, nil
	}
	closed, err := st.Close(row.ID, opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonNotNeeded}, "api")
	if err == nil {
		return closed, nil
	}
	cur, getErr := st.Get(row.ID)
	if getErr != nil || cur.Status != opjournal.StatusClosed {
		return opjournal.Op{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: recording op %s, the stop of session %s, which runs no turn: %v", row.ID, row.Conversation, err)
	}
	return cur, nil
}

// retryLaneStop answers a /stop whose Idempotency-Key already names a stop,
// prior: an open lane op is awaited, a closed op replayed, and the row a
// named stop left under its key followed to the turn's op, also when that
// row's close did not land (finishTurnStopRecord), as the row of a stop that
// found no turn is finished (answerLegacyStop). A retry that names a turn
// prior did not stop, a session-scoped stop's included, is
// idempotency_mismatch, as OJ-4's is (stopOpRequest).
func (s *Server) retryLaneStop(ctx context.Context, st *opjournal.Store, store beads.Store, id string, prior opjournal.Op, named string) (syncAnswer, error) {
	if named != "" && prior.Scope.Turn != named {
		return syncAnswer{}, apierr.IdempotencyMismatch.Msg(fmt.Sprintf("idempotency_mismatch: Idempotency-Key reused with a different request: it names a stop of turn %q, not turn %q", prior.Scope.Turn, named))
	}
	prior, err := finishTurnStopRecord(st, id, prior)
	if err != nil {
		return syncAnswer{}, err
	}
	target, err := followCoalesced(st, prior)
	if err != nil {
		return syncAnswer{}, err
	}
	return s.answerLaneStop(ctx, st, store, id, target, true)
}

// finishTurnStopRecord is the op a retry answers from when prior is the row
// a stop of a turn left under its own key and the row's close did not land
// (recordLaneStopUnderKey, or OJ-4's recordStopUnderKey, wrote the row and
// failed, or died, before closing it), and the turn's target-key op is
// stored. A row still open is closed skipped{coalesced_into: that op}, so a
// retry answers from the turn's op, not from a row no executor runs; a close
// that loses to another writer's (the sweep's) answers as that close. A row
// the sweep closed failed{deadline_in_flight}, once its finish_by passed
// with the row still open, answers from the turn's op when that is a lane
// op: the sweep closed the row, not the stop. Any other op is returned as
// it is.
func finishTurnStopRecord(st *opjournal.Store, id string, prior opjournal.Op) (opjournal.Op, error) {
	if prior.Executor == opjournal.ExecutorLane || prior.Scope.Turn == "" ||
		(prior.Status == opjournal.StatusClosed && !closedPastFinishBy(prior)) {
		return prior, nil
	}
	targetKey, err := opjournal.InterruptKey(prior.Scope.Generation, prior.Scope.Turn)
	if err != nil {
		return prior, nil
	}
	target, found, err := keyedLaneOp(st, opjournal.KindInterrupt, id, targetKey)
	switch {
	case err != nil:
		return opjournal.Op{}, err
	case !found || target.ID == prior.ID:
		return prior, nil
	}
	if prior.Status != opjournal.StatusClosed {
		closed, err := st.Close(prior.ID, opjournal.Outcome{
			Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonCoalescedInto, Result: opjournal.Result{CoalescedInto: target.ID},
		}, "api")
		if err == nil {
			return closed, nil
		}
		cur, getErr := st.Get(prior.ID)
		if getErr != nil || cur.Status != opjournal.StatusClosed {
			return opjournal.Op{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: closing op %s, the stop of turn %q under its Idempotency-Key, into op %s: %v", prior.ID, prior.Scope.Turn, target.ID, err)
		}
		prior = cur
	}
	if closedPastFinishBy(prior) && target.Executor == opjournal.ExecutorLane {
		return target, nil
	}
	return prior, nil
}

// closedPastFinishBy reports whether op closed failed{deadline_in_flight}:
// still open when its finish_by passed.
func closedPastFinishBy(op opjournal.Op) bool {
	arm, reason := op.Outcome.Effective()
	return op.Status == opjournal.StatusClosed && arm == opjournal.ArmFailed && reason == opjournal.ReasonDeadlineInFlight
}

// laneStopRequest is the interrupt op request of a /stop of session id under
// lane authority: session(generation)-scoped, under key, or a minted key
// when the request carries none. A session whose generation cannot be read
// is 503 op_journal_unavailable, nothing done.
func laneStopRequest(store beads.Store, id, key string) (opjournal.Request, error) {
	scope, why, whyErr := sessionOpScope(store, id)
	if why != "" {
		cause := why
		if whyErr != nil {
			cause += ": " + whyErr.Error()
		}
		return opjournal.Request{}, apierr.OpJournalUnavailable.Msg(fmt.Sprintf("op_journal_unavailable: the operation journal cannot record this stop (%s); nothing was done", cause))
	}
	req := opjournal.Request{
		Conversation: id,
		Kind:         opjournal.KindInterrupt,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    key,
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Policy:       interruptOpPolicy,
		Scope:        scope,
	}
	if key == "" {
		minted, err := legacyop.MintKey()
		if err != nil {
			return opjournal.Request{}, apierr.Internal.Msg(err.Error())
		}
		req.ClientKey, req.MintedKey = minted, true
	}
	return req, nil
}

// stopTurnLane is a /stop of turn under lane authority, req its
// session-scoped request (laneStopRequest): the op is keyed
// stop:<generation>:<turn> and scoped to the turn, so stops of one turn
// share it, and a request key of the client's own gets a row that coalesced
// into it (recordLaneStopUnderKey). A turn-keyed op that does not answer the
// stop (turnStopAnswers) never uses the turn up: this request stops the turn
// under its own key.
func (s *Server) stopTurnLane(ctx context.Context, st *opjournal.Store, store beads.Store, id string, req opjournal.Request, turn string) (syncAnswer, error) {
	req.Scope.Turn = turn
	targetKey, err := opjournal.InterruptKey(req.Scope.Generation, turn)
	if err != nil {
		return syncAnswer{}, apierr.ValidationFailed.Msg("invalid_turn: " + err.Error())
	}
	targeted := req
	targeted.ClientKey, targeted.MintedKey = targetKey, false
	op, created, err := s.proposeLaneOp(st, targeted)
	if err != nil {
		return syncAnswer{}, err
	}
	if !turnStopAnswers(op) {
		if op, created, err = s.proposeLaneOp(st, req); err != nil {
			return syncAnswer{}, err
		}
		return s.answerLaneStop(ctx, st, store, id, op, !created)
	}
	if !req.MintedKey {
		if err := recordLaneStopUnderKey(st, req, op.ID); err != nil {
			return syncAnswer{}, err
		}
	}
	return s.answerLaneStop(ctx, st, store, id, op, !created)
}

// turnStopAnswers reports whether op, the op under a turn's target key,
// answers a stop of that turn: a lane op still open, which the lane runs,
// or one that closed with success (laneStopSucceeded). One that failed, or
// an API-executed one still open (an OJ-4 stop left open at the flip, which
// nothing runs now: answerLegacyStop), does not.
func turnStopAnswers(op opjournal.Op) bool {
	if op.Status != opjournal.StatusClosed {
		return op.Executor == opjournal.ExecutorLane
	}
	return laneStopSucceeded(op)
}

// recordLaneStopUnderKey records, under a named stop's own Idempotency-Key
// (req.ClientKey), that the turn's target-key op target answers it: an
// API-executed interrupt row of the same scope, closed
// skipped{coalesced_into: target}, so a retry under the key follows it back
// to that op, as OJ-4's recordStopUnderKey does. A key that already names a
// row is left as it is.
func recordLaneStopUnderKey(st *opjournal.Store, req opjournal.Request, target string) error {
	req.Executor = opjournal.ExecutorLegacyAPI
	row, created, err := st.Propose(req)
	if err != nil {
		return proposeError(err)
	}
	if !created {
		return nil
	}
	if _, err := st.Close(row.ID, opjournal.Outcome{
		Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonCoalescedInto, Result: opjournal.Result{CoalescedInto: target},
	}, "api"); err != nil {
		return apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: recording the stop under its Idempotency-Key: %v", err)
	}
	return nil
}

// laneStopSucceeded reports whether a closed interrupt op answers its stop
// with success: it succeeded, or found nothing to stop (a legacy op's
// skipped{not_needed}).
func laneStopSucceeded(op opjournal.Op) bool {
	arm, reason := op.Outcome.Effective()
	return arm == opjournal.ArmSucceeded || arm == opjournal.ArmSkipped && reason == opjournal.ReasonNotNeeded
}

// answerLaneStop answers a /stop of session id from op, the interrupt it
// proposed or found (replayed: an earlier request's), once awaited at most
// laneControlAnswerBound (awaitLaneStop): an API-executed op as
// answerLegacyStop answers it, and a lane op from its outcome
// (laneStopOutcome), or 409 in_flight while it is still open at the bound.
func (s *Server) answerLaneStop(ctx context.Context, st *opjournal.Store, store beads.Store, id string, op opjournal.Op, replayed bool) (syncAnswer, error) {
	op, replayed, err := s.awaitLaneStop(ctx, st, store, id, op, replayed)
	switch {
	case err != nil:
		return syncAnswer{}, err
	case op.Executor != opjournal.ExecutorLane:
		return answerLegacyStop(st, id, op, replayed)
	case op.Status != opjournal.StatusClosed:
		return syncAnswer{}, laneOpInFlight(op)
	}
	return laneStopOutcome(id, op, replayed)
}

// answerLegacyStop answers a /stop from op, an API-executed interrupt op
// (OJ-4's, or a row the lane path recorded; replayed: an earlier
// request's): a closed op as OJ-4 replays it. The row of a stop that found
// no turn (noTurnStopRow) is 200 however it closed, and is closed first when
// it is still open (finishNoTurnStopRecord): its stop found no turn before
// it wrote the row, so a close the sweep made, failed{deadline_in_flight}
// once the row's finish_by passed with it still open, closed the row, not
// the stop. Any other op still open is a stop no executor runs while the
// lane runs stops, since OJ-4's path does not and only the sweep closes it,
// failed: its key never answers, so it is 409 op_already_failed, asking for
// a new key.
func answerLegacyStop(st *opjournal.Store, id string, op opjournal.Op, replayed bool) (syncAnswer, error) {
	op, err := finishNoTurnStopRecord(st, op)
	switch {
	case err != nil:
		return syncAnswer{}, err
	case op.Status != opjournal.StatusClosed:
		return syncAnswer{}, apierr.OpAlreadyFailed.Msg(fmt.Sprintf("%s: op %s, the stop under Idempotency-Key %q, was left open by a stop that did not finish, and nothing runs it while the conversation lane runs stops; resend with a new Idempotency-Key to act again", opAlreadyFailedCode, op.ID, op.ClientKey))
	case syncOpReplaysAsSuccess(op), noTurnStopRow(op):
		return syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: op.ClientKey, Replayed: replayed, Op: conversationOpWire(op)}}, nil
	}
	return syncAnswer{}, replayedSyncError(op, humaStopError)
}

// awaitLaneStop awaits op, a /stop's interrupt (replayed: an earlier
// request's), at most laneControlAnswerBound, and returns the op the stop
// answers from, closed or still open at the bound, and whether that op is an
// earlier request's. An API-executed op is not awaited: no lane runs it. The
// lane fences a lane op to the generation it was proposed for, and one it
// fenced off after session id restarted (opjournal.FencedOff) never ran: a
// stop naming no turn carries on past it (carryOnStop), within the same
// bound, whether this request awaited the op or found it closed, so the
// retry of a stop that answered in_flight carries on too, until the op's
// finish_by. An op the lane ran is answered as it closed, and a carry-on,
// once made, is followed, so a retry never stops a turn that began after the
// stop answered.
func (s *Server) awaitLaneStop(ctx context.Context, st *opjournal.Store, store beads.Store, id string, op opjournal.Op, replayed bool) (opjournal.Op, bool, error) {
	actx, cancel := context.WithTimeout(ctx, laneControlAnswerBound)
	defer cancel()
	for op.Executor == opjournal.ExecutorLane {
		if op.Status != opjournal.StatusClosed {
			var err error
			if op, err = s.awaitLaneOp(actx, id, op.ID); err != nil || op.Status != opjournal.StatusClosed {
				return op, replayed, err
			}
		}
		if !opjournal.FencedOff(op) || op.Scope.Turn != "" {
			return op, replayed, nil
		}
		next, created, err := s.carryOnStop(actx, st, store, id, op)
		if err != nil {
			return op, replayed, err
		}
		op, replayed = next, !created
	}
	return op, replayed, nil
}

// restartedStopKey is the client key of the stop that carries on stale, the
// id of a /stop's session-scoped lane interrupt the lane fenced off after
// its session restarted (carryOnStop): one per fenced op, which a same-key
// retry of the stop finds again.
func restartedStopKey(stale string) string {
	return "stop-restarted:" + stale
}

// carryOnStop carries a stop of session id on past op, its session-scoped
// interrupt that the lane fenced off unrun after the session restarted,
// under restartedStopKey(op.ID): to the op an earlier request carried it on
// with, else to the session's current incarnation, a lane op when that may
// have a turn to stop (sessionHasNoTurn), and otherwise a row recording that
// it has none (recordNoTurnStop), which a retry replays rather than stop a
// turn that began after. The new op is scoped no earlier than the generation
// that fenced op off (followUpScope) and is due by op's finish_by, so every
// carry-on of a chain is due by the stop's own. A stop is carried on only
// until then: a retry after that, finding no carry-on, is 409
// op_already_failed, nothing proposed, as is a carry-on the journal refuses
// because that finish_by passed meanwhile, so a stale key never stops the
// turn running when it comes back. created says this request wrote the op.
func (s *Server) carryOnStop(ctx context.Context, st *opjournal.Store, store beads.Store, id string, op opjournal.Op) (opjournal.Op, bool, error) {
	key := restartedStopKey(op.ID)
	carried, found, err := keyedLaneOp(st, opjournal.KindInterrupt, id, key)
	if err != nil || found {
		return carried, false, err
	}
	if time.Now().After(op.FinishBy) {
		return opjournal.Op{}, false, stopPastFinishBy(id, op)
	}
	req, err := laneStopRequest(store, id, key)
	if err != nil {
		return opjournal.Op{}, false, err
	}
	req.Scope = followUpScope(req.Scope, op.Outcome.Result.Generation)
	req.FinishBy = op.FinishBy
	noTurn, err := s.sessionHasNoTurn(ctx, store, id)
	if err != nil {
		return opjournal.Op{}, false, humaSessionManagerError(err)
	}
	// op's finish_by may have passed since the check above, while the session
	// was read, and Propose refuses a carry-on due by a finish_by already
	// past: Preview, which writes nothing, asks first.
	if _, err := st.Preview(req); err != nil && time.Now().After(op.FinishBy) {
		return opjournal.Op{}, false, stopPastFinishBy(id, op)
	}
	if noTurn {
		return recordNoTurnStop(st, req)
	}
	return s.proposeLaneOp(st, req)
}

// stopPastFinishBy is the answer of a stop of session id that carries on
// past op, its interrupt the lane fenced off unrun, once op's finish_by
// passed: 409 op_already_failed, asking for a new key.
func stopPastFinishBy(id string, op opjournal.Op) error {
	return apierr.OpAlreadyFailed.Msgf("%s: op %s, the stop under Idempotency-Key %q, was fenced off unrun after session %s restarted, and its finish_by passed before the stop carried on; resend with a new Idempotency-Key to act again", opAlreadyFailedCode, op.ID, op.ClientKey, id)
}

// followUpScope is scope, a follow-up interrupt's as read from the session
// row, at no earlier generation than fencedAt, the one the lane recorded
// fencing off the op it follows (opjournal.FencedOff), 0 for none. The lane
// fences against its effective incarnation, its runtime row's when that
// leads the session row, so an op at the lagging session row's generation
// would be fenced off again, where one at fencedAt waits for the lane's view
// to catch up (opjournal.WaitIncarnation) and then runs.
func followUpScope(scope opjournal.Scope, fencedAt int64) opjournal.Scope {
	scope.Generation = max(scope.Generation, fencedAt)
	return scope
}

// laneStopOutcome answers a /stop of session id from op, its closed lane
// interrupt (replayed: an earlier request's), as OJ-4 answers the same
// outcome: succeeded, or a stop that found nothing to stop, is 200; any
// other skip is 200 for a stop that named no turn (nothing ran) and 409
// turn_not_current for a named one, whose turn was not current when the
// lane came to it (skipped{stale}, or skipped{conversation_closed} once its
// session closed); failed{unsupported} 501; failed{no_boundary} the legacy
// not-ended answer; provider_unavailable and expired_before_start 503; and
// any other failure, or a cancel, 409 op_already_failed. An op scoped to a
// turn is a named stop's.
func laneStopOutcome(id string, op opjournal.Op, replayed bool) (syncAnswer, error) {
	ok := syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: op.ClientKey, Replayed: replayed, Op: conversationOpWire(op)}}
	named := op.Scope.Turn != ""
	arm, reason := op.Outcome.Effective()
	closed := func(what string) string {
		return fmt.Sprintf("op %s closed %s{%s}: %s", op.ID, arm, reason, what)
	}
	switch {
	case arm == opjournal.ArmSucceeded:
		return ok, nil
	case arm == opjournal.ArmSkipped && named && reason != opjournal.ReasonNotNeeded:
		return syncAnswer{}, humaStopError(fmt.Errorf("%s: %w", closed(fmt.Sprintf("turn %q of session %s was not current when the stop came to it", op.Scope.Turn, id)), runtime.ErrTurnNotCurrent))
	case arm == opjournal.ArmSkipped:
		return ok, nil
	case arm == opjournal.ArmFailed && reason == opjournal.ReasonUnsupported:
		cause := session.ErrTurnInterruptUnsupported
		if op.Scope.Turn != "" {
			cause = runtime.ErrTurnIdentityUnsupported
		}
		return syncAnswer{}, humaStopError(fmt.Errorf("%s: %w", closed("the session cannot be stopped this way"), cause))
	case arm == opjournal.ArmFailed && reason == opjournal.ReasonNoBoundary:
		return syncAnswer{}, humaStopError(fmt.Errorf("%s: %w", closed("the interrupted turn was not seen to end"), runtime.ErrTurnNotEnded))
	case arm == opjournal.ArmFailed && (reason == opjournal.ReasonProviderUnavailable || reason == opjournal.ReasonExpiredBeforeStart):
		return syncAnswer{}, humaStopError(fmt.Errorf("%s: %w", closed("the runtime did not answer before the stop's start_by"), runtime.ErrRuntimeUnavailable))
	}
	return syncAnswer{}, apierr.OpAlreadyFailed.Msg(fmt.Sprintf("%s: op %s closed %s; resend with a new Idempotency-Key to act again", opAlreadyFailedCode, op.ID, laneOutcomeText(op)))
}

// laneOutcomeText names a closed op's outcome, arm{reason}, and its last
// error when it has one.
func laneOutcomeText(op opjournal.Op) string {
	arm, reason := op.Outcome.Effective()
	text := string(arm)
	if reason != "" {
		text += "{" + reason + "}"
	}
	if last := strings.TrimSpace(op.LastError); last != "" {
		text += " (" + last + ")"
	}
	return text
}

// runCancelLaneKey is the client key of the interrupt a cancel of run runID
// proposes for a holder session in generation gen: one op per run and holder
// incarnation, so a holder that restarted since an earlier cancel of the run
// gets an op of its own.
func runCancelLaneKey(runID string, gen int64) string {
	return "run-cancel:" + runID + ":" + strconv.FormatInt(gen, 10)
}

// runCancelRetryKey is the client key of the interrupt that follows prev,
// the id of a holder's run-cancel interrupt in generation gen that closed
// before a later cancel of the run reached it: one per closed op, which
// every later cancel of the run finds again.
func runCancelRetryKey(runID string, gen int64, prev string) string {
	return runCancelLaneKey(runID, gen) + ":retry:" + prev
}

// interruptRunHoldersLane is interruptRunHolders under lane authority: one
// interrupt op per live holder session ids names
// (proposeRunCancelInterrupt), awaited together at most
// laneControlAnswerBound (awaitRunCancelInterrupt). A session whose op
// closed succeeded is interrupted; one whose op failed, or is still open at
// the bound, is a failure; one whose op closed skipped (nothing to stop) is
// neither.
func (s *Server) interruptRunHoldersLane(ctx context.Context, store beads.Store, runID string, ids []string, fail func(string, error)) []string {
	st, err := s.conversationOpStore()
	if err != nil {
		for _, id := range ids {
			fail(id, err)
		}
		return nil
	}
	type proposed struct {
		id string
		op opjournal.Op
	}
	var ops []proposed
	for _, id := range ids {
		op, err := s.proposeRunCancelInterrupt(st, store, runID, id, 0)
		if err != nil {
			fail(id, err)
			continue
		}
		ops = append(ops, proposed{id: id, op: op})
	}
	actx, cancel := context.WithTimeout(ctx, laneControlAnswerBound)
	defer cancel()
	results := make([]opjournal.Op, len(ops))
	errs := make([]error, len(ops))
	done := make(chan int, len(ops))
	for i, p := range ops {
		go func() {
			results[i], errs[i] = s.awaitRunCancelInterrupt(actx, st, store, runID, p.id, p.op)
			done <- i
		}()
	}
	for range ops {
		<-done
	}
	var interrupted []string
	for i, p := range ops {
		op := results[i]
		arm, _ := op.Outcome.Effective()
		switch {
		case errs[i] != nil:
			fail(p.id, errs[i])
		case op.Status != opjournal.StatusClosed:
			fail(p.id, fmt.Errorf("op %s still running", op.ID))
		case arm == opjournal.ArmSucceeded:
			interrupted = append(interrupted, p.id)
		case arm == opjournal.ArmSkipped:
		default:
			fail(p.id, fmt.Errorf("op %s closed %s", op.ID, laneOutcomeText(op)))
		}
	}
	return interrupted
}

// awaitRunCancelInterrupt awaits op, the interrupt a cancel of run runID
// sent holder session id, until ctx ends, and returns the holder's op as
// last read. The lane fences an op to the generation it was proposed for,
// so one it fenced off unrun after the holder restarted
// (opjournal.FencedOff) is followed, when the new incarnation may have a
// turn to stop (sessionHasNoTurn), by that incarnation's interrupt, keyed by
// its generation, no earlier than the one that fenced the op off
// (proposeRunCancelInterrupt), awaited until ctx ends too.
func (s *Server) awaitRunCancelInterrupt(ctx context.Context, st *opjournal.Store, store beads.Store, runID, id string, op opjournal.Op) (opjournal.Op, error) {
	for {
		got, err := s.awaitLaneOp(ctx, id, op.ID)
		if err != nil || !opjournal.FencedOff(got) {
			return got, err
		}
		noTurn, err := s.sessionHasNoTurn(ctx, store, id)
		if err != nil || noTurn {
			return got, err
		}
		if op, err = s.proposeRunCancelInterrupt(st, store, runID, id, got.Outcome.Result.Generation); err != nil {
			return got, err
		}
	}
}

// proposeRunCancelInterrupt proposes the interrupt a cancel of run runID
// sends holder session id, live now, and returns the op the cancel answers
// from. It is scoped at the holder's generation, no earlier than fencedAt,
// the one that fenced off the holder's previous op of this cancel, 0 for
// none (followUpScope). The holder's ops for the run form a chain per
// incarnation: the first keyed run-cancel:<run id>:<generation>
// (runCancelLaneKey), with the run as its causation, and each op that closed
// followed by the next (runCancelRetryKey). The cancel walks the chain to its
// first op still open, proposing the next one when every op there closed. So
// cancels that overlap share one open op per holder and await it, and each
// later cancel interrupts the holder again, as legacy's StopTurn of every
// live holder on every cancel does: the holder may have started another turn
// since.
func (s *Server) proposeRunCancelInterrupt(st *opjournal.Store, store beads.Store, runID, id string, fencedAt int64) (opjournal.Op, error) {
	scope, why, whyErr := sessionOpScope(store, id)
	if why != "" {
		return opjournal.Op{}, fmt.Errorf("the operation journal cannot record its interrupt (%s): %w", why, errors.Join(whyErr, errors.New("nothing sent")))
	}
	scope = followUpScope(scope, fencedAt)
	req := opjournal.Request{
		Conversation: id,
		Kind:         opjournal.KindInterrupt,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    runCancelLaneKey(runID, scope.Generation),
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Policy:       interruptOpPolicy,
		Scope:        scope,
		Causation:    runID,
	}
	for {
		// An op this cancel created is new, so the walk ends there: it only
		// ever steps past an op an earlier cancel proposed.
		op, created, err := s.proposeLaneOp(st, req)
		if err != nil || created || op.Status != opjournal.StatusClosed {
			return op, err
		}
		req.ClientKey = runCancelRetryKey(runID, scope.Generation, op.ID)
	}
}

// --- /respond ---

// laneRespondAnswerBound is how long a /respond under lane authority awaits
// its op before it answers 202 accepted with the op still open, 202's
// meaning since OJ-4. A var so tests can shorten it.
var laneRespondAnswerBound = 20 * time.Second

// refusedKeyReuse closes, after refused:, the row under a keyed lane
// /respond's key (the row that pins the key, or OJ-4's op the lane adopted)
// when the lane op that answers its occurrence records another answer: the
// request is 409 key_reuse, and so is every retry under the key.
const refusedKeyReuse = "key_reuse"

// respondSessionLane is /respond under lane authority (respond listed): a
// respond op keyed by the interaction occurrence it answers, which the lane
// host runs at most once, awaited at most until ctx ends. key is the
// Idempotency-Key header.
//
// A key that already names a respond is its retry (retryLaneRespond).
// Otherwise the front door reads the pending interaction as today and writes
// nothing on a refusal (laneRespondPending). The occurrence is the latest
// unresolved raising of the pending request id, or the next one recorded now
// (laneRespondOccurrence), numbered as the lane numbers it, and the op is
// keyed RespondKey of it: a different answer to the same occurrence from the
// same principal is 409 key_reuse, and the first answer wins. A request that
// carries a key pins the key to the occurrence before it proposes the op
// (pinLaneRespondKey), so however the request ends, a retry under the key
// answers that occurrence, never a later, identical prompt. A key that is the
// op's own (RespondKey of the occurrence, as a producer that saw the raising
// keys its answer) needs no pin: the op is the row under it. Any other key in
// that namespace (opjournal.IsRespondKey) that names no op names no raising
// pending now (one that ended, one of another request, one not raised yet,
// or none), so it is 409 no_pending, nothing proposed or pinned. A pin would
// send its answer to the prompt pending now, and would take the id of the
// operator's op answering the raising the key names, so every answer to that
// raising would be 409 key_reuse.
func (s *Server) respondSessionLane(ctx context.Context, mgr *session.Manager, store beads.Store, id, key string, resp runtime.InteractionResponse) (syncAnswer, error) {
	st, err := s.conversationOpStore()
	if err != nil {
		return syncAnswer{}, err
	}
	prior, retry, err := keyedLaneOp(st, opjournal.KindRespond, id, key)
	if err != nil {
		return syncAnswer{}, err
	}
	if retry {
		return s.retryLaneRespond(ctx, mgr, store, st, id, key, prior, resp)
	}
	pending, err := laneRespondPending(mgr, id, resp)
	if err != nil {
		return syncAnswer{}, humaSessionManagerError(err)
	}
	scope, req, err := laneRespondOccurrence(st, store, id, pending.RequestID, resp)
	if err != nil {
		return syncAnswer{}, err
	}
	if key == "" || key == req.ClientKey {
		op, created, err := s.proposeLaneOp(st, req)
		if err != nil {
			return syncAnswer{}, err
		}
		return s.answerLaneRespond(ctx, id, key, op, !created)
	}
	if opjournal.IsRespondKey(key) {
		return syncAnswer{}, humaSessionManagerError(fmt.Errorf("session %s: Idempotency-Key %q answers a raising that is not pending: the one pending now is interaction %s, keyed %q: %w", id, key, req.Scope.Interaction, req.ClientKey, session.ErrNoPendingInteraction))
	}
	row, created, err := pinLaneRespondKey(st, id, key, scope, resp, req.Scope.Interaction)
	if err != nil {
		return syncAnswer{}, err
	}
	if !created {
		// A request under the key pinned it first: this one is its retry.
		return s.retryLaneRespond(ctx, mgr, store, st, id, key, row, resp)
	}
	return s.respondUnderPin(ctx, st, id, key, req, s.closeLaneRespondPin(st, id, key, row))
}

// laneRespondOccurrence is the request of the lane respond op that answers
// requestID, the request session id shows pending, with resp: keyed by its
// occurrence in the session's generation (sessionOpScope), the latest
// unresolved raising of requestID or the next one recorded now
// (Store.RecordRaising). scope is that generation's session scope, naming
// requestID. A generation that does not read, or a raising that cannot be
// recorded, is 503 op_journal_unavailable, nothing done.
func laneRespondOccurrence(st *opjournal.Store, store beads.Store, id, requestID string, resp runtime.InteractionResponse) (scope opjournal.Scope, req opjournal.Request, err error) {
	scope, why, whyErr := sessionOpScope(store, id)
	if why != "" {
		cause := why
		if whyErr != nil {
			cause += ": " + whyErr.Error()
		}
		return opjournal.Scope{}, opjournal.Request{}, apierr.OpJournalUnavailable.Msg(fmt.Sprintf("op_journal_unavailable: the operation journal cannot record this respond (%s); nothing was done", cause))
	}
	raising, _, err := st.RecordRaising(id, scope.Generation, requestID, opjournal.FactBasisObserved, time.Now())
	if err != nil {
		return opjournal.Scope{}, opjournal.Request{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: recording the interaction's raising: %v", err)
	}
	if req, err = laneRespondOpRequest(id, raising.Raised, resp); err != nil {
		return opjournal.Scope{}, opjournal.Request{}, err
	}
	scope.Interaction = requestID
	return scope, req, nil
}

// laneRespondOpRequest is the request of the lane respond op that answers
// raised, an interaction.raised fact of session conv, with resp: keyed
// RespondKey of raised, scoped to its generation and occurrence,
// at_most_once, its payload resp's answer in canonical form.
func laneRespondOpRequest(conv string, raised opjournal.Fact, resp runtime.InteractionResponse) (opjournal.Request, error) {
	key, err := opjournal.RespondKey(raised)
	if err != nil {
		return opjournal.Request{}, apierr.Internal.Msgf("keying the respond: %v", err)
	}
	payload, err := opjournal.EncodeRespondPayload(opjournal.RespondPayload{Action: resp.Action, Text: resp.Text, Metadata: resp.Metadata})
	if err != nil {
		return opjournal.Request{}, apierr.ValidationFailed.Msg("invalid_request: " + err.Error())
	}
	return opjournal.Request{
		Conversation: conv,
		Kind:         opjournal.KindRespond,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    key,
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:      payload,
		Policy:       opjournal.Policy{Guarantee: opjournal.GuaranteeAtMostOnce},
		Scope:        opjournal.Scope{Kind: opjournal.ScopeSession, Generation: raised.Generation, Interaction: raised.Interaction},
	}, nil
}

// laneRespondPending is a lane /respond's front door, OJ-4's: the pending
// interaction of session id that resp answers, or the legacy refusal's
// error, nothing written, when the runtime cannot report one, none is
// pending, or resp names another request. humaSessionManagerError maps it
// onto the wire (501, 409 no_pending, 409 invalid_interaction), and
// syncOpResult onto the outcome OJ-4 closes its op with for it.
func laneRespondPending(mgr *session.Manager, id string, resp runtime.InteractionResponse) (*runtime.PendingInteraction, error) {
	pending, supported, err := mgr.Pending(id)
	switch {
	case err != nil:
		return nil, err
	case !supported:
		return nil, fmt.Errorf("session %s: %w", id, session.ErrInteractionUnsupported)
	case pending == nil || pending.RequestID == "":
		return nil, fmt.Errorf("session %s: %w", id, session.ErrNoPendingInteraction)
	case resp.RequestID != "" && resp.RequestID != pending.RequestID:
		return nil, fmt.Errorf("%w: pending interaction %q does not match request %q", session.ErrInteractionMismatch, pending.RequestID, resp.RequestID)
	}
	return pending, nil
}

// retryLaneRespond answers resp, a /respond of session id whose
// Idempotency-Key key names prior, a respond. A request that asks again what
// prior recorded (sameRespondRequest) is its retry: a row that names its
// occurrence (laneRespondPin: the row a keyed lane respond pins its key
// with, or OJ-4's own op the lane adopted) answers through that occurrence's
// op (respondUnderPin), whatever state the row is in, and a lane op the key
// names itself is awaited while open and replayed once closed. So a retry
// never resolves the occurrence again, and never answers an identical prompt
// raised later. OJ-4's own op still open and naming none is finished through
// the lane (adoptLegacyRespond), and one closed coalesced into a lane op
// answers from that op; any other op the key names (OJ-4's own, closed) is
// answered as OJ-4 replays it. One that asks anything else is 422
// idempotency_mismatch, nothing written, as OJ-4 answers it.
func (s *Server) retryLaneRespond(ctx context.Context, mgr *session.Manager, store beads.Store, st *opjournal.Store, id, key string, prior opjournal.Op, resp runtime.InteractionResponse) (syncAnswer, error) {
	same, err := sameRespondRequest(st, prior, resp)
	if err != nil {
		return syncAnswer{}, err
	}
	if !same {
		return syncAnswer{}, respondKeyMismatch(prior)
	}
	if occ, pinned := laneRespondPin(prior); pinned {
		req, err := pinnedLaneRespond(st, id, prior, occ, resp)
		if err != nil {
			return syncAnswer{}, err
		}
		return s.respondUnderPin(ctx, st, id, key, req, s.closeLaneRespondPin(st, id, key, prior))
	}
	if prior.Executor == opjournal.ExecutorLegacyAPI && prior.Status != opjournal.StatusClosed {
		return s.adoptLegacyRespond(ctx, mgr, store, st, id, key, prior, resp)
	}
	target, err := followCoalesced(st, prior)
	if err != nil {
		return syncAnswer{}, err
	}
	return s.answerLaneRespond(ctx, id, key, target, true)
}

// adoptLegacyRespond answers resp, a /respond under the lane whose
// Idempotency-Key key names prior, OJ-4's own respond op (legacy:api) still
// open: the API that admitted it ended before it closed it, and respond was
// listed when the city's controller started again. OJ-4's admission takes
// the op as it takes it while respond is not listed (legacyop.Recorder.Admit,
// through serverOps.laneAdopt), whatever ops.control says: an op in flight in
// this process is 409 in_flight, one past its finish_by closes
// failed{deadline_in_flight} and replays, and one inside it is adopted. The
// lane, which executes respond now, then answers the adopted op's request,
// as OJ-4's adopted respond answers only the request id its op names: the
// front door reads that request pending, the adopted op names that
// request's occurrence as its causation (Store.SetCausation), as the row
// that pins a key names it (laneRespondPin), and only then is the lane op of
// the occurrence proposed to answer it (respondUnderPin), the adopted op
// closed skipped{coalesced_into} that lane op. So a retry under the key, and
// a rollback's replay (replayLaneRespond), follow the op to that lane op
// even while it stays open, its close lost with the API that adopted it, or
// once the op sweeper closed it failed{deadline_in_flight}: neither runs the
// respond again, which could answer a later, identical prompt. The API sends
// the runtime nothing itself. A refusal closes the op as OJ-4 closes the
// same refusal: no prompt pending, another one, or a session that moved to
// another generation since, whose prompt is a later one, is skipped{stale}.
// A lane op that records another answer to the occurrence closes it
// failed{refused:key_reuse}, and a write that fails before the lane op
// exists, the occurrence's name included, leaves it open for a retry to
// adopt again.
func (s *Server) adoptLegacyRespond(ctx context.Context, mgr *session.Manager, store beads.Store, st *opjournal.Store, id, key string, prior opjournal.Op, resp runtime.InteractionResponse) (syncAnswer, error) {
	payload, err := encodeRespondOpPayload(resp)
	if err != nil {
		return syncAnswer{}, err
	}
	adm, err := s.ops.laneAdopt.Admit(legacyop.Request{
		Conversation: id,
		Principal:    legacyop.PrincipalOperator,
		Actor:        opjournal.ActorOperator,
		Key:          key,
		Payload:      payload,
		Kind:         opjournal.KindRespond,
		Policy:       respondOpPolicy,
		Scope:        prior.Scope,
	})
	switch {
	case err != nil:
		return syncAnswer{}, opAdmitError(err, key)
	case adm.Decision == legacyop.Replay && adm.Cached != nil:
		return syncAnswer{}, requestInFlight()
	case adm.Decision == legacyop.Replay:
		target, err := followCoalesced(st, adm.Op)
		if err != nil {
			return syncAnswer{}, err
		}
		return s.answerLaneRespond(ctx, id, key, target, true)
	}
	handedOff := false
	defer releaseOnPanic(adm, &handedOff)
	refused := func(err error) error {
		adm.Finish(syncOpResult(opjournal.KindRespond, err, false))
		return humaSessionManagerError(err)
	}
	resp.RequestID = prior.Scope.Interaction
	pending, err := laneRespondPending(mgr, id, resp)
	if err != nil {
		return syncAnswer{}, refused(err)
	}
	_, req, err := laneRespondOccurrence(st, store, id, pending.RequestID, resp)
	if err != nil {
		adm.Finish(legacyop.Result{LeaveOpen: true})
		return syncAnswer{}, err
	}
	if req.Scope.Generation != prior.Scope.Generation {
		return syncAnswer{}, refused(fmt.Errorf("session %s runs generation %d, and op %s answers a prompt of generation %d: %w", id, req.Scope.Generation, prior.ID, prior.Scope.Generation, session.ErrNoPendingInteraction))
	}
	if _, err := st.SetCausation(adm.Op.ID, adm.Op.Attempt, req.Scope.Interaction, "api"); err != nil {
		adm.Finish(legacyop.Result{LeaveOpen: true})
		return syncAnswer{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: naming interaction %s on op %s, as the occurrence whose lane op answers it, so nothing was sent: %v", req.Scope.Interaction, adm.Op.ID, err)
	}
	return s.respondUnderPin(ctx, st, id, key, req, func(_ string, res legacyop.Result) { adm.Finish(res) })
}

// respondKeyMismatch is the 422 idempotency_mismatch of a /respond whose
// Idempotency-Key names prior, a respond that records another answer or
// request_id.
func respondKeyMismatch(prior opjournal.Op) error {
	return apierr.IdempotencyMismatch.Msgf("idempotency_mismatch: Idempotency-Key reused with a different request: it names respond op %s, which records another answer or request_id", prior.ID)
}

// replayLaneRespond answers resp, a /respond of session id while OJ-4
// answers /respond (respond not listed: a rollback, or no lane host runs),
// when its Idempotency-Key key names a respond the lane path wrote or
// answered (laneRespondKeyRow), whatever ops.control says: a lane op, a row
// that names its occurrence (laneRespondPin: the row a keyed lane respond
// pins its key with, or OJ-4's own op the lane adopted, adoptLegacyRespond),
// or OJ-4's own op the lane closed. As under the lane (retryLaneRespond), a
// request that asks again what that row recorded answers from the lane op,
// never from a respond run again, and one that asks anything else is 422
// idempotency_mismatch. Nothing proposes a lane op, which no lane executes
// now: a closed op replays under key, one still open is 409 in_flight, as
// OJ-4 answers a request it holds, and a named occurrence whose op records
// another answer is 409 key_reuse. A row whose occurrence has no op answers
// from the row itself: the request that named the occurrence ended before
// it proposed the op, so the lane sent nothing for it, and the row is
// in_flight until the op sweeper closes it at its finish_by, then replays
// that close. answered is false, for OJ-4 to answer the request, when key
// names no such row.
func (s *Server) replayLaneRespond(ctx context.Context, store beads.SessionStore, id, key string, resp runtime.InteractionResponse) (answer syncAnswer, answered bool, err error) {
	st, prior, found, err := s.laneRespondKeyRow(store, id, key)
	if err != nil || !found {
		return syncAnswer{}, err != nil, err
	}
	same, err := sameRespondRequest(st, prior, resp)
	switch {
	case err != nil:
		return syncAnswer{}, true, err
	case !same:
		return syncAnswer{}, true, respondKeyMismatch(prior)
	}
	op := prior
	if occ, pinned := laneRespondPin(prior); pinned {
		req, err := pinnedLaneRespond(st, id, prior, occ, resp)
		if err != nil {
			return syncAnswer{}, true, err
		}
		stored, ok, err := storedLaneOp(st, req)
		if err != nil {
			return syncAnswer{}, true, err
		}
		if ok {
			op = stored
		}
	} else if op, err = followCoalesced(st, prior); err != nil {
		return syncAnswer{}, true, err
	}
	if op.Status != opjournal.StatusClosed {
		return syncAnswer{}, true, requestInFlight()
	}
	answer, err = s.answerLaneRespond(ctx, id, key, op, true)
	return answer, true, err
}

// laneRespondKeyRow reads the respond row Idempotency-Key key names on
// session conv when the lane path wrote or answered it: a lane op, a row
// that names the occurrence whose lane op answers it (laneRespondPin: the
// row a keyed lane respond pins its key with, or OJ-4's own op a lane retry
// adopted, adoptLegacyRespond), open or closed, or a respond closed
// coalesced into the lane op that answered it, or failed{refused:key_reuse}
// when another answer won its occurrence, closes only the lane path writes
// on a respond. It reads the lane path's own store (openOpStore, which
// conversationOpStore opens), which no ops.* gate governs: OJ-4 reads no row
// in off, nor in auto by its builtin default over a sessions store that is
// not SQLite-backed, such as the native Dolt store a lane host runs over, so
// a rollback that read through OJ-4 would run the respond again. found is
// false when key is empty or names no such row, and when the sessions store
// can never hold one, which the store itself says: it cannot apply guarded
// batches, which every lane host needs (so a bd-served store pays no
// op-store open per request), or it names no id namespace
// (opjournal.ErrNoNamespace), so no op id derives over it. OJ-4 then answers
// the request. Any other op store that does not open, and a read that fails,
// is 503 op_journal_unavailable, nothing done: the key may name an answer
// the lane sent. NewStore reports storage it cannot reach, or a store closed
// under it, as the ErrConditionalWriteUnsupported it reports for a store
// that cannot fence writes, so that error does not say the lane never wrote
// there.
func (s *Server) laneRespondKeyRow(store beads.SessionStore, conv, key string) (st *opjournal.Store, row opjournal.Op, found bool, err error) {
	if key == "" {
		return nil, opjournal.Op{}, false, nil
	}
	if _, ok := beads.GuardedBatcherFor(store); !ok {
		return nil, opjournal.Op{}, false, nil
	}
	if st, err = s.openOpStore(); err != nil {
		if errors.Is(err, opjournal.ErrNoNamespace) {
			return nil, opjournal.Op{}, false, nil
		}
		return nil, opjournal.Op{}, false, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: opening the operation journal to read the op Idempotency-Key %q may name, so nothing was done: %v", key, err)
	}
	id, err := opjournal.DeriveID(st.Prefix(), conv, legacyop.PrincipalOperator, key)
	if err != nil {
		// The lane path refuses a key no op id derives from, so none names
		// a row of its own.
		return nil, opjournal.Op{}, false, nil
	}
	row, err = st.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return nil, opjournal.Op{}, false, nil
	case err != nil:
		return nil, opjournal.Op{}, false, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading op %s, which Idempotency-Key %q names, so nothing was done: %v", id, key, err)
	}
	_, pinned := laneRespondPin(row)
	lane := pinned || row.Executor == opjournal.ExecutorLane || row.Outcome.Result.CoalescedInto != "" ||
		row.Outcome.Reason == opjournal.RefusedReasonPrefix+refusedKeyReuse
	if row.Kind != opjournal.KindRespond || !lane {
		return nil, opjournal.Op{}, false, nil
	}
	return st, row, true, nil
}

// sameRespondRequest reports whether resp asks again what prior, the respond
// a /respond's Idempotency-Key names, recorded: its request_id, when it names
// one, is the request prior answers, and its answer is prior's. A lane op is
// compared by its answer's canonical form (sameLaneRespond). A row of the
// legacy op's frozen shape (OJ-4's op, or the row a keyed lane respond pins
// its key with) is compared by its request digest under its own scope, as
// OJ-4's admission compares a same-key retry, which adopts that scope.
func sameRespondRequest(st *opjournal.Store, prior opjournal.Op, resp runtime.InteractionResponse) (bool, error) {
	if prior.Executor == opjournal.ExecutorLane {
		return sameLaneRespond(prior, resp)
	}
	if resp.RequestID != "" && resp.RequestID != prior.Scope.Interaction {
		return false, nil
	}
	payload, err := encodeRespondOpPayload(resp)
	if err != nil {
		return false, err
	}
	again, err := st.Preview(opjournal.Request{
		Conversation: prior.Conversation,
		Kind:         opjournal.KindRespond,
		Executor:     prior.Executor,
		ClientKey:    prior.ClientKey,
		Producer:     prior.Producer,
		Payload:      payload,
		Policy:       respondOpPolicy,
		Scope:        prior.Scope,
	})
	if err != nil {
		return false, proposeError(err)
	}
	return again.PayloadDigest == prior.PayloadDigest, nil
}

// sameLaneRespond is sameRespondRequest for prior, a lane respond: resp's
// request_id, when it names one, is the request of the occurrence prior
// answers, and its answer in canonical form (EncodeRespondPayload) is
// prior's payload. An answer the canonical form refuses is not prior's.
func sameLaneRespond(prior opjournal.Op, resp runtime.InteractionResponse) (bool, error) {
	requestID, _, err := opjournal.ParseInteractionOccurrence(prior.Scope.Interaction)
	if err != nil {
		return false, apierr.Internal.Msgf("reading the occurrence op %s answers: %v", prior.ID, err)
	}
	if resp.RequestID != "" && resp.RequestID != requestID {
		return false, nil
	}
	payload, err := opjournal.EncodeRespondPayload(opjournal.RespondPayload{Action: resp.Action, Text: resp.Text, Metadata: resp.Metadata})
	return err == nil && payload == prior.Payload, nil
}

// pinLaneRespondKey writes, under a lane /respond's Idempotency-Key key, the
// row that pins the key to occ, the occurrence the request answers, before
// the op answering it is proposed: a respond row of the legacy op's frozen
// shape (the request-id scope, respondOpPayload, at_least_once) with occ as
// its causation, which the request digest does not cover, so OJ-4 compares a
// same-key retry with the row as with its own op. Its executor is
// ExecutorLegacy, which no surface executes or adopts: the lane op answers
// the row, and OJ-4's admission never runs a respond for it, which could
// answer a later, identical prompt (replayLaneRespond). created is false
// when a request under the key wrote the row first.
func pinLaneRespondKey(st *opjournal.Store, conv, key string, scope opjournal.Scope, resp runtime.InteractionResponse, occ string) (opjournal.Op, bool, error) {
	payload, err := encodeRespondOpPayload(resp)
	if err != nil {
		return opjournal.Op{}, false, err
	}
	row, created, err := st.Propose(opjournal.Request{
		Conversation: conv,
		Kind:         opjournal.KindRespond,
		Executor:     opjournal.ExecutorLegacy,
		ClientKey:    key,
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:      payload,
		Policy:       respondOpPolicy,
		Scope:        scope,
		Causation:    occ,
	})
	if err != nil {
		return opjournal.Op{}, false, proposeError(err)
	}
	return row, created, nil
}

// laneRespondPin is the occurrence row pins its key to when row names one:
// the row a keyed lane /respond pins its key with (pinLaneRespondKey), or
// OJ-4's own respond op the lane adopted, which names the occurrence before
// the lane op answering it is proposed (adoptLegacyRespond). Its causation
// is then an occurrence of the request id its scope names. pinned is false
// for any other op, such as a lane op, or OJ-4's own respond op the lane
// never adopted.
func laneRespondPin(row opjournal.Op) (occ string, pinned bool) {
	if row.Executor == opjournal.ExecutorLane {
		return "", false
	}
	requestID, _, err := opjournal.ParseInteractionOccurrence(row.Causation)
	return row.Causation, err == nil && requestID == row.Scope.Interaction
}

// pinnedLaneRespond is the request of the lane respond op that answers occ,
// the occurrence row pins a /respond's key to, with resp: laneRespondOpRequest
// of occ's interaction.raised fact in session conv. A read of the raising
// that fails, or finds none, is 503 op_journal_unavailable, nothing done.
func pinnedLaneRespond(st *opjournal.Store, conv string, row opjournal.Op, occ string, resp runtime.InteractionResponse) (opjournal.Request, error) {
	r, found, err := st.Raising(row.Conversation, row.Scope.Generation, occ)
	switch {
	case err != nil:
		return opjournal.Request{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading interaction %s, which op %s pins its key to, so nothing was done: %v", occ, row.ID, err)
	case !found:
		return opjournal.Request{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: interaction %s, which op %s pins its key to, has no raising recorded, so nothing was done", occ, row.ID)
	}
	return laneRespondOpRequest(conv, r.Raised, resp)
}

// respondUnderPin proposes req, the lane respond op that answers the
// occurrence the row under a /respond's key key stands for (the row that
// pins the key, or OJ-4's op the lane adopted), ends that row with end, and
// answers from the op. end gets the op and the row's result: closed
// skipped{coalesced_into} the op once the op exists. The op is proposed
// create-if-absent, so a retry finds the op an earlier request under the key
// proposed, or proposes it when that request ended first. An op that cannot
// be proposed leaves the row open (LeaveOpen), for a retry to propose again,
// except an occurrence whose op records another answer: that 409 key_reuse
// is every retry's answer too, so the row closes failed{refused:key_reuse}.
func (s *Server) respondUnderPin(ctx context.Context, st *opjournal.Store, id, key string, req opjournal.Request, end func(op string, res legacyop.Result)) (syncAnswer, error) {
	op, created, err := st.Propose(req)
	if err != nil {
		var reuse *opjournal.KeyReuseError
		if errors.As(err, &reuse) {
			end(reuse.Existing.ID, legacyop.Result{Outcome: opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.RefusedReasonPrefix + refusedKeyReuse}})
		} else {
			end("", legacyop.Result{LeaveOpen: true})
		}
		return syncAnswer{}, proposeError(err)
	}
	if created {
		s.pokeConversationLane(id)
	}
	end(op.ID, legacyop.Result{Outcome: opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonCoalescedInto, Result: opjournal.Result{CoalescedInto: op.ID}}})
	return s.answerLaneRespond(ctx, id, key, op, !created)
}

// closeLaneRespondPin is the end respondUnderPin gives row, the row that
// pins a /respond's key key to an occurrence: it closes row from the result
// while row is open, and leaves it open for LeaveOpen. A close that fails is
// logged and the request answers all the same: row stays pinned, so a retry
// under the key reaches the occurrence's op again, as it does once the op
// sweeper has closed row failed{deadline_in_flight}.
func (s *Server) closeLaneRespondPin(st *opjournal.Store, id, key string, row opjournal.Op) func(op string, res legacyop.Result) {
	return func(op string, res legacyop.Result) {
		if res.LeaveOpen || row.Status == opjournal.StatusClosed {
			return
		}
		if _, err := st.Close(row.ID, res.Outcome, "api"); err != nil {
			s.ops.logf("api: respond of session %s: closing op %s %s{%s}, which pins Idempotency-Key %q to op %s's occurrence: %v; it stays open, and a retry under the key reaches op %s again", id, row.ID, res.Outcome.Arm, res.Outcome.Reason, key, op, err, op)
		}
	}
}

// storedLaneOp reads the lane respond op req names, as Propose finds it,
// without proposing it: found is false when none is stored, and a stored op
// that records another answer to the occurrence is 409 key_reuse, as
// Propose answers it. A read that fails is 503 op_journal_unavailable,
// nothing done.
func storedLaneOp(st *opjournal.Store, req opjournal.Request) (op opjournal.Op, found bool, err error) {
	want, err := st.Preview(req)
	if err != nil {
		return opjournal.Op{}, false, proposeError(err)
	}
	got, err := st.Get(want.ID)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return opjournal.Op{}, false, nil
	case err != nil:
		return opjournal.Op{}, false, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading op %s, which answers interaction %s, so nothing was done: %v", want.ID, req.Scope.Interaction, err)
	case got.PayloadDigest != want.PayloadDigest:
		return opjournal.Op{}, false, proposeError(&opjournal.KeyReuseError{Existing: got})
	}
	return got, true, nil
}

// answerLaneRespond awaits op, the respond a /respond of session id proposed
// or found, at most laneRespondAnswerBound or until ctx ends, and answers
// from its outcome: succeeded is 202 accepted with the op, a failure the
// refusal laneRespondRefusal maps it to, and an op still open a provisional
// 202 accepted with the open op, which the Idempotency-Key LRU does not keep.
// The answer carries key, the request's Idempotency-Key, or the op's own key
// when the request carried none, and is replayed when the request found op
// (replayed) and op is closed: an open op carries no stored outcome. A
// legacy op a key names is answered as OJ-4 replays it.
func (s *Server) answerLaneRespond(ctx context.Context, id, key string, op opjournal.Op, replayed bool) (syncAnswer, error) {
	if op.Executor != opjournal.ExecutorLane {
		if op.Status != opjournal.StatusClosed {
			return syncAnswer{}, requestInFlight()
		}
		if syncOpReplaysAsSuccess(op) {
			return syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: op.ClientKey, Replayed: true, Op: conversationOpWire(op)}}, nil
		}
		return syncAnswer{}, replayedSyncError(op, humaSessionManagerError)
	}
	if op.Status != opjournal.StatusClosed {
		actx, cancel := context.WithTimeout(ctx, laneRespondAnswerBound)
		defer cancel()
		var err error
		if op, err = s.awaitLaneOp(actx, id, op.ID); err != nil {
			return syncAnswer{}, err
		}
	}
	if key == "" {
		key = op.ClientKey
	}
	answer := syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: key, Op: conversationOpWire(op)}}
	if op.Status != opjournal.StatusClosed {
		answer.provisional = true
		return answer, nil
	}
	if err := laneRespondRefusal(op); err != nil {
		return syncAnswer{}, err
	}
	answer.Replayed = replayed
	return answer, nil
}

// laneRespondRefusal is the refusal a closed lane respond answers with, nil
// when it succeeded: skipped{stale} is 409 no_pending; failed{unsupported}
// 501; refused:action the runtime's refusal, 409 invalid_interaction; and
// any other failure, or a cancel, 409 op_already_failed. That last one
// offers a resend under a new Idempotency-Key only when nothing was sent. An
// answer that may have reached its prompt (maybe_delivered, or
// failed{ambiguous_delivery}) is never sent again, and a prompt pending now
// may be a later, identical one, which a resend would answer as the next
// occurrence (opjournal.RespondKey); so that answer offers none.
func laneRespondRefusal(op opjournal.Op) error {
	arm, reason := op.Outcome.Effective()
	closed := fmt.Sprintf("op %s closed %s", op.ID, laneOutcomeText(op))
	switch {
	case arm == opjournal.ArmSucceeded:
		return nil
	case arm == opjournal.ArmSkipped && reason == opjournal.ReasonStale:
		return humaSessionManagerError(fmt.Errorf("%s: the prompt it answers is no longer pending: %w", closed, session.ErrNoPendingInteraction))
	case arm == opjournal.ArmFailed && reason == opjournal.ReasonUnsupported:
		return humaSessionManagerError(fmt.Errorf("%s: %w", closed, session.ErrInteractionUnsupported))
	case arm == opjournal.ArmFailed && op.Outcome.Reason == opjournal.ReasonRefusedAction:
		return humaSessionManagerError(fmt.Errorf("%s: %w: %w", closed, session.ErrInteractionMismatch, runtime.ErrInteractionActionUnavailable))
	case op.MaybeDelivered > 0 || reason == opjournal.ReasonAmbiguousDelivery:
		return apierr.OpAlreadyFailed.Msg(fmt.Sprintf("%s: %s: the answer may have reached its prompt, so it is never sent again; read the session's pending interaction before answering again", opAlreadyFailedCode, closed))
	}
	return apierr.OpAlreadyFailed.Msg(fmt.Sprintf("%s: %s with nothing sent; resend with a new Idempotency-Key to act again", opAlreadyFailedCode, closed))
}

// --- /submit, /messages and extmsg notices ---

// laneSubmitSettle is how long a /submit or /messages under lane authority
// waits for its op before it emits the request's terminal event: closed
// succeeded is the result, queued false; closed otherwise request.failed;
// still open the result, queued true, as a legacy message that was queued
// succeeded. Its later fate is op.changed's. A var so tests can shorten it.
var laneSubmitSettle = 5 * time.Second

// extmsgNoticePrincipal is the principal of the notices extmsg proposes for a
// conversation's members; their label is extmsg.
const extmsgNoticePrincipal = "gc:extmsg"

// LaneMessageOpPolicy is the lane policy of a submit intent
// (lanesubmit.Policy): the API's and its tests' name for it.
func LaneMessageOpPolicy(intent session.SubmitIntent) opjournal.Policy {
	return lanesubmit.Policy(intent)
}

// acceptMessageLane is /submit and /messages under lane authority (message
// listed): it accepts req as a lane message op through lanesubmit.Accept,
// the acceptance gc session submit's local fallback shares (the key, the
// preview, the resolution that materializes a configured named session's
// row, the op under the key, the front door's refusals, the proposal, the
// resume and the cancel of an op this request wrote whose resume is
// refused), maps its refusals onto the wire (laneAcceptError), and answers
// from the op (answerLaneMessage): 202 once the op is committed, its
// request_id the op id. The resolution is the API's
// (resolveSessionIDMaterializingNamedWithContext, unless req's target
// already resolved) and so is the front door (rejectPendingInteraction). A
// same-key retry, on the API process that answered the first request too
// (the Idempotency-Key LRU keeps no lane answer: withMessageIdempotency),
// and whatever the session waits on, is answered from the op under the key:
// a closed op replays (OJ-2b's path), an open legacy op is 409 in_flight,
// and an open lane op is accepted again once its resume runs again. The
// request's terminal event follows laneSubmitSettle after its acceptance.
func (s *Server) acceptMessageLane(ctx context.Context, store beads.Store, req *messageOpRequest, ep events.Recorder, cursor string) (asyncAcceptedBody, error) {
	st, err := s.conversationOpStore()
	if err != nil {
		return asyncAcceptedBody{}, err
	}
	acc, err := lanesubmit.Accept(ctx, st, session.NewStore(beads.SessionStore{Store: store}), lanesubmit.Request{
		Key:    req.key,
		Text:   req.text,
		Intent: req.intent,
		Resume: req.resume,
		Resolve: func(ctx context.Context) (string, error) {
			if req.conv != "" {
				return req.conv, nil
			}
			id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, req.target)
			if err != nil {
				return "", humaResolveError(err)
			}
			return id, nil
		},
		Pending: func(ctx context.Context, id string) error {
			return s.rejectPendingInteraction(ctx, store, id)
		},
	})
	if err != nil {
		return asyncAcceptedBody{}, s.laneAcceptError(err)
	}
	return s.answerLaneMessage(store, st, acc.Op, acc.Owned, req, ep, acc.Key, cursor)
}

// laneAcceptError maps a refusal of lanesubmit.Accept onto the wire: a key
// that cannot be minted is 500, an invalid one 422 invalid_idempotency_key,
// the journal's refusal laneMessageProposeError's, a read of the op under
// the key that fails 503 op_journal_unavailable, a key that names an op of
// another kind 422 idempotency_mismatch, and the session's refusal of the
// resume laneResumeError's: once the op is written, an op this request
// wrote was canceled first (its cancel logged) and the error says the key is
// spent (laneKeySpent). The resolution's and the front door's errors are on
// the wire already.
func (s *Server) laneAcceptError(err error) error {
	var (
		mint    *lanesubmit.MintError
		key     *lanesubmit.KeyError
		journal *lanesubmit.JournalError
		read    *lanesubmit.ReadError
		kind    *lanesubmit.KindError
		resume  *lanesubmit.ResumeError
	)
	switch {
	case errors.As(err, &mint):
		return apierr.Internal.Msg(mint.Err.Error())
	case errors.As(err, &key):
		return apierr.InvalidIdempotencyKey.Msg("invalid_idempotency_key: " + key.Err.Error())
	case errors.As(err, &journal):
		return laneMessageProposeError(journal.Err)
	case errors.As(err, &read):
		return apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading op %s: %v", read.ID, read.Err)
	case errors.As(err, &kind):
		return apierr.IdempotencyMismatch.Msgf("idempotency_mismatch: Idempotency-Key reused with a different request: it names a %s op", kind.Op.Kind)
	case errors.As(err, &resume):
		mapped := laneResumeError(resume.Session, resume.Err)
		if !resume.Owned {
			return mapped
		}
		canceled := s.logLaneCancel(resume.Op.ID, lanesubmit.ResumeRefusedWhy, resume.Cancel, resume.CancelErr)
		return laneKeySpent(mapped, resume.Op.ID, canceled)
	}
	return err
}

// laneResumeError maps the refusal of a resume of session id onto the wire:
// a closed, closing or archived session is refused as /wake refuses it
// (409), one whose drain is stopping it 409 session_stopping, and a
// revision fence lost on every attempt 409 conflict. nil stays nil.
func laneResumeError(id string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, session.ErrSessionStopping):
		return humaSessionManagerError(err)
	case errors.Is(err, session.ErrWakeRequestContended):
		return apierr.SessionConflict.Msg("conflict: " + err.Error())
	}
	return wakeHTTPError(id, err)
}

// laneKeySpent is err, the refused resume of message op id, which this
// request wrote and then tried to cancel (cancelLaneMessage), saying that
// the op's key is spent: a same-key retry reports that op instead of
// sending again. Only an op its cancel closed (canceled) delivers nothing,
// so only then does it say to resend under a new Idempotency-Key. An op
// the lane host's scan took first, or whose cancel failed, may still
// deliver the message, which a resend would deliver twice: then it says
// that a same-key retry reports what became of the op.
func laneKeySpent(err error, id string, canceled bool) error {
	var problem *apierr.ErrorModel
	if !errors.As(err, &problem) {
		return err
	}
	if canceled {
		problem.Detail += fmt.Sprintf("; op %s, which this request wrote, was canceled: a retry under this Idempotency-Key reports that op instead of sending again, so resend with a new Idempotency-Key", id)
	} else {
		problem.Detail += fmt.Sprintf("; op %s, which this request wrote, may still deliver the message, as its cancel did not close it: a retry under this Idempotency-Key reports what became of it instead of sending again", id)
	}
	return err
}

// laneMessageProposeError maps a refused lane message proposal onto the
// wire as the legacy admission maps it: a key that names another request is
// 422 idempotency_mismatch.
func laneMessageProposeError(err error) error {
	if errors.Is(err, opjournal.ErrKeyReuse) {
		return apierr.IdempotencyMismatch.Msg("idempotency_mismatch: Idempotency-Key reused with a different request")
	}
	return proposeError(err)
}

// answerLaneMessage answers req from its message op op, which this request
// wrote (owned, acceptMessageLane) or found under its key. A closed op
// replays its outcome with its terminal event re-emitted
// (storedReplayAnswer), and an open legacy op is 409 in_flight. An open lane
// op is accepted, whether this request wrote it or retries it: its
// acceptance is recorded, the lane host rung, and its terminal event follows
// laneSubmitSettle after the cursor (emitLaneMessageResult, which reads the
// session from store), so a retry's request_id resolves as the first
// request's does. The ring is idempotent, and a retry rings too: the
// request that wrote the op may have died before its own. A retry's
// acceptance waits, 409 in_flight, while a terminal event of the op's stays
// kept (cityOps.flushKeeps). An owned op whose acceptance the event log
// refused is canceled before the lane host is rung, so it
// delivers nothing unless the lane host's scan took it first: then the
// cancel is only requested, and a same-key retry reports what became of it.
func (s *Server) answerLaneMessage(store beads.Store, st *opjournal.Store, op opjournal.Op, owned bool, req *messageOpRequest, ep events.Recorder, key, cursor string) (asyncAcceptedBody, error) {
	switch {
	case op.Status == opjournal.StatusClosed:
		body, err := s.storedReplayAnswer(op, key, req.operation, cursor)
		if err != nil {
			return asyncAcceptedBody{}, err
		}
		return *body, nil
	case op.Executor != opjournal.ExecutorLane:
		return asyncAcceptedBody{}, requestInFlight()
	case !owned && s.ops.city.flushKeeps(s, op.ID):
		return asyncAcceptedBody{}, requestInFlight()
	}
	finish, err := s.recordAsyncAcceptance(ep, op.ID, req.operation)
	if err != nil {
		if owned {
			s.cancelLaneMessage(st, op.ID, "the request's acceptance could not be recorded")
		}
		return asyncAcceptedBody{}, err
	}
	s.pokeConversationLane(op.Conversation)
	go func() {
		defer finish()
		s.emitLaneMessageResult(store, op, req.operation)
	}()
	return asyncAcceptedBody{Status: "accepted", RequestID: op.ID, EventCursor: cursor, IdempotencyKey: key, Op: conversationOpWire(op)}, nil
}

// cancelLaneMessage cancels lane message op id for the request that created
// it, which answers with an error; why says what failed. It reports whether
// the cancel closed the op (opjournal.CancelClosed). It runs before the lane
// host is rung, so the op delivers nothing unless the lane host's scan took
// it first: then the cancel is only requested and is logged, as is a cancel
// that fails, and a same-key retry reports what became of the op.
func (s *Server) cancelLaneMessage(st *opjournal.Store, id, why string) bool {
	res, _, err := st.Cancel(id, legacyop.PrincipalOperator, why)
	return s.logLaneCancel(id, why, res, err)
}

// logLaneCancel logs a cancel of lane message op id (why says what failed)
// that answered res and err and did not close the op, and reports whether
// it closed it.
func (s *Server) logLaneCancel(id, why string, res opjournal.CancelResult, err error) bool {
	switch {
	case err != nil:
		s.ops.logf("api: canceling op %s (%s): %v", id, why, err)
	case res != opjournal.CancelClosed:
		s.ops.logf("api: op %s was taken by the lane host before its cancel (%s: %s); a same-key retry reports its outcome", id, why, res)
	}
	return err == nil && res == opjournal.CancelClosed
}

// emitLaneMessageResult waits laneSubmitSettle for lane message op op and
// emits its request's terminal event for operation: the result, queued
// false, once it closed succeeded; request.failed once it closed otherwise;
// the result, queued true, while it is still open, saying whether the
// controller will start the session for it (laneWillStart, reading the
// session from store). A /submit result's intent is the one the op's
// policy runs (submitIntentForWhen), as its replay's is. A read of the op
// that fails emits the result from op as this request accepted it, queued
// true, on purpose: the lane may still deliver the message, which a
// request.failed would invite the client to send again, and emitting
// nothing would leave the client waiting on its request_id. Its later
// fate, a close the read missed included, is op.changed's
// (laneSubmitSettle).
func (s *Server) emitLaneMessageResult(store beads.Store, op opjournal.Op, operation string) {
	ctx, cancel := context.WithTimeout(context.Background(), laneSubmitSettle)
	defer cancel()
	cur, err := s.awaitLaneOp(ctx, op.Conversation, op.ID)
	if err != nil {
		s.ops.logf("api: op %s: reading its outcome for the request's result: %v", op.ID, err)
		cur = op
	}
	wire := conversationOpWire(cur)
	if arm, reason := cur.Outcome.Effective(); cur.Status == opjournal.StatusClosed && arm != opjournal.ArmSucceeded {
		s.emitRequestFailedOp(op.ID, operation, messageReplayErrorCode(operation, reason), fmt.Sprintf("op %s closed %s", op.ID, laneOutcomeText(cur)), wire)
		return
	}
	out := messageOutcome{SubmitOutcome: session.SubmitOutcome{Queued: cur.Status != opjournal.StatusClosed}}
	if out.Queued {
		out.willStart, out.willNotStartReason = s.laneWillStart(store, op.Conversation, op.CreatedAt)
	}
	if operation == RequestOperationSessionSubmit {
		s.emitSessionSubmitSucceeded(op.ID, op.Conversation, out, string(submitIntentForWhen(op.Policy.Message.When)), wire)
		return
	}
	s.emitSessionMessageSucceeded(op.ID, op.Conversation, out, wire)
}

// laneSuspendedReason is will_not_start_reason for a lane message waiting
// on a session that only a suspend stamped by the message's proposal holds.
// The lane host's wake request cannot pass it; a resend of the request with
// resume: true consumes it (session.Store.ResumeHeld). The resend names the
// request by the idempotency_key of its 202, the client's or one the server
// minted: under another key it would be a second message.
const laneSuspendedReason = "the session is suspended, so the message waits until it is resumed; resending this request with resume: true under the idempotency_key of its 202 resumes it"

// laneHeldByReason is will_not_start_reason for a lane message waiting on
// holds, named, that a resend of the request with resume: true leaves
// standing.
func laneHeldByReason(holds []string) string {
	return "the session is held by " + strings.Join(holds, " and ") + ", so the message waits until the hold ends; resending this request with resume: true does not lift it (resume lifts only a suspend placed before the message)"
}

// laneSuspendedHeldByReason is will_not_start_reason for a lane message
// waiting on a session that a suspend stamped by the message's proposal
// holds beside other holds, named, that a resend of the request with
// resume: true leaves standing: the resend consumes the suspend, and the
// message then waits until the others end.
func laneSuspendedHeldByReason(holds []string) string {
	return "the session is suspended and held by " + strings.Join(holds, " and ") + "; resending this request with resume: true under the idempotency_key of its 202 resumes it, and the message then waits until the hold ends"
}

// laneDrainingReason is will_not_start_reason for a lane message waiting on
// holds, named, among them a suspend stamped by the message's proposal on a
// session a drain is stopping (session.ErrSessionStopping): the drain's stop
// owns the runtime and refuses a resume, 409 session_stopping, until it is
// done; then a resend of the request with resume: true lifts the suspend
// (session.Store.ResumeHeld), and the message waits until any other hold
// ends.
func laneDrainingReason(holds []string) string {
	return "the session is held by " + strings.Join(holds, " and ") + ", and a drain is stopping it, so the message waits until the hold ends; once the drain is done, resending this request with resume: true under the idempotency_key of its 202 lifts the suspend"
}

// laneHeldReason is will_not_start_reason for a lane message proposed at
// proposed that waits on a held session (info and persisted, its runtime
// reading live), told from the row a resend of the request with resume:
// true would leave (session.ResumeHeldPreview, read as the lane host reads
// the row: laneWakeVerdict): laneSuspendedReason when that row is unheld or
// carries no hold; laneSuspendedHeldByReason when the resend lifts the
// suspend, the one hold a resume lifts, and other holds stand; the row's
// holds and the drain (laneDrainingReason) when a drain stopping the
// session refuses the resend for now, though it lifts the suspend once the
// drain is done; else the holds that resend leaves standing
// (laneHeldByReason): a wait, a quarantine, a heartbeat or a suspend
// stamped after the proposal.
func laneHeldReason(info session.Info, persisted session.PersistedResponse, live bool, proposed, now time.Time) string {
	holds := session.DecodeDisposition(persisted.Metadata).ConsumeHolds(now)
	resumed, after, err := session.ResumeHeldPreview(info, persisted, proposed, now)
	switch {
	case errors.Is(err, session.ErrSessionStopping):
		return laneDrainingReason(holds)
	case err != nil:
		return laneHeldByReason(holds)
	}
	outcome, err := laneWakeVerdict(resumed, after, live, now)
	standing := session.DecodeDisposition(after.Metadata).ConsumeHolds(now)
	switch {
	case err == nil && outcome != session.WakeHeld, len(standing) == 0:
		return laneSuspendedReason
	case len(standing) < len(holds):
		return laneSuspendedHeldByReason(standing)
	}
	return laneHeldByReason(standing)
}

// laneWakeVerdict is session.WakeRequestVerdict on a row (info and
// persisted) as the lane host reads it, its runtime reading live: a dormant
// row (asleep or drained) as not running, as the lane host's wake request
// reads it (requestWake), since a dormant row is never probed; any other row
// with live, as the lane host's content gate reads a probed one
// (session.ContentGate), so a running session an agent's heartbeat keeps up
// is not held.
func laneWakeVerdict(info session.Info, persisted session.PersistedResponse, live bool, now time.Time) (session.WakeRequestOutcome, error) {
	state := session.State(strings.TrimSpace(info.MetadataState))
	dormant := state == session.StateAsleep || state == session.StateDrained
	return session.WakeRequestVerdict(info, persisted, live && !dormant, now)
}

// laneWillStart is will_start and will_not_start_reason for a lane message
// op to session id, proposed at proposed, that is still open once
// laneSubmitSettle passed: deferToController's verdict for a queued
// message, read alone and as the lane host reads the row (laneWakeVerdict).
// The lane host's wake request records the session's wake and asks the
// controller to start it, so nothing is written or poked here. A held
// session will not start (laneHeldReason). One the controller will not
// start says why, with no resume: true to start it, since under the lane
// resume: true starts nothing. A session that runs and nothing holds, whose
// message waits for its boundary, has neither; nor has one that is
// starting, closed, or unreadable (logged), or that the controller may
// start only for demand.
func (s *Server) laneWillStart(store beads.Store, id string, proposed time.Time) (*bool, string) {
	info, persisted, err := session.NewStore(beads.SessionStore{Store: store}).GetPersistedResponse(id)
	if err != nil {
		s.ops.logf("api: reading session %s for a queued message's will_start: %v", id, err)
		return nil, ""
	}
	now := time.Now().UTC()
	live := sessionRuntimeRunning(s.state.SessionProvider(), info.SessionName)
	outcome, err := laneWakeVerdict(info, persisted, live, now)
	no := false
	switch {
	case err != nil, outcome == session.WakeNotDormant:
		return nil, ""
	case outcome == session.WakeHeld:
		return &no, laneHeldReason(info, persisted, live, proposed, now)
	}
	certain := true
	if refuser, ok := s.state.(WakeStartRefuser); ok {
		var why string
		if why, certain = refuser.WakeStartRefusal(info); why != "" {
			return &no, why
		}
	}
	if !certain {
		return nil, ""
	}
	yes := true
	return &yes, ""
}

// checkInitialMessage refuses, before anything is created, the initial
// message of a session create that the lane cannot carry when the city's
// lane host executes message: one over opjournal.MaxInlinePayload bytes is
// 422 payload_too_large, as /submit and /messages refuse it, rather than a
// create rolled back once its message's proposal fails. A create without a
// message, or whose city's lane does not execute message, passes.
func (s *Server) checkInitialMessage(message string) error {
	msg := strings.TrimSpace(message)
	if msg == "" || !s.laneExecutes(opjournal.KindMessage) {
		return nil
	}
	if err := opjournal.ValidatePayload(opjournal.KindMessage, msg); err != nil {
		return proposeError(err)
	}
	return nil
}

// deliverInitialMessage delivers the initial message a session create
// carries to the session it created, id. When the city's lane host executes
// message it proposes the message as an operator lane message op at the
// boundary under a minted key (proposeInitialMessage), so the lane host
// delivers it and the op journal records it; otherwise it submits it through
// the session layer (submitMessageToSession). An error means the message was
// neither proposed nor delivered.
func (s *Server) deliverInitialMessage(ctx context.Context, store beads.Store, id, message string) error {
	if s.laneExecutes(opjournal.KindMessage) {
		return s.proposeInitialMessage(id, message)
	}
	_, err := s.submitMessageToSession(ctx, store, id, message, session.SubmitIntentDefault, false)
	return err
}

// proposeInitialMessage proposes a create's initial message to session id as
// an operator lane message op (LaneMessageOpPolicy of the default intent)
// under a minted key.
func (s *Server) proposeInitialMessage(id, message string) error {
	st, err := s.conversationOpStore()
	if err != nil {
		return err
	}
	key, err := legacyop.MintKey()
	if err != nil {
		return err
	}
	_, _, err = s.proposeLaneOp(st, opjournal.Request{
		Conversation: id,
		Kind:         opjournal.KindMessage,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    key,
		MintedKey:    true,
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:      message,
		Policy:       LaneMessageOpPolicy(session.SubmitIntentDefault),
	})
	return err
}

// proposeExtmsgNotice is an extmsg member notice under lane authority: a lane
// notice from gc:extmsg, delivered at the session's boundary, waking it if
// needed, under a minted key (the notice path carries no message id).
func (s *Server) proposeExtmsgNotice(id, message string) error {
	st, err := s.conversationOpStore()
	if err != nil {
		return err
	}
	key, err := legacyop.MintKey()
	if err != nil {
		return err
	}
	_, _, err = s.proposeLaneOp(st, opjournal.Request{
		Conversation: id,
		Kind:         opjournal.KindMessage,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    key,
		MintedKey:    true,
		Producer:     opjournal.Producer{Principal: extmsgNoticePrincipal, Actor: opjournal.ActorBackground},
		Payload:      message,
		Policy:       opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeIfNeeded}},
	})
	return err
}

// extmsgNoticeCutNote ends the inbound text of an extmsg notice that was cut
// to fit a lane op's payload.
const extmsgNoticeCutNote = " [... cut to fit the notice; the conversation's transcript has the whole message]"

// extmsgNotice is the member notice formatExtmsgNotifyReminder writes for r.
// Under lane authority (message listed) the notice is a lane op, whose
// payload is at most opjournal.MaxInlinePayload bytes: an inbound text too
// long for it is cut at a rune boundary and says so (extmsgNoticeCutNote),
// so the member is still notified. The conversation's transcript keeps the
// whole message.
func (s *Server) extmsgNotice(r extmsgNotifyReminder) string {
	text := formatExtmsgNotifyReminder(r)
	if len(text) <= opjournal.MaxInlinePayload || !s.laneExecutes(opjournal.KindMessage) {
		return text
	}
	// The reminder carries the text once, sanitized, which only deletes:
	// the rest of the reminder is its fixed cost.
	cut := r
	cut.Text = ""
	room := opjournal.MaxInlinePayload - len(formatExtmsgNotifyReminder(cut)) - len(extmsgNoticeCutNote)
	cut.Text = cutUTF8(r.Text, room) + extmsgNoticeCutNote
	return formatExtmsgNotifyReminder(cut)
}

// cutUTF8 is s cut to at most n bytes at a rune boundary.
func cutUTF8(s string, n int) string {
	switch {
	case n <= 0:
		return ""
	case len(s) <= n:
		return s
	}
	for i := 0; i < utf8.UTFMax && n > 0 && !utf8.RuneStart(s[n]); i++ {
		n--
	}
	return s[:n]
}
