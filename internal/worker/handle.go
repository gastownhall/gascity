package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pricing"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
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
	Start(context.Context, sessionpkg.Actor) error
	StartResolved(context.Context, sessionpkg.Actor, string, runtime.Config) error
	Attach(context.Context, sessionpkg.Actor) error
	Create(context.Context, CreateMode) (sessionpkg.Info, error)
	Reset(context.Context) error
	Stop(context.Context, sessionpkg.Actor) error
	// StopForShutdown is Stop with CITY-SHUTDOWN intent (an ActorSweep), for
	// the `gc stop` / `gc restart` sweep only. It carries latitude on lifecycle
	// states that have no live turn to suspend (notably draining), which a
	// targeted operator Stop must keep rejecting.
	StopForShutdown(context.Context) error
	// StopIdle is Stop for the chat idle auto-suspend: the operator's
	// transition rules, no operator hold.
	StopIdle(context.Context, sessionpkg.Actor) error
	Kill(context.Context, sessionpkg.Actor) error
	Close(context.Context, sessionpkg.Actor) error
	CloseDetailed(context.Context, sessionpkg.Actor) (sessionpkg.CloseResult, error)
	Rename(context.Context, string) error
	StateHandle
}

// FencedKillHandle exposes the operator kill under the session kill fence. A
// handle bound to a session bead implements it; a bare runtime handle has no
// row to fence.
type FencedKillHandle interface {
	KillFenced(context.Context, sessionpkg.Actor, func(string)) (sessionpkg.KillReport, error)
	// KillRuntimeGone is KillFenced's "runtime already gone" verdict taken
	// without the lease (session.Manager.KillRuntimeGone).
	KillRuntimeGone(context.Context) (bool, error)
}

// MessagingHandle exposes live input delivery operations.
type MessagingHandle interface {
	// Message and Nudge may start or resume the session as the Actor may
	// (CONTRACT v5.9 D8).
	Message(context.Context, sessionpkg.Actor, MessageRequest) (MessageResult, error)
	Interrupt(context.Context, InterruptRequest) error
	Nudge(context.Context, sessionpkg.Actor, NudgeRequest) (NudgeResult, error)
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
	TranscriptRecords(context.Context) ([]json.RawMessage, error)
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
	// Deferred: the actor queued it (session.SubmitOutcome.Deferred).
	Deferred bool `json:"-"`
}

// CreateMode controls how a worker session should be materialized.
type CreateMode string

const (
	// CreateModeDeferred creates the session without starting live runtime work.
	CreateModeDeferred CreateMode = "deferred"
	// CreateModeStarted creates the session and starts the live runtime.
	CreateModeStarted CreateMode = "started"
)

// InterruptRequest controls one interrupt of the worker's current turn.
type InterruptRequest struct {
	// Fence, when set, confines every key and interrupt to the incarnation
	// it names (see runtime.InputFence). A runtime that cannot fence the
	// session refuses with runtime.ErrInputFenceUnsupported before sending
	// anything; a miss is a *runtime.FenceMismatchError, and a session that
	// is not running, or has no session row, is one finding it absent (both
	// handles). Past that the handles differ. A SessionHandle maps the
	// liveness it reads before the stop as session.Manager.StopTurnFenced
	// documents: an exited agent is a miss finding it dead, and a read that
	// cannot answer is runtime.ErrRuntimeUnavailable. A RuntimeHandle
	// takes no liveness read and leaves the verdict to the runtime's guard;
	// tmux's checks the agent pane, not the agent process, so an interrupt
	// into an exited agent whose pane lives on can return nil. It is a
	// capability and never serialized.
	Fence runtime.InputFence `json:"-"`
}

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
	// DeliveryKey is the caller's stable identity for this one delivery. A
	// caller that redelivers the same content after an unknown outcome
	// passes the same key, so a runtime that deduplicates keyed deliveries
	// ([runtime.KeyedNudgeProvider]) replays the first delivery instead of
	// delivering twice. Distinct content needs a distinct key. Empty
	// delivers unkeyed. Only default delivery is sure to carry the key:
	// immediate and wait-idle delivery can go out unkeyed, as through
	// [runtime.ImmediateNudgeProvider], whose NudgeNow takes no key.
	DeliveryKey string `json:"delivery_key,omitempty"`
	// Fence, when set, delivers only to the incarnation it names (see
	// runtime.InputFence): a miss is a *runtime.FenceMismatchError, and a
	// runtime that cannot fence the session refuses with
	// runtime.ErrInputFenceUnsupported unless AllowUnfenced. A session with
	// no session row, or not running, is a miss finding it absent and one
	// whose agent exited a miss finding it dead, where an unfenced nudge
	// reports it undelivered; a liveness read that fails is
	// runtime.ErrRuntimeUnavailable (both handles). A fenced nudge must be
	// live-only with default or immediate delivery (ErrFencedNudgeShape). It
	// is a capability and never serialized.
	Fence runtime.InputFence `json:"-"`
	// AllowUnfenced lets a fenced nudge to a runtime that cannot fence the
	// session go out unfenced; the result then reports Unfenced.
	AllowUnfenced bool `json:"allow_unfenced,omitempty"`
}

// ErrFencedNudgeShape reports a fenced nudge that is not live-only, or that
// waits for idle: a fenced nudge never starts a runtime, and the wait-idle
// path cannot report a refused send.
var ErrFencedNudgeShape = errors.New("a fenced nudge must be live-only with default or immediate delivery")

// validateFencedNudge refuses a fenced request that names no incarnation
// or lacks the fenced shape. A request with no fence passes.
func validateFencedNudge(req NudgeRequest) error {
	if req.Fence.IsZero() {
		return nil
	}
	if err := req.Fence.Validate(); err != nil {
		return err
	}
	if normalizeNudgeWakePolicy(req.Wake) != NudgeWakeLiveOnly || req.Delivery == NudgeDeliveryWaitIdle {
		return ErrFencedNudgeShape
	}
	switch req.Delivery {
	case "", NudgeDeliveryDefault, NudgeDeliveryImmediate:
		return nil
	}
	return ErrFencedNudgeShape
}

// fencedNudgeContext applies req's fence to ctx. The request's field is the
// worker's only fence: a request with no fence returns ctx with any input
// fence it carried cleared, so a caller's ctx fence never reaches a path
// (wait-idle, a wake that starts the runtime) the fenced shape refuses.
// Otherwise the request must pass validateFencedNudge; then fencing (the
// runtime's fencing of the session) decides: a runtime that cannot fence is
// refused with runtime.ErrInputFenceUnsupported, or, when req.AllowUnfenced,
// delivered without the fence and reported unfenced.
func fencedNudgeContext(ctx context.Context, req NudgeRequest, fencing func() (runtime.InputFencing, error)) (context.Context, bool, error) {
	if req.Fence.IsZero() {
		if _, ok := runtime.InputFenceFrom(ctx); ok {
			ctx = runtime.WithInputFence(ctx, runtime.InputFence{})
		}
		return ctx, false, nil
	}
	if err := validateFencedNudge(req); err != nil {
		return ctx, false, err
	}
	f, err := fencing()
	if err != nil {
		return ctx, false, err
	}
	if f == runtime.InputFencingNone {
		if req.AllowUnfenced {
			return runtime.WithInputFence(ctx, runtime.InputFence{}), true, nil
		}
		return ctx, false, runtime.ErrInputFenceUnsupported
	}
	return runtime.WithInputFence(ctx, req.Fence), false, nil
}

// nudgeDeliveryContext scopes req's delivery key to ctx for this one
// delivery (see [runtime.WithNudgeDeliveryKey]).
func nudgeDeliveryContext(ctx context.Context, req NudgeRequest) context.Context {
	if req.DeliveryKey == "" {
		return ctx
	}
	return runtime.WithNudgeDeliveryKey(ctx, req.DeliveryKey)
}

// NudgeResult reports whether the requested live delivery actually happened.
type NudgeResult struct {
	Delivered bool `json:"delivered"`
	// Undelivered names WHY a live delivery did not happen, for the callers
	// that downgrade to the queue and have to tell a human what they did.
	// Empty when Delivered is true, and empty for a downgrade this type does
	// not (yet) distinguish — a caller must treat it as a hint, never as a
	// second copy of Delivered.
	Undelivered NudgeUndeliveredReason `json:"undelivered,omitempty"`
	// Unfenced reports a fenced request (NudgeRequest.Fence) delivered
	// without its fence, as AllowUnfenced permits for a runtime that cannot
	// fence the session. Set only when Delivered.
	Unfenced bool `json:"unfenced,omitempty"`
}

// NudgeUndeliveredReason is the closed set of reasons a live nudge did not
// reach the session. It is a typed enum rather than free text because it is
// interpolated into operator-facing output.
type NudgeUndeliveredReason string

const (
	// NudgeUndeliveredProviderUnsupported means this provider/transport cannot
	// take a live wait-idle delivery at all, so the nudge can only be queued. It
	// is a property of the runtime, not of the session's current state.
	NudgeUndeliveredProviderUnsupported NudgeUndeliveredReason = "live_delivery_unsupported"
	// NudgeUndeliveredNoIdleBoundary means the provider CAN take live delivery
	// but the session never reached the idle boundary within the wait window.
	NudgeUndeliveredNoIdleBoundary NudgeUndeliveredReason = "no_idle_boundary"
	// NudgeUndeliveredPendingInteraction means the session is waiting on a
	// pending interaction such as a tool permission prompt. Typing into it
	// would answer the prompt, so the nudge waits until it is resolved.
	NudgeUndeliveredPendingInteraction NudgeUndeliveredReason = "pending_interaction"
	// NudgeUndeliveredHeld means the session is held, which a nudge does not
	// resume (CONTRACT v5.9 D8), and nothing queued the nudge: the caller
	// must queue it (a wait-idle nudge).
	NudgeUndeliveredHeld NudgeUndeliveredReason = "session_held"
	// NudgeQueuedHeld means the session is held and the nudge was queued for
	// it; the caller must not queue it again.
	NudgeQueuedHeld NudgeUndeliveredReason = "session_held_queued"
	// NudgeQueued means the session could not take the nudge now (it is held,
	// (ActorBackground) not running, or its agent is dead in a pane still up)
	// and queued it itself; the caller must not queue it again.
	NudgeQueued NudgeUndeliveredReason = "queued"
	// NudgeUndeliveredAgentDead means the pane is up but its agent is dead (a
	// crash the controller has not healed), or a fresh read could not vouch
	// for it: text typed now would be lost with the pane, and nothing queued
	// the nudge, so the caller queues it (mc-48az4).
	NudgeUndeliveredAgentDead NudgeUndeliveredReason = "agent_dead"
)

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
	TailCompactions int `json:"tail_compactions,omitempty"`
	// LogicalID, when set, replaces the session's own logical conversation ID
	// (session.Info.LogicalConversationID). The handle stitches snapshots that
	// share it, so a caller-supplied value must change whenever the session
	// starts a new conversation.
	LogicalID     string `json:"logical_conversation_id,omitempty"`
	BeforeEntryID string `json:"before_entry_id,omitempty"`
	AfterEntryID  string `json:"after_entry_id,omitempty"`
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
	UsageSink   usage.Sink
	Session     SessionSpec
	// Pricing estimates per-invocation cost for telemetry. Nil falls back
	// to the registry built from shipped defaults.
	Pricing *pricing.Registry
}

// SessionHandle is the production worker handle backed by session.Manager.
type SessionHandle struct {
	mu             sync.Mutex
	manager        *sessionpkg.Manager
	adapter        SessionLogAdapter
	recorder       events.Recorder
	usageSink      usage.Sink
	searchPaths    []string
	session        SessionSpec
	sessionID      string
	history        *HistorySnapshot
	historyRaw     historyGeneration
	pricing        *pricing.Registry
	invTelemetryMu sync.Mutex
	// sidecarDoneID is the session id whose transcript-session sidecar has been
	// confirmed written, guarded by sidecarMu. The keyed transcript path is stable
	// once the session key exists, so a matching id lets repeated turn/poll calls
	// skip both the keyed-path resolve and the write once the sidecar is current.
	sidecarMu     sync.Mutex
	sidecarDoneID string
	// sidecarRetrySchedule is nil in production, where the sidecar retry uses
	// time.AfterFunc. Tests supply a channel-backed scheduler to drive retries
	// deterministically.
	sidecarRetrySchedule  func(time.Duration, func())
	sidecarRetryID        string
	sidecarRetryAttempts  int
	sidecarRetryScheduled bool
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
