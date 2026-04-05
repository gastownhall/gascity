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

// CallerKind identifies the type of caller making an extmsg request.
type CallerKind string

const (
	// CallerController identifies a controller-originated call.
	CallerController CallerKind = "controller"
	// CallerAdapter identifies an adapter-originated call.
	CallerAdapter CallerKind = "adapter"
)

// Caller identifies who is making an extmsg request.
type Caller struct {
	Kind      CallerKind `json:"kind"`
	ID        string     `json:"id"`
	Provider  string     `json:"provider"`
	AccountID string     `json:"account_id"`
}

// ConversationKind classifies the shape of a conversation.
type ConversationKind string

const (
	// ConversationDM is a direct message conversation.
	ConversationDM ConversationKind = "dm"
	// ConversationRoom is a room/channel conversation.
	ConversationRoom ConversationKind = "room"
	// ConversationThread is a threaded conversation.
	ConversationThread ConversationKind = "thread"
)

// ConversationRef uniquely identifies a conversation across providers.
type ConversationRef struct {
	ScopeID              string           `json:"scope_id"`
	Provider             string           `json:"provider"`
	AccountID            string           `json:"account_id"`
	ConversationID       string           `json:"conversation_id"`
	ParentConversationID string           `json:"parent_conversation_id,omitempty"`
	Kind                 ConversationKind `json:"kind"`
}

// InboundPayload carries the raw inbound webhook payload.
type InboundPayload struct {
	Body        []byte
	ContentType string
	Headers     map[string][]string
	ReceivedAt  time.Time
}

// ExternalActor represents a user or bot on an external platform.
type ExternalActor struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	IsBot       bool   `json:"is_bot"`
}

// ExternalAttachment represents a file attached to an external message.
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

const (
	// BindingActive indicates the binding is currently active.
	BindingActive BindingStatus = "active"
	// BindingEnded indicates the binding has been terminated.
	BindingEnded BindingStatus = "ended"
)

// SessionBindingRecord links a conversation to a session.
type SessionBindingRecord struct {
	ID                string
	SchemaVersion     int
	Conversation      ConversationRef
	SessionID         string
	Status            BindingStatus
	BoundAt           time.Time
	ExpiresAt         *time.Time
	BindingGeneration int64
	Metadata          map[string]string
}

// DeliveryContextRecord tracks outbound delivery state for a session-conversation pair.
type DeliveryContextRecord struct {
	ID                string
	SchemaVersion     int
	SessionID         string
	Conversation      ConversationRef
	BindingGeneration int64
	LastPublishedAt   time.Time
	LastMessageID     string
	SourceSessionID   string
	Metadata          map[string]string
}

// ExternalOriginEnvelope carries binding context for externally-originated messages.
type ExternalOriginEnvelope struct {
	Conversation      ConversationRef
	BindingID         string
	BindingGeneration int64
	Passive           bool
}

// AdapterCapabilities describes what a transport adapter supports.
type AdapterCapabilities struct {
	SupportsChildConversations bool
	SupportsAttachments        bool
	MaxMessageLength           int
}

// PublishRequest is a request to publish a message to an external conversation.
type PublishRequest struct {
	Conversation     ConversationRef
	Text             string
	ReplyToMessageID string
	IdempotencyKey   string
	Metadata         map[string]string
}

// PublishFailureKind classifies the reason a publish attempt failed.
type PublishFailureKind string

const (
	// PublishFailureUnsupported means the adapter does not support this operation.
	PublishFailureUnsupported PublishFailureKind = "unsupported"
	// PublishFailureTransient means a temporary failure occurred.
	PublishFailureTransient PublishFailureKind = "transient"
	// PublishFailureRateLimited means the request was rate-limited.
	PublishFailureRateLimited PublishFailureKind = "rate_limited"
	// PublishFailurePermanent means a permanent failure occurred.
	PublishFailurePermanent PublishFailureKind = "permanent"
	// PublishFailureAuth means an authentication failure occurred.
	PublishFailureAuth PublishFailureKind = "auth"
	// PublishFailureNotFound means the target conversation was not found.
	PublishFailureNotFound PublishFailureKind = "not_found"
)

// PublishReceipt is the result of a publish attempt.
type PublishReceipt struct {
	MessageID    string
	Conversation ConversationRef
	Delivered    bool
	FailureKind  PublishFailureKind
	RetryAfter   time.Duration
	Metadata     map[string]string
}

// ErrAdapterUnsupported is returned when the adapter does not support the requested operation.
var ErrAdapterUnsupported = errors.New("adapter unsupported")

// TranscriptMessageKind classifies a transcript entry as inbound or outbound.
type TranscriptMessageKind string

const (
	// TranscriptMessageInbound is a message received from the external platform.
	TranscriptMessageInbound TranscriptMessageKind = "inbound"
	// TranscriptMessageOutbound is a message sent to the external platform.
	TranscriptMessageOutbound TranscriptMessageKind = "outbound"
)

// TranscriptProvenance indicates how a transcript entry was obtained.
type TranscriptProvenance string

const (
	// TranscriptProvenanceLive means the entry was captured in real time.
	TranscriptProvenanceLive TranscriptProvenance = "live"
	// TranscriptProvenanceHydrated means the entry was backfilled from history.
	TranscriptProvenanceHydrated TranscriptProvenance = "hydrated"
)

// ConversationTranscriptRecord is a single entry in a conversation transcript.
type ConversationTranscriptRecord struct {
	ID                string
	SchemaVersion     int
	Conversation      ConversationRef
	Sequence          int64
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

// MembershipBackfillPolicy controls how much transcript history a member receives.
type MembershipBackfillPolicy string

const (
	// MembershipBackfillAll delivers the entire transcript history.
	MembershipBackfillAll MembershipBackfillPolicy = "all"
	// MembershipBackfillSinceJoin delivers only entries since the member joined.
	MembershipBackfillSinceJoin MembershipBackfillPolicy = "since_join"
)

// MembershipOwner identifies what created a membership record.
type MembershipOwner string

const (
	// MembershipOwnerManual means the membership was created manually.
	MembershipOwnerManual MembershipOwner = "manual"
	// MembershipOwnerBinding means the membership was created by a binding.
	MembershipOwnerBinding MembershipOwner = "binding"
	// MembershipOwnerGroup means the membership was created by a group.
	MembershipOwnerGroup MembershipOwner = "group"
)

// ConversationMembershipRecord tracks a session's membership in a conversation.
type ConversationMembershipRecord struct {
	ID               string
	SchemaVersion    int
	Conversation     ConversationRef
	SessionID        string
	JoinedAt         time.Time
	JoinedSequence   int64
	LastReadSequence int64
	BackfillPolicy   MembershipBackfillPolicy
	ManualBackfill   MembershipBackfillPolicy
	Owners           []MembershipOwner
	Metadata         map[string]string
}

// HydrationStatus tracks the state of transcript hydration for a conversation.
type HydrationStatus string

const (
	// HydrationLiveOnly means only live messages are available.
	HydrationLiveOnly HydrationStatus = "live_only"
	// HydrationPending means hydration has been requested but not completed.
	HydrationPending HydrationStatus = "pending"
	// HydrationComplete means hydration finished successfully.
	HydrationComplete HydrationStatus = "complete"
	// HydrationFailed means hydration failed.
	HydrationFailed HydrationStatus = "failed"
)

// ConversationTranscriptStateRecord tracks the global state of a conversation's transcript.
type ConversationTranscriptStateRecord struct {
	ID                        string
	SchemaVersion             int
	Conversation              ConversationRef
	NextSequence              int64
	EarliestAvailableSequence int64
	HydrationStatus           HydrationStatus
	OldestHydratedMessageID   string
	MaxRetainedEntries        int
	Metadata                  map[string]string
}

// GroupMode defines the operating mode of a conversation group.
type GroupMode string

const (
	// GroupModeLauncher routes messages through a launcher participant.
	GroupModeLauncher GroupMode = "launcher"
)

// FanoutPolicy controls how messages are distributed within a group.
type FanoutPolicy struct {
	Enabled                    bool
	AllowUntargetedPublication bool
	MaxPeerTriggeredPublishes  int
	MaxTotalPeerDeliveries     int
}

// ConversationGroupRecord defines a group of related conversations.
type ConversationGroupRecord struct {
	ID                  string
	SchemaVersion       int
	RootConversation    ConversationRef
	Mode                GroupMode
	DefaultHandle       string
	LastAddressedHandle string
	FanoutPolicy        FanoutPolicy
	Metadata            map[string]string
}

// ConversationGroupParticipant represents a participant in a conversation group.
type ConversationGroupParticipant struct {
	ID        string
	GroupID   string
	Handle    string
	SessionID string
	Public    bool
	Metadata  map[string]string
}

// GroupRouteMatch classifies how a message was routed within a group.
type GroupRouteMatch string

const (
	// GroupRouteExplicitTarget means the message explicitly targeted a participant.
	GroupRouteExplicitTarget GroupRouteMatch = "explicit_target"
	// GroupRouteLastAddressed means the message was routed to the last addressed participant.
	GroupRouteLastAddressed GroupRouteMatch = "last_addressed"
	// GroupRouteDefault means the message was routed to the default participant.
	GroupRouteDefault GroupRouteMatch = "default"
	// GroupRouteNoMatch means no routing match was found.
	GroupRouteNoMatch GroupRouteMatch = "no_match"
)

// GroupRouteDecision is the result of routing an inbound message within a group.
type GroupRouteDecision struct {
	Match           GroupRouteMatch
	TargetSessionID string
	UpdateCursor    bool
}

// BindInput is the input for creating a session binding.
type BindInput struct {
	Conversation ConversationRef
	SessionID    string
	ExpiresAt    *time.Time
	Metadata     map[string]string
	Now          time.Time
}

// UnbindInput is the input for removing a session binding.
type UnbindInput struct {
	Conversation *ConversationRef
	SessionID    string
	Now          time.Time
}

// EnsureGroupInput is the input for creating or updating a conversation group.
type EnsureGroupInput struct {
	RootConversation    ConversationRef
	Mode                GroupMode
	DefaultHandle       string
	LastAddressedHandle string
	FanoutPolicy        FanoutPolicy
	Metadata            map[string]string
}

// UpsertParticipantInput is the input for adding or updating a group participant.
type UpsertParticipantInput struct {
	GroupID   string
	Handle    string
	SessionID string
	Public    bool
	Metadata  map[string]string
}

// RemoveParticipantInput is the input for removing a group participant.
type RemoveParticipantInput struct {
	GroupID string
	Handle  string
}

// UpdateCursorInput is the input for updating the last-addressed cursor.
type UpdateCursorInput struct {
	RootConversation ConversationRef
	Handle           string
}

// AppendTranscriptInput is the input for appending a transcript entry.
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

// EnsureMembershipInput is the input for creating or updating a conversation membership.
type EnsureMembershipInput struct {
	Caller         Caller
	Conversation   ConversationRef
	SessionID      string
	BackfillPolicy MembershipBackfillPolicy
	Owner          MembershipOwner
	Metadata       map[string]string
	Now            time.Time
}

// UpdateMembershipInput is the input for updating an existing membership.
type UpdateMembershipInput struct {
	Caller         Caller
	Conversation   ConversationRef
	SessionID      string
	BackfillPolicy MembershipBackfillPolicy
	Metadata       map[string]string
}

// RemoveMembershipInput is the input for removing a membership.
type RemoveMembershipInput struct {
	Caller       Caller
	Conversation ConversationRef
	SessionID    string
	Owner        MembershipOwner
	Now          time.Time
}

// ListTranscriptInput is the input for listing transcript entries.
type ListTranscriptInput struct {
	Caller        Caller
	Conversation  ConversationRef
	AfterSequence int64
	Limit         int
}

// ListBackfillInput is the input for listing backfill entries for a member.
type ListBackfillInput struct {
	Caller       Caller
	Conversation ConversationRef
	SessionID    string
	Limit        int
}

// AckMembershipInput is the input for acknowledging transcript entries up to a sequence.
type AckMembershipInput struct {
	Caller       Caller
	Conversation ConversationRef
	SessionID    string
	Sequence     int64
}

// BindingService manages session-to-conversation bindings.
type BindingService interface {
	Bind(ctx context.Context, caller Caller, input BindInput) (SessionBindingRecord, error)
	ResolveByConversation(ctx context.Context, ref ConversationRef) (*SessionBindingRecord, error)
	ListBySession(ctx context.Context, sessionID string) ([]SessionBindingRecord, error)
	Touch(ctx context.Context, caller Caller, bindingID string, now time.Time) error
	Unbind(ctx context.Context, caller Caller, input UnbindInput) ([]SessionBindingRecord, error)
}

// DeliveryContextService tracks outbound delivery state per session-conversation pair.
type DeliveryContextService interface {
	Record(ctx context.Context, caller Caller, input DeliveryContextRecord) error
	Resolve(ctx context.Context, sessionID string, ref ConversationRef) (*DeliveryContextRecord, error)
	ClearForConversation(ctx context.Context, sessionID string, ref ConversationRef) error
}

// GroupService manages conversation groups and participant routing.
type GroupService interface {
	EnsureGroup(ctx context.Context, caller Caller, input EnsureGroupInput) (ConversationGroupRecord, error)
	FindByConversation(ctx context.Context, caller Caller, ref ConversationRef) (*ConversationGroupRecord, error)
	UpsertParticipant(ctx context.Context, caller Caller, input UpsertParticipantInput) (ConversationGroupParticipant, error)
	RemoveParticipant(ctx context.Context, caller Caller, input RemoveParticipantInput) error
	ResolveInbound(ctx context.Context, event ExternalInboundMessage) (*GroupRouteDecision, error)
	UpdateCursor(ctx context.Context, caller Caller, input UpdateCursorInput) error
}

// TranscriptService manages conversation transcripts and memberships.
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

// TransportAdapter bridges between the SDK and an external messaging platform.
type TransportAdapter interface {
	Name() string
	Capabilities() AdapterCapabilities
	VerifyAndNormalizeInbound(ctx context.Context, payload InboundPayload) (*ExternalInboundMessage, error)
	Publish(ctx context.Context, req PublishRequest) (*PublishReceipt, error)
	EnsureChildConversation(ctx context.Context, ref ConversationRef, label string) (*ConversationRef, error)
}
