package runtime

import (
	"context"
	"time"
)

// SessionEventKind classifies a SessionEvent.
type SessionEventKind string

const (
	// SessionEventResync marks the (re)establishment of the provider's event
	// stream. Events may have been missed while the stream was down (provider
	// event transports carry no sequence numbers, so a gap is undetectable),
	// so consumers must reconcile from polled state (ListRunning etc.) before
	// trusting subsequent events. Emitted as the first event of every
	// connection cycle, and again after in-stream loss (consumer
	// backpressure).
	SessionEventResync SessionEventKind = "resync"
	// SessionEventExited means the session's process exited. Its pane/box may
	// still exist; SessionEventClosed reports the removal.
	SessionEventExited SessionEventKind = "exited"
	// SessionEventClosed means the session's pane/box was removed.
	SessionEventClosed SessionEventKind = "closed"
	// SessionEventAgentDetected means the provider recognized an agent TUI
	// running inside the session.
	SessionEventAgentDetected SessionEventKind = "agent_detected"
	// SessionEventAgentStatus reports a change of the session agent's
	// activity status (see SessionEvent.AgentStatus).
	SessionEventAgentStatus SessionEventKind = "agent_status"
)

// SessionEvent is a push notification about one session, delivered by a
// SessionEventProvider stream.
//
// Events are level-triggered hints, not authoritative state transitions:
// providers may re-deliver stale events (herdr replays a backlog of recent
// session events to every new subscription, and frames carry nothing that
// would let the stream filter the replay). A consumer reacts to an event by
// checking the session's live state, never by applying the event's content
// as the current truth.
type SessionEvent struct {
	// Kind classifies the event.
	Kind SessionEventKind
	// Session is the gc session name the event concerns. Empty when the
	// provider cannot attribute the event to a session it knows (e.g. a pane
	// with no agent mapping); consumers should treat unattributed lifecycle
	// events as a hint to reconcile broadly rather than ignore them.
	Session string
	// AgentStatus carries the new status for SessionEventAgentStatus events,
	// in the provider's native vocabulary (herdr: idle | working | blocked |
	// done | unknown).
	AgentStatus string
	// Ref identifies the provider-native object the event came from (e.g. the
	// herdr pane id). For logging and diagnosis only — not a stable handle.
	Ref string
	// Time is the local receive time; provider events carry no timestamps.
	Time time.Time
}

// SessionEventProvider is an optional extension for providers with a native
// push event stream. It lets consumers react to session death and agent
// activity in real time instead of polling; providers without one (tmux) are
// simply not asserted to it and stay on the polled paths.
//
// Contract:
//   - The stream self-heals: on transport failure it reconnects with backoff
//     until ctx is canceled. Subscribing does not require the provider's
//     server to be up yet; the stream attaches when it appears.
//   - Every connection cycle begins with a SessionEventResync event, and a
//     resync is also emitted after any in-stream loss. Consumers must treat
//     resync as "poll now": events between cycles may be lost. Events right
//     after a resync may also be a replayed backlog of things that happened
//     before it (see SessionEvent) — level-triggered consumption absorbs
//     both.
//   - Under consumer backpressure the provider may drop events, coalescing
//     the loss into a later SessionEventResync — never a silent gap.
//   - Canceling ctx ends the stream and closes the channel.
type SessionEventProvider interface {
	SubscribeSessionEvents(ctx context.Context) (<-chan SessionEvent, error)
}
