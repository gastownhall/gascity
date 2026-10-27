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

// awaitLaneOp waits, at most bound, for op id of conv to close, and returns
// it as last read: closed, or still open at the bound. It waits through the
// lane host's AwaitConversationOp, which wakes at the end of each pass over
// conv. A read that fails is 503 op_journal_unavailable, as is a State with
// no lane host, which executes no kind (laneExecutes).
func (s *Server) awaitLaneOp(ctx context.Context, conv, id string, bound time.Duration) (opjournal.Op, error) {
	h, ok := s.state.(ConversationLaneHost)
	if !ok {
		return opjournal.Op{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: awaiting op %s: no conversation lane host runs", id)
	}
	actx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	op, err := h.AwaitConversationOp(actx, conv, id)
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
// named turn keeps OJ-4's front-door checks (a refusal writes nothing) and
// is stopped by its turn's op (stopTurnLane). Without a named turn the op is
// session(generation)-scoped under the request's key, or a minted one, and
// the executor reads the turn itself.
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
		return s.retryLaneStop(ctx, st, id, prior, named)
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
	if named != "" {
		return s.stopTurnLane(ctx, st, id, req, named)
	}
	op, created, err := s.proposeLaneOp(st, req)
	if err != nil {
		return syncAnswer{}, err
	}
	return s.answerLaneStop(ctx, id, op, false, !created)
}

// retryLaneStop answers a /stop whose Idempotency-Key already names a stop,
// prior: an open lane op is awaited, a closed op replayed, and the row a
// named stop left under its key followed to the turn's op. A retry that
// names a turn prior did not stop, a session-scoped stop's included, is
// idempotency_mismatch, as OJ-4's is (stopOpRequest).
func (s *Server) retryLaneStop(ctx context.Context, st *opjournal.Store, id string, prior opjournal.Op, named string) (syncAnswer, error) {
	if named != "" && prior.Scope.Turn != named {
		return syncAnswer{}, apierr.IdempotencyMismatch.Msg(fmt.Sprintf("idempotency_mismatch: Idempotency-Key reused with a different request: it names a stop of turn %q, not turn %q", prior.Scope.Turn, named))
	}
	target, err := followCoalesced(st, prior)
	if err != nil {
		return syncAnswer{}, err
	}
	return s.answerLaneStop(ctx, id, target, target.Scope.Turn != "", true)
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
// into it (recordLaneStopUnderKey). A turn-keyed op that closed without
// success never uses the turn up: this request retries it under its own
// key.
func (s *Server) stopTurnLane(ctx context.Context, st *opjournal.Store, id string, req opjournal.Request, turn string) (syncAnswer, error) {
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
	if op.Status == opjournal.StatusClosed && !laneStopSucceeded(op) {
		// A failed stop of the turn never uses it up: this request retries
		// it under its own key.
		if op, created, err = s.proposeLaneOp(st, req); err != nil {
			return syncAnswer{}, err
		}
		return s.answerLaneStop(ctx, id, op, true, !created)
	}
	if !req.MintedKey {
		if err := recordLaneStopUnderKey(st, req, op.ID); err != nil {
			return syncAnswer{}, err
		}
	}
	return s.answerLaneStop(ctx, id, op, true, !created)
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

// answerLaneStop awaits op, the interrupt a /stop of session id proposed or
// found, and answers from its outcome as OJ-4 answers the same outcome:
// succeeded, or a stop that found nothing to stop, is 200; skipped{stale} is
// 200 for a stop that named no turn (nothing ran) and 409 turn_not_current
// for a named one; failed{unsupported} 501; failed{no_boundary} the legacy
// not-ended answer; provider_unavailable and expired_before_start 503; any
// other failure, or a cancel, 409 op_already_failed; and an op still open
// at laneControlAnswerBound 409 in_flight. A legacy op a key names is
// answered as OJ-4 replays it.
func (s *Server) answerLaneStop(ctx context.Context, id string, op opjournal.Op, named, replayed bool) (syncAnswer, error) {
	if op.Executor != opjournal.ExecutorLane {
		if op.Status != opjournal.StatusClosed {
			return syncAnswer{}, requestInFlight()
		}
		if syncOpReplaysAsSuccess(op) {
			return syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: op.ClientKey, Replayed: true, Op: conversationOpWire(op)}}, nil
		}
		return syncAnswer{}, replayedSyncError(op, humaStopError)
	}
	if op.Status != opjournal.StatusClosed {
		var err error
		if op, err = s.awaitLaneOp(ctx, id, op.ID, laneControlAnswerBound); err != nil {
			return syncAnswer{}, err
		}
		if op.Status != opjournal.StatusClosed {
			return syncAnswer{}, laneOpInFlight(op)
		}
	}
	ok := syncAnswer{ID: id, syncOpFields: syncOpFields{IdempotencyKey: op.ClientKey, Replayed: replayed, Op: conversationOpWire(op)}}
	arm, reason := op.Outcome.Effective()
	closed := func(what string) string {
		return fmt.Sprintf("op %s closed %s{%s}: %s", op.ID, arm, reason, what)
	}
	switch {
	case arm == opjournal.ArmSucceeded:
		return ok, nil
	case arm == opjournal.ArmSkipped && reason == opjournal.ReasonStale && named:
		return syncAnswer{}, humaStopError(fmt.Errorf("%s: %w", closed(fmt.Sprintf("turn %q of session %s was not the current turn when the stop ran", op.Scope.Turn, id)), runtime.ErrTurnNotCurrent))
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

// runCancelRetryKey is the client key of the interrupt that retries missed,
// the id of a holder's run-cancel interrupt in generation gen that closed
// without interrupting it (runCancelInterruptMissed): one retry per missed
// op, which every later cancel of the run finds again.
func runCancelRetryKey(runID string, gen int64, missed string) string {
	return runCancelLaneKey(runID, gen) + ":retry:" + missed
}

// runCancelInterruptMissed reports whether op, a holder's run-cancel
// interrupt, closed without interrupting the holder: failed, canceled, or
// skipped (no turn to stop when it ran).
func runCancelInterruptMissed(op opjournal.Op) bool {
	if op.Status != opjournal.StatusClosed {
		return false
	}
	arm, _ := op.Outcome.Effective()
	return arm != opjournal.ArmSucceeded
}

// interruptRunHoldersLane is interruptRunHolders under lane authority: one
// interrupt op per live holder session ids names
// (proposeRunCancelInterrupt), awaited together at most
// laneControlAnswerBound. A session whose op closed succeeded is
// interrupted; one whose op failed, or is still open at the bound, is a
// failure; one whose op closed skipped (nothing to stop) is neither.
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
		op, err := s.proposeRunCancelInterrupt(st, store, runID, id)
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
			results[i], errs[i] = s.awaitLaneOp(actx, p.id, p.op.ID, laneControlAnswerBound)
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

// proposeRunCancelInterrupt proposes the interrupt a cancel of run runID
// sends holder session id, live now, and returns the op the cancel answers
// from. The holder's ops for the run form a chain per incarnation: the first
// keyed run-cancel:<run id>:<generation> (runCancelLaneKey), with the run as
// its causation, and each op that closed without interrupting the holder
// (runCancelInterruptMissed) followed by its retry (runCancelRetryKey). The
// cancel walks the chain to its first op that did not miss, proposing it
// when no earlier cancel did. So cancels of the run share one open op per
// holder, await it while it is open, answer from it once it closed
// succeeded, and never replay a miss for a holder live now.
func (s *Server) proposeRunCancelInterrupt(st *opjournal.Store, store beads.Store, runID, id string) (opjournal.Op, error) {
	scope, why, whyErr := sessionOpScope(store, id)
	if why != "" {
		return opjournal.Op{}, fmt.Errorf("the operation journal cannot record its interrupt (%s): %w", why, errors.Join(whyErr, errors.New("nothing sent")))
	}
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
		if err != nil || created || !runCancelInterruptMissed(op) {
			return op, err
		}
		req.ClientKey = runCancelRetryKey(runID, scope.Generation, op.ID)
	}
}
