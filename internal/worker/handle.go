package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

var (
	// ErrHandleConfig reports that a worker handle was constructed with an
	// incomplete or invalid configuration.
	ErrHandleConfig = errors.New("worker handle configuration is invalid")
	// ErrHistoryUnavailable reports that the worker has no discoverable
	// transcript yet.
	ErrHistoryUnavailable = errors.New("worker history is unavailable")
)

// StateHandle exposes worker lifecycle state queries.
type StateHandle interface {
	State(context.Context) (State, error)
}

// LifecycleHandle exposes worker lifecycle control operations.
type LifecycleHandle interface {
	Start(context.Context) error
	StartResolved(context.Context, string, runtime.Config) error
	Attach(context.Context) error
	Create(context.Context, CreateMode) (sessionpkg.Info, error)
	Reset(context.Context) error
	Stop(context.Context) error
	Kill(context.Context) error
	Close(context.Context) error
	Rename(context.Context, string) error
	StateHandle
}

// MessagingHandle exposes live input delivery operations.
type MessagingHandle interface {
	Message(context.Context, MessageRequest) (MessageResult, error)
	Interrupt(context.Context, InterruptRequest) error
	Nudge(context.Context, NudgeRequest) (NudgeResult, error)
}

// HistoryHandle exposes normalized transcript history reads.
type HistoryHandle interface {
	History(context.Context, HistoryRequest) (*HistorySnapshot, error)
}

// TranscriptHandle exposes provider transcript reads and agent transcript access.
type TranscriptHandle interface {
	HistoryHandle
	Transcript(context.Context, TranscriptRequest) (*TranscriptResult, error)
	TranscriptPath(context.Context) (string, error)
	AgentMappings(context.Context) ([]AgentMapping, error)
	AgentTranscript(context.Context, string) (*AgentTranscriptResult, error)
}

// InteractionHandle exposes worker blocking-interaction queries and responses.
type InteractionHandle interface {
	Pending(context.Context) (*PendingInteraction, error)
	PendingStatus(context.Context) (*PendingInteraction, bool, error)
	Respond(context.Context, InteractionResponse) error
}

// PeekHandle exposes best-effort live output peeking.
type PeekHandle interface {
	Peek(context.Context, int) (string, error)
}

// LiveObservationHandle exposes runtime presence observations for a worker target.
type LiveObservationHandle interface {
	LiveObservation(context.Context) (LiveObservation, error)
}

// Handle is the canonical in-memory worker API.
type Handle interface {
	LifecycleHandle
	MessagingHandle
	TranscriptHandle
	InteractionHandle
	PeekHandle
	LiveObservationHandle
}

// Phase captures the worker-level lifecycle state surfaced by [Handle.State].
type Phase string

const (
	// PhaseUnknown reports that the worker lifecycle phase is not yet known.
	PhaseUnknown Phase = "unknown"
	// PhaseStarting reports that the worker is starting up.
	PhaseStarting Phase = "starting"
	// PhaseReady reports that the worker is idle and ready for input.
	PhaseReady Phase = "ready"
	// PhaseBusy reports that the worker is actively processing work.
	PhaseBusy Phase = "busy"
	// PhaseBlocked reports that the worker is waiting on an interaction.
	PhaseBlocked Phase = "blocked"
	// PhaseStopping reports that shutdown is in progress.
	PhaseStopping Phase = "stopping"
	// PhaseStopped reports that the worker is not running.
	PhaseStopped Phase = "stopped"
	// PhaseFailed reports that the worker reached a terminal failure.
	PhaseFailed Phase = "failed"
)

// State is the worker-level lifecycle view.
type State struct {
	Phase       Phase               `json:"phase"`
	SessionID   string              `json:"session_id,omitempty"`
	SessionName string              `json:"session_name,omitempty"`
	Provider    string              `json:"provider,omitempty"`
	Detail      string              `json:"detail,omitempty"`
	Pending     *PendingInteraction `json:"pending,omitempty"`
}

// DeliveryIntent controls how a message should be delivered.
type DeliveryIntent string

const (
	// DeliveryIntentDefault submits a normal follow-up turn.
	DeliveryIntentDefault DeliveryIntent = "default"
	// DeliveryIntentFollowUp explicitly marks the turn as a follow-up.
	DeliveryIntentFollowUp DeliveryIntent = "follow_up"
	// DeliveryIntentInterruptNow replaces current work immediately if possible.
	DeliveryIntentInterruptNow DeliveryIntent = "interrupt_now"
)

// MessageRequest submits a user turn to the worker.
type MessageRequest struct {
	Text     string         `json:"text"`
	Delivery DeliveryIntent `json:"delivery,omitempty"`
}

// MessageResult reports whether a worker turn was queued or delivered now.
type MessageResult struct {
	Queued bool `json:"queued"`
}

// CreateMode controls how a worker session should be materialized.
type CreateMode string

const (
	// CreateModeDeferred creates the session without starting live runtime work.
	CreateModeDeferred CreateMode = "deferred"
	// CreateModeStarted creates the session and starts the live runtime.
	CreateModeStarted CreateMode = "started"
)

// InterruptRequest is reserved for future interrupt controls.
type InterruptRequest struct{}

// NudgeDelivery controls how a nudge should be delivered.
type NudgeDelivery string

const (
	// NudgeDeliveryDefault uses the provider's default nudge path.
	NudgeDeliveryDefault NudgeDelivery = "default"
	// NudgeDeliveryImmediate delivers the nudge as soon as possible.
	NudgeDeliveryImmediate NudgeDelivery = "immediate"
	// NudgeDeliveryWaitIdle waits for an idle boundary before nudging.
	NudgeDeliveryWaitIdle NudgeDelivery = "wait_idle"
)

// NudgeRequest delivers a best-effort wake or redirect message.
type NudgeRequest struct {
	Text     string          `json:"text"`
	Delivery NudgeDelivery   `json:"delivery,omitempty"`
	Source   string          `json:"source,omitempty"`
	Wake     NudgeWakePolicy `json:"wake,omitempty"`
}

// NudgeResult reports whether the requested live delivery actually happened.
type NudgeResult struct {
	Delivered bool `json:"delivered"`
}

// NudgeWakePolicy controls whether a nudge may wake a stopped session.
type NudgeWakePolicy string

const (
	// NudgeWakeIfNeeded allows the nudge to wake a stopped session first.
	NudgeWakeIfNeeded NudgeWakePolicy = "wake_if_needed"
	// NudgeWakeLiveOnly only delivers the nudge to already-live sessions.
	NudgeWakeLiveOnly NudgeWakePolicy = "live_only"
)

func normalizeNudgeWakePolicy(policy NudgeWakePolicy) NudgeWakePolicy {
	if policy == NudgeWakeLiveOnly {
		return policy
	}
	return NudgeWakeIfNeeded
}

// HistoryRequest scopes transcript loading for a worker.
type HistoryRequest struct {
	TailCompactions int    `json:"tail_compactions,omitempty"`
	LogicalID       string `json:"logical_conversation_id,omitempty"`
}

// PendingInteraction is the worker-level view of a blocking interaction.
type PendingInteraction struct {
	RequestID string            `json:"request_id,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Prompt    string            `json:"prompt,omitempty"`
	Options   []string          `json:"options,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// InteractionResponse resolves a pending interaction.
type InteractionResponse struct {
	RequestID string            `json:"request_id,omitempty"`
	Action    string            `json:"action"`
	Text      string            `json:"text,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// SessionSpec describes the concrete session materialized by a session-backed
// worker handle.
type SessionSpec struct {
	ID           string
	Profile      Profile
	Template     string
	Title        string
	Alias        string
	ExplicitName string
	Command      string
	WorkDir      string
	Provider     string
	Transport    string
	Env          map[string]string
	Resume       sessionpkg.ProviderResume
	Hints        runtime.Config
	Metadata     map[string]string
}

// SessionHandleConfig configures a [SessionHandle].
type SessionHandleConfig struct {
	Manager     *sessionpkg.Manager
	SearchPaths []string
	Adapter     SessionLogAdapter
	Recorder    events.Recorder
	Session     SessionSpec
}

// SessionHandle is the production worker handle backed by session.Manager.
type SessionHandle struct {
	mu          sync.Mutex
	manager     *sessionpkg.Manager
	adapter     SessionLogAdapter
	recorder    events.Recorder
	searchPaths []string
	session     SessionSpec
	sessionID   string
	history     *HistorySnapshot
	historyRaw  historyGeneration
}

var (
	_ Handle                = (*SessionHandle)(nil)
	_ LifecycleHandle       = (*SessionHandle)(nil)
	_ MessagingHandle       = (*SessionHandle)(nil)
	_ TranscriptHandle      = (*SessionHandle)(nil)
	_ HistoryHandle         = (*SessionHandle)(nil)
	_ InteractionHandle     = (*SessionHandle)(nil)
	_ PeekHandle            = (*SessionHandle)(nil)
	_ LiveObservationHandle = (*SessionHandle)(nil)
)

type historyGeneration struct {
	TranscriptStreamID string
	GenerationID       string
}

func cloneHistoryRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
