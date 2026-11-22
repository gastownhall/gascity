package api

import (
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// ConversationOp is the operation a session command was recorded as
// (DES-O0API §10.2). It is present only when the server recorded the request:
// an ops gate is auto or require and the op store could record it.
//
// The op routes (GET .../session/{id}/ops) and the op.changed event carry the
// op's whole record (conversationOpRecord): the fields from Executor on are set
// there only, so a session command's answer is unchanged.
type ConversationOp struct {
	ID           string                 `json:"id" doc:"Operation id. Equals request_id on async responses."`
	Conversation string                 `json:"conversation" doc:"Session (conversation) id the operation targets."`
	Kind         string                 `json:"kind" enum:"message,interrupt,respond,create,wake,suspend,resume,restart,kill,close" doc:"Operation kind."`
	Status       string                 `json:"status" enum:"submitted,accepted,executing,settling,closed" doc:"Operation status. A legacy-executed operation stays submitted until it closes."`
	Attempt      int                    `json:"attempt,omitempty" doc:"How many times the operation was executed, counted by its executor. A legacy-executed operation counts executions after the first (an adopted orphan), so its first run reads 0. A lane operation counts the lane's takes, its first included, so one delivered once reads 1 and a redelivery 2; maybe_delivered says whether an earlier take may have reached the agent."`
	Outcome      *ConversationOpOutcome `json:"outcome,omitempty" doc:"Present when status is closed."`

	Executor        string     `json:"executor,omitempty" enum:"lane,legacy,legacy:api,legacy:cli" doc:"Who executes the operation: lane (the conversation lane) or a legacy path. Set on the op routes and op.changed only."`
	ClientKey       string     `json:"client_key,omitempty" doc:"The producer's client key: the Idempotency-Key, or the key the server minted. Set on the op routes and op.changed only."`
	Principal       string     `json:"principal,omitempty" doc:"The producer's principal: operator, conversation:<id>, t3:<environment id>, or gc:<producer> for gc's own producers. Asserted, not authenticated. Set on the op routes and op.changed only."`
	Actor           string     `json:"actor,omitempty" doc:"The producer's actor class, an open token. Set on the op routes and op.changed only."`
	ClientSeq       uint64     `json:"client_seq,omitempty" doc:"The producer's own sequence number, which orders its operations of equal precedence. Set on the op routes and op.changed only."`
	LaneSeq         uint64     `json:"lane_seq,omitempty" doc:"The conversation lane's sequence number of the operation's last take. Set on the op routes and op.changed only."`
	MaybeDelivered  int        `json:"maybe_delivered,omitempty" doc:"The last attempt that may have reached the agent (a redelivery after an ambiguous attempt), 0 when none may have. Set on the op routes and op.changed only."`
	CancelRequested bool       `json:"cancel_requested,omitempty" doc:"True once a cancel is recorded on an operation that was already executing; its outcome says whether the cancel won. Set on the op routes and op.changed only."`
	Causation       string     `json:"causation,omitempty" doc:"What caused the operation, as the producer named it. Set on the op routes and op.changed only."`
	Correlation     string     `json:"correlation,omitempty" doc:"The producer's correlation id. Set on the op routes and op.changed only."`
	CreatedAt       *time.Time `json:"created_at,omitempty" doc:"When the operation was proposed. Set on the op routes and op.changed only."`
	At              *time.Time `json:"at,omitempty" doc:"When the operation last changed status. Set on the op routes and op.changed only."`
}

// ConversationOpOutcome is a closed operation's single terminal outcome.
type ConversationOpOutcome struct {
	Arm         string `json:"arm" enum:"succeeded,failed,skipped,canceled" doc:"Outcome arm."`
	Reason      string `json:"reason,omitempty" doc:"Why a failed or skipped operation ended (open vocabulary; refused:<why> names a refusal)."`
	Degradation string `json:"degradation,omitempty" doc:"How a succeeded operation fell short of its policy, e.g. unfenced_interrupt."`
	Via         string `json:"via,omitempty" doc:"How it was carried out; legacy.* in this release (legacy.delivered, legacy.queued)."`
	// Result is set on the op routes and op.changed only.
	Result *ConversationOpResult `json:"result,omitempty" doc:"What a succeeded or coalesced operation produced. Set on the op routes and op.changed only."`
}

// ConversationOpResult is what a succeeded (or coalesced) operation produced:
// the session and turn it ran in and, for a conversation-lane delivery, the
// delivery record and the turn link with its confidence (DES-O1 §4.8). An
// operation the lane fenced off unrun, skipped{stale} because its session
// had moved on, records the generation and epoch that fenced it off.
type ConversationOpResult struct {
	Generation    int64                   `json:"generation,omitempty" doc:"Session generation the operation ran in, or, for one skipped{stale} unrun because its session moved on, the generation that fenced it off."`
	ContextEpoch  int64                   `json:"context_epoch,omitempty" doc:"Context epoch the operation ran in, or, for one skipped{stale} unrun, the epoch that fenced it off."`
	Turn          string                  `json:"turn,omitempty" doc:"The turn the operation ran in or was linked to; empty when the link is none."`
	TurnLink      string                  `json:"turn_link,omitempty" enum:"exact,correlated,ambiguous,none" doc:"Confidence of the turn link: exact when the runtime named the turn, correlated or ambiguous from observed turns, none when no turn could be linked."`
	TurnBasis     string                  `json:"turn_basis,omitempty" doc:"The rule that decided the turn link (open vocabulary, e.g. runtime_turn, history_diff, no_source)."`
	CoalescedInto string                  `json:"coalesced_into,omitempty" doc:"The operation this one was delivered with, when coalesced."`
	Delivery      *ConversationOpDelivery `json:"delivery,omitempty" doc:"The conversation lane's delivery record."`
}

// ConversationOpDelivery is the conversation lane's record of one delivery. It
// carries no text.
type ConversationOpDelivery struct {
	Seq       uint64     `json:"seq,omitempty" doc:"lane_seq of the delivery's head operation."`
	At        *time.Time `json:"at,omitempty" doc:"The lane's clock just before the delivery's first input."`
	Ack       string     `json:"ack,omitempty" doc:"The runtime's answer (open vocabulary, e.g. delivered, delivered_unobserved, in_band)."`
	MessageID string     `json:"message_id,omitempty" doc:"The runtime's id for the delivered message, when it reports one."`
	MessageAt *time.Time `json:"message_at,omitempty" doc:"The runtime's creation time for the delivered message, when it reports one."`
}

// ConversationFact is one occurrence fact: a session or turn started or
// ended, an interaction raised or resolved.
type ConversationFact struct {
	ID           string    `json:"id" doc:"Fact id, keyed by the occurrence."`
	Kind         string    `json:"kind" enum:"session.started,session.ended,turn.started,turn.ended,interaction.raised,interaction.resolved" doc:"Fact kind."`
	Conversation string    `json:"conversation" doc:"Session (conversation) id the fact belongs to."`
	Generation   int64     `json:"generation" doc:"Session generation the fact happened in."`
	Turn         string    `json:"turn,omitempty" doc:"The turn of a turn.* fact, as its runtime names it."`
	Interaction  string    `json:"interaction,omitempty" doc:"The occurrence of an interaction.* fact."`
	CauseOps     []string  `json:"cause_ops,omitempty" doc:"Operations that caused the fact."`
	Reason       string    `json:"reason,omitempty" doc:"Open reason token, e.g. a turn.ended reason (completed, interrupted, superseded)."`
	Basis        string    `json:"basis,omitempty" doc:"How the fact is known: runtime (the provider recorded it), observed (gc inferred it) or lane. Open: a newer build may add values."`
	Detail       string    `json:"detail,omitempty" doc:"The source's own word for the occurrence, e.g. a provider stop reason."`
	At           time.Time `json:"at" doc:"When it happened: the source's time, or the observer's clock when the source gives none."`
}

// conversationOpWire projects op onto a session command's answer.
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

// conversationOpRecord projects op's whole record, without its payload, onto
// the op routes and the op.changed event.
func conversationOpRecord(op opjournal.Op) ConversationOp {
	out := *conversationOpWire(op)
	out.Executor = string(op.Executor)
	out.ClientKey = op.ClientKey
	out.Principal = op.Producer.Principal
	out.Actor = op.Producer.Actor
	out.ClientSeq = op.ClientSeq
	out.LaneSeq = op.LaneSeq
	out.MaybeDelivered = op.MaybeDelivered
	out.CancelRequested = op.Cancel.Recorded() && op.Outcome.Arm != opjournal.ArmCanceled
	out.Causation = op.Causation
	out.Correlation = op.Correlation
	out.CreatedAt = optionalTime(op.CreatedAt)
	if n := len(op.Transitions); n > 0 {
		out.At = optionalTime(op.Transitions[n-1].At)
	}
	if out.Outcome != nil {
		out.Outcome.Result = conversationOpResultWire(op.Outcome.Result)
	}
	return out
}

// conversationOpResultWire projects r, nil when it records nothing.
func conversationOpResultWire(r opjournal.Result) *ConversationOpResult {
	out := ConversationOpResult{
		Generation:    r.Generation,
		ContextEpoch:  r.ContextEpoch,
		Turn:          r.Turn,
		TurnLink:      string(r.TurnLink),
		TurnBasis:     string(r.TurnBasis),
		CoalescedInto: r.CoalescedInto,
	}
	if d := r.Delivery; !d.IsZero() {
		out.Delivery = &ConversationOpDelivery{
			Seq:       d.Seq,
			At:        optionalTime(d.At),
			Ack:       string(d.Ack),
			MessageID: d.MessageID,
			MessageAt: optionalTime(d.MessageAt),
		}
	}
	if out == (ConversationOpResult{}) {
		return nil
	}
	return &out
}

// conversationFactWire projects f.
func conversationFactWire(f opjournal.Fact) ConversationFact {
	return ConversationFact{
		ID:           f.ID,
		Kind:         string(f.Kind),
		Conversation: f.Conversation,
		Generation:   f.Generation,
		Turn:         f.Turn,
		Interaction:  f.Interaction,
		CauseOps:     f.CauseOps,
		Reason:       f.Reason,
		Basis:        string(f.Basis),
		Detail:       f.Detail,
		At:           f.At.UTC(),
	}
}

// optionalTime is t in UTC, nil when t is zero.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
