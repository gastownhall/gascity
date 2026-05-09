package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type workerOperation string

const (
	workerOperationStart         workerOperation = "start"
	workerOperationStartResolved workerOperation = "start_resolved"
	workerOperationAttach        workerOperation = "attach"
	workerOperationCreate        workerOperation = "create"
	workerOperationReset         workerOperation = "reset"
	workerOperationStop          workerOperation = "stop"
	workerOperationKill          workerOperation = "kill"
	workerOperationClose         workerOperation = "close"
	workerOperationRename        workerOperation = "rename"
	workerOperationMessage       workerOperation = "message"
	workerOperationInterrupt     workerOperation = "interrupt"
	workerOperationNudge         workerOperation = "nudge"
	workerOperationHistory       workerOperation = "history"
)

type operationResult string

const (
	operationResultSucceeded operationResult = "succeeded"
	operationResultFailed    operationResult = "failed"
)

type operationEventPayload struct {
	OpID        string          `json:"op_id"`
	Operation   string          `json:"operation"`
	Result      operationResult `json:"result"`
	SessionID   string          `json:"session_id,omitempty"`
	SessionName string          `json:"session_name,omitempty"`
	Provider    string          `json:"provider,omitempty"`
	Transport   string          `json:"transport,omitempty"`
	Template    string          `json:"template,omitempty"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  time.Time       `json:"finished_at"`
	DurationMs  int64           `json:"duration_ms"`
	Queued      *bool           `json:"queued,omitempty"`
	Delivered   *bool           `json:"delivered,omitempty"`
	Error       string          `json:"error,omitempty"`

	// 1a fields (issue #1252). Mirror api.WorkerOperationEventPayload —
	// the api package re-uses the same JSON shape on the wire and the
	// fields stay in sync via TestEveryKnownEventTypeHasRegisteredPayload.
	// All fields are best-effort; absent data leaves zero values.
	Model               string  `json:"model,omitempty"`
	AgentName           string  `json:"agent_name,omitempty"`
	PromptVersion       string  `json:"prompt_version,omitempty"`
	PromptSHA           string  `json:"prompt_sha,omitempty"`
	BeadID              string  `json:"bead_id,omitempty"`
	PromptTokens        int     `json:"prompt_tokens,omitempty"`
	CompletionTokens    int     `json:"completion_tokens,omitempty"`
	CacheReadTokens     int     `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int     `json:"cache_creation_tokens,omitempty"`
	LatencyMs           int64   `json:"latency_ms,omitempty"`
	CostUSDEstimate     float64 `json:"cost_usd_estimate,omitempty"`
}

type operationEventTarget interface {
	operationEventRecordingEnabled() bool
	populateOperationEventIdentity(*operationEventPayload)
	recordWorkerOperationEvent(operationEventPayload)
}

type operationEvent struct {
	target     operationEventTarget
	payload    operationEventPayload
	suppressed bool
}

type operationEventsSuppressedKey struct{}

// WithoutOperationEvents returns a context that suppresses worker operation
// event emission for internal polling and derived-state reads.
func WithoutOperationEvents(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, operationEventsSuppressedKey{}, true)
}

func newOperationEvent(ctx context.Context, target operationEventTarget, op workerOperation, provider, transport, template string) *operationEvent {
	if operationEventsSuppressed(ctx) || target == nil || !target.operationEventRecordingEnabled() {
		return &operationEvent{target: target, suppressed: true}
	}
	startedAt := time.Now().UTC()
	payload := operationEventPayload{
		OpID:      newWorkerOperationID(),
		Operation: string(op),
		Provider:  strings.TrimSpace(provider),
		Transport: strings.TrimSpace(transport),
		Template:  strings.TrimSpace(template),
		StartedAt: startedAt,
	}
	target.populateOperationEventIdentity(&payload)
	return &operationEvent{
		target:  target,
		payload: payload,
	}
}

func (h *SessionHandle) beginOperationEvent(ctx context.Context, op workerOperation) *operationEvent {
	return newOperationEvent(ctx, h, op, h.providerLabel(), h.session.Transport, h.session.Template)
}

func (e *operationEvent) finish(err error) {
	if e == nil || e.target == nil || e.suppressed {
		return
	}
	e.payload.FinishedAt = time.Now().UTC()
	e.payload.DurationMs = e.payload.FinishedAt.Sub(e.payload.StartedAt).Milliseconds()
	if err != nil {
		e.payload.Result = operationResultFailed
		e.payload.Error = err.Error()
	} else {
		e.payload.Result = operationResultSucceeded
	}
	e.target.populateOperationEventIdentity(&e.payload)
	e.target.recordWorkerOperationEvent(e.payload)
}

func (h *SessionHandle) populateOperationEventIdentity(payload *operationEventPayload) {
	if payload == nil {
		return
	}
	if payload.SessionID == "" {
		payload.SessionID = h.currentSessionID()
	}
	if info, ok := h.currentOperationSessionInfo(); ok {
		payload.SessionID = info.ID
		fallback := h.operationEventFallbackSessionName()
		if payload.SessionName == "" || payload.SessionName == fallback {
			payload.SessionName = info.SessionName
		}
		if strings.TrimSpace(payload.Provider) == "" {
			payload.Provider = info.Provider
		}
		if strings.TrimSpace(payload.Template) == "" {
			payload.Template = strings.TrimSpace(info.Template)
		}
		if strings.TrimSpace(payload.AgentName) == "" {
			payload.AgentName = strings.TrimSpace(info.AgentName)
		}
		if strings.TrimSpace(payload.AgentName) == "" {
			payload.AgentName = strings.TrimSpace(info.Alias)
		}
	}
	if payload.SessionName == "" {
		switch {
		case strings.TrimSpace(h.session.ExplicitName) != "":
			payload.SessionName = strings.TrimSpace(h.session.ExplicitName)
		case strings.TrimSpace(h.session.Title) != "":
			payload.SessionName = strings.TrimSpace(h.session.Title)
		default:
			payload.SessionName = strings.TrimSpace(h.session.Template)
		}
	}
	if strings.TrimSpace(payload.Provider) == "" {
		payload.Provider = h.providerLabel()
	}
	if strings.TrimSpace(payload.Transport) == "" {
		payload.Transport = strings.TrimSpace(h.session.Transport)
	}
	if strings.TrimSpace(payload.Template) == "" {
		payload.Template = strings.TrimSpace(h.session.Template)
	}
}

func (h *SessionHandle) currentOperationSessionInfo() (sessionpkg.Info, bool) {
	id := h.currentSessionID()
	if id == "" {
		return sessionpkg.Info{}, false
	}
	info, err := h.manager.Get(id)
	if err != nil {
		return sessionpkg.Info{}, false
	}
	return info, true
}

func (h *SessionHandle) recordWorkerOperationEvent(payload operationEventPayload) {
	recordOperationEvent(h.recorder, payload)
}

func operationEventsSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	suppressed, _ := ctx.Value(operationEventsSuppressedKey{}).(bool)
	return suppressed
}

func (h *SessionHandle) operationEventRecordingEnabled() bool {
	return h != nil && h.recorder != nil && h.recorder != events.Discard
}

func (h *SessionHandle) operationEventFallbackSessionName() string {
	switch {
	case strings.TrimSpace(h.session.ExplicitName) != "":
		return strings.TrimSpace(h.session.ExplicitName)
	case strings.TrimSpace(h.session.Title) != "":
		return strings.TrimSpace(h.session.Title)
	default:
		return strings.TrimSpace(h.session.Template)
	}
}

func boolPointer(v bool) *bool {
	b := v
	return &b
}

func recordOperationEvent(recorder events.Recorder, payload operationEventPayload) {
	if recorder == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	subject := payload.SessionID
	if strings.TrimSpace(subject) == "" {
		subject = payload.SessionName
	}
	recorder.Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: subject,
		Payload: raw,
	})
}

func newWorkerOperationID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(buf)
}
