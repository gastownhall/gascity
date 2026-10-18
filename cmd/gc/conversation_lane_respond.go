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
const laneRespondRefusedAction = opjournal.RefusedReasonPrefix + "action"

// Respond implements opjournal.Executor (DES-O1 §10.2): it answers the
// request the call names, through the handle its probe resolved, only while
// the runtime still shows that request. RequestID is always set on the
// response, so the runtime refuses to answer another prompt in its place.
// The answer is unfenced: no runtime fences a response to an incarnation
// yet, so it records DegradationUnfencedDelivery.
func (e *conversationLaneExec) Respond(ctx context.Context, c opjournal.RespondCall) opjournal.ControlResult {
	h := e.live
	if h == nil {
		return opjournal.NotDelivered{Transient: true, Cause: errLaneNoProbedHandle}
	}
	pending, supported, err := h.PendingStatus(ctx)
	switch {
	case err != nil:
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("re-reading the pending interaction before answering %s: %w", c.Interaction, err)}
	case !supported:
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
	case pending == nil || pending.RequestID != c.RequestID:
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	}
	err = h.Respond(ctx, worker.InteractionResponse{
		RequestID: c.RequestID,
		Action:    c.Response.Action,
		Text:      c.Response.Text,
		Metadata:  maps.Clone(c.Response.Metadata),
	})
	return laneRespondResult(ctx, err)
}

// laneRespondResult maps one response's answer to the lane's result. A
// refusal that sent nothing is definitive or transient; an error after the
// answer may have reached the prompt is Unknown.
func laneRespondResult(ctx context.Context, err error) opjournal.ControlResult {
	switch {
	case err == nil:
		return opjournal.Settled{Via: laneViaLive, Degradation: opjournal.DegradationUnfencedDelivery}
	case errors.Is(err, session.ErrInteractionUnsupported), errors.Is(err, runtime.ErrInteractionUnsupported):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
	case errors.Is(err, runtime.ErrInteractionActionUnavailable), errors.Is(err, runtime.ErrInteractionResponseInvalid):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: laneRespondRefusedAction}
	case errors.Is(err, session.ErrNoPendingInteraction), errors.Is(err, session.ErrInteractionMismatch):
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	case errors.Is(err, runtime.ErrSessionNotFound), errors.Is(err, session.ErrSessionNotFound), errors.Is(err, runtime.ErrRuntimeUnavailable):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	default:
		// A canceled or timed-out answer, or a prompt that did not clear
		// after its key, may have been answered.
		return opjournal.Unknown{Cause: errors.Join(err, ctx.Err())}
	}
}
