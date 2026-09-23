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
	// SessionEventAgentIdle means the session's agent reached the state where
	// it is ready for input. It is a semantic kind, not a relayed provider
	// status: each provider decides which of its own states is the
	// idle-equivalent and translates at its boundary, so no provider
	// vocabulary reaches a generic consumer.
	SessionEventAgentIdle SessionEventKind = "agent_idle"
	// SessionEventAgentStateChanged means the provider reported the session's
	// agent in some state other than the idle-equivalent, including a state the
	// provider cannot classify or did not name. It is deliberately weaker than
	// a transition: nothing here compares against a previous state, so a
	// provider that re-reports an unchanged state, or replays its recent
	// backlog on resubscribe, produces this kind more than once for one state.
	// It also does not say WHICH state. The arrival is the information, and a
	// consumer acts on it by reconciling against live state rather than by
	// trusting the event to describe one. Providers emit it instead of relaying
	// their own spelling, so a consumer needing a specific state asks for a
	// kind for it rather than matching a string.
	SessionEventAgentStateChanged SessionEventKind = "agent_state_changed"
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

// SessionEventStaleAfter bounds how long a session-event source may go
// silent before it is no longer trusted as live. A provider's stream
// self-heals without ever closing on a transient transport failure (see
// SessionEventProvider's contract), so silence — not a channel close — is
// the only signal an outage leaves behind. Composite providers (auto,
// hybrid) use this to detect partial backend failure when fanning in
// multiple backends' streams: without a per-backend bound, a healthy
// backend's traffic keeps the merged stream looking alive indefinitely even
// while the other backend's stream has gone silent, masking that backend's
// outage from every consumer of the merged stream. 30s is 6x herdr's max
// reconnect backoff (5s, internal/runtime/herdr/events.go), giving margin
// for reconnect latency and scheduling jitter before declaring a source
// stale.
const SessionEventStaleAfter = 30 * time.Second

// EventCapableRouter is implemented by composite providers (e.g. auto,
// hybrid) that route different sessions to different backends. Asserting a
// provider against SessionEventProvider alone answers "is ANY routed backend
// event-capable", which for a composite is true whenever its local side is,
// even for sessions it routes elsewhere. A provider that can report
// per-session capability must be asked per-session.
type EventCapableRouter interface {
	EventCapableRoute(name string) bool
}

// CompositeSessionEventStaleness is implemented by composite providers (e.g.
// auto, hybrid) whose SubscribeSessionEvents fans in more than one backend's
// stream into one merged channel. MergedStreamStale reports whether any
// fanned-in backend has gone silent past SessionEventStaleAfter, WITHOUT
// terminating the merged channel: a consumer computing liveness off the
// single merged stream (e.g. cmd/gc's sessionEventPump.flowing()) would
// otherwise never see one backend's outage as long as the other kept
// producing (see SessionEventStaleAfter). Closing the merged channel to
// surface that was tried and rejected: a channel close is permanent and the
// only two production callers of pump.restart are startup and a
// provider-changing config reload, so a merely-idle-for-30s backend that was
// never actually broken would have killed event-driven liveness for the
// WHOLE composite, including the still-healthy backend, until one of those
// rare events happened to fire. Reporting staleness at query time instead
// lets the healthy backend's events keep flowing and lets a recovered
// backend clear its own staleness on its next event, with no restart
// needed.
type CompositeSessionEventStaleness interface {
	MergedStreamStale() bool
}
