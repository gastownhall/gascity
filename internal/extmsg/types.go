package extmsg

import (
	"context"
	"errors"
	"time"
)

const (
	schemaVersion  = 1
	metadataPrefix = "meta."
)

// CallerKind identifies the category of an API caller.
type CallerKind string

// CallerKind constants.
const (
	CallerController CallerKind = "controller"
	CallerAdapter    CallerKind = "adapter"
)

// Caller identifies who is performing an extmsg operation.
type Caller struct {
	Kind      CallerKind `json:"kind"`
	ID        string     `json:"id"`
	Provider  string     `json:"provider"`
	AccountID string     `json:"account_id"`
}

// ConversationKind classifies a conversation topology.
type ConversationKind string

// ConversationKind constants.
const (
	ConversationDM     ConversationKind = "dm"
	ConversationRoom   ConversationKind = "room"
	ConversationThread ConversationKind = "thread"
)

// ConversationRef is a fully-qualified reference to an external conversation.
type ConversationRef struct {
	ScopeID              string           `json:"scope_id"`
	Provider             string           `json:"provider"`
	AccountID            string           `json:"account_id"`
	ConversationID       string           `json:"conversation_id"`
	ParentConversationID string           `json:"parent_conversation_id,omitempty"`
	Kind                 ConversationKind `json:"kind"`
}

// InboundPayload is the raw HTTP payload received from a transport adapter.
type InboundPayload struct {
	Body        []byte
	ContentType string
	Headers     map[string][]string
	ReceivedAt  time.Time
}

// ExternalActor represents a user or bot in an external messaging platform.
type ExternalActor struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	IsBot       bool   `json:"is_bot"`
}

// ExternalAttachment is a file or media attachment on an external message.
type ExternalAttachment struct {
	ProviderID string `json:"provider_id"`
	URL        string `json:"url"`
	MIMEType   string `json:"mime_type"`
}

// ExternalInboundMessage is a normalized inbound message from an external platform.
type ExternalInboundMessage struct {
	ProviderMessageID string               `json:"provider_message_id"`
	Conversation      ConversationRef      `json:"conversation"`
	Actor             ExternalActor        `json:"actor"`
	Text              string               `json:"text"`
	ExplicitTarget    string               `json:"explicit_target,omitempty"`
	ReplyToMessageID  string               `json:"reply_to_message_id,omitempty"`
	Attachments       []ExternalAttachment `json:"attachments,omitempty"`
	DedupKey          string               `json:"dedup_key,omitempty"`
	ReceivedAt        time.Time            `json:"received_at"`
}

// BindingStatus represents the lifecycle state of a session binding.
type BindingStatus string

// BindingStatus constants.
const (
	BindingActive BindingStatus = "active"
	BindingEnded  BindingStatus = "ended"
)

// SessionBindingRecord is the persisted state of a conversation-to-session binding.
type SessionBindingRecord struct {
	ID                string            `json:"id"`
	SchemaVersion     int               `json:"schema_version"`
	Conversation      ConversationRef   `json:"conversation"`
	SessionID         string            `json:"session_id"`
	Status            BindingStatus     `json:"status"`
	BoundAt           time.Time         `json:"bound_at"`
	ExpiresAt         *time.Time        `json:"expires_at,omitempty"`
	BindingGeneration int64             `json:"binding_generation"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

// DeliveryContextRecord tracks the last message delivered to a session for a conversation.
type DeliveryContextRecord struct {
	ID                string            `json:"id"`
	SchemaVersion     int               `json:"schema_version"`
	SessionID         string            `json:"session_id"`
	Conversation      ConversationRef   `json:"conversation"`
	BindingGeneration int64             `json:"binding_generation"`
	LastPublishedAt   time.Time         `json:"last_published_at"`
	LastMessageID     string            `json:"last_message_id"`
	SourceSessionID   string            `json:"source_session_id,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

// ExternalOriginEnvelope carries binding context for an inbound message routed to a session.
type ExternalOriginEnvelope struct {
	Conversation      ConversationRef
	BindingID         string
	BindingGeneration int64
	Passive           bool
}

// AdapterCapabilities describes the features supported by a transport adapter.
type AdapterCapabilities struct {
	SupportsChildConversations bool `json:"supports_child_conversations"`
	SupportsAttachments        bool `json:"supports_attachments"`
	MaxMessageLength           int  `json:"max_message_length"`
}

// PublishRequest is a request to publish a message to an external conversation.
type PublishRequest struct {
	Conversation     ConversationRef   `json:"conversation"`
	Text             string            `json:"text"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// PublishFailureKind classifies why a publish attempt failed.
type PublishFailureKind string

// PublishFailureKind constants.
const (
	PublishFailureUnsupported PublishFailureKind = "unsupported"
	PublishFailureTransient   PublishFailureKind = "transient"
	PublishFailureRateLimited PublishFailureKind = "rate_limited"
	PublishFailurePermanent   PublishFailureKind = "permanent"
	PublishFailureAuth        PublishFailureKind = "auth"
	PublishFailureNotFound    PublishFailureKind = "not_found"
)

// PublishReceipt is the result of a publish attempt to an external platform.
type PublishReceipt struct {
	MessageID    string             `json:"message_id,omitempty"`
	Conversation ConversationRef    `json:"conversation"`
	Delivered    bool               `json:"delivered"`
	FailureKind  PublishFailureKind `json:"failure_kind,omitempty"`
	RetryAfter   time.Duration      `json:"retry_after,omitempty"`
	Metadata     map[string]string  `json:"metadata,omitempty"`
}

// ErrAdapterUnsupported indicates the adapter does not support the requested operation.
var ErrAdapterUnsupported = errors.New("adapter unsupported")

// TranscriptMessageKind distinguishes inbound from outbound transcript entries.
type TranscriptMessageKind string

// TranscriptMessageKind constants.
const (
	TranscriptMessageInbound  TranscriptMessageKind = "inbound"
	TranscriptMessageOutbound TranscriptMessageKind = "outbound"
)

// TranscriptProvenance records how a transcript entry was obtained.
type TranscriptProvenance string

// TranscriptProvenance constants.
const (
	TranscriptProvenanceLive     TranscriptProvenance = "live"
	TranscriptProvenanceHydrated TranscriptProvenance = "hydrated"
)

// ConversationTranscriptRecord is a single entry in a conversation transcript.
type ConversationTranscriptRecord struct {
	ID                string                `json:"id"`
	SchemaVersion     int                   `json:"schema_version"`
	Conversation      ConversationRef       `json:"conversation"`
	Sequence          int64                 `json:"sequence"`
	Kind              TranscriptMessageKind `json:"kind"`
	Provenance        TranscriptProvenance  `json:"provenance"`
	ProviderMessageID string                `json:"provider_message_id"`
	Actor             ExternalActor         `json:"actor"`
	Text              string                `json:"text"`
	ExplicitTarget    string                `json:"explicit_target,omitempty"`
	ReplyToMessageID  string                `json:"reply_to_message_id,omitempty"`
	Attachments       []ExternalAttachment  `json:"attachments,omitempty"`
	SourceSessionID   string                `json:"source_session_id,omitempty"`
	CreatedAt         time.Time             `json:"created_at"`
	Metadata          map[string]string     `json:"metadata,omitempty"`
}

// MembershipBackfillPolicy controls how far back transcript entries are visible to a member.
type MembershipBackfillPolicy string

// MembershipBackfillPolicy constants.
const (
	MembershipBackfillAll       MembershipBackfillPolicy = "all"
	MembershipBackfillSinceJoin MembershipBackfillPolicy = "since_join"
)

// MembershipOwner identifies the subsystem that created a membership.
type MembershipOwner string

// MembershipOwner constants.
const (
	MembershipOwnerManual  MembershipOwner = "manual"
	MembershipOwnerBinding MembershipOwner = "binding"
	MembershipOwnerGroup   MembershipOwner = "group"
)

// ConversationMembershipRecord tracks a session's membership in a conversation.
type ConversationMembershipRecord struct {
	ID               string                   `json:"id"`
	SchemaVersion    int                      `json:"schema_version"`
	Conversation     ConversationRef          `json:"conversation"`
	SessionID        string                   `json:"session_id"`
	JoinedAt         time.Time                `json:"joined_at"`
	JoinedSequence   int64                    `json:"joined_sequence"`
	LastReadSequence int64                    `json:"last_read_sequence"`
	BackfillPolicy   MembershipBackfillPolicy `json:"backfill_policy"`
	ManualBackfill   MembershipBackfillPolicy `json:"manual_backfill,omitempty"`
	Owners           []MembershipOwner        `json:"owners"`
	Metadata         map[string]string        `json:"metadata,omitempty"`
}

// HydrationStatus tracks the state of transcript backfill from the external platform.
type HydrationStatus string

// HydrationStatus constants.
const (
	HydrationLiveOnly HydrationStatus = "live_only"
	HydrationPending  HydrationStatus = "pending"
	HydrationComplete HydrationStatus = "complete"
	HydrationFailed   HydrationStatus = "failed"
)

// ConversationTranscriptStateRecord tracks per-conversation transcript metadata.
type ConversationTranscriptStateRecord struct {
	ID                        string            `json:"id"`
	SchemaVersion             int               `json:"schema_version"`
	Conversation              ConversationRef   `json:"conversation"`
	NextSequence              int64             `json:"next_sequence"`
	EarliestAvailableSequence int64             `json:"earliest_available_sequence"`
	HydrationStatus           HydrationStatus   `json:"hydration_status"`
	OldestHydratedMessageID   string            `json:"oldest_hydrated_message_id,omitempty"`
	MaxRetainedEntries        int               `json:"max_retained_entries"`
	Metadata                  map[string]string `json:"metadata,omitempty"`
}

// GroupMode defines how a conversation group routes messages.
type GroupMode string

// GroupMode constants.
const (
	GroupModeLauncher GroupMode = "launcher"
)

// FanoutPolicy controls peer-triggered message delivery within a group.
type FanoutPolicy struct {
	Enabled                    bool `json:"enabled"`
	AllowUntargetedPublication bool `json:"allow_untargeted_publication"`
	MaxPeerTriggeredPublishes  int  `json:"max_peer_triggered_publishes"`
	MaxTotalPeerDeliveries     int  `json:"max_total_peer_deliveries"`
}

// ConversationGroupRecord is the persisted state of a conversation group.
type ConversationGroupRecord struct {
	ID                  string            `json:"id"`
	SchemaVersion       int               `json:"schema_version"`
	RootConversation    ConversationRef   `json:"root_conversation"`
	Mode                GroupMode         `json:"mode"`
	DefaultHandle       string            `json:"default_handle,omitempty"`
	LastAddressedHandle string            `json:"last_addressed_handle,omitempty"`
	FanoutPolicy        FanoutPolicy      `json:"fanout_policy"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

// ConversationGroupParticipant is a session participating in a conversation group under a handle.
type ConversationGroupParticipant struct {
	ID        string            `json:"id"`
	GroupID   string            `json:"group_id"`
	Handle    string            `json:"handle"`
	SessionID string            `json:"session_id"`
	Public    bool              `json:"public"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// GroupRouteMatch classifies how a group resolved an inbound message to a target session.
type GroupRouteMatch string

// GroupRouteMatch constants.
const (
	GroupRouteExplicitTarget GroupRouteMatch = "explicit_target"
	GroupRouteLastAddressed  GroupRouteMatch = "last_addressed"
	GroupRouteDefault        GroupRouteMatch = "default"
	GroupRouteNoMatch        GroupRouteMatch = "no_match"
)

// GroupRouteDecision is the result of routing an inbound message through a group.
type GroupRouteDecision struct {
	Match           GroupRouteMatch `json:"match"`
	TargetSessionID string          `json:"target_session_id"`
	UpdateCursor    bool            `json:"update_cursor"`
}

// BindInput contains the parameters for creating or refreshing a session binding.
type BindInput struct {
	Conversation ConversationRef
	SessionID    string
	ExpiresAt    *time.Time
	Metadata     map[string]string
	Now          time.Time
}

// UnbindInput contains the parameters for removing a session binding.
type UnbindInput struct {
	Conversation *ConversationRef
	SessionID    string
	Now          time.Time
}

// EnsureGroupInput contains the parameters for creating or updating a conversation group.
type EnsureGroupInput struct {
	RootConversation    ConversationRef
	Mode                GroupMode
	DefaultHandle       string
	LastAddressedHandle string
	FanoutPolicy        FanoutPolicy
	Metadata            map[string]string
}

// UpsertParticipantInput contains the parameters for adding or updating a group participant.
type UpsertParticipantInput struct {
	GroupID   string
	Handle    string
	SessionID string
	Public    bool
	Metadata  map[string]string
}

// RemoveParticipantInput contains the parameters for removing a group participant.
type RemoveParticipantInput struct {
	GroupID string
	Handle  string
}

// UpdateCursorInput contains the parameters for updating a group's last-addressed cursor.
type UpdateCursorInput struct {
	RootConversation ConversationRef
	Handle           string
}

// AppendTranscriptInput contains the parameters for appending a transcript entry.
type AppendTranscriptInput struct {
	Caller            Caller
	Conversation      ConversationRef
	Kind              TranscriptMessageKind
	Provenance        TranscriptProvenance
	ProviderMessageID string
	Actor             ExternalActor
	Text              string
	ExplicitTarget    string
	ReplyToMessageID  string
	Attachments       []ExternalAttachment
	SourceSessionID   string
	CreatedAt         time.Time
	Metadata          map[string]string
}

// EnsureMembershipInput contains the parameters for creating or refreshing a transcript membership.
type EnsureMembershipInput struct {
	Caller         Caller
	Conversation   ConversationRef
	SessionID      string
	BackfillPolicy MembershipBackfillPolicy
	Owner          MembershipOwner
	Metadata       map[string]string
	Now            time.Time
}

// UpdateMembershipInput contains the parameters for updating a transcript membership.
type UpdateMembershipInput struct {
	Caller         Caller
	Conversation   ConversationRef
	SessionID      string
	BackfillPolicy MembershipBackfillPolicy
	Metadata       map[string]string
}

// RemoveMembershipInput contains the parameters for removing a transcript membership.
type RemoveMembershipInput struct {
	Caller       Caller
	Conversation ConversationRef
	SessionID    string
	Owner        MembershipOwner
	Now          time.Time
}

// ListTranscriptInput contains the parameters for listing transcript entries.
type ListTranscriptInput struct {
	Caller        Caller
	Conversation  ConversationRef
	AfterSequence int64
	Limit         int
}

// ListBackfillInput contains the parameters for listing backfill entries for a session.
type ListBackfillInput struct {
	Caller       Caller
	Conversation ConversationRef
	SessionID    string
	Limit        int
}

// AckMembershipInput contains the parameters for acknowledging transcript entries up to a sequence.
type AckMembershipInput struct {
	Caller       Caller
	Conversation ConversationRef
	SessionID    string
	Sequence     int64
}

// BindingService manages conversation-to-session bindings.
type BindingService interface {
	Bind(ctx context.Context, caller Caller, input BindInput) (SessionBindingRecord, error)
	ResolveByConversation(ctx context.Context, ref ConversationRef) (*SessionBindingRecord, error)
	ListBySession(ctx context.Context, sessionID string) ([]SessionBindingRecord, error)
	Touch(ctx context.Context, caller Caller, bindingID string, now time.Time) error
	Unbind(ctx context.Context, caller Caller, input UnbindInput) ([]SessionBindingRecord, error)
}

// DeliveryContextService tracks the last message delivered to a session for each conversation.
type DeliveryContextService interface {
	Record(ctx context.Context, caller Caller, input DeliveryContextRecord) error
	Resolve(ctx context.Context, sessionID string, ref ConversationRef) (*DeliveryContextRecord, error)
	ClearForConversation(ctx context.Context, sessionID string, ref ConversationRef) error
}

// GroupService manages conversation groups, participants, and inbound routing.
type GroupService interface {
	EnsureGroup(ctx context.Context, caller Caller, input EnsureGroupInput) (ConversationGroupRecord, error)
	FindByConversation(ctx context.Context, caller Caller, ref ConversationRef) (*ConversationGroupRecord, error)
	UpsertParticipant(ctx context.Context, caller Caller, input UpsertParticipantInput) (ConversationGroupParticipant, error)
	RemoveParticipant(ctx context.Context, caller Caller, input RemoveParticipantInput) error
	ResolveInbound(ctx context.Context, event ExternalInboundMessage) (*GroupRouteDecision, error)
	UpdateCursor(ctx context.Context, caller Caller, input UpdateCursorInput) error
}

// TranscriptService manages conversation transcripts, memberships, and hydration state.
type TranscriptService interface {
	Append(ctx context.Context, input AppendTranscriptInput) (ConversationTranscriptRecord, error)
	List(ctx context.Context, input ListTranscriptInput) ([]ConversationTranscriptRecord, error)
	EnsureMembership(ctx context.Context, input EnsureMembershipInput) (ConversationMembershipRecord, error)
	UpdateMembership(ctx context.Context, input UpdateMembershipInput) (ConversationMembershipRecord, error)
	RemoveMembership(ctx context.Context, input RemoveMembershipInput) error
	ListMemberships(ctx context.Context, caller Caller, ref ConversationRef) ([]ConversationMembershipRecord, error)
	ListConversationsBySession(ctx context.Context, caller Caller, sessionID string) ([]ConversationMembershipRecord, error)
	ListBackfill(ctx context.Context, input ListBackfillInput) ([]ConversationTranscriptRecord, error)
	Ack(ctx context.Context, input AckMembershipInput) error
	BeginHydration(ctx context.Context, caller Caller, ref ConversationRef, metadata map[string]string) (ConversationTranscriptStateRecord, error)
	CompleteHydration(ctx context.Context, caller Caller, ref ConversationRef) (ConversationTranscriptStateRecord, error)
	MarkHydrationFailed(ctx context.Context, caller Caller, ref ConversationRef, metadata map[string]string) (ConversationTranscriptStateRecord, error)
	State(ctx context.Context, caller Caller, ref ConversationRef) (*ConversationTranscriptStateRecord, error)
}

// TransportAdapter is the interface that external messaging platform integrations must implement.
type TransportAdapter interface {
	Name() string
	Capabilities() AdapterCapabilities
	VerifyAndNormalizeInbound(ctx context.Context, payload InboundPayload) (*ExternalInboundMessage, error)
	Publish(ctx context.Context, req PublishRequest) (*PublishReceipt, error)
	EnsureChildConversation(ctx context.Context, ref ConversationRef, label string) (*ConversationRef, error)
}
