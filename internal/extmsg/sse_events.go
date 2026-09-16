package extmsg

import "time"

// SSEEventType is the type discriminator for SSE event data payloads.
type SSEEventType = string

const (
	// SSEEventTypeMessage is the event type for a session reply message.
	SSEEventTypeMessage = "message"
	// SSEEventTypeHeartbeat is the event type for a keepalive heartbeat.
	SSEEventTypeHeartbeat = "heartbeat"
	// SSEEventTypeError is the event type for a stream error.
	SSEEventTypeError = "error"
)

// SSEEvent is the union of SSE event payload types carried over the stream.
type SSEEvent interface {
	sseEvent()
}

// SSEMessageEvent is the data payload for event: message.
// Emitted when the target session produces a reply for this conversation.
//
// Field stability: all fields are stable per wire contract §4.1.
// Clients may switch on Version and Session.
type SSEMessageEvent struct {
	Version      string          `json:"version"`
	Event        string          `json:"event"`
	Text         string          `json:"text"`
	SessionID    string          `json:"session_id"`
	Conversation ConversationRef `json:"conversation"`
	Sequence     int64           `json:"sequence"`
	CreatedAt    time.Time       `json:"created_at"`
}

func (SSEMessageEvent) sseEvent() {}

// SSEHeartbeatEvent is the data payload for event: heartbeat.
// Emitted when no message or error has been sent within the heartbeat interval.
//
// Field stability: version and event are stable; ts is informational.
type SSEHeartbeatEvent struct {
	Version string    `json:"version"`
	Event   string    `json:"event"`
	TS      time.Time `json:"ts"`
}

func (SSEHeartbeatEvent) sseEvent() {}

// SSEErrorEvent is the data payload for event: error.
// Emitted when the server must close the stream.
//
// RetryAfterMs is a pointer so that non-retryable errors omit the field
// entirely (omitempty on a *int64 produces no key, not null).
type SSEErrorEvent struct {
	Version      string `json:"version"`
	Event        string `json:"event"`
	Code         string `json:"code"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable"`
	RetryAfterMs *int64 `json:"retry_after_ms,omitempty"`
}

func (SSEErrorEvent) sseEvent() {}

// NewSSEMessageEvent builds a message event with the v1 version tag.
func NewSSEMessageEvent(text, sessionID string, conv ConversationRef, seq int64, createdAt time.Time) SSEMessageEvent {
	return SSEMessageEvent{
		Version:      "1",
		Event:        SSEEventTypeMessage,
		Text:         text,
		SessionID:    sessionID,
		Conversation: conv,
		Sequence:     seq,
		CreatedAt:    createdAt.UTC(),
	}
}

// NewSSEHeartbeatEvent builds a heartbeat event with the v1 version tag.
func NewSSEHeartbeatEvent(ts time.Time) SSEHeartbeatEvent {
	return SSEHeartbeatEvent{
		Version: "1",
		Event:   SSEEventTypeHeartbeat,
		TS:      ts.UTC(),
	}
}

// NewSSEErrorEvent builds an error event with the v1 version tag.
// retryAfterMs is nil for non-retryable errors.
func NewSSEErrorEvent(code, message string, retryable bool, retryAfterMs *int64) SSEErrorEvent {
	return SSEErrorEvent{
		Version:      "1",
		Event:        SSEEventTypeError,
		Code:         code,
		Message:      message,
		Retryable:    retryable,
		RetryAfterMs: retryAfterMs,
	}
}
