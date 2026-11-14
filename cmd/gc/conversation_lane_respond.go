package main

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// laneRespondRefusedAction closes a respond whose action the pending prompt
// does not offer, or whose answer the runtime refuses: nothing was sent, and
// the same answer would be refused again.
const laneRespondRefusedAction = opjournal.ReasonRefusedAction

// Respond implements opjournal.Executor (DES-O1 §10.2): it answers the
// request the call names, through the handle its probe resolved, only while
// the runtime still shows that request. RequestID is always set on the
// response, so the runtime refuses to answer another prompt in its place.
// The answer is fenced to the call's incarnation where the runtime fences
// answers (DES-O1 §6.2: tmux, atomically); where it cannot, or the row names
// no token, it goes unfenced and records DegradationUnfencedDelivery.
//
// The re-check and the answer run under the session mutation lock, which
// legacy Manager.Respond holds over its own: otherwise both could pass the
// runtime's pre-verify of one prompt and type two keys, the second landing
// in whatever shows next. A lock held past ctx, or a re-check that outlasts
// it, sends nothing, and the op retries. The re-check reads the occurrence
// too: the prompt showing may be a later raising of the call's request id (a
// content hash), so an end another writer recorded since the lane planned
// the answer sends nothing. The probe resolves a RuntimeHandle (it saw the
// runtime running as the row's incarnation), whose Respond takes no session
// lock; a SessionHandle's would wait on the lock held here.
func (e *conversationLaneExec) Respond(ctx context.Context, c opjournal.RespondCall) opjournal.ControlResult {
	h := e.live
	if h == nil {
		return opjournal.NotDelivered{Transient: true, Cause: errLaneNoProbedHandle}
	}
	var res opjournal.ControlResult
	if err := session.WithSessionMutationLockContext(ctx, e.info.ID, func() error {
		res = e.respondLocked(ctx, h, c)
		return nil
	}); err != nil {
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("answering %s: %w", c.Interaction, err)}
	}
	return res
}

// respondLocked is Respond's re-check and answer, under the session
// mutation lock.
func (e *conversationLaneExec) respondLocked(ctx context.Context, h worker.Handle, c opjournal.RespondCall) opjournal.ControlResult {
	pending, supported, err := h.PendingStatus(ctx)
	switch {
	case err != nil:
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("re-reading the pending interaction before answering %s: %w", c.Interaction, err)}
	case !supported:
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
	case pending == nil || pending.RequestID != c.RequestID:
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	}
	if res := e.respondOccurrence(c); res != nil {
		return res
	}
	// The re-check need not honor ctx, so it can spend the call's budget:
	// an answer begun now would fail before its key, so none is sent.
	if err := ctx.Err(); err != nil {
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("answering %s after the re-check: %w", c.Interaction, err)}
	}
	fence := e.respondFence(c.Fence)
	err = h.Respond(ctx, worker.InteractionResponse{
		RequestID: c.RequestID,
		Action:    c.Response.Action,
		Text:      c.Response.Text,
		Metadata:  maps.Clone(c.Response.Metadata),
		Fence:     fence,
	})
	return laneRespondResult(ctx, err, fence.IsZero())
}

// errLaneNoOpStore is a Respond with no op store to re-read its occurrence
// from. The lane host always passes one, so it is a bug; nothing is sent.
var errLaneNoOpStore = errors.New("conversation lane: respond without an op store to read its occurrence")

// respondOccurrence re-reads the occurrence c answers: nil while it is raised
// and unresolved, else the result that ends the call with nothing sent,
// skipped{stale} for an occurrence that has ended and a transient miss for a
// read that failed.
func (e *conversationLaneExec) respondOccurrence(c opjournal.RespondCall) opjournal.ControlResult {
	if e.deps.lane == nil {
		return opjournal.NotDelivered{Transient: true, Cause: errLaneNoOpStore}
	}
	r, found, err := e.deps.lane.Raising(c.Op.Conversation, c.Op.Scope.Generation, c.Interaction)
	switch {
	case err != nil:
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("re-reading interaction %s before answering it: %w", c.Interaction, err)}
	case !found || r.Resolved:
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	}
	return nil
}

// respondFence is the runtime fence an answer under f carries: f's
// incarnation where the runtime fences answers to the session and the row
// names a token, else none (an unfenced answer).
func (e *conversationLaneExec) respondFence(f opjournal.Fence) runtime.InputFence {
	if f.Token == "" || runtime.ResponseFencingFor(e.deps.sp, e.target.sessionName) == runtime.InputFencingNone {
		return runtime.InputFence{}
	}
	return runtime.InputFence{Generation: f.Generation, Epoch: f.Epoch, Token: f.Token}
}

// laneRespondResult maps one response's answer to the lane's result. A
// refusal that sent nothing is definitive or transient; an error after the
// answer may have reached the prompt is Unknown, and the lane never sends
// that respond again (it is at most once). unfenced says the answer carried
// no fence.
func laneRespondResult(ctx context.Context, err error, unfenced bool) opjournal.ControlResult {
	var miss *runtime.FenceMismatchError
	switch {
	case err == nil && unfenced:
		return opjournal.Settled{Via: laneViaLive, Degradation: opjournal.DegradationUnfencedDelivery}
	case err == nil:
		return opjournal.Settled{Via: laneViaLive}
	case errors.Is(err, session.ErrInteractionUnsupported), errors.Is(err, runtime.ErrInteractionUnsupported):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
	case errors.Is(err, runtime.ErrInteractionActionUnavailable), errors.Is(err, runtime.ErrInteractionResponseInvalid):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: laneRespondRefusedAction}
	case errors.Is(err, runtime.ErrInteractionGone), errors.Is(err, session.ErrNoPendingInteraction), errors.Is(err, session.ErrInteractionMismatch):
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	case errors.As(err, &miss) && miss.Partial:
		return opjournal.Unknown{Cause: err}
	case errors.As(err, &miss), errors.Is(err, runtime.ErrInputFenceUnsupported), errors.Is(err, runtime.ErrInvalidInputFence),
		errors.Is(err, runtime.ErrSessionNotFound), errors.Is(err, session.ErrSessionNotFound), errors.Is(err, runtime.ErrRuntimeUnavailable),
		errors.Is(err, runtime.ErrInputNotSent):
		// Nothing was sent: another incarnation, a runtime that would not
		// fence, a fence naming no incarnation, a runtime that could not be
		// reached or read before the key, or an answer that failed before
		// its key, even one cut short by ctx.
		return opjournal.NotDelivered{Transient: true, Cause: err}
	default:
		// A canceled or timed-out answer the runtime does not place before
		// its key, or a prompt that did not clear after it, may have been
		// answered.
		return opjournal.Unknown{Cause: errors.Join(err, ctx.Err())}
	}
}
