package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	return &RuntimeHandle{
		provider:     cfg.Provider,
		sessionName:  strings.TrimSpace(cfg.SessionName),
		providerName: strings.TrimSpace(cfg.ProviderName),
		transport:    strings.TrimSpace(cfg.Transport),
		processNames: append([]string(nil), cfg.ProcessNames...),
	}, nil
}

func (h *RuntimeHandle) Start(context.Context) error {
	if h.provider.IsRunning(h.sessionName) {
		return nil
	}
	return fmt.Errorf("%w: start requires a bead-backed session", ErrOperationUnsupported)
}

func (h *RuntimeHandle) StartResolved(ctx context.Context, _ string, _ runtime.Config) error {
	return h.Start(ctx)
}

func (h *RuntimeHandle) Attach(context.Context) error {
	if !h.provider.IsRunning(h.sessionName) {
		return fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
	}
	return h.provider.Attach(h.sessionName)
}

func (h *RuntimeHandle) Create(context.Context, CreateMode) (sessionpkg.Info, error) {
	return sessionpkg.Info{}, fmt.Errorf("%w: create requires a bead-backed session", ErrOperationUnsupported)
}

func (h *RuntimeHandle) Stop(context.Context) error {
	return h.provider.Stop(h.sessionName)
}

func (h *RuntimeHandle) Kill(context.Context) error {
	return h.provider.Stop(h.sessionName)
}

func (h *RuntimeHandle) Close(context.Context) error {
	return h.provider.Stop(h.sessionName)
}

func (h *RuntimeHandle) Rename(context.Context, string) error {
	return fmt.Errorf("%w: rename requires a bead-backed session", ErrOperationUnsupported)
}

func (h *RuntimeHandle) Peek(_ context.Context, lines int) (string, error) {
	if !h.provider.IsRunning(h.sessionName) {
		return "", fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
	}
	return h.provider.Peek(h.sessionName, lines)
}

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

func (h *RuntimeHandle) Message(_ context.Context, req MessageRequest) (MessageResult, error) {
	if strings.TrimSpace(req.Text) == "" {
		return MessageResult{}, fmt.Errorf("message text is required")
	}
	if !h.provider.IsRunning(h.sessionName) {
		return MessageResult{}, fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
	}
	if err := h.provider.Nudge(h.sessionName, runtime.TextContent(req.Text)); err != nil {
		return MessageResult{}, err
	}
	return MessageResult{Queued: false}, nil
}

func (h *RuntimeHandle) Interrupt(context.Context, InterruptRequest) error {
	return h.provider.Interrupt(h.sessionName)
}

func (h *RuntimeHandle) Nudge(_ context.Context, req NudgeRequest) (NudgeResult, error) {
	if strings.TrimSpace(req.Text) == "" {
		return NudgeResult{}, fmt.Errorf("nudge text is required")
	}
	if !h.provider.IsRunning(h.sessionName) {
		if normalizeNudgeWakePolicy(req.Wake) == NudgeWakeLiveOnly {
			return NudgeResult{Delivered: false}, nil
		}
		return NudgeResult{Delivered: false}, fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
	}
	switch req.Delivery {
	case "", NudgeDeliveryDefault:
		if err := h.provider.Nudge(h.sessionName, runtime.TextContent(req.Text)); err != nil {
			return NudgeResult{}, err
		}
		return NudgeResult{Delivered: true}, nil
	case NudgeDeliveryImmediate:
		if err := h.nudgeNow(req.Text); err != nil {
			return NudgeResult{}, err
		}
		return NudgeResult{Delivered: true}, nil
	case NudgeDeliveryWaitIdle:
		return h.nudgeWaitIdle(req)
	default:
		return NudgeResult{}, fmt.Errorf("unsupported nudge delivery %q", req.Delivery)
	}
}

func (h *RuntimeHandle) Transcript(context.Context, TranscriptRequest) (*TranscriptResult, error) {
	return nil, ErrHistoryUnavailable
}

func (h *RuntimeHandle) TranscriptPath(context.Context) (string, error) {
	return "", ErrHistoryUnavailable
}

func (h *RuntimeHandle) AgentMappings(context.Context) ([]AgentMapping, error) {
	return nil, ErrHistoryUnavailable
}

func (h *RuntimeHandle) AgentTranscript(context.Context, string) (*AgentTranscriptResult, error) {
	return nil, ErrHistoryUnavailable
}

func (h *RuntimeHandle) History(context.Context, HistoryRequest) (*HistorySnapshot, error) {
	return nil, ErrHistoryUnavailable
}

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
			lastCopy := time.Time(last)
			obs.LastActivity = &lastCopy
		}
	}
	return obs, nil
}

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

func (h *RuntimeHandle) nudgeWaitIdle(req NudgeRequest) (NudgeResult, error) {
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
	if err := waiter.WaitForIdle(context.Background(), h.sessionName, runtimeHandleWaitIdleTimeout); err != nil {
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
