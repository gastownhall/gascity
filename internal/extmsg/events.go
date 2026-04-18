package extmsg

import "github.com/gastownhall/gascity/internal/events"

// Extmsg event payloads. Each type implements events.Payload so it
// flows through the bus's central registry and emerges on the typed
// /v0/events/stream wire with a named schema (Principle 7).
//
// Event type constants live in internal/events (events.ExtMsg*).

// InboundEventPayload is emitted on events.ExtMsgInbound ("extmsg.inbound").
// Actor is the inbound speaker's display name; TargetSession is the
// resolved recipient session (empty if no routing match).
type InboundEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	Actor          string `json:"actor"`
	TargetSession  string `json:"target_session"`
}

func (InboundEventPayload) IsEventPayload() {}

// OutboundEventPayload is emitted on "extmsg.outbound" events.
type OutboundEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	Session        string `json:"session"`
	MessageID      string `json:"message_id"`
}

func (OutboundEventPayload) IsEventPayload() {}

// BoundEventPayload is emitted on events.ExtMsgBound (binding a
// conversation to a session).
type BoundEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
}

func (BoundEventPayload) IsEventPayload() {}

// UnboundEventPayload is emitted on events.ExtMsgUnbound.
type UnboundEventPayload struct {
	SessionID string `json:"session_id"`
	Count     int    `json:"count"`
}

func (UnboundEventPayload) IsEventPayload() {}

// GroupCreatedEventPayload is emitted on events.ExtMsgGroupCreated.
type GroupCreatedEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	Mode           string `json:"mode"`
}

func (GroupCreatedEventPayload) IsEventPayload() {}

// AdapterEventPayload is emitted on events.ExtMsgAdapterAdded and
// events.ExtMsgAdapterRemoved — both carry the same (provider, account)
// identity pair.
type AdapterEventPayload struct {
	Provider  string `json:"provider"`
	AccountID string `json:"account_id"`
}

func (AdapterEventPayload) IsEventPayload() {}

func init() {
	events.RegisterPayload(events.ExtMsgBound, BoundEventPayload{})
	events.RegisterPayload(events.ExtMsgUnbound, UnboundEventPayload{})
	events.RegisterPayload(events.ExtMsgGroupCreated, GroupCreatedEventPayload{})
	events.RegisterPayload(events.ExtMsgAdapterAdded, AdapterEventPayload{})
	events.RegisterPayload(events.ExtMsgAdapterRemoved, AdapterEventPayload{})
	events.RegisterPayload(events.ExtMsgInbound, InboundEventPayload{})
	events.RegisterPayload(events.ExtMsgOutbound, OutboundEventPayload{})
}
