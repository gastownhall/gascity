package api

import (
	"encoding/json"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"

	// Blank import: pgauth's init() registers PostgresCredentialResolvedPayload
	// in the events registry. The api package never references pgauth's types
	// directly (the payload bytes flow through events.Event.Payload as JSON),
	// so the import exists solely to fire the registration before the registry-
	// coverage tests run.
	_ "github.com/gastownhall/gascity/internal/pgauth"
)

// API-layer event payload types. Every API emitter takes one of these
// typed structs (or one defined in internal/extmsg) via the sealed
// events.Payload interface rather than map[string]any (Principle 7).
// The event bus stores payloads as []byte for domain-agnostic
// transport (Principle 4 edge case); the SSE projection uses the
// central events registry to decode the bytes back into the typed Go
// variant before emitting on the typed /v0/events/stream wire schema.

// MailEventPayload is the shape of every mail.* event payload
// (MailSent, MailRead, MailArchived, MailMarkedRead, MailMarkedUnread,
// MailReplied, MailDeleted). Message is nil for mark/archive/delete
// events; present for send/reply events.
type MailEventPayload struct {
	Rig     string        `json:"rig"`
	Message *mail.Message `json:"message,omitempty"`
}

// IsEventPayload marks MailEventPayload as an events.Payload variant.
func (MailEventPayload) IsEventPayload() {}

// Operation constants used by RequestFailedPayload.
const (
	RequestOperationCityCreate     = "city.create"
	RequestOperationCityUnregister = "city.unregister"
	RequestOperationSessionCreate  = "session.create"
	RequestOperationSessionMessage = "session.message"
	RequestOperationSessionSubmit  = "session.submit"
)

// --- Typed async request result payloads ---
//
// 5 success types (one per operation, fully typed) + 1 shared failure
// type. The event type encodes operation and outcome; no string
// discriminator fields on success payloads.

// CityCreateSucceededPayload is emitted on request.result.city.create.
type CityCreateSucceededPayload struct {
	RequestID string `json:"request_id" doc:"Correlation ID from the 202 response."`
	Name      string `json:"name" doc:"Resolved city name."`
	Path      string `json:"path" doc:"Resolved absolute city directory path."`
}

// IsEventPayload marks CityCreateSucceededPayload as an events.Payload variant.
func (CityCreateSucceededPayload) IsEventPayload() {}

// CityUnregisterSucceededPayload is emitted on request.result.city.unregister.
type CityUnregisterSucceededPayload struct {
	RequestID string `json:"request_id" doc:"Correlation ID from the 202 response."`
	Name      string `json:"name" doc:"City name that was unregistered."`
	Path      string `json:"path" doc:"Absolute city directory path."`
}

// IsEventPayload marks CityUnregisterSucceededPayload as an events.Payload variant.
func (CityUnregisterSucceededPayload) IsEventPayload() {}

// SessionCreateSucceededPayload is emitted on request.result.session.create.
type SessionCreateSucceededPayload struct {
	RequestID string          `json:"request_id" doc:"Correlation ID from the 202 response."`
	Session   sessionResponse `json:"session" doc:"Full session state as returned by GET /session/{id}. For session.create, this result is emitted only after the session has left creating and can accept normal metadata and lifecycle commands."`
}

// IsEventPayload marks SessionCreateSucceededPayload as an events.Payload variant.
func (SessionCreateSucceededPayload) IsEventPayload() {}

// SessionMessageSucceededPayload is emitted on request.result.session.message.
type SessionMessageSucceededPayload struct {
	RequestID string `json:"request_id" doc:"Correlation ID from the 202 response."`
	SessionID string `json:"session_id" doc:"Session ID that received the message."`
}

// IsEventPayload marks SessionMessageSucceededPayload as an events.Payload variant.
func (SessionMessageSucceededPayload) IsEventPayload() {}

// SessionSubmitSucceededPayload is emitted on request.result.session.submit.
type SessionSubmitSucceededPayload struct {
	RequestID string `json:"request_id" doc:"Correlation ID from the 202 response."`
	SessionID string `json:"session_id" doc:"Session ID that received the submission."`
	Queued    bool   `json:"queued" doc:"Whether the message was queued for later delivery."`
	Intent    string `json:"intent" doc:"Resolved submit intent (default, follow_up, interrupt_now)."`
}

// IsEventPayload marks SessionSubmitSucceededPayload as an events.Payload variant.
func (SessionSubmitSucceededPayload) IsEventPayload() {}

// ProjectIdentityStampedPayload carries one layer-write event for a scope
// identity reconcile. Source is one of generated, migrated_from_metadata,
// migrated_from_database, or cache_repair. Layer is one of L1, L2, or L3.
type ProjectIdentityStampedPayload struct {
	ScopeRoot string `json:"scope_root"`
	Source    string `json:"source"`
	Layer     string `json:"layer"`
	OldID     string `json:"old_id,omitempty"`
	NewID     string `json:"new_id"`
}

// IsEventPayload marks ProjectIdentityStampedPayload as an events.Payload variant.
func (ProjectIdentityStampedPayload) IsEventPayload() {}

// RequestFailedPayload is emitted on request.failed for any async
// operation that fails. The operation enum identifies which operation.
type RequestFailedPayload struct {
	RequestID    string `json:"request_id" doc:"Correlation ID from the 202 response."`
	Operation    string `json:"operation" enum:"city.create,city.unregister,session.create,session.message,session.submit" doc:"Which operation failed."`
	ErrorCode    string `json:"error_code" doc:"Machine-readable error code."`
	ErrorMessage string `json:"error_message" doc:"Human-readable error description."`
}

// IsEventPayload marks RequestFailedPayload as an events.Payload variant.
func (RequestFailedPayload) IsEventPayload() {}

// SupervisorShutdownPayload attributes a supervisor shutdown trigger so
// operators can diagnose why the supervisor exited without scraping
// macOS unified log or launchd state. Recorded immediately before the
// supervisor cancels its context and begins the city-stop cascade.
type SupervisorShutdownPayload struct {
	Source     string `json:"source" enum:"signal,socket_stop" doc:"Which path triggered the shutdown."`
	Signal     string `json:"signal,omitempty" doc:"For source=signal, the human-readable signal name (e.g. \"terminated\", \"interrupt\"). Empty for socket_stop."`
	ClientAddr string `json:"client_addr,omitempty" doc:"For source=socket_stop, the address reported by the connecting client. Typically empty for unix-socket peers."`
	Mode       string `json:"mode" enum:"destructive,preserve_sessions,unknown" doc:"Resulting shutdown mode."`
}

// IsEventPayload marks SupervisorShutdownPayload as an events.Payload variant.
func (SupervisorShutdownPayload) IsEventPayload() {}

// CityLifecyclePayload is the shape of non-terminal city.created and
// city.unregister_requested events recorded in the per-city event log
// during init/unregister for diagnostics.
type CityLifecyclePayload struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// IsEventPayload marks CityLifecyclePayload as an events.Payload variant.
func (CityLifecyclePayload) IsEventPayload() {}

// BeadEventPayload is the shape of every bead.* event payload
// (BeadCreated, BeadUpdated, BeadClosed, BeadDeleted). The payload carries a
// full snapshot of the bead as of the event; it is emitted by bd hooks and the
// beads CachingStore for local writes and reconcile-detected external changes.
type BeadEventPayload struct {
	Bead beads.Bead `json:"bead"`
}

// IsEventPayload marks BeadEventPayload as an events.Payload variant.
func (BeadEventPayload) IsEventPayload() {}

// UnmarshalJSON accepts the current {"bead": ...} payload shape and the
// legacy raw-bead shape emitted by older bd hook scripts.
func (p *BeadEventPayload) UnmarshalJSON(data []byte) error {
	var wrapped struct {
		Bead *json.RawMessage `json:"bead"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return err
	}
	if wrapped.Bead != nil {
		bead, err := decodeBeadEventPayloadBead(*wrapped.Bead)
		if err != nil {
			return err
		}
		p.Bead = bead
		return nil
	}

	bead, err := decodeBeadEventPayloadBead(data)
	if err != nil {
		return err
	}
	p.Bead = bead
	return nil
}

func decodeBeadEventPayloadBead(data []byte) (beads.Bead, error) {
	var wire struct {
		ID           string          `json:"id"`
		Title        string          `json:"title"`
		Status       string          `json:"status"`
		Type         string          `json:"issue_type"`
		TypeCompat   string          `json:"type,omitempty"`
		Priority     *int            `json:"priority,omitempty"`
		CreatedAt    time.Time       `json:"created_at"`
		Assignee     string          `json:"assignee,omitempty"`
		From         string          `json:"from,omitempty"`
		ParentID     string          `json:"parent,omitempty"`
		Ref          string          `json:"ref,omitempty"`
		Needs        []string        `json:"needs,omitempty"`
		Description  string          `json:"description,omitempty"`
		Labels       []string        `json:"labels,omitempty"`
		Metadata     beads.StringMap `json:"metadata,omitempty"`
		Dependencies []beads.Dep     `json:"dependencies,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return beads.Bead{}, err
	}
	bead := beads.Bead{
		ID:           wire.ID,
		Title:        wire.Title,
		Status:       wire.Status,
		Type:         wire.Type,
		Priority:     wire.Priority,
		CreatedAt:    wire.CreatedAt,
		Assignee:     wire.Assignee,
		From:         wire.From,
		ParentID:     wire.ParentID,
		Ref:          wire.Ref,
		Needs:        wire.Needs,
		Description:  wire.Description,
		Labels:       wire.Labels,
		Dependencies: wire.Dependencies,
	}
	if bead.Type == "" {
		bead.Type = wire.TypeCompat
	}
	if wire.Metadata != nil {
		bead.Metadata = map[string]string(wire.Metadata)
	}
	return bead, nil
}

// SessionLifecyclePayload is the typed payload for terminal session
// lifecycle events (session.stopped, session.crashed). Subscribers rely
// on SessionID to correlate the event with the session bead regardless
// of how the existing Subject envelope field is populated (sometimes a
// session ID, sometimes a template/display name — kept stable for
// backward compatibility). Template and Reason are diagnostic and may
// be empty when the emission site lacks the corresponding context.
type SessionLifecyclePayload struct {
	SessionID string `json:"session_id" doc:"Canonical session bead ID. Always present."`
	Template  string `json:"template,omitempty" doc:"Session template name when known at the emission site."`
	Reason    string `json:"reason,omitempty" doc:"Short human-readable reason."`
}

// IsEventPayload marks SessionLifecyclePayload as an events.Payload variant.
func (SessionLifecyclePayload) IsEventPayload() {}

// SessionLifecyclePayloadJSON builds the JSON wire form of a
// SessionLifecyclePayload for attachment to an events.Event.Payload
// field. Template and Reason are emitted only when non-empty.
func SessionLifecyclePayloadJSON(sessionID, template, reason string) json.RawMessage {
	b, _ := json.Marshal(SessionLifecyclePayload{
		SessionID: sessionID,
		Template:  template,
		Reason:    reason,
	})
	return b
}

// WorkerOperationEventPayload is the typed payload projected for
// worker.operation events on the supervisor event stream.
//
// Issue #1252 (1a) added the per-invocation cost/latency fields below
// (Model through CostUSDEstimate).
//
// # Consumer contract — read this before using the 1a fields
//
// Every 1a field is BEST-EFFORT and OPTIONAL. The wire encoding uses
// `omitempty` on each so an absent field literally does not appear in
// the JSON. Consumers MUST treat the absence of a field (or its
// zero value when reading typed Go) as "no data observed" and not as a
// real signal:
//
//   - PromptTokens=0 does NOT mean "this op was free"; it means token
//     extraction was not wired for this operation.
//   - CostUSDEstimate=0 does NOT mean "this op cost nothing"; it means
//     either tokens or pricing was not wired.
//   - Empty Model / PromptVersion / PromptSHA do NOT mean "no model" or
//     "version unknown"; they mean source-side wiring has not yet
//     propagated metadata into the event.
//
// Aggregations that sum across events (per-agent cost, per-rig token
// volumes) MUST filter to events that actually carry the field — for
// example by checking `Model != ""` before bucketing by model. New
// consumers should keep that presence check at their input boundary.
//
// # Wiring status (snapshot at PR #1272 merge)
//
// The "Wired" line on each field below tracks which source-side
// wiring has landed. As subsequent PRs land follow-up wiring, those
// lines should be updated. TestWorkerOperationPayload1aWiringStatusPin
// fails when the runtime drifts from these annotations, catching a
// missed update so consumer-side filtering can be revisited.
type WorkerOperationEventPayload struct {
	OpID        string    `json:"op_id"`
	Operation   string    `json:"operation"`
	Result      string    `json:"result"`
	SessionID   string    `json:"session_id,omitempty"`
	SessionName string    `json:"session_name,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Transport   string    `json:"transport,omitempty"`
	Template    string    `json:"template,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	DurationMs  int64     `json:"duration_ms"`
	Queued      *bool     `json:"queued,omitempty"`
	Delivered   *bool     `json:"delivered,omitempty"`
	Error       string    `json:"error,omitempty"`

	// 1a fields. All omitempty; consumers must treat the field set as
	// best-effort. See the consumer contract on the type doc above.

	// Model is the LLM model identifier observed in this operation
	// (e.g. "claude-opus-4-8"). Sourced from session metadata.
	//
	// Wired: TODO — follow-up will tail sessionlog at finish() to
	// extract msg.Model.
	Model string `json:"model,omitempty" doc:"LLM model identifier (best-effort, may be absent until follow-up wiring lands)."`
	// AgentName is the agent identity that ran this operation
	// (e.g. "rig/polecat-1"). Distinct from SessionName which carries
	// the canonical session identity.
	//
	// Wired: YES — sourced from session.Info.AgentName, with
	// session.Info.Alias as a compatibility fallback.
	AgentName string `json:"agent_name,omitempty" doc:"Qualified agent identity (best-effort, absent if the session has no agent_name metadata or alias)."`
	// PromptVersion is the human-readable template version label from
	// frontmatter (`version:` field). Surfaced in dashboards for grouping.
	//
	// Wired: TODO — promptmeta.FrontMatter is computed (PR #1272) but
	// not propagated through session metadata into operation events
	// yet. Currently always absent on the wire.
	PromptVersion string `json:"prompt_version,omitempty" doc:"Template version frontmatter (best-effort, currently always absent; #1256 follow-up)."`
	// PromptSHA is the SHA-256 hex digest of the rendered prompt.
	// Distinguishes two runs that share PromptVersion but differ in
	// rendered bytes (unbumped template edit).
	//
	// Wired: TODO — promptmeta.SHA is computed (PR #1272) but not
	// propagated through session metadata yet. Currently always absent.
	PromptSHA string `json:"prompt_sha,omitempty" doc:"SHA-256 of the rendered prompt (best-effort, currently always absent; #1256 follow-up)."`
	// BeadID is the work bead this operation is acting on, when one
	// exists. Empty for operations not tied to a bead (e.g. lifecycle
	// transitions).
	//
	// Wired: TODO — operation context plumbing pending.
	BeadID string `json:"bead_id,omitempty" doc:"Work bead this operation is acting on (best-effort, may be absent for non-bead-scoped ops)."`
	// PromptTokens is the count of regular (non-cached) input tokens.
	// Treat absence as "not measured", not "zero".
	//
	// Wired: TODO — sessionlog/tail.go already extracts the value for
	// Claude; finish-time wiring through to this field is pending.
	PromptTokens int `json:"prompt_tokens,omitempty" doc:"Non-cached input tokens (best-effort, currently always absent; treat zero as 'not measured', not 'free')."`
	// CompletionTokens is the count of output tokens generated.
	// Treat absence as "not measured".
	//
	// Wired: TODO — see PromptTokens.
	CompletionTokens int `json:"completion_tokens,omitempty" doc:"Output tokens (best-effort, currently always absent)."`
	// CacheReadTokens is the count of cached input tokens read.
	// Distinct from PromptTokens because cache-read pricing is roughly
	// 10× cheaper than prompt pricing on Claude. Treat absence as
	// "not measured".
	//
	// Wired: TODO — see PromptTokens.
	CacheReadTokens int `json:"cache_read_tokens,omitempty" doc:"Cached input tokens read (best-effort, currently always absent)."`
	// CacheCreationTokens is the count of input tokens written into
	// the prompt cache during this invocation.
	//
	// Wired: TODO — see PromptTokens.
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty" doc:"Input tokens written into the prompt cache (best-effort, currently always absent)."`
	// LatencyMs is the wall-clock latency of the LLM invocation
	// itself, where measurable. Distinct from DurationMs which times
	// the wrapping operation.
	//
	// Wired: TODO — no LLM-invocation latency source exists yet.
	LatencyMs int64 `json:"latency_ms,omitempty" doc:"LLM invocation wall-clock latency (best-effort, currently always absent — no source)."`
	// CostUSDEstimate is a decision-support cost estimate computed
	// against the pricing seam (#1255, 1d). NOT invoice-grade. Treat
	// absence as "not measured", not "free".
	//
	// Wired: TODO — pricing.Registry exists (PR #1272); finish-time
	// wiring to compute cost from token counts is pending.
	CostUSDEstimate float64 `json:"cost_usd_estimate,omitempty" doc:"Estimated invocation cost in USD (best-effort, currently always absent; see #1255 for pricing seam)."`
}

// IsEventPayload marks WorkerOperationEventPayload as an events.Payload variant.
func (WorkerOperationEventPayload) IsEventPayload() {}

// SessionDrainAckedWithAssignedWorkPayload carries the bead-side context for
// a session that drain-acked while still holding the assignee on an open or
// in-progress work bead. The reconciler emits this as a mechanism-only
// signal (per gastownhall/gascity#2293) so pack-level subscribers can apply
// recovery policy without baking pack-specific knowledge into the SDK.
type SessionDrainAckedWithAssignedWorkPayload struct {
	SessionID  string `json:"session_id" doc:"Canonical session bead ID for the session that drain-acked."`
	BeadID     string `json:"bead_id" doc:"ID of the work bead still holding this session as its assignee."`
	Template   string `json:"template,omitempty" doc:"Pool template name when known at the emission site."`
	BeadStatus string `json:"bead_status,omitempty" doc:"Status of the stranded bead at emission time (typically 'in_progress' for cap-hit, 'open' if recovery races claim)."`
	Reason     string `json:"reason,omitempty" doc:"Short diagnostic context. Today both emission sites pass 'drain_acked_with_assigned_work'; reserved for finer-grained shape discriminators if later Shape-N variants land."`
}

// IsEventPayload marks SessionDrainAckedWithAssignedWorkPayload as an events.Payload variant.
func (SessionDrainAckedWithAssignedWorkPayload) IsEventPayload() {}

// SessionDrainAckedWithAssignedWorkPayloadJSON builds the JSON wire form for
// attachment to an events.Event.Payload field. Template, BeadStatus, and
// Reason are emitted only when non-empty.
func SessionDrainAckedWithAssignedWorkPayloadJSON(sessionID, beadID, template, beadStatus, reason string) json.RawMessage {
	b, _ := json.Marshal(SessionDrainAckedWithAssignedWorkPayload{
		SessionID:  sessionID,
		BeadID:     beadID,
		Template:   template,
		BeadStatus: beadStatus,
		Reason:     reason,
	})
	return b
}

func init() {
	// mail.* — all seven types share one payload shape.
	events.RegisterPayload(events.MailSent, MailEventPayload{})
	events.RegisterPayload(events.MailRead, MailEventPayload{})
	events.RegisterPayload(events.MailArchived, MailEventPayload{})
	events.RegisterPayload(events.MailMarkedRead, MailEventPayload{})
	events.RegisterPayload(events.MailMarkedUnread, MailEventPayload{})
	events.RegisterPayload(events.MailReplied, MailEventPayload{})
	events.RegisterPayload(events.MailDeleted, MailEventPayload{})

	// bead.* — carry the bead snapshot.
	events.RegisterPayload(events.BeadCreated, BeadEventPayload{})
	events.RegisterPayload(events.BeadUpdated, BeadEventPayload{})
	events.RegisterPayload(events.BeadClosed, BeadEventPayload{})
	events.RegisterPayload(events.BeadDeleted, BeadEventPayload{})

	// session.* / convoy.* / controller.* / city.* / order.* /
	// provider.* — these events carry no structured payload today;
	// their semantics are fully captured by the envelope's Actor,
	// Subject, and Message fields. NoPayload registers an empty typed
	// shape so the spec still emits a discriminated-union variant
	// for the event type and the registry-coverage test passes.
	events.RegisterPayload(events.SessionWoke, events.NoPayload{})
	events.RegisterPayload(events.SessionStopped, SessionLifecyclePayload{})
	events.RegisterPayload(events.SessionCrashed, SessionLifecyclePayload{})
	events.RegisterPayload(events.SessionDraining, events.NoPayload{})
	events.RegisterPayload(events.SessionUndrained, events.NoPayload{})
	events.RegisterPayload(events.SessionQuarantined, events.NoPayload{})
	events.RegisterPayload(events.SessionIdleKilled, events.NoPayload{})
	events.RegisterPayload(events.SessionMaxAgeKilled, events.NoPayload{})
	events.RegisterPayload(events.SessionSuspended, events.NoPayload{})
	events.RegisterPayload(events.SessionUpdated, events.NoPayload{})
	events.RegisterPayload(events.SessionDrainAckedWithAssignedWork, SessionDrainAckedWithAssignedWorkPayload{})
	events.RegisterPayload(events.SessionStranded, events.NoPayload{})
	events.RegisterPayload(events.SessionResetStalled, events.SessionResetStalledPayload{})
	events.RegisterPayload(events.SessionWorkQueryFailed, SessionLifecyclePayload{})
	events.RegisterPayload(events.SessionColdStartTimeout, events.NoPayload{})
	events.RegisterPayload(events.ConvoyCreated, events.NoPayload{})
	events.RegisterPayload(events.ConvoyClosed, events.NoPayload{})
	events.RegisterPayload(events.ControllerStarted, events.NoPayload{})
	events.RegisterPayload(events.ControllerStopped, events.NoPayload{})
	events.RegisterPayload(events.SupervisorShutdownRequested, SupervisorShutdownPayload{})
	events.RegisterPayload(events.CitySuspended, events.NoPayload{})
	events.RegisterPayload(events.CityResumed, events.NoPayload{})
	// Typed async request result events.
	events.RegisterPayload(events.RequestResultCityCreate, CityCreateSucceededPayload{})
	events.RegisterPayload(events.RequestResultCityUnregister, CityUnregisterSucceededPayload{})
	events.RegisterPayload(events.RequestResultSessionCreate, SessionCreateSucceededPayload{})
	events.RegisterPayload(events.RequestResultSessionMessage, SessionMessageSucceededPayload{})
	events.RegisterPayload(events.RequestResultSessionSubmit, SessionSubmitSucceededPayload{})
	events.RegisterPayload(events.RequestFailed, RequestFailedPayload{})

	// Non-terminal city lifecycle events (diagnostics only).
	events.RegisterPayload(events.CityCreated, CityLifecyclePayload{})
	events.RegisterPayload(events.CityUnregisterRequested, CityLifecyclePayload{})

	events.RegisterPayload(events.OrderFired, events.NoPayload{})
	events.RegisterPayload(events.OrderCompleted, events.NoPayload{})
	events.RegisterPayload(events.OrderFailed, events.NoPayload{})
	events.RegisterPayload(events.ProviderSwapped, events.NoPayload{})
	events.RegisterPayload(events.WorkerOperation, WorkerOperationEventPayload{})
	events.RegisterPayload(events.ProjectIdentityStamped, ProjectIdentityStampedPayload{})

	// gc.store.maintenance.* — supervisor StoreMaintenanceLoop outcomes.
	events.RegisterPayload(events.StoreMaintenanceDone, events.StoreMaintenanceDonePayload{})
	events.RegisterPayload(events.StoreMaintenanceFailed, events.StoreMaintenanceFailedPayload{})
}
