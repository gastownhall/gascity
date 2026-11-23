package api

import "time"

// The producer API's wire types (DES-O1 §11): Propose, Get, the open-op list
// and Cancel over a session's conversation lane.

// ConversationProducerBody names who makes a request: an asserted principal
// and an actor class.
type ConversationProducerBody struct {
	Principal string `json:"principal,omitempty" pattern:"^(operator|t3:[A-Za-z0-9][A-Za-z0-9._-]{0,127})$" doc:"The asserted principal: operator (the default) or t3:<environment id> for a T3 Code environment. Until the front door exists it is asserted, not authenticated, like the operator identity of every API mutation; conversation:<id> and gc:<producer> stay reserved to gc's own producers. It keys the operation with the client key, so a retry must send the same one."`
	Actor     string `json:"actor,omitempty" maxLength:"64" pattern:"^[a-z0-9._:/-]+$" doc:"The actor class within the principal, e.g. user, system or mcp. Defaults to operator for the operator principal; required with a t3 principal."`
}

// ConversationMessagePolicyBody is a message operation's policy. Omitted
// fields take the kind's defaults (boundary, if_needed, prompt).
type ConversationMessagePolicyBody struct {
	When          string `json:"when,omitempty" enum:"boundary,after_turn,interrupt,if_idle,now,steer" doc:"When to deliver: boundary (the next turn boundary), after_turn (ride the next turn, never start one), interrupt, if_idle (skipped when the session is mid-turn), now (deliver now) or steer (into the running turn, where the runtime can steer)."`
	Wake          string `json:"wake,omitempty" enum:"if_needed,live_only" doc:"Whether the message may wake its session: if_needed or live_only."`
	Role          string `json:"role,omitempty" enum:"prompt,notice" doc:"How the agent sees it: prompt (a turn of its own) or notice (a reminder)."`
	OnUnsupported string `json:"on_unsupported,omitempty" enum:"boundary,fail" doc:"What a steer does on a runtime that cannot steer: boundary or fail."`
	NowGeneration int64  `json:"now_generation,omitempty" minimum:"0" doc:"With when now: the session generation the requester saw."`
	NowTurn       string `json:"now_turn,omitempty" maxLength:"128" doc:"With when now: the turn the requester saw."`
}

// ConversationInterruptPolicyBody is an interrupt operation's policy.
type ConversationInterruptPolicyBody struct {
	OnFailure string `json:"on_failure,omitempty" enum:"fail,restart" doc:"What the interrupt does when no boundary is observed: fail or restart."`
}

// ConversationOpPolicyBody is an operation's policy; only the kind's own
// sub-policy may be set.
type ConversationOpPolicyBody struct {
	Guarantee string                           `json:"guarantee,omitempty" enum:"at_least_once,at_most_once" doc:"Execution guarantee: at_least_once (the default; a delivery is repeated only after an ambiguous attempt) or at_most_once (never repeated)."`
	Message   *ConversationMessagePolicyBody   `json:"message,omitempty" doc:"The message policy."`
	Interrupt *ConversationInterruptPolicyBody `json:"interrupt,omitempty" doc:"The interrupt policy."`
}

// ConversationOpScopeBody is an operation's validity scope.
type ConversationOpScopeBody struct {
	Kind        string `json:"kind" enum:"conversation,context,session" doc:"conversation (any session), context (one context epoch) or session (one session generation)."`
	Epoch       int64  `json:"epoch,omitempty" minimum:"0" doc:"The context epoch of a context scope."`
	Generation  int64  `json:"generation,omitempty" minimum:"0" doc:"The session generation of a session scope."`
	Turn        string `json:"turn,omitempty" maxLength:"128" doc:"The turn an interrupt targets."`
	Interaction string `json:"interaction,omitempty" doc:"The interaction occurrence a respond answers."`
}

// ConversationOpProposeBody is the body of POST .../session/{id}/ops.
type ConversationOpProposeBody struct {
	ConversationProducerBody
	Kind        string                    `json:"kind" enum:"message,interrupt,respond,create,wake,suspend,resume,restart,kill,close" doc:"Operation kind. Only kinds listed in [ops] authoritative are accepted (409 kind-not-enabled otherwise)."`
	ClientKey   string                    `json:"client_key,omitempty" doc:"The producer's idempotency key (e.g. T3's message id). The Idempotency-Key header may carry it instead; when both are set they must be equal. Minted and returned when both are absent."`
	Payload     string                    `json:"payload,omitempty" doc:"The operation's payload: a message's text."`
	Policy      *ConversationOpPolicyBody `json:"policy,omitempty" doc:"The operation's policy; omitted fields take the kind's defaults."`
	Scope       *ConversationOpScopeBody  `json:"scope,omitempty" doc:"The operation's validity scope; omitted takes the kind's default."`
	After       string                    `json:"after,omitempty" doc:"An operation id of the same session this one runs after."`
	ClientSeq   uint64                    `json:"client_seq,omitempty" maximum:"9223372036854775807" doc:"The producer's own sequence number: it keeps two quick proposals in their order even when relayed concurrently."`
	NotBefore   *time.Time                `json:"not_before,omitempty" doc:"Do not start before this time."`
	StartBy     *time.Time                `json:"start_by,omitempty" doc:"Close the operation failed if it has not started by this time."`
	FinishBy    *time.Time                `json:"finish_by,omitempty" doc:"The deadline for the operation to finish."`
	Causation   string                    `json:"causation,omitempty" doc:"What caused the operation."`
	Correlation string                    `json:"correlation,omitempty" doc:"The producer's correlation id."`
}

// ConversationOpProposeInput is the Huma input for POST
// /v0/city/{cityName}/session/{id}/ops.
type ConversationOpProposeInput struct {
	CityScope
	ID             string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"The client key, when the body does not carry one."`
	Body           ConversationOpProposeBody
}

// ConversationOpProposeResult is the body of a proposal's answer.
type ConversationOpProposeResult struct {
	OpID      string         `json:"op_id" doc:"The operation id, derived from the session, the principal and the client key."`
	Status    string         `json:"status" enum:"submitted,accepted,executing,settling,closed" doc:"The operation's status now."`
	Created   bool           `json:"created" doc:"True when this request wrote the operation (201), false when it already existed (200)."`
	ClientKey string         `json:"client_key" doc:"The client key the operation is recorded under: the producer's, or the one the server minted. Retry with it to get this operation back."`
	Cursor    string         `json:"cursor" doc:"City event-stream sequence captured before the proposal. Pass it as after_seq to /v0/city/{cityName}/events/stream to receive this operation's op.changed events."`
	Op        ConversationOp `json:"op" doc:"The operation."`
}

// ConversationOpProposeOutput is the Huma output of a proposal: 201 when it
// wrote the operation, 200 when the operation already existed.
type ConversationOpProposeOutput struct {
	Status int `json:"-"`
	Body   ConversationOpProposeResult
}

// ConversationOpGetInput is the Huma input for GET
// /v0/city/{cityName}/session/{id}/ops/{op}.
type ConversationOpGetInput struct {
	CityScope
	ID string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Op string `path:"op" pattern:"^[^\\s,\"@#]+-op-[a-z2-7]{26}$" doc:"Operation id."`
}

// ConversationOpCancelBody is the optional body of POST
// .../session/{id}/ops/{op}/cancel.
type ConversationOpCancelBody struct {
	Principal string `json:"principal,omitempty" pattern:"^(operator|t3:[A-Za-z0-9][A-Za-z0-9._-]{0,127})$" doc:"The asserted principal recorded as the canceler: operator (the default) or t3:<environment id>."`
	Reason    string `json:"reason,omitempty" maxLength:"256" doc:"Why the producer cancels."`
}

// ConversationOpCancelInput is the Huma input for POST
// /v0/city/{cityName}/session/{id}/ops/{op}/cancel.
type ConversationOpCancelInput struct {
	CityScope
	ID   string `path:"id" doc:"Session ID, alias, or runtime session_name."`
	Op   string `path:"op" pattern:"^[^\\s,\"@#]+-op-[a-z2-7]{26}$" doc:"Operation id."`
	Body *ConversationOpCancelBody
}

// ConversationOpCancelResult is the body of a cancel's answer.
type ConversationOpCancelResult struct {
	Result string         `json:"result" enum:"closed,requested,already_decided,already_terminal" doc:"What the cancel did: closed (the operation had not started and is now closed canceled), requested (it is executing; the cancel is recorded and its outcome says who won), already_decided (it is settling; nothing was written) or already_terminal (it was closed)."`
	Op     ConversationOp `json:"op" doc:"The operation after the cancel."`
}

// ConversationOpCancelOutput is the Huma output of a cancel.
type ConversationOpCancelOutput struct {
	Body ConversationOpCancelResult
}

// SessionTurn is a session's turn as its conversation lane's turn observer
// last recorded it (X11 §5.3). Every value is an observation with an age,
// never proof that a turn is or is not running.
type SessionTurn struct {
	Generation int64                `json:"generation" doc:"The session generation the turn cells describe."`
	State      string               `json:"state" doc:"running, ended, or unknown (never observed, or the cells describe another generation). Open: a newer build may add values."`
	ID         string               `json:"id,omitempty" doc:"The running turn's id, as the runtime names it."`
	At         *time.Time           `json:"at,omitempty" doc:"When the observer saw the current state begin."`
	LastID     string               `json:"last_id,omitempty" doc:"The last turn seen to end."`
	LastEnd    string               `json:"last_end,omitempty" doc:"Why the last turn ended (completed, interrupted, superseded, ...)."`
	LastAt     *time.Time           `json:"last_at,omitempty" doc:"When the observer saw the last turn end."`
	ObservedAt *time.Time           `json:"observed_at,omitempty" doc:"The observer's clock at the observation that last changed the cells."`
	Caps       []SessionTurnCapPair `json:"caps,omitempty" doc:"The runtime's turn capabilities (gc.rt.turn.caps): the turn axes, then source, interrupt and replace."`
}

// SessionTurnCapPair is one entry of the turn capabilities stamp.
type SessionTurnCapPair struct {
	Key   string `json:"key" doc:"Capability name, e.g. start, end, fence, link, source, interrupt, replace."`
	Value string `json:"value" doc:"Capability value."`
}
