package api

import (
	"github.com/gastownhall/gascity/internal/opjournal"
)

// ConversationOp is the operation a session command was recorded as
// (DES-O0API §10.2). It is present only when the server recorded the request:
// an ops gate is auto or require and the op store could record it.
type ConversationOp struct {
	ID           string                 `json:"id" doc:"Operation id. Equals request_id on async responses."`
	Conversation string                 `json:"conversation" doc:"Session (conversation) id the operation targets."`
	Kind         string                 `json:"kind" enum:"message,interrupt,respond,create,wake,suspend,resume,restart,kill,close" doc:"Operation kind."`
	Status       string                 `json:"status" enum:"submitted,accepted,executing,settling,closed" doc:"Operation status. A legacy-executed operation stays submitted until it closes."`
	Attempt      int                    `json:"attempt,omitempty" doc:"Executions after the first (an adopted orphan)."`
	Outcome      *ConversationOpOutcome `json:"outcome,omitempty" doc:"Present when status is closed."`
}

// ConversationOpOutcome is a closed operation's single terminal outcome.
type ConversationOpOutcome struct {
	Arm         string `json:"arm" enum:"succeeded,failed,skipped,canceled" doc:"Outcome arm."`
	Reason      string `json:"reason,omitempty" doc:"Why a failed or skipped operation ended (open vocabulary; refused:<why> names a refusal)."`
	Degradation string `json:"degradation,omitempty" doc:"How a succeeded operation fell short of its policy, e.g. unfenced_interrupt."`
	Via         string `json:"via,omitempty" doc:"How it was carried out; legacy.* in this release (legacy.delivered, legacy.queued)."`
}

// conversationOpWire projects op onto the wire.
func conversationOpWire(op opjournal.Op) *ConversationOp {
	out := &ConversationOp{
		ID:           op.ID,
		Conversation: op.Conversation,
		Kind:         string(op.Kind),
		Status:       string(op.Status),
		Attempt:      op.Attempt,
	}
	if op.Status == opjournal.StatusClosed {
		out.Outcome = &ConversationOpOutcome{
			Arm:         string(op.Outcome.Arm),
			Reason:      op.Outcome.Reason,
			Degradation: op.Outcome.Degradation,
			Via:         op.Outcome.Result.Via,
		}
	}
	return out
}
