package extmsg

import "github.com/gastownhall/gascity/internal/events"

// Extmsg event payloads. Each type implements events.Payload so it
// flows through the bus's central registry and emerges on the typed
// /v0/events/stream wire with a named schema (Principle 7).
//
// Event type constants live in internal/events (events.ExtMsg*).

// InboundEventPayload is emitted on events.ExtMsgInbound ("extmsg.inbound").
// Actor is the inbound speaker's display name; TargetSession is the
// resolved recipient session and TargetAgent the bound agent identity for
// agent-bound conversations (both empty if no routing match).
type InboundEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	Actor          string `json:"actor"`
	TargetSession  string `json:"target_session"`
	TargetAgent    string `json:"target_agent,omitempty"`
}

// IsEventPayload marks InboundEventPayload as an events.Payload variant.
func (InboundEventPayload) IsEventPayload() {}

// OutboundEventPayload is emitted on "extmsg.outbound" events.
type OutboundEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	Session        string `json:"session"`
	MessageID      string `json:"message_id"`
}

// IsEventPayload marks OutboundEventPayload as an events.Payload variant.
func (OutboundEventPayload) IsEventPayload() {}

// OutboundChannelMismatchPayload is emitted on
// events.ExtMsgOutboundChannelMismatch when a session tries to publish to a
// conversation owned by a different session. PostingSession is the caller
// that attempted the publish; OwnerSession is the session that actually owns
// the target conversation's binding. The publish is rejected — this payload
// makes the cross-wire observable rather than silent (RCA gc-5aie6).
type OutboundChannelMismatchPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	PostingSession string `json:"posting_session"`
	OwnerSession   string `json:"owner_session"`
}

// IsEventPayload marks OutboundChannelMismatchPayload as an events.Payload variant.
func (OutboundChannelMismatchPayload) IsEventPayload() {}

// BoundEventPayload is emitted on events.ExtMsgBound (binding a
// conversation to a session or to a configured agent identity).
type BoundEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	AgentName      string `json:"agent_name,omitempty"`
}

// IsEventPayload marks BoundEventPayload as an events.Payload variant.
func (BoundEventPayload) IsEventPayload() {}

// UnboundEventPayload is emitted on events.ExtMsgUnbound.
type UnboundEventPayload struct {
	SessionID string `json:"session_id"`
	Count     int    `json:"count"`
}

// IsEventPayload marks UnboundEventPayload as an events.Payload variant.
func (UnboundEventPayload) IsEventPayload() {}

// GroupCreatedEventPayload is emitted on events.ExtMsgGroupCreated.
type GroupCreatedEventPayload struct {
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	Mode           string `json:"mode"`
}

// IsEventPayload marks GroupCreatedEventPayload as an events.Payload variant.
func (GroupCreatedEventPayload) IsEventPayload() {}

// AdapterEventPayload is emitted on events.ExtMsgAdapterAdded and
// events.ExtMsgAdapterRemoved — both carry the same (provider, account)
// identity pair.
type AdapterEventPayload struct {
	Provider  string `json:"provider"`
	AccountID string `json:"account_id"`
}

// IsEventPayload marks AdapterEventPayload as an events.Payload variant.
func (AdapterEventPayload) IsEventPayload() {}

func init() {
	events.RegisterPayload(events.ExtMsgBound, BoundEventPayload{})
	events.RegisterPayload(events.ExtMsgUnbound, UnboundEventPayload{})
	events.RegisterPayload(events.ExtMsgGroupCreated, GroupCreatedEventPayload{})
	events.RegisterPayload(events.ExtMsgAdapterAdded, AdapterEventPayload{})
	events.RegisterPayload(events.ExtMsgAdapterRemoved, AdapterEventPayload{})
	events.RegisterPayload(events.ExtMsgInbound, InboundEventPayload{})
	events.RegisterPayload(events.ExtMsgOutbound, OutboundEventPayload{})
	events.RegisterPayload(events.ExtMsgOutboundChannelMismatch, OutboundChannelMismatchPayload{})
}
