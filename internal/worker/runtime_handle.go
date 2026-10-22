package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/promptsafe"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ErrOperationUnsupported reports that a worker handle cannot support the
// requested operation because it lacks the required backing state.
var ErrOperationUnsupported = errors.New("worker operation is unsupported")

// RuntimeHandleConfig configures a worker handle for a legacy runtime-only
// session target that has no bead-backed session identity.
type RuntimeHandleConfig struct {
	Provider    runtime.Provider
	SessionName string
	// ProviderName is the builtin provider family (for example "claude"), not
	// the configured provider name. Callers resolve a wrapped alias first with
	// config.BuiltinFamily or ResolvedProvider.BuiltinAncestor.
	ProviderName string
	Transport    string
	ProcessNames []string
	Recorder     events.Recorder
	// CityPath is the city the session belongs to, which scopes the
	// handle's bounded turn reads as a SessionHandle's are; empty scopes
	// them to Provider, so they share no read with the city's
	// SessionHandles. Set it wherever the handle reads turns.
	CityPath string
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
	cityPath     string
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
		cityPath:     strings.TrimSpace(cfg.CityPath),
	}, nil
}

// Start reports unsupported for runtime-only handles that lack bead-backed state.
func (h *RuntimeHandle) Start(ctx context.Context, _ sessionpkg.Actor) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationStart)
	defer func() { event.finish(err) }()

	if h.provider.IsRunning(h.sessionName) {
		return nil
	}
	err = fmt.Errorf("%w: start requires a bead-backed session", ErrOperationUnsupported)
	return err
}

// StartResolved starts a runtime-only handle using the provided resolved
// command. It has no session row, so it has no incarnation to run under: the
// legacy bead-less start (§3.3 S8), allowlisted until legacy is deleted.
func (h *RuntimeHandle) StartResolved(ctx context.Context, by sessionpkg.Actor, _ sessionpkg.Incarnation, startCommand string, cfg runtime.Config) (err error) {
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
	if err = by.CheckStarts(); err != nil {
		return err
	}
	release, err := sessionpkg.LeaseRuntimeName(ctx, by, h.sessionName)
	if err != nil {
		return err
	}
	defer release()
	err = h.provider.Start(ctx, h.sessionName, startCfg)
	return err
}

// stopUnderLease stops the runtime under the name's flock (by's lease mode).
func (h *RuntimeHandle) stopUnderLease(ctx context.Context, by sessionpkg.Actor) error {
	release, err := sessionpkg.LeaseRuntimeName(ctx, by, h.sessionName)
	if err != nil {
		return err
	}
	defer release()
	return runtime.StopForCleanup(h.provider, h.sessionName)
}

// Attach attaches to the live runtime session if it is currently running.
func (h *RuntimeHandle) Attach(ctx context.Context, _ sessionpkg.Actor) (err error) {
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

// Stop asks the provider to stop the live runtime session. A session that is
// already gone satisfies the request, so it reports success — see
// [runtime.StopForCleanup].
func (h *RuntimeHandle) Stop(ctx context.Context, by sessionpkg.Actor) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationStop)
	defer func() { event.finish(err) }()

	err = h.stopUnderLease(ctx, by)
	return err
}

// StopForShutdown is Stop for the city shutdown sweep. A RuntimeHandle has no
// session bead and therefore no lifecycle state to reject, so the shutdown
// intent adds nothing here and this is deliberately identical to Stop. It exists
// so the sweep can express its intent through one interface regardless of which
// handle kind it holds.
func (h *RuntimeHandle) StopForShutdown(ctx context.Context) error {
	return h.Stop(ctx, sessionpkg.Actor{Kind: sessionpkg.ActorSweep})
}

// StopIdle is Stop for the chat idle auto-suspend; like StopForShutdown it
// adds nothing to a handle with no session bead.
func (h *RuntimeHandle) StopIdle(ctx context.Context, by sessionpkg.Actor) error {
	return h.Stop(ctx, by)
}

// Kill asks the provider to stop the live runtime session immediately. A
// session that is already gone satisfies the request, so it reports success —
// see [runtime.StopForCleanup].
func (h *RuntimeHandle) Kill(ctx context.Context, by sessionpkg.Actor) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationKill)
	defer func() { event.finish(err) }()

	err = h.stopUnderLease(ctx, by)
	return err
}

// Close asks the provider to close the live runtime session. A session that is
// already gone satisfies the request, so it reports success — see
// [runtime.StopForCleanup].
func (h *RuntimeHandle) Close(ctx context.Context, by sessionpkg.Actor) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationClose)
	defer func() { event.finish(err) }()

	err = h.stopUnderLease(ctx, by)
	return err
}

// CloseDetailed asks the provider to close the live runtime session and
// returns no bead cleanup artifacts for runtime-only handles.
func (h *RuntimeHandle) CloseDetailed(ctx context.Context, by sessionpkg.Actor) (sessionpkg.CloseResult, error) {
	if err := h.Close(ctx, by); err != nil {
		return sessionpkg.CloseResult{}, err
	}
	return sessionpkg.CloseResult{}, nil
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
// Runtime-only sessions are permanently excluded from invocation
// telemetry (gc.agent.tokens.*, gc.agent.invocation.cost_usd); see
// SessionHandle.recordInvocationTelemetry. Do not add a telemetry hook here.
func (h *RuntimeHandle) Message(ctx context.Context, _ sessionpkg.Actor, req MessageRequest) (result MessageResult, err error) {
	event := h.beginOperationEvent(ctx, workerOperationMessage)
	defer func() {
		event.payload.Queued = boolPointer(result.Queued)
		event.finish(err)
	}()

	if strings.TrimSpace(req.Text) == "" {
		err = fmt.Errorf("message text is required")
		return MessageResult{}, err
	}
	if _, held, lookErr := heldDelivery(ctx, req.Redelivery, req.DeliveryKey, h.lookupDelivery); lookErr != nil || held {
		err = lookErr
		result = MessageResult{Replayed: held}
		return result, err
	}
	if !h.provider.IsRunning(h.sessionName) {
		err = fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
		return MessageResult{}, err
	}
	// A message goes out unfenced, as it always has, under its key alone.
	ctx, _, _ = fencedNudgeContext(ctx, NudgeRequest{}, nil)
	ctx = nudgeDeliveryContext(ctx, NudgeRequest{DeliveryKey: req.DeliveryKey})
	if _, err := runtime.NudgeWithReceipt(ctx, h.provider, h.sessionName, runtime.TextContent(req.Text), false); err != nil {
		return MessageResult{}, runtimeInteractionError(err)
	}
	result = MessageResult{Queued: false}
	return result, nil
}

// lookupDelivery is the session's delivery lookup for heldDelivery.
func (h *RuntimeHandle) lookupDelivery(ctx context.Context, key string) (runtime.DeliveryReceipt, bool, error) {
	return runtime.LookupDelivery(ctx, h.provider, h.sessionName, key)
}

// Interrupt asks the provider to interrupt the live runtime session. With no
// session bead to pick provider-specific keys, keystroke and protocol
// runtimes both get Interrupt; a runtime that cannot interrupt the turn is
// sent nothing and reported with [sessionpkg.ErrTurnInterruptUnsupported].
// A turn-scoped interrupt (req.Turn) needs a provider that observes and
// fences turns itself, since a RuntimeHandle reads no transcript: it runs
// the provider's InterruptTurn and WaitTurnEnded, and is refused, nothing
// sent, on any other provider, and with an input fence, which a protocol
// turn interrupt cannot carry (runtime.ErrInputFenceUnsupported).
func (h *RuntimeHandle) Interrupt(ctx context.Context, req InterruptRequest) (err error) {
	event := h.beginOperationEvent(ctx, workerOperationInterrupt)
	defer func() { event.finish(err) }()

	if req.Turn != "" {
		if !req.Fence.IsZero() {
			if err = req.Fence.Validate(); err != nil {
				return err
			}
		}
		err = h.interruptTurn(ctx, req.Turn, req.Fence)
		return err
	}
	if !req.Fence.IsZero() {
		if err = req.Fence.Validate(); err != nil {
			return err
		}
		if runtime.InputFencingFor(h.provider, h.sessionName) == runtime.InputFencingNone {
			err = fmt.Errorf("%w: %s", runtime.ErrInputFenceUnsupported, h.sessionName)
			return err
		}
	}
	if runtime.TurnInterruptModeFor(h.provider, h.sessionName) == runtime.TurnInterruptUnsupported {
		err = fmt.Errorf("%w: %s", sessionpkg.ErrTurnInterruptUnsupported, h.sessionName)
		return err
	}
	if !req.Fence.IsZero() {
		err = runtime.InterruptWithContext(runtime.WithInputFence(ctx, req.Fence), h.provider, h.sessionName)
		return err
	}
	err = h.provider.Interrupt(h.sessionName)
	return err
}

// interruptTurn is the turn-scoped interrupt from the provider alone: its
// turn capabilities decide the refusal, and its turn fence the interrupt.
func (h *RuntimeHandle) interruptTurn(ctx context.Context, turn string, fence runtime.InputFence) error {
	caps := h.turnCapabilities()
	if err := turnInterruptRefusal(caps, h.sessionName, turn); err != nil {
		return err
	}
	tf, ok := h.provider.(runtime.TurnFenceProvider)
	if caps.Source != TurnSourceProtocol || !ok {
		return fmt.Errorf("interrupting turn %q of %s: %w", turn, h.sessionName, runtime.ErrTurnIdentityUnsupported)
	}
	if err := protocolTurnInterruptFenceRefusal(fence, h.sessionName, turn); err != nil {
		return err
	}
	_, err := sessionpkg.InterruptTurnOn(ctx, tf, h.sessionName, turn)
	return err
}

// Nudge submits a best-effort reminder to the live runtime session.
// Like Message, it is permanently excluded from invocation telemetry;
// see SessionHandle.recordInvocationTelemetry.
func (h *RuntimeHandle) Nudge(ctx context.Context, _ sessionpkg.Actor, req NudgeRequest) (result NudgeResult, err error) {
	ctx = nudgeDeliveryContext(ctx, req)
	event := h.beginOperationEvent(ctx, workerOperationNudge)
	defer func() {
		event.payload.Delivered = boolPointer(result.Delivered)
		event.finish(err)
	}()

	if strings.TrimSpace(req.Text) == "" {
		err = fmt.Errorf("nudge text is required")
		return NudgeResult{}, err
	}
	ctx, unfenced, err := fencedNudgeContext(ctx, req, func() (runtime.InputFencing, error) {
		return runtime.InputFencingFor(h.provider, h.sessionName), nil
	})
	if err != nil {
		return NudgeResult{}, err
	}
	defer func() { result.Unfenced = unfenced && result.Delivered && !result.Replayed }()
	receipt, held, err := heldDelivery(ctx, req.Redelivery, req.DeliveryKey, h.lookupDelivery)
	if err != nil {
		return NudgeResult{}, err
	}
	if held {
		result = NudgeResult{Delivered: true, Replayed: true, Receipt: receipt}
		return result, nil
	}
	// Decided on a fresh read, not the provider's cache: the tmux state
	// cache can show a crashed agent alive for seconds after the crash.
	live, liveErr := runtime.ObserveLivenessSince(h.provider, h.sessionName, h.processNames, time.Now())
	// A fenced nudge reports what the read found instead of undelivered.
	if err = runtime.LivenessFenceMiss(ctx, h.sessionName, live, liveErr); err != nil {
		return NudgeResult{}, err
	}
	if liveErr != nil || live.Running && !live.Alive {
		// A dead agent's pane takes nothing (mc-48az4): the caller queues it.
		result = NudgeResult{Undelivered: NudgeUndeliveredAgentDead}
		return result, nil
	}
	if !live.Running {
		if normalizeNudgeWakePolicy(req.Wake) == NudgeWakeLiveOnly {
			result = NudgeResult{Delivered: false}
			return result, nil
		}
		err = fmt.Errorf("%w: %s", sessionpkg.ErrSessionInactive, h.sessionName)
		result = NudgeResult{Delivered: false}
		return result, err
	}
	switch req.Delivery {
	case "", NudgeDeliveryDefault, NudgeDeliveryImmediate:
		immediate := req.Delivery == NudgeDeliveryImmediate
		receipt, err = runtime.NudgeWithReceipt(ctx, h.provider, h.sessionName, runtime.TextContent(req.Text), immediate)
		if err != nil {
			return NudgeResult{}, runtimeInteractionError(err)
		}
		result = NudgeResult{Delivered: true, Receipt: receipt}
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

// TranscriptRecords reports unavailable because runtime-only handles have no transcript.
func (h *RuntimeHandle) TranscriptRecords(context.Context) ([]json.RawMessage, error) {
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
func (h *RuntimeHandle) Pending(ctx context.Context) (*PendingInteraction, error) {
	pending, _, err := h.PendingStatus(ctx)
	return pending, err
}

// PendingStatus returns the pending interaction plus whether the provider
// supports it. Support is keyed on the provider's answer, not on the interface:
// a provider that implements InteractionProvider but answers
// runtime.ErrInteractionUnsupported (a router whose backend has no
// interactions) is unsupported. A vanished session reports no prompt, and so
// does a session this provider instance does not own (ACP answers that with
// runtime.ErrSessionNotFound). Any other probe error is returned, so callers
// can tell "could not tell" from "nothing pending". The mapping mirrors
// session.Manager.PendingByName.
func (h *RuntimeHandle) PendingStatus(context.Context) (*PendingInteraction, bool, error) {
	ip, ok := h.provider.(runtime.InteractionProvider)
	if !ok {
		return nil, false, nil
	}
	pending, err := ip.Pending(h.sessionName)
	switch {
	case errors.Is(err, runtime.ErrInteractionUnsupported):
		return nil, false, nil
	case errors.Is(err, runtime.ErrSessionNotFound):
		return nil, true, nil
	case err != nil:
		return nil, true, fmt.Errorf("getting pending interaction for %q: %w", h.sessionName, err)
	case pending == nil:
		return nil, true, nil
	}
	return &PendingInteraction{
		RequestID: pending.RequestID,
		Kind:      pending.Kind,
		Prompt:    pending.Prompt,
		Options:   append([]string(nil), pending.Options...),
		Metadata:  cloneStringMap(pending.Metadata),
	}, true, nil
}

// LiveObservation reports runtime presence metadata for a legacy runtime-only
// worker target.
func (h *RuntimeHandle) LiveObservation(_ context.Context) (LiveObservation, error) {
	liveness, err := runtime.ObserveLivenessWithError(h.provider, h.sessionName, h.processNames)
	if err != nil {
		return LiveObservation{}, err
	}
	obs := LiveObservation{
		Running:     liveness.Running,
		Alive:       liveness.Alive,
		SessionName: h.sessionName,
	}
	if suspended, err := h.provider.GetMeta(h.sessionName, "suspended"); err == nil && strings.TrimSpace(suspended) == "true" {
		obs.Suspended = true
	}
	if sessionID, err := h.provider.GetMeta(h.sessionName, "GC_SESSION_ID"); err == nil {
		obs.RuntimeSessionID = strings.TrimSpace(sessionID)
	}
	if obs.Running {
		attached, err := runtime.IsAttachedWithError(h.provider, h.sessionName)
		if err != nil && runtime.AttachProbeHolds(attached, err) {
			obs.AttachedErr = err
		}
		obs.Attached = attached && err == nil
		last, err := h.provider.GetLastActivity(h.sessionName)
		if errors.Is(err, runtime.ErrRuntimeUnavailable) {
			return LiveObservation{}, fmt.Errorf("observe last activity for %q: %w", h.sessionName, err)
		}
		if err == nil && !last.IsZero() {
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
	return runtimeInteractionError(ip.Respond(h.sessionName, runtime.InteractionResponse{
		RequestID: req.RequestID,
		Action:    req.Action,
		Text:      req.Text,
		Metadata:  cloneStringMap(req.Metadata),
	}))
}

// runtimeInteractionError reports the runtime's interaction refusals in the
// session vocabulary, as session.Manager does, so a caller maps them the same
// way for a runtime-only handle: a nudge refused while an interaction is
// pending is session.ErrPendingInteraction, and a response the provider
// rejects, or one naming an action the pending prompt does not offer, is
// session.ErrInteractionMismatch. The runtime error stays wrapped.
func runtimeInteractionError(err error) error {
	switch {
	case errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction):
		return fmt.Errorf("%w: %w", sessionpkg.ErrPendingInteraction, err)
	case errors.Is(err, runtime.ErrInteractionResponseInvalid),
		errors.Is(err, runtime.ErrInteractionActionUnavailable):
		return fmt.Errorf("%w: %w", sessionpkg.ErrInteractionMismatch, err)
	}
	return err
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
			return NudgeResult{}, runtimeInteractionError(err)
		}
		return NudgeResult{Delivered: true}, nil
	}
	// Not a silent false: a caller that downgrades to the queue has to be able
	// to say WHY, and "this provider cannot take live delivery" is a permanent
	// property of the runtime rather than a transient miss. Reporting it as a
	// bare Delivered:false is how `gc session nudge` came to print an
	// unqualified success line for a delivery path that is a no-op end to end.
	if h.providerName != "claude" {
		return NudgeResult{Delivered: false, Undelivered: NudgeUndeliveredProviderUnsupported}, nil
	}
	waiter, ok := h.provider.(runtime.IdleWaitProvider)
	if !ok {
		return NudgeResult{Delivered: false, Undelivered: NudgeUndeliveredProviderUnsupported}, nil
	}
	if err := waiter.WaitForIdle(ctx, h.sessionName, runtimeHandleWaitIdleTimeout); err != nil {
		// A provider type can implement the idle wait while the runtime behind
		// it cannot (an exec pack that does not declare an idle boundary, or a
		// composite routing to a backend without one): that is the same
		// permanent "no live delivery" as having no waiter at all.
		if errors.Is(err, runtime.ErrInteractionUnsupported) {
			return NudgeResult{Delivered: false, Undelivered: NudgeUndeliveredProviderUnsupported}, nil
		}
		if errors.Is(err, context.Canceled) {
			return NudgeResult{Delivered: false}, err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return NudgeResult{Delivered: false}, ctxErr
			}
			return NudgeResult{Delivered: false, Undelivered: NudgeUndeliveredNoIdleBoundary}, nil
		}
		return NudgeResult{Delivered: false, Undelivered: NudgeUndeliveredNoIdleBoundary}, nil
	}
	if err := h.nudgeNow(formatRuntimeWaitIdleReminder(req.Source, req.Text)); err != nil {
		return NudgeResult{}, runtimeInteractionError(err)
	}
	return NudgeResult{Delivered: true}, nil
}

func formatRuntimeWaitIdleReminder(source, message string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "session"
	}
	// Sanitize attacker-controllable fields before interpolating into the
	// <system-reminder> block. The deferred-nudge body is sender-supplied, so
	// without this a sender can embed </system-reminder> sequences to break out
	// of the reminder and inject a forged operator/system directive.
	// See gastownhall/gascity#2195 and the ga-vs7 notification-injection incident.
	source = promptsafe.SanitizeForSystemReminder(source)
	message = promptsafe.SanitizeForSystemReminder(message)
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
