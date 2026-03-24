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

type CallerKind string

const (
	CallerController CallerKind = "controller"
	CallerAdapter    CallerKind = "adapter"
)

type Caller struct {
	Kind      CallerKind
	ID        string
	Provider  string
	AccountID string
}

type ConversationKind string

const (
	ConversationDM     ConversationKind = "dm"
	ConversationRoom   ConversationKind = "room"
	ConversationThread ConversationKind = "thread"
)

type ConversationRef struct {
	ScopeID              string
	Provider             string
	AccountID            string
	ConversationID       string
	ParentConversationID string
	Kind                 ConversationKind
}

type InboundPayload struct {
	Body        []byte
	ContentType string
	Headers     map[string][]string
	ReceivedAt  time.Time
}

type ExternalActor struct {
	ID          string
	DisplayName string
	IsBot       bool
}

type ExternalAttachment struct {
	ProviderID string
	URL        string
	MIMEType   string
}

type ExternalInboundMessage struct {
	ProviderMessageID string
	Conversation      ConversationRef
	Actor             ExternalActor
	Text              string
	ExplicitTarget    string
	ReplyToMessageID  string
	Attachments       []ExternalAttachment
	DedupKey          string
	ReceivedAt        time.Time
}

type BindingStatus string

const (
	BindingActive BindingStatus = "active"
	BindingEnded  BindingStatus = "ended"
)

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

type ExternalOriginEnvelope struct {
	Conversation      ConversationRef
	BindingID         string
	BindingGeneration int64
	Passive           bool
}

type AdapterCapabilities struct {
	SupportsChildConversations bool
	SupportsAttachments        bool
	MaxMessageLength           int
}

type PublishRequest struct {
	Conversation     ConversationRef
	Text             string
	ReplyToMessageID string
	IdempotencyKey   string
	Metadata         map[string]string
}

type PublishFailureKind string

const (
	PublishFailureUnsupported PublishFailureKind = "unsupported"
	PublishFailureTransient   PublishFailureKind = "transient"
	PublishFailureRateLimited PublishFailureKind = "rate_limited"
	PublishFailurePermanent   PublishFailureKind = "permanent"
	PublishFailureAuth        PublishFailureKind = "auth"
	PublishFailureNotFound    PublishFailureKind = "not_found"
)

type PublishReceipt struct {
	MessageID    string
	Conversation ConversationRef
	Delivered    bool
	FailureKind  PublishFailureKind
	RetryAfter   time.Duration
	Metadata     map[string]string
}

var ErrAdapterUnsupported = errors.New("adapter unsupported")

type TranscriptMessageKind string

const (
	TranscriptMessageInbound  TranscriptMessageKind = "inbound"
	TranscriptMessageOutbound TranscriptMessageKind = "outbound"
)

type TranscriptProvenance string

const (
	TranscriptProvenanceLive     TranscriptProvenance = "live"
	TranscriptProvenanceHydrated TranscriptProvenance = "hydrated"
)

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

type MembershipBackfillPolicy string

const (
	MembershipBackfillAll       MembershipBackfillPolicy = "all"
	MembershipBackfillSinceJoin MembershipBackfillPolicy = "since_join"
)

type MembershipOwner string

const (
	MembershipOwnerManual  MembershipOwner = "manual"
	MembershipOwnerBinding MembershipOwner = "binding"
	MembershipOwnerGroup   MembershipOwner = "group"
)

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

type HydrationStatus string

const (
	HydrationLiveOnly HydrationStatus = "live_only"
	HydrationPending  HydrationStatus = "pending"
	HydrationComplete HydrationStatus = "complete"
	HydrationFailed   HydrationStatus = "failed"
)

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

type GroupMode string

const (
	GroupModeLauncher GroupMode = "launcher"
)

type FanoutPolicy struct {
	Enabled                    bool
	AllowUntargetedPublication bool
	MaxPeerTriggeredPublishes  int
	MaxTotalPeerDeliveries     int
}

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

type ConversationGroupParticipant struct {
	ID        string
	GroupID   string
	Handle    string
	SessionID string
	Public    bool
	Metadata  map[string]string
}

type GroupRouteMatch string

const (
	GroupRouteExplicitTarget GroupRouteMatch = "explicit_target"
	GroupRouteLastAddressed  GroupRouteMatch = "last_addressed"
	GroupRouteDefault        GroupRouteMatch = "default"
	GroupRouteNoMatch        GroupRouteMatch = "no_match"
)

type GroupRouteDecision struct {
	Match           GroupRouteMatch
	TargetSessionID string
	UpdateCursor    bool
}

type BindInput struct {
	Conversation ConversationRef
	SessionID    string
	ExpiresAt    *time.Time
	Metadata     map[string]string
	Now          time.Time
}

type UnbindInput struct {
	Conversation *ConversationRef
	SessionID    string
	Now          time.Time
}

type EnsureGroupInput struct {
	RootConversation    ConversationRef
	Mode                GroupMode
	DefaultHandle       string
	LastAddressedHandle string
	FanoutPolicy        FanoutPolicy
	Metadata            map[string]string
}

type UpsertParticipantInput struct {
	GroupID   string
	Handle    string
	SessionID string
	Public    bool
	Metadata  map[string]string
}

type RemoveParticipantInput struct {
	GroupID string
	Handle  string
}

type UpdateCursorInput struct {
	RootConversation ConversationRef
	Handle           string
}

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

type EnsureMembershipInput struct {
	Caller         Caller
	Conversation   ConversationRef
	SessionID      string
	BackfillPolicy MembershipBackfillPolicy
	Owner          MembershipOwner
	Metadata       map[string]string
	Now            time.Time
}

type UpdateMembershipInput struct {
	Caller         Caller
	Conversation   ConversationRef
	SessionID      string
	BackfillPolicy MembershipBackfillPolicy
	Metadata       map[string]string
}

type RemoveMembershipInput struct {
	Caller       Caller
	Conversation ConversationRef
	SessionID    string
	Owner        MembershipOwner
	Now          time.Time
}

type ListTranscriptInput struct {
	Caller        Caller
	Conversation  ConversationRef
	AfterSequence int64
	Limit         int
}

type ListBackfillInput struct {
	Caller       Caller
	Conversation ConversationRef
	SessionID    string
	Limit        int
}

type AckMembershipInput struct {
	Caller       Caller
	Conversation ConversationRef
	SessionID    string
	Sequence     int64
}

type BindingService interface {
	Bind(ctx context.Context, caller Caller, input BindInput) (SessionBindingRecord, error)
	ResolveByConversation(ctx context.Context, ref ConversationRef) (*SessionBindingRecord, error)
	ListBySession(ctx context.Context, sessionID string) ([]SessionBindingRecord, error)
	Touch(ctx context.Context, caller Caller, bindingID string, now time.Time) error
	Unbind(ctx context.Context, caller Caller, input UnbindInput) ([]SessionBindingRecord, error)
}

type DeliveryContextService interface {
	Record(ctx context.Context, caller Caller, input DeliveryContextRecord) error
	Resolve(ctx context.Context, sessionID string, ref ConversationRef) (*DeliveryContextRecord, error)
	ClearForConversation(ctx context.Context, sessionID string, ref ConversationRef) error
}

type GroupService interface {
	EnsureGroup(ctx context.Context, caller Caller, input EnsureGroupInput) (ConversationGroupRecord, error)
	UpsertParticipant(ctx context.Context, caller Caller, input UpsertParticipantInput) (ConversationGroupParticipant, error)
	RemoveParticipant(ctx context.Context, caller Caller, input RemoveParticipantInput) error
	ResolveInbound(ctx context.Context, event ExternalInboundMessage) (*GroupRouteDecision, error)
	UpdateCursor(ctx context.Context, caller Caller, input UpdateCursorInput) error
}

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

type TransportAdapter interface {
	Name() string
	Capabilities() AdapterCapabilities
	VerifyAndNormalizeInbound(ctx context.Context, payload InboundPayload) (*ExternalInboundMessage, error)
	Publish(ctx context.Context, req PublishRequest) (*PublishReceipt, error)
	EnsureChildConversation(ctx context.Context, ref ConversationRef, label string) (*ConversationRef, error)
}
