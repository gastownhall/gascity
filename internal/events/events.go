// Package events provides tier-0 observability for Gas City.
//
// Events are infrastructure records of what happened (agent lifecycle,
// bead operations, controller state). The recorder writes JSON lines to
// .gc/events.jsonl; the reader scans them back. Recording is best-effort:
// errors are logged to stderr but never returned to callers.
//
// Agent observation data (messages, tool calls, thinking) is read directly
// from provider session logs via the sessionlog package, not the event bus.
package events

import (
	"context"
	"encoding/json"
	"time"
)

// Event type constants. Only types we actually emit today.
const (
	SessionWoke        = "session.woke"
	SessionStopped     = "session.stopped"
	SessionCrashed     = "session.crashed"
	BeadCreated        = "bead.created"
	BeadClosed         = "bead.closed"
	BeadUpdated        = "bead.updated"
	MailSent           = "mail.sent"
	MailRead           = "mail.read"
	MailArchived       = "mail.archived"
	MailMarkedRead     = "mail.marked_read"
	MailMarkedUnread   = "mail.marked_unread"
	MailReplied        = "mail.replied"
	MailDeleted        = "mail.deleted"
	SessionDraining    = "session.draining"
	SessionUndrained   = "session.undrained"
	SessionQuarantined = "session.quarantined"
	SessionIdleKilled  = "session.idle_killed"
	// SessionMaxAgeKilled fires when the controller preemptively restarts a
	// long-running session because its wall-clock age exceeded the agent's
	// max_session_age threshold. Motivating case: provider SDKs that cache
	// credentials at session start and wedge when the cached token expires.
	SessionMaxAgeKilled = "session.max_age_killed"
	SessionSuspended    = "session.suspended"
	SessionUpdated      = "session.updated"
	ConvoyCreated       = "convoy.created"
	ConvoyClosed        = "convoy.closed"
	ControllerStarted   = "controller.started"
	ControllerStopped   = "controller.stopped"
	CitySuspended       = "city.suspended"
	CityResumed         = "city.resumed"
	// Typed async request result events. 5 success types (one per
	// operation, fully typed payload) + 1 shared failure type.
	RequestResultCityCreate     = "request.result.city.create"
	RequestResultCityUnregister = "request.result.city.unregister"
	RequestResultSessionCreate  = "request.result.session.create"
	RequestResultSessionMessage = "request.result.session.message"
	RequestResultSessionSubmit  = "request.result.session.submit"
	RequestFailed               = "request.failed"

	// Non-terminal city lifecycle events recorded in the per-city
	// event log during init/unregister for diagnostics.
	CityCreated             = "city.created"
	CityUnregisterRequested = "city.unregister_requested"
	OrderFired              = "order.fired"
	OrderCompleted          = "order.completed"
	OrderFailed             = "order.failed"
	ProviderSwapped         = "provider.swapped"
	WorkerOperation         = "worker.operation"

	// External messaging events.
	ExtMsgBound          = "extmsg.bound"
	ExtMsgUnbound        = "extmsg.unbound"
	ExtMsgGroupCreated   = "extmsg.group_created"
	ExtMsgAdapterAdded   = "extmsg.adapter_added"
	ExtMsgAdapterRemoved = "extmsg.adapter_removed"
	ExtMsgInbound        = "extmsg.inbound"
	ExtMsgOutbound       = "extmsg.outbound"
)

// KnownEventTypes lists every event-type constant this package defines.
// The SSE projection uses this set (via a test) to verify that every
// event type has a registered payload — a missing registration is a
// programming error that fails CI, not a runtime condition.
var KnownEventTypes = []string{
	SessionWoke, SessionStopped, SessionCrashed,
	SessionDraining, SessionUndrained, SessionQuarantined,
	SessionIdleKilled, SessionMaxAgeKilled, SessionSuspended, SessionUpdated,
	BeadCreated, BeadClosed, BeadUpdated,
	MailSent, MailRead, MailArchived, MailMarkedRead, MailMarkedUnread,
	MailReplied, MailDeleted,
	ConvoyCreated, ConvoyClosed,
	ControllerStarted, ControllerStopped,
	CitySuspended, CityResumed,
	RequestResultCityCreate, RequestResultCityUnregister,
	RequestResultSessionCreate, RequestResultSessionMessage,
	RequestResultSessionSubmit, RequestFailed,
	CityCreated, CityUnregisterRequested,
	OrderFired, OrderCompleted, OrderFailed,
	ProviderSwapped, WorkerOperation,
	ExtMsgBound, ExtMsgUnbound, ExtMsgGroupCreated,
	ExtMsgAdapterAdded, ExtMsgAdapterRemoved,
	ExtMsgInbound, ExtMsgOutbound,
}

// Event is a single recorded occurrence in the system.
type Event struct {
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	Ts      time.Time       `json:"ts"`
	Actor   string          `json:"actor"`
	Subject string          `json:"subject,omitempty"`
	Message string          `json:"message,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Recorder records events. Safe for concurrent use. Best-effort.
// This sub-interface is used by callers that only need to write events.
type Recorder interface {
	Record(e Event)
}

// Provider is the full interface for event backends. It embeds Recorder
// for writing and adds reading, querying, and watching. Implementations
// include FileRecorder (built-in JSONL file) and exec (user-supplied
// script via fork/exec).
type Provider interface {
	Recorder

	// List returns events matching the filter.
	List(filter Filter) ([]Event, error)

	// LatestSeq returns the highest sequence number, or 0 if empty.
	LatestSeq() (uint64, error)

	// Watch returns a Watcher that yields events with Seq > afterSeq.
	// The watcher blocks on Next() until an event arrives or ctx is
	// canceled. Callers must call Close() when done.
	Watch(ctx context.Context, afterSeq uint64) (Watcher, error)

	// Close releases any resources held by the provider.
	Close() error
}

// TailProvider is an optional extension for providers that can return the
// trailing matching events without scanning or materializing the whole history.
type TailProvider interface {
	ListTail(filter Filter, limit int) ([]Event, error)
}

// Watcher yields events one at a time. Created by [Provider.Watch].
// Callers must call Close() when done watching.
type Watcher interface {
	// Next blocks until the next event is available, the context is
	// canceled, or the watcher is closed. Returns the event or an error.
	// Implementations must unblock any in-flight Next call when Close
	// is called or the parent context is canceled.
	Next() (Event, error)

	// Close stops the watcher, unblocks any pending Next call, and
	// releases resources. Safe to call concurrently with Next.
	Close() error
}

// Discard silently drops all events.
var Discard Recorder = discardRecorder{}

type discardRecorder struct{}

func (discardRecorder) Record(Event) {}
