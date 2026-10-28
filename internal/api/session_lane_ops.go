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

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
