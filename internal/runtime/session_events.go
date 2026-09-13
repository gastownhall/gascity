package runtime

import (
	"context"
	"fmt"
	"sync"
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

// MergeSessionEvents subscribes to every non-nil provider and fans their
// streams into one channel, so a composite runtime.Provider wrapping more
// than one SessionEventProvider backend (e.g. a city where both the default
// and ACP backends happen to support the interface) can satisfy
// SessionEventProvider itself without consumers needing to know how many
// backends are underneath. Each backend's own self-healing (reconnect with
// backoff, resync framing) is untouched; this only relays what each stream
// emits.
//
// A subscribe failure on one provider does not fail the merge as long as at
// least one other succeeds — a partially event-capable composite is still
// better than falling back to polling entirely. Only when every provider
// fails to subscribe does this return an error, so the caller (typically an
// interface assertion elsewhere expecting a real stream) gets an explicit
// signal instead of a channel that will never deliver anything.
func MergeSessionEvents(ctx context.Context, providers ...SessionEventProvider) (<-chan SessionEvent, error) {
	out := make(chan SessionEvent)
	var wg sync.WaitGroup
	var firstErr error
	subscribed := 0
	for _, sep := range providers {
		if sep == nil {
			continue
		}
		ch, err := sep.SubscribeSessionEvents(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		subscribed++
		wg.Add(1)
		go func(ch <-chan SessionEvent) {
			defer wg.Done()
			for {
				select {
				case ev, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}(ch)
	}
	if subscribed == 0 {
		close(out)
		if firstErr != nil {
			return out, firstErr
		}
		return out, fmt.Errorf("no session-event providers to subscribe to")
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, nil
}
