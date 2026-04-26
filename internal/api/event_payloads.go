package api

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
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

const (
	RequestOperationCityCreate      = "city.create"
	RequestOperationCityUnregister  = "city.unregister"
	RequestOperationSessionCreate   = "session.create"
	RequestOperationSessionMessage  = "session.message"
	RequestOperationSessionSubmit   = "session.submit"
	RequestStatusSucceeded          = "succeeded"
	RequestStatusFailed             = "failed"
)

// RequestResultPayload is emitted by request.result events when an
// asynchronous API operation completes. Every long-running mutation
// (city create, city unregister, provider session create, message
// delivery to a suspended session) returns 202 with a request_id;
// the completion event carries the same request_id so clients can
// correlate responses to requests across the event stream.
type RequestResultPayload struct {
	RequestID    string `json:"request_id" doc:"Correlation ID returned by the 202 response that started this operation."`
	Operation    string `json:"operation" doc:"The operation that completed (city.create, city.unregister, session.create, session.message, session.submit)."`
	Status       string `json:"status" doc:"succeeded or failed."`
	ResourceID   string `json:"resource_id,omitempty" doc:"ID of the created/affected resource (city name, session ID, etc.)."`
	ErrorCode    string `json:"error_code,omitempty" doc:"Machine-readable error code when status is failed."`
	ErrorMessage string `json:"error_message,omitempty" doc:"Human-readable error description when status is failed."`
}

// IsEventPayload marks RequestResultPayload as an events.Payload variant.
func (RequestResultPayload) IsEventPayload() {}

// CityLifecyclePayload is used by deprecated city.* events during
// migration to request.result. Remove once all emission sites are updated.
type CityLifecyclePayload struct {
	Name            string   `json:"name"`
	Path            string   `json:"path"`
	Error           string   `json:"error,omitempty"`
	PhasesCompleted []string `json:"phases_completed,omitempty"`
}

func (CityLifecyclePayload) IsEventPayload() {}

// CityCreatedPayload is deprecated; use RequestResultPayload.
type CityCreatedPayload = CityLifecyclePayload

// CityReadyPayload is deprecated; use RequestResultPayload.
type CityReadyPayload = CityLifecyclePayload

// CityInitFailedPayload is deprecated; use RequestResultPayload.
type CityInitFailedPayload = CityLifecyclePayload

// CityUnregisterRequestedPayload is deprecated; use RequestResultPayload.
type CityUnregisterRequestedPayload = CityLifecyclePayload

// CityUnregisteredPayload is deprecated; use RequestResultPayload.
type CityUnregisteredPayload = CityLifecyclePayload

// CityUnregisterFailedPayload is deprecated; use RequestResultPayload.
type CityUnregisterFailedPayload = CityLifecyclePayload

// BeadEventPayload is the shape of every bead.* event payload
// (BeadCreated, BeadUpdated, BeadClosed). The payload carries a full
// snapshot of the bead as of the event; it is emitted by the beads
// CachingStore's reconcile loop when external changes are detected.
type BeadEventPayload struct {
	Bead beads.Bead `json:"bead"`
}

// IsEventPayload marks BeadEventPayload as an events.Payload variant.
func (BeadEventPayload) IsEventPayload() {}

// WorkerOperationEventPayload is the typed payload projected for
// worker.operation events on the supervisor event stream.
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
}

// IsEventPayload marks WorkerOperationEventPayload as an events.Payload variant.
func (WorkerOperationEventPayload) IsEventPayload() {}

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

	// session.* / convoy.* / controller.* / city.* / order.* /
	// provider.* — these events carry no structured payload today;
	// their semantics are fully captured by the envelope's Actor,
	// Subject, and Message fields. NoPayload registers an empty typed
	// shape so the spec still emits a discriminated-union variant
	// for the event type and the registry-coverage test passes.
	events.RegisterPayload(events.SessionWoke, events.NoPayload{})
	events.RegisterPayload(events.SessionStopped, events.NoPayload{})
	events.RegisterPayload(events.SessionCrashed, events.NoPayload{})
	events.RegisterPayload(events.SessionDraining, events.NoPayload{})
	events.RegisterPayload(events.SessionUndrained, events.NoPayload{})
	events.RegisterPayload(events.SessionQuarantined, events.NoPayload{})
	events.RegisterPayload(events.SessionIdleKilled, events.NoPayload{})
	events.RegisterPayload(events.SessionSuspended, events.NoPayload{})
	events.RegisterPayload(events.SessionUpdated, events.NoPayload{})
	events.RegisterPayload(events.ConvoyCreated, events.NoPayload{})
	events.RegisterPayload(events.ConvoyClosed, events.NoPayload{})
	events.RegisterPayload(events.ControllerStarted, events.NoPayload{})
	events.RegisterPayload(events.ControllerStopped, events.NoPayload{})
	events.RegisterPayload(events.CitySuspended, events.NoPayload{})
	events.RegisterPayload(events.CityResumed, events.NoPayload{})
	events.RegisterPayload(events.RequestResult, RequestResultPayload{})

	// Deprecated city.* payloads — remove as emission sites migrate.
	events.RegisterPayload(events.CityCreated, CityCreatedPayload{})
	events.RegisterPayload(events.CityReady, CityReadyPayload{})
	events.RegisterPayload(events.CityInitFailed, CityInitFailedPayload{})
	events.RegisterPayload(events.CityUnregisterRequested, CityUnregisterRequestedPayload{})
	events.RegisterPayload(events.CityUnregistered, CityUnregisteredPayload{})
	events.RegisterPayload(events.CityUnregisterFailed, CityUnregisterFailedPayload{})

	events.RegisterPayload(events.OrderFired, events.NoPayload{})
	events.RegisterPayload(events.OrderCompleted, events.NoPayload{})
	events.RegisterPayload(events.OrderFailed, events.NoPayload{})
	events.RegisterPayload(events.ProviderSwapped, events.NoPayload{})
	events.RegisterPayload(events.WorkerOperation, WorkerOperationEventPayload{})
}
