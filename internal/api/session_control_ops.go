package api

// The API's O0 shadow ops on the synchronous session commands (DES-O0API §7;
// OJ-4): POST .../session/{id}/stop and .../respond under the ops.control
// gate, and .../wake, .../suspend, .../kill, .../close and .../reset under
// ops.lifecycle.
//
// Each request is recorded as an op before the legacy call runs and closed
// from its result after it, through the legacyop front door; the answer
// carries the op, the key it was recorded under and replayed. Off records
// nothing and answers as before. The Idempotency-Key LRU stays in front; the
// op row backs it, so a completed command replays from its stored outcome
// across an API restart instead of acting again. Unlike the content verbs, a
// sync command caches no in-flight answer: a same-key request while the
// first runs is answered 409 in_flight. A retryable error leaves a
// client-keyed op open, so a same-key retry before its finish_by adopts it
// and runs the command again, as the LRU's "an error releases the key" does;
// a minted-key op closes failed{refused:...} (D10).
//
// /stop resolves the turn it interrupts in X11 D10's order: the turn the
// request names (its optional turn field), then, while ops.control records
// and the session's turns come from its runtime's protocol (t3bridge, ACP),
// a live read of the session's turn through the worker (TI-2b). A known turn
// is interrupted turn-scoped, and only while it is current. On a protocol
// turn source a stop never sends an unfenced interrupt (DES-T3I §14 item 3):
// no turn running is nothing to stop, and a turn source that cannot be read
// is refused, nothing sent. Every other runtime (tmux included) keeps the
// session-scoped legacy stop, and so does a request auto could not record
// (I8). A turn the request names is the op's target key,
// stop:<generation>:<turn>, so two stops of one turn are one op; so is a
// resolved turn when the request carries no Idempotency-Key. A target key
// replays only a stop that succeeded or found nothing to do: one whose op
// failed is retried under the request's own key (minted when it has none),
// so a failed stop never uses up its turn. A named stop that the target
// key's op answers leaves a row under its own Idempotency-Key that coalesced
// into that op, before it sends anything, so a retry under the key follows
// the row back to that op, as a stop of the turn it named, however the stop
// ended; a refusal of the row is the stop's answer, nothing sent, except that
// in auto a row its store does not record leaves the key naming nothing and
// the stop goes on. A request that carries a key otherwise keeps it, and a
// retry under a key that names a stop reuses its first request's scope, so
// the retry adopts or replays that stop instead of interrupting a later turn
// (a retry that names another turn is idempotency_mismatch). A retry's reads
// of the session's turns refuse only an adopted stop, which runs: a closed
// one replays whatever they read now. A turn the request did not name that
// ends before the interrupt lands was nothing to stop; only a named one is
// refused turn_not_current.
//
// A recorded /respond answers only the interaction its op names: the
// request's request_id, or the interaction it read pending, or its first
// request's on a same-key retry.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// stopTurnReadBound bounds /stop's live read of the session's turn
// (DES-T3I D6's per-read bound).
const stopTurnReadBound = 10 * time.Second

// Error codes the session commands' turn-scoped stop answers with.
const (
	turnNotCurrentCode          = "turn_not_current"
	turnIdentityUnsupportedCode = "turn_identity_unsupported"
	opAlreadyFailedCode         = "op_already_failed"
)

// Refusal reasons the session commands close an op with, after refused:.
const (
	refusedSessionStarting = "session_starting"
	refusedWillNotStart    = "will_not_start"
	// refusedTargetMoved closes a kill whose session moved to another
	// generation than the one its op is scoped to, as gc session kill does.
	refusedTargetMoved = "target_moved"
	// refusedWakeConflict stands in for a wake conflict state that leaves
	// no token characters.
	refusedWakeConflict = "wake_conflict"
)

// errWakeWillNotStart classifies a wake the controller will not act on.
var errWakeWillNotStart = errors.New("wake will not start")

// wakeWillNotStartError is a recorded wake the controller will not start;
// message is the wire detail.
type wakeWillNotStartError struct{ message string }

// Error is the wire detail.
func (e *wakeWillNotStartError) Error() string { return e.message }

// Unwrap classifies it as errWakeWillNotStart.
func (e *wakeWillNotStartError) Unwrap() error { return errWakeWillNotStart }

// syncVerbSentinels maps the session commands' typed errors onto op outcomes
// (DES-O0API §7.2).
var syncVerbSentinels = legacyop.Sentinels{
	Unsupported: []error{session.ErrTurnInterruptUnsupported, session.ErrInteractionUnsupported, runtime.ErrTurnIdentityUnsupported},
	Stale:       []error{session.ErrNoPendingInteraction, session.ErrInteractionMismatch, runtime.ErrTurnNotCurrent},
	NotNeeded:   []error{session.ErrSessionClosed, session.ErrSessionInactive},
	NotASession: []error{session.ErrNotSession, session.ErrNotSessionBead},
	NoBoundary:  []error{runtime.ErrTurnNotEnded},
	ProviderUnavailable: []error{
		runtime.ErrRuntimeUnavailable,
	},
	Refusals: []legacyop.Refusal{
		{Err: session.ErrSessionStarting, Why: refusedSessionStarting},
		{Err: session.ErrRuntimeLeaseBusy, Why: refusedSessionStarting},
		{Err: session.ErrSessionStopping, Why: "session_stopping"},
		{Err: session.ErrSessionKillPending, Why: "kill_pending"},
		{Err: session.ErrIllegalTransition, Why: "illegal_transition"},
		{Err: errWakeWillNotStart, Why: refusedWillNotStart},
	},
}

// replaySentinels rebuilds a replayed op's refusal from its refused:<why>
// reason (DES-O0API §7.3).
var replaySentinels = map[string]error{
	legacyop.RefusedNotASession: session.ErrNotSession,
	refusedSessionStarting:      session.ErrSessionStarting,
	"session_stopping":          session.ErrSessionStopping,
	"kill_pending":              session.ErrSessionKillPending,
	"illegal_transition":        session.ErrIllegalTransition,
}

// syncOpFields are the op fields a recorded session command's answer
// carries; all empty when the command was not recorded.
type syncOpFields struct {
	IdempotencyKey string
	Replayed       bool
	Op             *ConversationOp
}

// syncAnswer is a session command's answer: the session it acted on and its
// op fields. The Idempotency-Key LRU caches it.
type syncAnswer struct {
	ID string
	syncOpFields
}

// okWithID is the OKWithIDResponse of answer a.
func (a syncAnswer) okWithID() *OKWithIDResponse {
	out := &OKWithIDResponse{}
	out.Body.Status = "ok"
	out.Body.ID = a.ID
	out.Body.IdempotencyKey = a.IdempotencyKey
	out.Body.Replayed = a.Replayed
	out.Body.Op = a.Op
	return out
}

// statusBody is the SessionCommandStatusBody of answer a.
func (a syncAnswer) statusBody() SessionCommandStatusBody {
	return SessionCommandStatusBody{Status: "ok", IdempotencyKey: a.IdempotencyKey, Replayed: a.Replayed, Op: a.Op}
}

// syncOpRequest is one session command's request to record.
type syncOpRequest struct {
	kind opjournal.Kind
	conv string
	// key is the op's client key: the Idempotency-Key header, a target key
	// (stop:<generation>:<turn>), or "" for the recorder to mint one.
	key string
	// turnKeyed says key is a target key; successorKey is then the key the
	// request is recorded under instead when the target key's op closed
	// without success (the Idempotency-Key header, "" to mint one).
	turnKeyed    bool
	successorKey string
	scope        opjournal.Scope
	policy       opjournal.Policy
	payload      string
	// unrecordable, when set, is the legacyop.Reason* the request cannot be
	// recorded for (a session row with no generation, or one that did not
	// read): auto runs it unrecorded, require refuses it.
	unrecordable    string
	unrecordableErr error
}

// recorderFor is the recorder of kind's ops gate and that gate's key.
func (o *serverOps) recorderFor(kind opjournal.Kind) (*legacyop.Recorder, string) {
	switch kind {
	case opjournal.KindInterrupt, opjournal.KindRespond:
		return o.control, rollout.KeyOpsControl
	}
	return o.lifecycle, rollout.KeyOpsLifecycle
}

// recordingMode reports whether mode records ops at all.
func recordingMode(mode gate.Mode) bool { return mode == gate.Auto || mode == gate.Require }

// checkBeforeEffect runs, in require, the refusals the recorder of kind makes
// before it reads the conversation (an invalid key, a store that does not
// open), so a command that materializes a configured named session first
// (wake, suspend) refuses those before any write. Other modes check nothing.
func (s *Server) checkBeforeEffect(kind opjournal.Kind, key string) error {
	rec, _ := s.ops.recorderFor(kind)
	if err := rec.Check(legacyop.Request{Key: key, Kind: kind}); err != nil {
		return opAdmitError(err, key)
	}
	return nil
}

// admitSyncOp admits req under its kind's gate (DES-O0API §7.1 step 4).
func (s *Server) admitSyncOp(req syncOpRequest) (*legacyop.Admission, error) {
	rec, gateKey := s.ops.recorderFor(req.kind)
	if req.unrecordable != "" && recordingMode(rec.Mode) {
		if rec.Mode == gate.Require {
			rec.Refused(req.kind, req.unrecordable)
			cause := req.unrecordable
			if req.unrecordableErr != nil {
				cause += ": " + req.unrecordableErr.Error()
			}
			return nil, apierr.OpJournalUnavailable.Msg(fmt.Sprintf("op_journal_unavailable: the operation journal cannot record this request (%s); nothing was done", cause))
		}
		return rec.Unrecorded(req.kind, req.key, req.unrecordable), nil
	}
	adm, err := rec.Admit(legacyop.Request{
		Conversation: req.conv,
		Principal:    legacyop.PrincipalOperator,
		Actor:        opjournal.ActorOperator,
		Key:          req.key,
		Payload:      req.payload,
		Kind:         req.kind,
		Policy:       req.policy,
		Scope:        req.scope,
	})
	if err != nil {
		return nil, opAdmitError(err, req.key)
	}
	if adm.Recorded {
		s.ops.healthyGate(gateKey, legacyop.StageOpen, legacyop.StagePropose)
		if adm.Adopted {
			s.ops.healthyGate(gateKey, legacyop.StageAdopt)
		}
	}
	return adm, nil
}

// runSyncOp records req, runs the legacy command through run unless the op
// already finished, and closes the op from run's result, before the answer
// (DES-O0API §7.1). run gets the admission (whether it recorded, whether the
// recorder minted the op's key, the op) and returns the op result and the
// wire error the command answers with. A same-key retry of a finished op is
// answered from the stored outcome without running anything; its refusals
// are rebuilt through mapErr. A target key's op that closed without success
// is not replayed: the request is admitted again under its successor key, so
// a failed stop of a turn does not refuse every later stop of it.
func (s *Server) runSyncOp(req syncOpRequest, mapErr func(error) error, run func(adm *legacyop.Admission) (legacyop.Result, error)) (syncOpFields, error) {
	adm, err := s.admitSyncOp(req)
	if err != nil {
		return syncOpFields{}, err
	}
	if req.turnKeyed && adm.Decision == legacyop.Replay && adm.Cached == nil &&
		adm.Op.Status == opjournal.StatusClosed && !syncOpReplaysAsSuccess(adm.Op) {
		req.key, req.turnKeyed = req.successorKey, false
		if adm, err = s.admitSyncOp(req); err != nil {
			return syncOpFields{}, err
		}
	}
	if adm.Decision == legacyop.Replay {
		return s.syncReplay(adm, mapErr)
	}
	handedOff := false
	defer releaseOnPanic(adm, &handedOff)
	res, runErr := run(adm)
	return s.finishSyncOp(adm, res), runErr
}

// finishSyncOp closes adm from res (or leaves it open, res.LeaveOpen) and
// returns the answer's op fields: the closed op, or the open one as stored
// now.
func (s *Server) finishSyncOp(adm *legacyop.Admission, res legacyop.Result) syncOpFields {
	f := adm.Finish(res)
	if !adm.Recorded {
		return syncOpFields{}
	}
	fields := syncOpFields{IdempotencyKey: adm.Key}
	if f.Closed {
		fields.Op = conversationOpWire(f.Op)
	} else {
		fields.Op = s.currentOpWire(adm.Op)
	}
	return fields
}

// syncOpReplaysAsSuccess reports whether a closed op answers a same-key retry
// with the command's success: it succeeded, or a kill or a stop found
// nothing to do.
func syncOpReplaysAsSuccess(op opjournal.Op) bool {
	arm, reason := op.Outcome.Effective()
	if arm == opjournal.ArmSucceeded {
		return true
	}
	return arm == opjournal.ArmSkipped && reason == opjournal.ReasonNotNeeded &&
		(op.Kind == opjournal.KindKill || op.Kind == opjournal.KindInterrupt)
}

// syncReplay answers a same-key retry of a finished op (DES-O0API §7.3): the
// command's success with replayed and the stored op, or the error the
// original produced, rebuilt from the op's reason and mapped through mapErr
// with its message naming the op; one the reason cannot rebuild is 409
// op_already_failed. An op that coalesced into another (the row a named stop
// leaves under its own key) replays that op under that op's key, as the
// first request's answer carried them.
func (s *Server) syncReplay(adm *legacyop.Admission, mapErr func(error) error) (syncOpFields, error) {
	if adm.Cached != nil || adm.Op.Status != opjournal.StatusClosed {
		// A sync command caches no in-flight answer; a held op is busy.
		return syncOpFields{}, requestInFlight()
	}
	op, key := adm.Op, adm.Key
	if into := op.Outcome.Result.CoalescedInto; into != "" {
		target, err := s.coalescedOp(op.Kind, into)
		if err != nil {
			return syncOpFields{}, err
		}
		if target.Status != opjournal.StatusClosed {
			return syncOpFields{}, requestInFlight()
		}
		op, key = target, target.ClientKey
	}
	if syncOpReplaysAsSuccess(op) {
		return syncOpFields{IdempotencyKey: key, Replayed: true, Op: conversationOpWire(op)}, nil
	}
	return syncOpFields{}, replayedSyncError(op, mapErr)
}

// coalescedOp reads op id, the op a replayed op of kind coalesced into; a
// read that fails is 503 op_journal_unavailable, nothing done.
func (s *Server) coalescedOp(kind opjournal.Kind, id string) (opjournal.Op, error) {
	rec, _ := s.ops.recorderFor(kind)
	st, err := rec.Open()
	if err == nil {
		var op opjournal.Op
		if op, err = st.Get(id); err == nil {
			return op, nil
		}
	}
	return opjournal.Op{}, apierr.OpJournalUnavailable.Msg(fmt.Sprintf("op_journal_unavailable: reading op %s, which the replayed op coalesced into, so nothing was done: %v", id, err))
}

// replayedSyncError rebuilds the error a closed, unsuccessful op answered
// with, its message prefixed with the op it replays.
func replayedSyncError(op opjournal.Op, mapErr func(error) error) error {
	arm, reason := op.Outcome.Effective()
	replayed := func(err error) error { return mapErr(fmt.Errorf("replayed op %s: %w", op.ID, err)) }
	switch {
	case reason == opjournal.ReasonUnsupported:
		switch op.Kind {
		case opjournal.KindRespond:
			return replayed(session.ErrInteractionUnsupported)
		case opjournal.KindInterrupt:
			if op.Scope.Turn != "" {
				return replayed(runtime.ErrTurnIdentityUnsupported)
			}
		}
		return replayed(session.ErrTurnInterruptUnsupported)
	case reason == opjournal.ReasonStale && op.Kind == opjournal.KindInterrupt:
		// The stored outcome says the turn was not current when the op ran,
		// not which turn ran then or runs now.
		return replayed(fmt.Errorf("session %q: turn %q was not the current turn when the stop ran: %w", op.Conversation, op.Scope.Turn, runtime.ErrTurnNotCurrent))
	case reason == opjournal.ReasonStale:
		return replayed(session.ErrNoPendingInteraction)
	case strings.HasPrefix(reason, opjournal.RefusedReasonPrefix):
		why := strings.TrimPrefix(reason, opjournal.RefusedReasonPrefix)
		if sentinel, ok := replaySentinels[why]; ok {
			return replayed(sentinel)
		}
		switch {
		case op.Kind == opjournal.KindKill && why == refusedTargetMoved:
			return apierr.SessionConflict.Msg(fmt.Sprintf("%s: replayed op %s: session %s had moved past generation %d, which the kill was recorded for, so nothing was killed; resend with a new Idempotency-Key to kill the new run", refusedTargetMoved, op.ID, op.Conversation, op.Scope.Generation))
		case op.Kind == opjournal.KindWake && why == refusedWillNotStart:
			return apierr.WakeWillNotStart.Msg(fmt.Sprintf("replayed op %s: the wake was recorded for session %s, but it will not start", op.ID, op.Conversation))
		case op.Kind == opjournal.KindWake:
			return apierr.SessionConflict.Msg(fmt.Sprintf("replayed op %s: session %s was %s", op.ID, op.Conversation, why))
		}
	}
	text := string(arm)
	if reason != "" {
		text += "{" + reason + "}"
	}
	return apierr.OpAlreadyFailed.Msg(fmt.Sprintf("%s: op %s closed %s; resend with a new Idempotency-Key to act again", opAlreadyFailedCode, op.ID, text))
}

// syncOpResult is the op result of a session command's legacy error (nil is
// success), for a key the recorder minted or not.
func syncOpResult(kind opjournal.Kind, err error, minted bool) legacyop.Result {
	return syncVerbSentinels.SyncResult(kind, err, minted)
}

// wakeOpResult is a wake's op result: a wake conflict state is a retryable
// refused:<state>, the rest as for every command.
func wakeOpResult(err error, minted bool) legacyop.Result {
	if state, conflict := session.WakeConflictState(err); conflict {
		return legacyop.Result{
			Outcome:   opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.RefusedReasonPrefix + refusedToken(state)},
			LeaveOpen: !minted,
		}
	}
	return syncOpResult(opjournal.KindWake, err, minted)
}

// refusedToken turns a state into a refused reason token: lowercase, with
// anything outside [a-z0-9_] as '_', cut to fit a token, and wake_conflict
// when nothing is left.
func refusedToken(state string) string {
	why := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		default:
			return '_'
		}
	}, state)
	if why == "" {
		return refusedWakeConflict
	}
	if len(why) > opjournal.MaxRefusedWhyLen {
		why = why[:opjournal.MaxRefusedWhyLen]
	}
	return why
}

// sessionOpScope is the session(generation) scope of a command on session id,
// read from its row, and "" or the unrecordable reason when it has none: a
// row that did not read is unreadable_target, with its read error; one that
// read without a generation is no_generation.
func sessionOpScope(store beads.Store, id string) (opjournal.Scope, string, error) {
	info, err := session.NewStore(beads.SessionStore{Store: store}).Get(id)
	if err != nil {
		return opjournal.Scope{}, legacyop.ReasonUnreadableTarget, err
	}
	gen, err := strconv.ParseInt(strings.TrimSpace(info.Generation), 10, 64)
	if err != nil || gen < 1 {
		return opjournal.Scope{}, legacyop.ReasonNoGeneration, nil
	}
	return opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen}, "", nil
}

// keyedOp reads the op an operator's client key already names on conv: the
// op a same-key admission answers from. found is false when key is empty or
// names no op; a failed read is reported as none, and the admission that
// follows meets it again and reports it.
func (s *Server) keyedOp(kind opjournal.Kind, conv, key string) (opjournal.Op, bool) {
	rec, _ := s.ops.recorderFor(kind)
	if key == "" || !recordingMode(rec.Mode) {
		return opjournal.Op{}, false
	}
	st, err := rec.Open()
	if err != nil {
		return opjournal.Op{}, false
	}
	id, err := opjournal.DeriveID(st.Prefix(), conv, legacyop.PrincipalOperator, key)
	if err != nil {
		return opjournal.Op{}, false
	}
	op, err := st.Get(id)
	if err != nil {
		return opjournal.Op{}, false
	}
	return op, true
}

// priorSessionScope is the session scope of the op a client key already
// names for kind on conv, when that op is of kind and session-scoped: a
// same-key retry reuses it, so it finds its first request again although the
// session moved to another generation or turn since. ok is false otherwise.
func (s *Server) priorSessionScope(kind opjournal.Kind, conv, key string) (opjournal.Scope, bool) {
	prior, ok := s.priorSessionOp(kind, conv, key)
	return prior.Scope, ok
}

// priorSessionOp is the op whose scope priorSessionScope reuses.
func (s *Server) priorSessionOp(kind opjournal.Kind, conv, key string) (opjournal.Op, bool) {
	prior, found := s.keyedOp(kind, conv, key)
	if !found || prior.Kind != kind || prior.Scope.Kind != opjournal.ScopeSession {
		return opjournal.Op{}, false
	}
	return prior, true
}

// --- /stop ---

// stopPlan is how a /stop interrupts its session.
type stopPlan struct {
	// turn, when set, is interrupted turn-scoped through handle, whose turn
	// capabilities are caps. named says the request named it: a turn that is
	// no longer current is then refused turn_not_current; a turn read live
	// (or reused from the op a same-key retry adopts, which read it live)
	// that ended was nothing to stop.
	turn   string
	named  bool
	handle worker.Handle
	caps   worker.TurnCapabilities
	// noTurn says a protocol turn source read no turn running: nothing to
	// stop.
	noTurn bool
	// unreadable is a turn source that could not be read: refuse, nothing
	// sent.
	unreadable error
}

// interruptOpPolicy is a /stop's interrupt policy (DES-O0API §3.1).
var interruptOpPolicy = opjournal.Policy{Interrupt: opjournal.InterruptPolicy{OnFailure: opjournal.OnFailureFail}}

// humaStopError maps a /stop error onto the wire: the turn-scoped refusals
// first, the rest as every session command maps them.
func humaStopError(err error) error {
	switch {
	case errors.Is(err, runtime.ErrTurnNotCurrent):
		return apierr.SessionConflict.Msg(turnNotCurrentCode + ": " + err.Error())
	case errors.Is(err, runtime.ErrTurnIdentityUnsupported):
		return apierr.NotImplemented.Msg(turnIdentityUnsupportedCode + ": " + err.Error())
	}
	return humaSessionManagerError(err)
}

// stopSession stops target's turn, recorded as an interrupt op under
// ops.control, and returns the answer. key is the Idempotency-Key header and
// named the turn the request names, "" for none. When the city's lane host
// executes interrupt, the stop is a lane op instead (stopSessionLane), and
// ops.control does not govern it.
func (s *Server) stopSession(ctx context.Context, target, key, named string) (syncAnswer, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return syncAnswer{}, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	id, err := s.resolveSessionIDWithConfig(store.Store, target)
	if err != nil {
		return syncAnswer{}, humaResolveError(err)
	}
	if s.laneExecutes(opjournal.KindInterrupt) {
		return s.stopSessionLane(ctx, store.Store, id, key, named)
	}
	// A key that already names a stop (its own op, or the row a stop of a
	// turn's target key left under it) makes this request its retry.
	prior, retry := s.priorSessionOp(opjournal.KindInterrupt, id, key)
	plan, err := s.frontDoorStopPlan(ctx, store.Store, id, named, prior, retry)
	if err != nil {
		return syncAnswer{}, err
	}
	req := syncOpRequest{kind: opjournal.KindInterrupt, conv: id, key: key, policy: interruptOpPolicy}
	if recordingMode(s.ops.control.Mode) {
		// The client key is the op's key, or a target key's successor: in
		// require it is checked even when a target key stands in for it.
		if err := s.checkBeforeEffect(opjournal.KindInterrupt, key); err != nil {
			return syncAnswer{}, err
		}
		if req, plan, err = s.stopOpRequest(ctx, store.Store, req, plan, prior, retry); err != nil {
			return syncAnswer{}, err
		}
	}
	fields, err := s.runSyncOp(req, humaStopError, func(adm *legacyop.Admission) (legacyop.Result, error) {
		return s.runStop(store.Store, id, req, plan, adm)
	})
	if err != nil {
		return syncAnswer{}, err
	}
	if fields.Replayed && fields.Op != nil {
		if err := s.recordStopUnderKey(req, fields.IdempotencyKey, fields.Op.ID); err != nil {
			return syncAnswer{}, err
		}
	}
	return syncAnswer{ID: id, syncOpFields: fields}, nil
}

// runStop runs a /stop admitted as adm on session id: a recorded stop first
// writes the row under its own key (recordStopUnderKey) and sends nothing
// when that is refused, leaving the op open, as a retryable error does, for
// the next stop of its turn; one auto could not record runs as in off (I8),
// unless it names its turn.
func (s *Server) runStop(store beads.Store, id string, req syncOpRequest, plan stopPlan, adm *legacyop.Admission) (legacyop.Result, error) {
	if !adm.Recorded {
		if !plan.named {
			plan = stopPlan{}
		}
		return s.executeStop(store, id, plan, adm.Minted)
	}
	if err := s.recordStopUnderKey(req, adm.Key, adm.Op.ID); err != nil {
		return syncOpResult(opjournal.KindInterrupt, err, adm.Minted), err
	}
	return s.executeStop(store, id, plan, adm.Minted)
}

// recordStopUnderKey records, under a stop's own Idempotency-Key, that its
// turn's target-key op answers it (op target, admitted under answeredKey,
// req's target key): an interrupt op of the same scope, closed
// skipped{coalesced_into: that op}. A stop that runs the op records it before
// it sends anything, so the key names the stop however the stop ends
// (succeeded, failed, refused turn_not_current, or its API died); one
// answered from the op's outcome records it after. A later request under the
// key then finds the stop once the Idempotency-Key cache is gone:
// stopOpRequest follows the row back to the target key, so a retry with no
// turn, or naming the same one, adopts the op while it is open and replays it
// once closed instead of stopping the turn running then, and one that names
// another turn is idempotency_mismatch. It records nothing for a request
// without a key of its own, one its own key's op answers, or one whose key
// already names an op (a retry that followed its row). The row is admitted
// as the request would be under its key, and a refusal (require's
// op_journal_unavailable, in_flight, idempotency_mismatch) is returned for
// the stop to answer with: a stop that runs the op then sends nothing, and
// one answered from it answers no success. In auto a row the op store does
// not record is logged and the stop goes on, its key naming nothing, as an
// unrecorded request's (I8).
func (s *Server) recordStopUnderKey(req syncOpRequest, answeredKey, target string) error {
	if !req.turnKeyed || req.successorKey == "" || answeredKey != req.key {
		return nil
	}
	req.key, req.turnKeyed, req.successorKey = req.successorKey, false, ""
	adm, err := s.admitSyncOp(req)
	switch {
	case err != nil:
		return err
	case !adm.Recorded:
		s.ops.logf("api: stop of session %s: its Idempotency-Key was not recorded for op %s, so it names nothing; auto stops all the same", req.conv, target)
		return nil
	case adm.Decision == legacyop.Replay:
		return nil
	case adm.Adopted:
		// Another request recorded an op under the key meanwhile: leave it to
		// its own request.
		adm.Finish(legacyop.Result{LeaveOpen: true})
		return nil
	}
	adm.Finish(legacyop.Result{Outcome: opjournal.Outcome{
		Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonCoalescedInto, Result: opjournal.Result{CoalescedInto: target},
	}})
	return nil
}

// frontDoorStopPlan is a /stop's plan at the front door: none for a request
// that names no turn, and for a named turn namedStopPlan's checks, unless
// the request's key already names a stop of that turn (prior, retry): the
// retry replays or adopts that stop, so it takes keyedStopPlan, which
// refuses nothing.
func (s *Server) frontDoorStopPlan(ctx context.Context, store beads.Store, id, named string, prior opjournal.Op, retry bool) (stopPlan, error) {
	switch {
	case named == "":
		return stopPlan{}, nil
	case retry && prior.Scope.Turn == named:
		return s.keyedStopPlan(ctx, store, id, named), nil
	}
	return s.namedStopPlan(ctx, store, id, named)
}

// namedStopPlan checks a turn the request names at the front door (X11
// D10): a valid turn token, on a session whose runtime records turn starts.
// A refusal records no op.
func (s *Server) namedStopPlan(ctx context.Context, store beads.Store, id, turn string) (stopPlan, error) {
	if _, err := opjournal.InterruptKey(1, turn); err != nil {
		return stopPlan{}, apierr.ValidationFailed.Msg("invalid_turn: " + err.Error())
	}
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		return stopPlan{}, humaSessionManagerError(err)
	}
	caps, err := handle.TurnCapabilities(ctx)
	if err != nil {
		return stopPlan{}, humaStopError(err)
	}
	if caps.Start != runtime.TurnStartRecorded {
		return stopPlan{}, humaStopError(fmt.Errorf("session %s: its runtime does not record turn starts, so it cannot stop turn %q: %w", id, turn, runtime.ErrTurnIdentityUnsupported))
	}
	return stopPlan{turn: turn, named: true, handle: handle, caps: caps}, nil
}

// stopOpRequest completes a recorded /stop's op request and plan (X11 D10,
// DES-O0API §3.2): the session(generation) scope; when the client key already
// names a stop (prior, retry), that stop's scope (a named turn must be that
// stop's, or the request is idempotency_mismatch), under the key, or, when
// the key names the row a stop of a turn's target key left under it, under
// that target key as a stop of the turn it names; else a named turn in the
// scope, keyed stop:<generation>:<turn>; else the turn a live read of a
// protocol turn source finds running, keyed by it when the request carries
// no key. A target key's successor is the request's own key. A request that
// cannot be recorded resolves nothing: it runs as in off.
func (s *Server) stopOpRequest(ctx context.Context, store beads.Store, req syncOpRequest, plan stopPlan, prior opjournal.Op, retry bool) (syncOpRequest, stopPlan, error) {
	scope, why, whyErr := sessionOpScope(store, req.conv)
	if why != "" {
		req.unrecordable, req.unrecordableErr = why, whyErr
		return req, plan, nil
	}
	clientKey := req.key
	switch {
	case retry && plan.named && prior.Scope.Turn != plan.turn:
		return req, plan, apierr.IdempotencyMismatch.Msg(fmt.Sprintf("idempotency_mismatch: Idempotency-Key reused with a different request: it names a stop of turn %q, not turn %q", prior.Scope.Turn, plan.turn))
	case retry && prior.Outcome.Result.CoalescedInto != "":
		// The row is a named stop's (recordStopUnderKey): the retry is that
		// stop again, of the turn it named, under that turn's target key,
		// whose op it adopts while open and replays once closed, whether
		// the retry names the turn or not.
		if !plan.named {
			plan = s.keyedStopPlan(ctx, store, req.conv, prior.Scope.Turn)
		}
		scope = prior.Scope
		req.key = stopTargetKey(scope.Generation, scope.Turn, req.key)
	case retry && plan.named:
		scope = prior.Scope
	case retry:
		// The scope must be the first request's for the retry to find its
		// op; a session-scoped one's turn is read again, so an adopted stop
		// is fenced as a fresh one is.
		scope = prior.Scope
		if scope.Turn != "" {
			plan = s.priorStopPlan(ctx, store, req.conv, scope.Turn)
		} else {
			plan = s.resolveStopTurn(ctx, store, req.conv)
		}
	case plan.named:
		scope.Turn = plan.turn
		req.key = stopTargetKey(scope.Generation, plan.turn, req.key)
	default:
		plan = s.resolveStopTurn(ctx, store, req.conv)
		if plan.turn != "" {
			scope.Turn = plan.turn
			if req.key == "" {
				req.key = stopTargetKey(scope.Generation, plan.turn, "")
			}
		}
	}
	if req.key != clientKey {
		req.turnKeyed, req.successorKey = true, clientKey
	}
	req.scope = scope
	return req, plan, nil
}

// stopTargetKey is the target key of a stop of turn in generation gen,
// stop:<gen>:<turn>, or fallback when the pair makes no valid key.
func stopTargetKey(gen int64, turn, fallback string) string {
	key, err := opjournal.InterruptKey(gen, turn)
	if err != nil {
		return fallback
	}
	return key
}

// priorStopPlan is the plan of a same-key retry of a stop scoped to turn: it
// interrupts that turn only, as the request that resolved it did. That
// request read the turn live, since a named turn is always target-keyed, so
// the retry is not named either: a turn that ended meanwhile (the first
// attempt's interrupt landed before it died) was nothing to stop.
func (s *Server) priorStopPlan(ctx context.Context, store beads.Store, id, turn string) stopPlan {
	plan := stopPlan{turn: turn}
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		plan.unreadable = fmt.Errorf("reading the turns of session %s before stopping turn %q, nothing sent: %w", id, turn, err)
		return plan
	}
	caps, err := handle.TurnCapabilities(ctx)
	if err != nil {
		plan.unreadable = fmt.Errorf("reading the turns of session %s before stopping turn %q, nothing sent: %w", id, turn, err)
		return plan
	}
	plan.handle, plan.caps = handle, caps
	return plan
}

// keyedStopPlan is the plan of a retry whose Idempotency-Key already names a
// stop of turn, as a stop of a named turn: priorStopPlan's, named. Its read
// of the turn source refuses nothing here: one that does not read refuses
// only an adopted op, which runs, nothing sent, so a closed stop the key
// names replays whatever the session's turns read now (closed, or a runtime
// that does not answer).
func (s *Server) keyedStopPlan(ctx context.Context, store beads.Store, id, turn string) stopPlan {
	plan := s.priorStopPlan(ctx, store, id, turn)
	plan.named = true
	return plan
}

// resolveStopTurn reads the turn running on session id now (X11 D10 step 2,
// TI-2b's live read), within stopTurnReadBound, when its turns come from its
// runtime's protocol. A running turn is stopped turn-scoped, no turn running
// is nothing to stop, and a session whose turns cannot be read is refused,
// nothing sent: a protocol turn source never gets the unfenced session-scoped
// stop (DES-T3I §14 item 3). A session whose turn source cannot be told is
// refused the same way. Every other turn source keeps the legacy
// session-scoped stop, unread.
func (s *Server) resolveStopTurn(ctx context.Context, store beads.Store, id string) stopPlan {
	unreadable := func(err error) stopPlan {
		return stopPlan{unreadable: fmt.Errorf("reading the turn of session %s before stopping it, nothing sent: %w", id, err)}
	}
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		return unreadable(err)
	}
	caps, err := handle.TurnCapabilities(ctx)
	if err != nil {
		return unreadable(err)
	}
	if caps.Source != worker.TurnSourceProtocol {
		return stopPlan{}
	}
	readCtx, cancel := context.WithTimeout(ctx, stopTurnReadBound)
	defer cancel()
	hist, err := handle.CurrentTurn(readCtx)
	switch {
	case err == nil && hist.Current.Active():
		return stopPlan{turn: hist.Current.TurnID, handle: handle, caps: caps}
	case err == nil, errors.Is(err, runtime.ErrSessionNotFound) && !errors.Is(err, runtime.ErrRuntimeUnavailable):
		// No turn running, or no runtime session bound (DES-T3I D2).
		return stopPlan{noTurn: true}
	case errors.Is(err, runtime.ErrRuntimeUnavailable), errors.Is(err, runtime.ErrTurnIdentityUnsupported):
		return unreadable(err)
	default:
		return unreadable(fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err))
	}
}

// executeStop runs plan on session id and returns the op result and the
// wire error. A turn-scoped stop that succeeds is unfenced unless the
// runtime fences turns atomically; the session-scoped stop always is.
func (s *Server) executeStop(store beads.Store, id string, plan stopPlan, minted bool) (legacyop.Result, error) {
	switch {
	case plan.noTurn:
		return legacyop.Result{Outcome: opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonNotNeeded}}, nil
	case plan.unreadable != nil:
		return syncOpResult(opjournal.KindInterrupt, plan.unreadable, minted), humaStopError(plan.unreadable)
	case plan.turn != "":
		err := plan.handle.Interrupt(context.Background(), worker.InterruptRequest{Turn: plan.turn})
		switch {
		case err == nil:
			res := syncOpResult(opjournal.KindInterrupt, nil, minted)
			if plan.caps.Fence != runtime.TurnFenceAtomic {
				res.Outcome.Degradation = session.DegradationUnfencedInterrupt
			}
			return res, nil
		case !plan.named && errors.Is(err, runtime.ErrTurnNotCurrent):
			// The turn the stop read ended on its own before the interrupt
			// landed (nothing was sent): nothing to stop, as for an idle
			// session.
			return legacyop.Result{Outcome: opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonNotNeeded}}, nil
		default:
			return syncOpResult(opjournal.KindInterrupt, err, minted), humaStopError(err)
		}
	}
	if err := s.sessionManager(store).StopTurn(id); err != nil {
		return syncOpResult(opjournal.KindInterrupt, err, minted), humaSessionManagerError(err)
	}
	res := syncOpResult(opjournal.KindInterrupt, nil, minted)
	res.Outcome.Degradation = session.DegradationUnfencedInterrupt
	return res, nil
}

// --- /respond ---

// respondOpPayload is a legacy /respond op's payload, JSON of the response
// (DES-O0API §3.1): text is always present, empty metadata is absent. It is
// not opjournal.EncodeRespondPayload's lane form, so rows a previous build
// recorded keep their request digest and a same-key retry replays them.
type respondOpPayload struct {
	Action   string            `json:"action"`
	Text     string            `json:"text"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// respondOpPolicy is a legacy /respond's policy: at_least_once, the guarantee
// it has always been recorded with. Nothing acts on it for a legacy op; it
// keeps the request digest of rows a previous build recorded.
var respondOpPolicy = opjournal.Policy{Guarantee: opjournal.GuaranteeAtLeastOnce}

// respondSession answers target's pending interaction with resp, recorded as
// a respond op under ops.control, and returns the answer. key is the
// Idempotency-Key header. When the city's lane host executes respond, the
// answer is a lane op instead (respondSessionLane), and ops.control does not
// govern it.
func (s *Server) respondSession(target, key string, resp runtime.InteractionResponse) (syncAnswer, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return syncAnswer{}, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	id, err := s.resolveSessionIDWithConfig(store.Store, target)
	if err != nil {
		return syncAnswer{}, humaResolveError(err)
	}
	mgr := s.sessionManager(store.Store)
	if s.laneExecutes(opjournal.KindRespond) {
		return s.respondSessionLane(mgr, store.Store, id, key, resp)
	}
	req := syncOpRequest{kind: opjournal.KindRespond, conv: id, key: key, policy: respondOpPolicy}
	if recordingMode(s.ops.control.Mode) {
		if req, err = s.respondOpRequest(mgr, store.Store, req, resp); err != nil {
			return syncAnswer{}, err
		}
	}
	fields, err := s.runSyncOp(req, humaSessionManagerError, func(adm *legacyop.Admission) (legacyop.Result, error) {
		run := resp
		if adm.Recorded && run.RequestID == "" {
			// A recorded respond answers only the interaction its op names:
			// one adopted after the agent raised another prompt, or a second
			// click that read the same prompt pending, answers nothing
			// (skipped{stale}) rather than the prompt pending now. A runtime
			// that derives request IDs from content (tmux) gives an identical
			// prompt raised again the same ID, so that one is answered: this
			// shadow op names the request id, not the interaction occurrence
			// a lane respond is keyed by (opjournal.RespondKey).
			run.RequestID = adm.Op.Scope.Interaction
		}
		err := mgr.Respond(id, run)
		if err != nil {
			return syncOpResult(opjournal.KindRespond, err, adm.Minted), humaSessionManagerError(err)
		}
		return syncOpResult(opjournal.KindRespond, nil, adm.Minted), nil
	})
	if err != nil {
		return syncAnswer{}, err
	}
	return syncAnswer{ID: id, syncOpFields: fields}, nil
}

// respondOpRequest completes a recorded /respond's op request: the payload
// (respondOpPayload) and the session(generation) scope with the interaction
// it answers.
// The interaction is the request's request_id, else, under a client key that
// already names a respond of that interaction, that respond's scope, else
// the pending interaction read now; the respond then answers that
// interaction only. A respond with no interaction to name cannot be
// recorded: auto runs it unrecorded, and require refuses it at the front
// door with the answer the legacy respond gives (no_pending, or
// unsupported), nothing sent.
func (s *Server) respondOpRequest(mgr *session.Manager, store beads.Store, req syncOpRequest, resp runtime.InteractionResponse) (syncOpRequest, error) {
	payload := respondOpPayload{Action: resp.Action, Text: resp.Text}
	if len(resp.Metadata) > 0 {
		payload.Metadata = resp.Metadata
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return req, apierr.Internal.Msg("encoding the respond op payload: " + err.Error())
	}
	req.payload = string(raw)
	scope, why, whyErr := sessionOpScope(store, req.conv)
	if why != "" {
		req.unrecordable, req.unrecordableErr = why, whyErr
		return req, nil
	}
	if prior, ok := s.priorSessionScope(opjournal.KindRespond, req.conv, req.key); ok &&
		(resp.RequestID == "" || resp.RequestID == prior.Interaction) {
		req.scope = prior
		return req, nil
	}
	scope.Interaction = resp.RequestID
	if scope.Interaction == "" {
		pending, supported, err := mgr.Pending(req.conv)
		switch {
		case err == nil && pending != nil:
			scope.Interaction = pending.RequestID
		case s.ops.control.Mode != gate.Require:
			req.unrecordable = legacyop.ReasonInvalidRequest
			return req, nil
		case err != nil:
			return req, humaSessionManagerError(err)
		case !supported:
			return req, humaSessionManagerError(fmt.Errorf("session %s: %w", req.conv, session.ErrInteractionUnsupported))
		default:
			return req, humaSessionManagerError(fmt.Errorf("session %s: %w", req.conv, session.ErrNoPendingInteraction))
		}
	}
	req.scope = scope
	return req, nil
}

// --- lifecycle commands ---

// lifecycleOp runs one conversation-scoped lifecycle command on session id
// as an op of kind under ops.lifecycle (DES-O0API §7.1): run is the legacy
// call, its error mapped onto the wire by humaSessionManagerError.
func (s *Server) lifecycleOp(kind opjournal.Kind, id, key string, policy opjournal.Policy, run func() error) (syncOpFields, error) {
	req := syncOpRequest{kind: kind, conv: id, key: key, policy: policy}
	return s.runSyncOp(req, humaSessionManagerError, func(adm *legacyop.Admission) (legacyop.Result, error) {
		if err := run(); err != nil {
			return syncOpResult(kind, err, adm.Minted), humaSessionManagerError(err)
		}
		return syncOpResult(kind, nil, adm.Minted), nil
	})
}

// closeOpRequest is a close's op request on session id. A close that deletes
// its bead carries its sessionCloseRequest as the payload, canonical JSON
// (DES-O0API §3.1), so the op's digest covers the delete: a same-key request
// that changes it is another request to the op, refused before anything
// runs. A plain close carries no payload, as gc session close's op does, so
// the two surfaces record one close alike.
func closeOpRequest(id, key string, del bool) (syncOpRequest, error) {
	req := syncOpRequest{kind: opjournal.KindClose, conv: id, key: key}
	if !del {
		return req, nil
	}
	raw, err := json.Marshal(sessionCloseRequest{Delete: del})
	if err != nil {
		return req, apierr.Internal.Msg("encoding the close op payload: " + err.Error())
	}
	req.payload = string(raw)
	return req, nil
}

// killOpRequest is a kill's op request on session id: scoped to the session
// generation its row names, or, under a client key that already names a kill
// of this session, to that kill's scope, so a same-key retry after the
// session restarted answers from the first kill.
func (s *Server) killOpRequest(store beads.Store, id, key string) syncOpRequest {
	req := syncOpRequest{kind: opjournal.KindKill, conv: id, key: key}
	if !recordingMode(s.ops.lifecycle.Mode) {
		return req
	}
	scope, why, whyErr := sessionOpScope(store, id)
	if why != "" {
		req.unrecordable, req.unrecordableErr = why, whyErr
		return req
	}
	if prior, ok := s.priorSessionScope(opjournal.KindKill, id, key); ok {
		scope = prior
	}
	req.scope = scope
	return req
}

// errKillTargetMoved classifies a recorded kill whose session moved to
// another generation than the one its op is scoped to.
var errKillTargetMoved = errors.New("the kill's target moved")

// killScopeMoved checks a recorded kill's session row, read under the kill's
// runtime lease, against the generation its op is scoped to (adm.Op.Scope):
// a same-key retry that adopts an op left open, after the session restarted,
// or a start that ran while the kill waited for the lease, would otherwise
// stop a run the op does not name. A row at another generation, or at none,
// is an error wrapping errKillTargetMoved naming both generations
// (refused:target_moved); a row that does not read is a retryable error,
// nothing killed. A kill that is not recorded passes.
func killScopeMoved(store beads.Store, id string, adm *legacyop.Admission) error {
	if !adm.Recorded || adm.Op.Scope.Kind != opjournal.ScopeSession {
		return nil
	}
	now, why, err := sessionOpScope(store, id)
	switch {
	case why == legacyop.ReasonUnreadableTarget:
		return fmt.Errorf("reading session %s under the kill's runtime lease, so nothing was killed: %w", id, err)
	case now.Generation == adm.Op.Scope.Generation:
		return nil
	}
	return fmt.Errorf("%s: session %s is at generation %d, not generation %d, which op %s was recorded for, so nothing was killed; resend with a new Idempotency-Key to kill the new run: %w",
		refusedTargetMoved, id, now.Generation, adm.Op.Scope.Generation, adm.Op.ID, errKillTargetMoved)
}

// recordedKillLease takes the runtime lease a recorded kill of a closed row
// checks its scope under. The lease is held from before the generation check
// through the kill (I-LEASE), so a start cannot slip a new run in between. A
// closed row keeps no lease record, so its kill takes the runtime name's
// flock alone, which holds off a reopened row's start all the same. An open
// row's lease is the operator kill's own (Server.operatorKill), taken under
// the same rule. A row that does not read is a retryable error, nothing
// killed. It returns nil, taking none, for a kill that is not recorded at the
// session's generation, an open row, or an actor with no city.
func (s *Server) recordedKillLease(ctx context.Context, store beads.Store, id string, by session.Actor, adm *legacyop.Admission) (*session.RuntimeLease, error) {
	if adm == nil || !adm.Recorded || adm.Op.Scope.Kind != opjournal.ScopeSession || by.City.IsZero() {
		return nil, nil
	}
	info, err := session.NewStore(beads.SessionStore{Store: store}).Get(id)
	if err != nil {
		return nil, fmt.Errorf("reading session %s before taking its runtime lease, so nothing was killed: %w", id, err)
	}
	if !info.Closed {
		return nil, nil
	}
	return session.WaitOperatorRuntimeLease(ctx, nil, session.RuntimeLeaseRequest{City: by.City.Path(), Name: info.SessionName})
}

// killOpResult is a kill's op result: a moved target closes
// failed{refused:target_moved} under any key, since a retry under the same
// key would find the same op scoped to the generation that is gone; the rest
// as for every command.
func killOpResult(err error, minted bool) legacyop.Result {
	if errors.Is(err, errKillTargetMoved) {
		return legacyop.Result{Outcome: opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.RefusedReasonPrefix + refusedTargetMoved}}
	}
	return syncOpResult(opjournal.KindKill, err, minted)
}

// wakeHTTPError maps a wake's error onto the wire.
func wakeHTTPError(id string, err error) error {
	var willNot *wakeWillNotStartError
	switch {
	case errors.As(err, &willNot):
		return apierr.WakeWillNotStart.Msg(willNot.message)
	case errors.Is(err, session.ErrNotSessionBead):
		return apierr.InvalidRequest.Msg(id + " is not a session")
	}
	if state, conflict := session.WakeConflictState(err); conflict {
		return apierr.SessionConflict.Msg("session " + id + " is " + state)
	}
	// Route every remaining store error through humaStoreError: the fused
	// Get error keeps its original 404 "not_found: "/500 "internal: "
	// mapping, and a mid-wake write error keeps the "internal: " prefix.
	return humaStoreError(err)
}
