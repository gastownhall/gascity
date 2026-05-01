package api

import (
	"encoding/json"
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
	Session   sessionResponse `json:"session" doc:"Full session state as returned by GET /session/{id}."`
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
// (BeadCreated, BeadUpdated, BeadClosed). The payload carries a full
// snapshot of the bead as of the event; it is emitted by bd hooks and by
// the beads CachingStore's reconcile loop when external changes are detected.
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
}
