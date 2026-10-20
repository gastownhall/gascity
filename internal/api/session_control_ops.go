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
// so a failed stop never uses up its turn. A request that carries a key
// otherwise keeps it, and a retry under it reuses its first request's scope,
// so the retry replays that stop instead of interrupting a later turn. A
// turn the request did not name that ends before the interrupt lands was
// nothing to stop; only a named one is refused turn_not_current.

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
		return syncReplay(adm, mapErr)
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
// op_already_failed.
func syncReplay(adm *legacyop.Admission, mapErr func(error) error) (syncOpFields, error) {
	if adm.Cached != nil || adm.Op.Status != opjournal.StatusClosed {
		// A sync command caches no in-flight answer; a held op is busy.
		return syncOpFields{}, requestInFlight()
	}
	op := adm.Op
	if syncOpReplaysAsSuccess(op) {
		return syncOpFields{IdempotencyKey: adm.Key, Replayed: true, Op: conversationOpWire(op)}, nil
	}
	return syncOpFields{}, replayedSyncError(op, mapErr)
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
	prior, found := s.keyedOp(kind, conv, key)
	if !found || prior.Kind != kind || prior.Scope.Kind != opjournal.ScopeSession {
		return opjournal.Scope{}, false
	}
	return prior.Scope, true
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
// named the turn the request names, "" for none.
func (s *Server) stopSession(ctx context.Context, target, key, named string) (syncAnswer, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return syncAnswer{}, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	id, err := s.resolveSessionIDWithConfig(store.Store, target)
	if err != nil {
		return syncAnswer{}, humaResolveError(err)
	}
	var plan stopPlan
	if named != "" {
		if plan, err = s.namedStopPlan(ctx, store.Store, id, named); err != nil {
			return syncAnswer{}, err
		}
	}
	req := syncOpRequest{kind: opjournal.KindInterrupt, conv: id, key: key, policy: interruptOpPolicy}
	if recordingMode(s.ops.control.Mode) {
		// The client key is the op's key, or a target key's successor: in
		// require it is checked even when a target key stands in for it.
		if err := s.checkBeforeEffect(opjournal.KindInterrupt, key); err != nil {
			return syncAnswer{}, err
		}
		req, plan = s.stopOpRequest(ctx, store.Store, req, plan)
	}
	fields, err := s.runSyncOp(req, humaStopError, func(adm *legacyop.Admission) (legacyop.Result, error) {
		run := plan
		if !adm.Recorded && !run.named {
			// I8: a stop auto could not record runs as it does in off.
			run = stopPlan{}
		}
		return s.executeStop(store.Store, id, run, adm.Minted)
	})
	if err != nil {
		return syncAnswer{}, err
	}
	return syncAnswer{ID: id, syncOpFields: fields}, nil
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
// DES-O0API §3.2): the session(generation) scope; a named turn in it, keyed
// stop:<generation>:<turn>; else, under a client key that already names a
// stop, that stop's scope; else the turn a live read of a protocol turn
// source finds running, keyed by it when the request carries no key. A
// target key's successor is the request's own key. A request that cannot be
// recorded resolves nothing: it runs as in off.
func (s *Server) stopOpRequest(ctx context.Context, store beads.Store, req syncOpRequest, plan stopPlan) (syncOpRequest, stopPlan) {
	scope, why, whyErr := sessionOpScope(store, req.conv)
	if why != "" {
		req.unrecordable, req.unrecordableErr = why, whyErr
		return req, plan
	}
	clientKey := req.key
	switch {
	case plan.named:
		scope.Turn = plan.turn
		req.key = stopTargetKey(scope.Generation, plan.turn, req.key)
	default:
		if prior, ok := s.priorSessionScope(opjournal.KindInterrupt, req.conv, req.key); ok {
			// The scope must be the first request's for the retry to find
			// its op; a session-scoped one's turn is read again, so an
			// adopted stop is fenced as a fresh one is.
			scope = prior
			if prior.Turn != "" {
				plan = s.priorStopPlan(ctx, store, req.conv, prior.Turn)
			} else {
				plan = s.resolveStopTurn(ctx, store, req.conv)
			}
			break
		}
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
	return req, plan
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

// respondOpPayload is a respond op's payload, canonical JSON of the response
// (DES-O0API §3.1); empty metadata is absent.
type respondOpPayload struct {
	Action   string            `json:"action"`
	Text     string            `json:"text"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// respondSession answers target's pending interaction with resp, recorded as
// a respond op under ops.control, and returns the answer. key is the
// Idempotency-Key header.
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
	req := syncOpRequest{kind: opjournal.KindRespond, conv: id, key: key}
	if recordingMode(s.ops.control.Mode) {
		if req, err = s.respondOpRequest(mgr, store.Store, req, resp); err != nil {
			return syncAnswer{}, err
		}
	}
	fields, err := s.runSyncOp(req, humaSessionManagerError, func(adm *legacyop.Admission) (legacyop.Result, error) {
		err := mgr.Respond(id, resp)
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

// respondOpRequest completes a recorded /respond's op request: the canonical
// payload and the session(generation) scope with the interaction it answers.
// The interaction is the request's request_id, else, under a client key that
// already names a respond of that interaction, that respond's scope, else
// the pending interaction read now. A respond with no interaction to name
// cannot be recorded: auto runs it unrecorded, and require refuses it at the
// front door with the answer the legacy respond gives (no_pending, or
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
// stop a run the op does not name. It returns an error wrapping
// errKillTargetMoved naming both generations, as gc session kill refuses
// (refused:target_moved). A kill that is not recorded, or a row that does not
// read with a generation, passes: the kill's own read then decides.
func killScopeMoved(store beads.Store, id string, adm *legacyop.Admission) error {
	if !adm.Recorded || adm.Op.Scope.Kind != opjournal.ScopeSession {
		return nil
	}
	now, why, _ := sessionOpScope(store, id)
	if why != "" || now.Generation == adm.Op.Scope.Generation {
		return nil
	}
	return fmt.Errorf("%s: session %s is at generation %d, not generation %d, which op %s was recorded for, so nothing was killed; resend with a new Idempotency-Key to kill the new run: %w",
		refusedTargetMoved, id, now.Generation, adm.Op.Scope.Generation, adm.Op.ID, errKillTargetMoved)
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
