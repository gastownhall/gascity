package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
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

	// RunID is the run-root this operation belongs to, resolved per-operation
	// from the bead metadata chain (workflow_id || molecule_id ||
	// gc.root_bead_id-or-self || bead id || session id for manual chat). Lets
	// consumers roll per-operation cost/latency up to a run.
	RunID string `json:"run_id,omitempty"`
	// Unpriced is a tri-state flag: nil = pricing not evaluated, true = tokens
	// observed but no price resolved (CostUSDEstimate not authoritative), false
	// = priced. Mirrors the Queued/Delivered pointer convention.
	Unpriced *bool `json:"unpriced,omitempty"`
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
	return &operationEvent{target: target, payload: payload}
}

func (h *SessionHandle) beginOperationEvent(ctx context.Context, op workerOperation) *operationEvent {
	return newOperationEvent(ctx, h, op, h.providerLabel(), h.session.Transport, h.session.Template)
}

// sealTiming freezes the operation's FinishedAt/DurationMs/Result the
// moment the wrapped work completes. DurationMs times the operation only:
// call sites that stamp best-effort telemetry after completion (Message,
// Nudge) seal first, stamp, then emit, so telemetry I/O never inflates the
// duration the dashboard renders.
func (e *operationEvent) sealTiming(err error) {
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
}

// emit records the sealed event.
func (e *operationEvent) emit() {
	if e == nil || e.target == nil || e.suppressed {
		return
	}
	e.target.populateOperationEventIdentity(&e.payload)
	e.target.recordWorkerOperationEvent(e.payload)
}

// finish seals and emits in one call — the contract for operations with no
// post-completion stamping.
func (e *operationEvent) finish(err error) {
	e.sealTiming(err)
	e.emit()
}

func (h *SessionHandle) populateOperationEventIdentity(payload *operationEventPayload) {
	if payload == nil {
		return
	}
	if payload.SessionID == "" {
		payload.SessionID = h.currentSessionID()
	}
	if info, bead, ok := h.currentOperationSessionInfo(); ok {
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
		// Per-operation run-root resolution off the session bead's own run chain
		// (workflow_id || molecule_id || gc.root_bead_id-or-self || bead id ||
		// session id for manual chat), shared with the compute-fact emitter via
		// beadmeta.ResolveRunID so a run's model and compute facts agree. Per-work-bead
		// attribution via a mutable work-bead pointer is deferred until a dispatch/claim
		// writer exists, so pooled sessions resolve per-session today
		// (engdocs/design/usage-facts-v0.md).
		if strings.TrimSpace(payload.RunID) == "" {
			payload.RunID = beadmeta.ResolveRunID(bead.Metadata, bead.ID, info.ID)
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

func (h *SessionHandle) currentOperationSessionInfo() (sessionpkg.Info, beads.Bead, bool) {
	id := h.currentSessionID()
	if id == "" {
		return sessionpkg.Info{}, beads.Bead{}, false
	}
	info, bead, err := h.manager.GetWithBead(id)
	if err != nil {
		return sessionpkg.Info{}, beads.Bead{}, false
	}
	return info, bead, true
}

// recordModelUsageFact writes one model usage fact to the handle's usage sink.
// Best-effort and non-blocking: the sink derives its own write deadline, and a
// failed write is logged (never a silent drop) rather than surfaced to the
// prompt path. Facts are built from real transcript usage by the
// invocation-telemetry seam (see recordInvocationTelemetry); a nil/discard sink
// is a no-op.
func (h *SessionHandle) recordModelUsageFact(f usage.Fact) {
	if h.usageSink == nil || h.usageSink == usage.Discard {
		return
	}
	// A fresh background context: this runs after the prompt op returns, so the
	// request context may already be canceled, and a durable fact write must not
	// be aborted by that. The sink enforces its own timeout.
	if err := h.usageSink.Record(context.Background(), f); err != nil {
		// Best-effort, but never a silent drop (engdocs/design/usage-facts-v0.md):
		// a misconfigured exec: sink, a full disk, or a permissions error must be
		// visible to the operator rather than quietly losing usage facts.
		slog.Warn("recording model usage fact failed; fact dropped",
			slog.String("run_id", f.RunID),
			slog.String("upstream_req_id", f.UpstreamReqID),
			slog.Any("error", err))
	}
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

// usageFactRecordingEnabled reports whether this handle can record usage facts,
// i.e. it has a live (non-discard) usage sink. It is independent of
// operationEventRecordingEnabled so model facts flow from the invocation-telemetry
// seam on handles configured with a sink but no event recorder (the CLI factory
// path).
func (h *SessionHandle) usageFactRecordingEnabled() bool {
	return h != nil && h.usageSink != nil && h.usageSink != usage.Discard
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

// operationUsageSummary aggregates the pending invocation batch that
// recordInvocationTelemetry recorded, so the wrapping worker.operation event
// carries the same tokens, model, and cost the usage facts do — the two
// surfaces read from one extraction and must agree. Aggregation is additive
// across the batch (a single prompt-op finish usually sees one invocation).
// Model takes the newest non-empty model observed (the batch is in file
// order). Cost sums only priced invocations; Unpriced is true when any
// token-bearing invocation had no resolved price, so a partial-price total is
// never read as authoritative.
type operationUsageSummary struct {
	model               string
	promptTokens        int
	completionTokens    int
	cacheReadTokens     int
	cacheCreationTokens int
	costUSD             float64
	unpriced            bool
}

// stampOperationEventUsage copies the extracted invocation usage onto the
// operation event payload. It is a no-op for a nil or suppressed event (the
// event won't emit), and it always sets Unpriced because it is only called
// once at least one token-bearing invocation was observed — mirroring the 1a
// contract that a set field means "measured", an absent field means "no data".
func stampOperationEventUsage(event *operationEvent, s operationUsageSummary) {
	if event == nil || event.suppressed || event.target == nil {
		return
	}
	event.payload.Model = s.model
	event.payload.PromptTokens = s.promptTokens
	event.payload.CompletionTokens = s.completionTokens
	event.payload.CacheReadTokens = s.cacheReadTokens
	event.payload.CacheCreationTokens = s.cacheCreationTokens
	event.payload.CostUSDEstimate = s.costUSD
	event.payload.Unpriced = boolPointer(s.unpriced)
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
