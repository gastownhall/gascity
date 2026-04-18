package extmsg

// EventPayload is the sealed interface implemented by every payload
// type that can flow through an extmsg event-emit callback
// (InboundDeps.EmitEvent, OutboundDeps.EmitEvent, and the API-layer
// bridge built on top of them). Sealing the interface with an
// unexported marker method keeps map[string]any and other ad-hoc
// shapes out of every emitter call site — emitting an event is a
// compile-time choice among the typed variants below (Principle 7).
type EventPayload interface {
	isExtMsgEventPayload()
}

// InboundEventPayload is emitted on "extmsg.inbound" events. Actor is
// the inbound speaker's display name; TargetSession is the resolved
// recipient session (empty if no routing match).
type InboundEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	Actor          string `json:"actor"`
	TargetSession  string `json:"target_session"`
}

func (InboundEventPayload) isExtMsgEventPayload() {}

// OutboundEventPayload is emitted on "extmsg.outbound" events.
type OutboundEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	Session        string `json:"session"`
	MessageID      string `json:"message_id"`
}

func (OutboundEventPayload) isExtMsgEventPayload() {}

// BoundEventPayload is emitted on events.ExtMsgBound (binding a
// conversation to a session).
type BoundEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
}

func (BoundEventPayload) isExtMsgEventPayload() {}

// UnboundEventPayload is emitted on events.ExtMsgUnbound.
type UnboundEventPayload struct {
	SessionID string `json:"session_id"`
	Count     int    `json:"count"`
}

func (UnboundEventPayload) isExtMsgEventPayload() {}

// GroupCreatedEventPayload is emitted on events.ExtMsgGroupCreated.
type GroupCreatedEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	Mode           string `json:"mode"`
}

func (GroupCreatedEventPayload) isExtMsgEventPayload() {}

// AdapterEventPayload is emitted on events.ExtMsgAdapterAdded and
// events.ExtMsgAdapterRemoved — both carry the same (provider, account)
// identity pair.
type AdapterEventPayload struct {
	Provider  string `json:"provider"`
	AccountID string `json:"account_id"`
}

func (AdapterEventPayload) isExtMsgEventPayload() {}
