package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ErrOperationUnsupported reports that a worker handle cannot support the
// requested operation because it lacks the required backing state.
var ErrOperationUnsupported = errors.New("worker operation is unsupported")

// RuntimeHandleConfig configures a worker handle for a legacy runtime-only
// session target that has no bead-backed session identity.
type RuntimeHandleConfig struct {
	Provider     runtime.Provider
	SessionName  string
	ProviderName string
	Transport    string
	ProcessNames []string
	Recorder     events.Recorder
}

// RuntimeHandle adapts a legacy runtime session name to the canonical worker
// interface so higher layers do not bypass internal/worker for lifecycle or
// pending interaction operations.
type RuntimeHandle struct {
	provider     runtime.Provider
	sessionName  string
	providerName string
	transport    string
	processNames []string
	recorder     events.Recorder
}

var _ Handle = (*RuntimeHandle)(nil)

// NewRuntimeHandle constructs a worker handle for a runtime-only session.
func NewRuntimeHandle(cfg RuntimeHandleConfig) (*RuntimeHandle, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("%w: provider is required", ErrHandleConfig)
	}
	if strings.TrimSpace(cfg.SessionName) == "" {
		return nil, fmt.Errorf("%w: session name is required", ErrHandleConfig)
	}
	recorder := cfg.Recorder
	if recorder == nil {
		recorder = events.Discard
	}
	return &RuntimeHandle{
		provider:     cfg.Provider,
		sessionName:  strings.TrimSpace(cfg.SessionName),
		providerName: strings.TrimSpace(cfg.ProviderName),
		transport:    strings.TrimSpace(cfg.Transport),
		processNames: append([]string(nil), cfg.ProcessNames...),
		recorder:     recorder,
	}, nil
}

// Start reports unsupported for runtime-only handles that lack bead-backed state.
func (h *RuntimeHandle) Start(ctx context.Context) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationStart)
	defer func() { event.finish(err) }()

	if h.provider.IsRunning(h.sessionName) {
		return nil
	}
	err = fmt.Errorf("%w: start requires a bead-backed session", ErrOperationUnsupported)
	return err
}

// StartResolved starts a runtime-only handle using the provided resolved command.
func (h *RuntimeHandle) StartResolved(ctx context.Context, startCommand string, cfg runtime.Config) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationStartResolved)
	defer func() { event.finish(err) }()

	if h.provider.IsRunning(h.sessionName) {
		return nil
	}
	startCfg := cfg
	if strings.TrimSpace(startCfg.Command) == "" {
		startCfg.Command = strings.TrimSpace(startCommand)
	}
	if strings.TrimSpace(startCfg.Command) == "" {
		err = fmt.Errorf("%w: start requires a runtime command", ErrOperationUnsupported)
		return err
	}
	err = h.provider.Start(ctx, h.sessionName, startCfg)
	return err
}

// Attach attaches to the live runtime session if it is currently running.
func (h *RuntimeHandle) Attach(ctx context.Context) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationAttach)
	defer func() { event.finish(err) }()

	if !h.provider.IsRunning(h.sessionName) {
		err = fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
		return err
	}
	err = h.provider.Attach(h.sessionName)
	return err
}

// Create reports unsupported because runtime-only handles have no bead-backed creation path.
func (h *RuntimeHandle) Create(ctx context.Context, _ CreateMode) (info sessionpkg.Info, err error) {
	event := h.beginOperationEvent(ctx, workerOperationCreate)
	defer func() { event.finish(err) }()

	err = fmt.Errorf("%w: create requires a bead-backed session", ErrOperationUnsupported)
	return sessionpkg.Info{}, err
}

// Reset reports unsupported because runtime-only handles have no reset path.
func (h *RuntimeHandle) Reset(ctx context.Context) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationReset)
	defer func() { event.finish(err) }()

	err = fmt.Errorf("%w: reset requires a bead-backed session", ErrOperationUnsupported)
	return err
}

// Stop asks the provider to stop the live runtime session.
func (h *RuntimeHandle) Stop(ctx context.Context) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationStop)
	defer func() { event.finish(err) }()

	err = h.provider.Stop(h.sessionName)
	return err
}

// Kill asks the provider to stop the live runtime session immediately.
func (h *RuntimeHandle) Kill(ctx context.Context) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationKill)
	defer func() { event.finish(err) }()

	err = h.provider.Stop(h.sessionName)
	return err
}

// Close asks the provider to close the live runtime session.
func (h *RuntimeHandle) Close(ctx context.Context) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationClose)
	defer func() { event.finish(err) }()

	err = h.provider.Stop(h.sessionName)
	return err
}

// Rename reports unsupported because runtime-only handles have no persisted name update.
func (h *RuntimeHandle) Rename(ctx context.Context, _ string) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationRename)
	defer func() { event.finish(err) }()

	err = fmt.Errorf("%w: rename requires a bead-backed session", ErrOperationUnsupported)
	return err
}

// Peek returns recent runtime output lines for the live session.
func (h *RuntimeHandle) Peek(_ context.Context, lines int) (string, error) {
	if !h.provider.IsRunning(h.sessionName) {
		return "", fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
	}
	return h.provider.Peek(h.sessionName, lines)
}

// State projects runtime-only observations into the canonical worker state view.
func (h *RuntimeHandle) State(context.Context) (State, error) {
	state := State{
		SessionName: h.sessionName,
		Provider:    h.providerName,
	}
	if !h.provider.IsRunning(h.sessionName) {
		state.Phase = PhaseStopped
		return state, nil
	}
	pending, err := h.Pending(context.Background())
	if err != nil {
		return State{}, err
	}
	if pending != nil {
		state.Phase = PhaseBlocked
		state.Pending = pending
		return state, nil
	}
	state.Phase = PhaseReady
	return state, nil
}

// Message submits a runtime nudge as a synchronous worker message.
func (h *RuntimeHandle) Message(ctx context.Context, req MessageRequest) (result MessageResult, err error) {
	event := h.beginOperationEvent(ctx, workerOperationMessage)
	defer func() {
		event.payload.Queued = boolPointer(result.Queued)
		event.finish(err)
	}()

	if strings.TrimSpace(req.Text) == "" {
		err = fmt.Errorf("message text is required")
		return MessageResult{}, err
	}
	if !h.provider.IsRunning(h.sessionName) {
		err = fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
		return MessageResult{}, err
	}
	if err := h.provider.Nudge(h.sessionName, runtime.TextContent(req.Text)); err != nil {
		return MessageResult{}, err
	}
	result = MessageResult{Queued: false}
	return result, nil
}

// Interrupt asks the provider to interrupt the live runtime session.
func (h *RuntimeHandle) Interrupt(ctx context.Context, _ InterruptRequest) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationInterrupt)
	defer func() { event.finish(err) }()

	err = h.provider.Interrupt(h.sessionName)
	return err
}

// Nudge submits a best-effort reminder to the live runtime session.
func (h *RuntimeHandle) Nudge(ctx context.Context, req NudgeRequest) (result NudgeResult, err error) {
	event := h.beginOperationEvent(ctx, workerOperationNudge)
	defer func() {
		event.payload.Delivered = boolPointer(result.Delivered)
		event.finish(err)
	}()

	if strings.TrimSpace(req.Text) == "" {
		err = fmt.Errorf("nudge text is required")
		return NudgeResult{}, err
	}
	if !h.provider.IsRunning(h.sessionName) {
		if normalizeNudgeWakePolicy(req.Wake) == NudgeWakeLiveOnly {
			result = NudgeResult{Delivered: false}
			return result, nil
		}
		err = fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
		result = NudgeResult{Delivered: false}
		return result, err
	}
	switch req.Delivery {
	case "", NudgeDeliveryDefault:
		if err := h.provider.Nudge(h.sessionName, runtime.TextContent(req.Text)); err != nil {
			return NudgeResult{}, err
		}
		result = NudgeResult{Delivered: true}
		return result, nil
	case NudgeDeliveryImmediate:
		if err := h.nudgeNow(req.Text); err != nil {
			return NudgeResult{}, err
		}
		result = NudgeResult{Delivered: true}
		return result, nil
	case NudgeDeliveryWaitIdle:
		result, err = h.nudgeWaitIdle(ctx, req)
		return result, err
	default:
		err = fmt.Errorf("unsupported nudge delivery %q", req.Delivery)
		return NudgeResult{}, err
	}
}

// Transcript reports unavailable because runtime-only handles have no transcript adapter.
func (h *RuntimeHandle) Transcript(context.Context, TranscriptRequest) (*TranscriptResult, error) {
	return nil, ErrHistoryUnavailable
}

// TranscriptPath reports unavailable because runtime-only handles have no transcript path.
func (h *RuntimeHandle) TranscriptPath(context.Context) (string, error) {
	return "", ErrHistoryUnavailable
}

// AgentMappings reports unavailable because runtime-only handles have no agent transcripts.
func (h *RuntimeHandle) AgentMappings(context.Context) ([]AgentMapping, error) {
	return nil, ErrHistoryUnavailable
}

// AgentTranscript reports unavailable because runtime-only handles have no agent transcripts.
func (h *RuntimeHandle) AgentTranscript(context.Context, string) (*AgentTranscriptResult, error) {
	return nil, ErrHistoryUnavailable
}

// History reports unavailable because runtime-only handles have no transcript history.
func (h *RuntimeHandle) History(ctx context.Context, _ HistoryRequest) (*HistorySnapshot, error) {
	event := h.beginOperationEvent(ctx, workerOperationHistory)
	err := ErrHistoryUnavailable
	defer func() { event.finish(err) }()
	return nil, err
}

// Pending returns the current blocking interaction for a runtime-only session if supported.
func (h *RuntimeHandle) Pending(context.Context) (*PendingInteraction, error) {
	ip, ok := h.provider.(runtime.InteractionProvider)
	if !ok {
		return nil, nil
	}
	pending, err := ip.Pending(h.sessionName)
	if errors.Is(err, runtime.ErrInteractionUnsupported) || pending == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &PendingInteraction{
		RequestID: pending.RequestID,
		Kind:      pending.Kind,
		Prompt:    pending.Prompt,
		Options:   append([]string(nil), pending.Options...),
		Metadata:  cloneStringMap(pending.Metadata),
	}, nil
}

// PendingStatus returns the pending interaction plus whether the provider supports it.
func (h *RuntimeHandle) PendingStatus(ctx context.Context) (*PendingInteraction, bool, error) {
	_, supported := h.provider.(runtime.InteractionProvider)
	pending, err := h.Pending(ctx)
	if err != nil {
		return nil, supported, err
	}
	return pending, supported, nil
}

// LiveObservation reports runtime presence metadata for a legacy runtime-only
// worker target.
func (h *RuntimeHandle) LiveObservation(_ context.Context) (LiveObservation, error) {
	obs := LiveObservation{
		Running:     h.provider.IsRunning(h.sessionName),
		Alive:       false,
		SessionName: h.sessionName,
	}
	if suspended, err := h.provider.GetMeta(h.sessionName, "suspended"); err == nil && strings.TrimSpace(suspended) == "true" {
		obs.Suspended = true
	}
	if sessionID, err := h.provider.GetMeta(h.sessionName, "GC_SESSION_ID"); err == nil {
		obs.RuntimeSessionID = strings.TrimSpace(sessionID)
	}
	if obs.Running {
		obs.Alive = h.provider.ProcessAlive(h.sessionName, h.processNames)
		obs.Attached = h.provider.IsAttached(h.sessionName)
		if last, err := h.provider.GetLastActivity(h.sessionName); err == nil && !last.IsZero() {
			lastCopy := last
			obs.LastActivity = &lastCopy
		}
	}
	return obs, nil
}

// Respond resolves a blocking interaction through the runtime provider.
func (h *RuntimeHandle) Respond(_ context.Context, req InteractionResponse) error {
	ip, ok := h.provider.(runtime.InteractionProvider)
	if !ok {
		return runtime.ErrInteractionUnsupported
	}
	return ip.Respond(h.sessionName, runtime.InteractionResponse{
		RequestID: req.RequestID,
		Action:    req.Action,
		Text:      req.Text,
		Metadata:  cloneStringMap(req.Metadata),
	})
}

const runtimeHandleWaitIdleTimeout = 30 * time.Second

func (h *RuntimeHandle) nudgeNow(message string) error {
	content := runtime.TextContent(message)
	if immediate, ok := h.provider.(runtime.ImmediateNudgeProvider); ok {
		return immediate.NudgeNow(h.sessionName, content)
	}
	return h.provider.Nudge(h.sessionName, content)
}

func (h *RuntimeHandle) nudgeWaitIdle(ctx context.Context, req NudgeRequest) (NudgeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h.transport == "acp" {
		if err := h.provider.Nudge(h.sessionName, runtime.TextContent(req.Text)); err != nil {
			return NudgeResult{}, err
		}
		return NudgeResult{Delivered: true}, nil
	}
	if h.providerName != "claude" {
		return NudgeResult{Delivered: false}, nil
	}
	waiter, ok := h.provider.(runtime.IdleWaitProvider)
	if !ok {
		return NudgeResult{Delivered: false}, nil
	}
	if err := waiter.WaitForIdle(ctx, h.sessionName, runtimeHandleWaitIdleTimeout); err != nil {
		if errors.Is(err, context.Canceled) {
			return NudgeResult{Delivered: false}, err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return NudgeResult{Delivered: false}, ctxErr
			}
			return NudgeResult{Delivered: false}, nil
		}
		return NudgeResult{Delivered: false}, nil
	}
	if err := h.nudgeNow(formatRuntimeWaitIdleReminder(req.Source, req.Text)); err != nil {
		return NudgeResult{}, err
	}
	return NudgeResult{Delivered: true}, nil
}

func formatRuntimeWaitIdleReminder(source, message string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "session"
	}
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	sb.WriteString("You have a deferred reminder that was queued until a safe boundary:\n\n")
	fmt.Fprintf(&sb, "- [%s] %s\n", source, message)
	sb.WriteString("\nHandle them after this turn.\n")
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

func (h *RuntimeHandle) beginOperationEvent(ctx context.Context, op workerOperation) *operationEvent {
	return newOperationEvent(ctx, h, op, h.providerName, h.transport, "")
}

func (h *RuntimeHandle) populateOperationEventIdentity(payload *operationEventPayload) {
	if payload == nil {
		return
	}
	if strings.TrimSpace(payload.SessionName) == "" {
		payload.SessionName = h.sessionName
	}
	if strings.TrimSpace(payload.Provider) == "" {
		payload.Provider = h.providerName
	}
	if strings.TrimSpace(payload.Transport) == "" {
		payload.Transport = h.transport
	}
}

func (h *RuntimeHandle) operationEventRecordingEnabled() bool {
	return h != nil && h.recorder != nil && h.recorder != events.Discard
}

func (h *RuntimeHandle) recordWorkerOperationEvent(payload operationEventPayload) {
	recordOperationEvent(h.recorder, payload)
}
