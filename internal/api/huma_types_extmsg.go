package api

// ExtMsg Huma input/output types. Mirror cmd/gc handlers in
// huma_handlers_extmsg.go. Split out of the original huma_types.go
// for navigability.

import (
	"github.com/gastownhall/gascity/internal/extmsg"
)

// --- ExtMsg types ---

// ExtMsgInboundInput is the Huma input for POST /v0/city/{cityName}/extmsg/inbound.
//
// Provider and AccountID are runtime-state-dependent: required only
// when Message is nil (the "raw payload" path). When Message is set,
// the payload is already normalized and provider/account aren't used.
// That dispatch lives in the handler, so the body fields stay
// optional here; the handler enforces the raw-path requirement.
type ExtMsgInboundInput struct {
	CityScope
	Body struct {
		Message   *extmsg.ExternalInboundMessage `json:"message,omitempty" doc:"Pre-normalized inbound message."`
		Provider  string                         `json:"provider,omitempty" doc:"Provider name for raw payloads (required when message is absent)."`
		AccountID string                         `json:"account_id,omitempty" doc:"Account ID for raw payloads (required when message is absent)."`
		Payload   []byte                         `json:"payload,omitempty" doc:"Raw payload bytes."`
	}
}

// ExtMsgInboundOutput is the Huma output for POST /v0/extmsg/inbound.
type ExtMsgInboundOutput struct {
	Body extmsg.InboundResult
}

// ExtMsgOutboundInput is the Huma input for POST /v0/city/{cityName}/extmsg/outbound.
type ExtMsgOutboundInput struct {
	CityScope
	Body struct {
		SessionID        string                 `json:"session_id" minLength:"1" doc:"Session ID."`
		Conversation     extmsg.ConversationRef `json:"conversation,omitempty" doc:"Target conversation."`
		Text             string                 `json:"text,omitempty" doc:"Message text."`
		ReplyToMessageID string                 `json:"reply_to_message_id,omitempty" doc:"Message ID to reply to."`
		IdempotencyKey   string                 `json:"idempotency_key,omitempty" doc:"Idempotency key."`
	}
}

// ExtMsgOutboundOutput is the Huma output for POST /v0/extmsg/outbound.
type ExtMsgOutboundOutput struct {
	Body extmsg.OutboundResult
}

// ExtMsgBindingListInput is the Huma input for GET /v0/city/{cityName}/extmsg/bindings.
type ExtMsgBindingListInput struct {
	CityScope
	SessionID string `query:"session_id" required:"false" doc:"Session ID to list bindings for."`
}

// ExtMsgBindInput is the Huma input for POST /v0/city/{cityName}/extmsg/bind.
type ExtMsgBindInput struct {
	CityScope
	Body struct {
		Conversation extmsg.ConversationRef `json:"conversation,omitempty" doc:"Conversation to bind."`
		SessionID    string                 `json:"session_id" minLength:"1" doc:"Session ID to bind."`
		Metadata     map[string]string      `json:"metadata,omitempty" doc:"Optional binding metadata."`
	}
}

// ExtMsgBindOutput is the Huma output for POST /v0/extmsg/bind.
type ExtMsgBindOutput struct {
	Body extmsg.SessionBindingRecord
}

// ExtMsgUnbindInput is the Huma input for POST /v0/city/{cityName}/extmsg/unbind.
type ExtMsgUnbindInput struct {
	CityScope
	Body struct {
		Conversation *extmsg.ConversationRef `json:"conversation,omitempty" doc:"Conversation to unbind (nil = all)."`
		SessionID    string                  `json:"session_id" minLength:"1" doc:"Session ID to unbind."`
	}
}

// ExtMsgUnbindOutput is the Huma output for POST /v0/extmsg/unbind.
// ExtMsgUnbindBody is the response body for POST /v0/extmsg/unbind.
type ExtMsgUnbindBody struct {
	Unbound []extmsg.SessionBindingRecord `json:"unbound" doc:"Bindings that were removed."`
}

type ExtMsgUnbindOutput struct {
	Body ExtMsgUnbindBody
}

// ExtMsgGroupLookupInput is the Huma input for GET /v0/city/{cityName}/extmsg/groups.
type ExtMsgGroupLookupInput struct {
	CityScope
	ScopeID        string `query:"scope_id" required:"false" doc:"Scope ID."`
	Provider       string `query:"provider" required:"false" doc:"Provider name."`
	AccountID      string `query:"account_id" required:"false" doc:"Account ID."`
	ConversationID string `query:"conversation_id" required:"false" doc:"Conversation ID."`
	Kind           string `query:"kind" required:"false" doc:"Conversation kind."`
}

// ExtMsgGroupOutput is the Huma output for GET /v0/extmsg/groups.
type ExtMsgGroupOutput struct {
	Body extmsg.ConversationGroupRecord
}

// ExtMsgGroupEnsureInput is the Huma input for POST /v0/city/{cityName}/extmsg/groups.
type ExtMsgGroupEnsureInput struct {
	CityScope
	Body struct {
		RootConversation extmsg.ConversationRef `json:"root_conversation,omitempty" doc:"Root conversation reference."`
		Mode             extmsg.GroupMode       `json:"mode,omitempty" doc:"Group mode (launcher, etc.)."`
		DefaultHandle    string                 `json:"default_handle,omitempty" doc:"Default handle for the group."`
		Metadata         map[string]string      `json:"metadata,omitempty" doc:"Group metadata."`
	}
}

// ExtMsgGroupEnsureOutput is the Huma output for POST /v0/extmsg/groups.
type ExtMsgGroupEnsureOutput struct {
	Body extmsg.ConversationGroupRecord
}

// ExtMsgParticipantUpsertInput is the Huma input for POST /v0/city/{cityName}/extmsg/participants.
type ExtMsgParticipantUpsertInput struct {
	CityScope
	Body struct {
		GroupID   string            `json:"group_id" minLength:"1" doc:"Group ID."`
		Handle    string            `json:"handle" minLength:"1" doc:"Participant handle."`
		SessionID string            `json:"session_id" minLength:"1" doc:"Session ID."`
		Public    bool              `json:"public,omitempty" doc:"Whether participant is public."`
		Metadata  map[string]string `json:"metadata,omitempty" doc:"Participant metadata."`
	}
}

// ExtMsgParticipantOutput is the Huma output for POST /v0/extmsg/participants.
type ExtMsgParticipantOutput struct {
	Body extmsg.ConversationGroupParticipant
}

// ExtMsgParticipantRemoveInput is the Huma input for DELETE /v0/city/{cityName}/extmsg/participants.
type ExtMsgParticipantRemoveInput struct {
	CityScope
	Body struct {
		GroupID string `json:"group_id" minLength:"1" doc:"Group ID."`
		Handle  string `json:"handle" minLength:"1" doc:"Participant handle."`
	}
}

// ExtMsgTranscriptListInput is the Huma input for GET /v0/city/{cityName}/extmsg/transcript.
type ExtMsgTranscriptListInput struct {
	CityScope
	ScopeID              string `query:"scope_id" required:"false" doc:"Scope ID."`
	Provider             string `query:"provider" required:"false" doc:"Provider name."`
	AccountID            string `query:"account_id" required:"false" doc:"Account ID."`
	ConversationID       string `query:"conversation_id" required:"false" doc:"Conversation ID."`
	ParentConversationID string `query:"parent_conversation_id" required:"false" doc:"Parent conversation ID."`
	Kind                 string `query:"kind" required:"false" doc:"Conversation kind."`
}

// ExtMsgTranscriptAckInput is the Huma input for POST /v0/city/{cityName}/extmsg/transcript/ack.
type ExtMsgTranscriptAckInput struct {
	CityScope
	Body struct {
		Conversation extmsg.ConversationRef `json:"conversation,omitempty" doc:"Conversation to acknowledge."`
		SessionID    string                 `json:"session_id" minLength:"1" doc:"Session ID."`
		Sequence     int64                  `json:"sequence,omitempty" doc:"Sequence number to acknowledge up to."`
	}
}

// ExtMsgAdapterListInput is the Huma input for GET /v0/city/{cityName}/extmsg/adapters.
type ExtMsgAdapterListInput struct {
	CityScope
}

// ExtMsgAdapterRegisterInput is the Huma input for POST /v0/city/{cityName}/extmsg/adapters.
type ExtMsgAdapterRegisterInput struct {
	CityScope
	Body struct {
		Provider     string                     `json:"provider" minLength:"1" doc:"Provider name."`
		AccountID    string                     `json:"account_id" minLength:"1" doc:"Account ID."`
		Name         string                     `json:"name,omitempty" doc:"Adapter display name."`
		CallbackURL  string                     `json:"callback_url,omitempty" doc:"Callback URL for outbound messages."`
		Capabilities extmsg.AdapterCapabilities `json:"capabilities,omitempty" doc:"Adapter capabilities."`
	}
}

// ExtMsgAdapterRegisterOutput is the Huma output for POST /v0/extmsg/adapters.
type ExtMsgAdapterRegisterOutput struct {
	Body struct {
		Status    string `json:"status" doc:"Operation result." example:"registered"`
		Provider  string `json:"provider" doc:"Provider name."`
		AccountID string `json:"account_id" doc:"Account ID."`
		Name      string `json:"name" doc:"Adapter name."`
	}
}

// ExtMsgAdapterUnregisterInput is the Huma input for DELETE /v0/city/{cityName}/extmsg/adapters.
type ExtMsgAdapterUnregisterInput struct {
	CityScope
	Body struct {
		Provider  string `json:"provider" minLength:"1" doc:"Provider name."`
		AccountID string `json:"account_id" minLength:"1" doc:"Account ID."`
	}
}
