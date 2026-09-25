package runtime

import (
	"context"
	"time"
)

// TurnEventKind classifies a TurnEvent.
type TurnEventKind string

const (
	// TurnEventStarted means the runtime handed a prompt to the session's
	// agent and a turn is now running.
	TurnEventStarted TurnEventKind = "started"
	// TurnEventCompleted means a running turn ended. TurnEvent.Status says
	// how.
	TurnEventCompleted TurnEventKind = "completed"
)

// TurnStatus is how a completed turn ended.
type TurnStatus string

const (
	// TurnStatusCompleted means the agent answered the prompt.
	// TurnEvent.StopReason carries the agent's reason verbatim.
	TurnStatusCompleted TurnStatus = "completed"
	// TurnStatusCancelled means the agent answered the prompt as canceled,
	// for example after an interrupt.
	TurnStatusCancelled TurnStatus = "cancelled" //nolint:misspell // matches the ACP wire spelling
	// TurnStatusFailed means the turn ended without an answer: the agent
	// returned an error, the prompt could not be delivered, or the agent
	// exited first. TurnEvent.Error says which.
	TurnStatusFailed TurnStatus = "failed"
)

// TurnUsage is the token usage an agent reported for one turn. Fields the
// agent did not report are zero.
type TurnUsage struct {
	// InputTokens is the number of input tokens.
	InputTokens int64
	// OutputTokens is the number of output tokens.
	OutputTokens int64
	// TotalTokens is the total the agent reported.
	TotalTokens int64
	// ThoughtTokens is the number of reasoning tokens.
	ThoughtTokens int64
	// CachedReadTokens is the number of input tokens read from a cache.
	CachedReadTokens int64
	// CachedWriteTokens is the number of input tokens written to a cache.
	CachedWriteTokens int64
}

// TurnEvent reports the start or end of one agent turn, delivered by a
// TurnEventProvider stream.
//
// Unlike SessionEvent, a TurnEvent is a fact, not a level-triggered hint:
// each turn produces exactly one started event and, when it ends, exactly
// one completed event with the same TurnID.
type TurnEvent struct {
	// Kind classifies the event.
	Kind TurnEventKind
	// Session is the runtime session name the turn ran in.
	Session string
	// SessionID is the gc session ID (GC_SESSION_ID) the runtime was started
	// with; empty when the runtime was started without one.
	SessionID string
	// TurnID identifies the turn; the started and completed events of one
	// turn share it.
	TurnID string
	// Status is how the turn ended. Set on completed events only.
	Status TurnStatus
	// StopReason is the agent's reason for ending the turn, recorded
	// verbatim. Set when the agent answered the prompt (Status
	// TurnStatusCompleted or TurnStatusCancelled).
	StopReason string
	// Usage is the token usage the agent reported for the turn; nil when it
	// reported none. Set on completed events only.
	Usage *TurnUsage
	// Error describes why the turn failed. Set when Status is
	// TurnStatusFailed.
	Error string
	// Time is when the turn started (started events) or ended (completed
	// events).
	Time time.Time
}

// TurnEventProvider is an optional extension for runtimes that observe a
// protocol-level turn boundary, so consumers can record when an agent's
// turn starts and how it ends without scraping output.
//
// Contract:
//   - A subscription reports turns that start after it was opened; a turn
//     already running at subscribe time is not reported, not even its end.
//   - Delivery is best-effort: each subscriber has a bounded buffer, and an
//     event that does not fit is dropped and the loss logged. The session
//     transcript, not this stream, is the durable record.
//   - Canceling ctx ends the stream and closes the channel.
type TurnEventProvider interface {
	SubscribeTurnEvents(ctx context.Context) (<-chan TurnEvent, error)
}
