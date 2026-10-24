package api

// Per-domain Huma input/output types for the events handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_events.go.

import (
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// --- Event types ---

// EventListInput is the Huma input for GET /v0/city/{cityName}/events.
type EventListInput struct {
	CityScope
	BlockingParam
	PaginationParam
	Type  string `query:"type" required:"false" doc:"Filter by event type."`
	Actor string `query:"actor" required:"false" doc:"Filter by actor."`
	Since string `query:"since" required:"false" doc:"Filter events since duration ago (Go duration string, e.g. 5m)."`
}

// EventEmitRequest is the request body for POST /v0/city/{cityName}/events.
type EventEmitRequest struct {
	Type    string `json:"type" doc:"Event type." minLength:"1"`
	Actor   string `json:"actor" doc:"Actor that produced the event." minLength:"1"`
	Subject string `json:"subject,omitempty" doc:"Event subject."`
	Message string `json:"message,omitempty" doc:"Event message."`
}

// EventEmitInput is the Huma input for POST /v0/city/{cityName}/events.
type EventEmitInput struct {
	CityScope
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           EventEmitRequest
}

// EventEmitOutput is the response body for POST /v0/events.
type EventEmitOutput struct {
	Body struct {
		Status string `json:"status" doc:"Operation result." example:"recorded"`
	}
}

// EventRotateInput is the Huma input for POST /v0/city/{cityName}/events/rotate.
type EventRotateInput struct {
	CityScope
	Wait bool `query:"wait" required:"false" doc:"Wait for archive compression to complete before returning."`
}

// EventRotateArchive describes the archive produced by a successful force
// rotation.
type EventRotateArchive struct {
	Path              string `json:"path" doc:"Absolute path to the archive."`
	FirstSeq          uint64 `json:"first_seq" doc:"First event sequence included in the archive."`
	LastSeq           uint64 `json:"last_seq" doc:"Last event sequence included in the archive."`
	CompressionStatus string `json:"compression_status" enum:"pending,complete" doc:"Archive compression status."`
}

// EventRotateAnchor describes the events.rotated anchor event written to the
// new active log after a successful force rotation.
type EventRotateAnchor struct {
	Seq  uint64    `json:"seq" doc:"Anchor event sequence."`
	Type string    `json:"type" doc:"Anchor event type." example:"events.rotated"`
	Ts   time.Time `json:"ts" doc:"Anchor event timestamp."`
}

// EventRotateResponse is the response body for POST
// /v0/city/{cityName}/events/rotate.
type EventRotateResponse struct {
	Rotated     bool                `json:"rotated" doc:"Whether an archive was produced."`
	Reason      string              `json:"reason,omitempty" doc:"No-op reason when rotated is false."`
	Archive     *EventRotateArchive `json:"archive,omitempty" doc:"Archive metadata when rotated is true."`
	AnchorEvent *EventRotateAnchor  `json:"anchor_event,omitempty" doc:"Anchor event metadata when rotated is true."`
}

// EventRotateOutput wraps EventRotateResponse for Huma.
type EventRotateOutput struct {
	Body EventRotateResponse
}

// EventStreamInput is the Huma input for GET /v0/city/{cityName}/events/stream.
type EventStreamInput struct {
	CityScope
	AfterSeq    string `query:"after_seq" required:"false" doc:"Reconnect position: only deliver events after this sequence number. Omit after_seq and Last-Event-ID to start at the current city event head. A value that is not a non-negative integer is rejected with 422."`
	LastEventID string `header:"Last-Event-ID" required:"false" doc:"SSE reconnect position from the last received event ID. Omit Last-Event-ID and after_seq to start at the current city event head. Takes precedence over after_seq; a value that is not a non-negative integer starts at the current city event head."`
}

// HeartbeatEvent is an empty event emitted periodically on SSE streams to keep
// the connection alive through proxies. Clients can ignore this event type.
type HeartbeatEvent struct {
	Timestamp string `json:"timestamp" doc:"ISO 8601 timestamp when the heartbeat was sent."`
}

// SessionActivityEvent reports the current activity state of a session stream.
// Emitted whenever the session transitions between idle and in-turn states.
type SessionActivityEvent struct {
	Activity string `json:"activity" doc:"Session activity state: 'idle' or 'in-turn'." example:"idle"`
}

// SessionPendingClearedEvent reports that a previously pending interaction is
// no longer awaiting a response.
type SessionPendingClearedEvent struct {
	RequestID string `json:"request_id" doc:"Request ID of the interaction that was cleared."`
}

// resolveAfterSeq returns the reconnect position. A present Last-Event-ID
// wins, as on the supervisor and session streams; one that is not a sequence
// number is not a position the stream can place, so ok is false and the
// caller head-starts rather than reading it as 0, which Watch treats as
// "replay the entire retained history". after_seq has already been validated
// by Resolve.
func (e *EventStreamInput) resolveAfterSeq() (seq uint64, ok bool) {
	raw := e.LastEventID
	if raw == "" {
		raw = e.AfterSeq
	}
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Resolve rejects a malformed after_seq with 422 before the stream commits,
// like BlockingParam.index. Last-Event-ID is deliberately not rejected:
// EventSource clients treat any non-200 as fatal and stop reconnecting, so an
// unusable Last-Event-ID head-starts instead (see resolveAfterSeq).
func (e *EventStreamInput) Resolve(_ huma.Context) []error {
	if e.AfterSeq == "" {
		return nil
	}
	if _, err := strconv.ParseUint(e.AfterSeq, 10, 64); err != nil {
		return []error{&huma.ErrorDetail{
			Location: "query.after_seq",
			Message:  "after_seq must be a non-negative integer",
			Value:    e.AfterSeq,
		}}
	}
	return nil
}
