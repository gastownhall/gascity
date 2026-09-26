package acp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file implements [runtime.SessionEventProvider] for the sessions one
// Provider instance owns in memory. Events are published from the JSON-RPC
// read loop, Nudge, the process monitor, and Stop, so the hub never blocks:
// a subscriber that cannot keep up loses events, and the loss is coalesced
// into a resync that is delivered as soon as the subscriber has room.
//
// Kinds:
//   - agent_state_changed when a session/prompt turn opens;
//   - agent_idle when the agent answers it (result, error, or cancel) or a
//     prompt that was never delivered is abandoned — never when the
//     connection drains, because a drained connection is gone, not idle;
//   - exited when the agent process exits;
//   - closed when Stop has torn the session down.

// sessionEventBuffer sizes each subscriber channel.
const sessionEventBuffer = 64

var _ runtime.SessionEventProvider = (*Provider)(nil)

// SubscribeSessionEvents implements [runtime.SessionEventProvider]. The
// stream opens with a resync, then carries turn, exit, and close events for
// sessions this Provider started. Sessions owned by another process are not
// reported. Canceling ctx closes the channel.
func (p *Provider) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	if ctx == nil {
		return nil, fmt.Errorf("acp: SubscribeSessionEvents requires a context")
	}
	return p.events.subscribe(ctx), nil
}

// sessionEventHub fans session events out to subscribers without blocking
// the publisher.
type sessionEventHub struct {
	mu   sync.Mutex
	subs map[*sessionEventSub]struct{}
}

// sessionEventSub is one subscriber. ch is sent to only under hub.mu by
// publishers, or by the subscriber's own goroutine, which is also the only
// closer; closing happens under hub.mu after removal, so no send can race it.
type sessionEventSub struct {
	ch chan runtime.SessionEvent
	// wake asks the subscriber goroutine to deliver a resync. Its one slot
	// coalesces drops: a drop either finds a wake still pending, whose
	// resync is sent after it, or queues a new one.
	wake chan struct{}
}

func newSessionEventHub() *sessionEventHub {
	return &sessionEventHub{subs: make(map[*sessionEventSub]struct{})}
}

// subscribe registers a subscriber whose stream opens with a resync.
func (h *sessionEventHub) subscribe(ctx context.Context) <-chan runtime.SessionEvent {
	sub := &sessionEventSub{
		ch:   make(chan runtime.SessionEvent, sessionEventBuffer),
		wake: make(chan struct{}, 1),
	}
	// The channel is empty and not yet shared, so this cannot block.
	sub.ch <- resyncEvent()
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	go h.serve(ctx, sub)
	return sub.ch
}

// serve delivers coalesced resyncs for sub until ctx is done, then closes it.
func (h *sessionEventHub) serve(ctx context.Context, sub *sessionEventSub) {
	defer h.unsubscribe(sub)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.wake:
		}
		// An event dropped from here on queues another wake, so every drop
		// is followed by a resync that is sent after it.
		select {
		case sub.ch <- resyncEvent():
		case <-ctx.Done():
			return
		}
	}
}

// unsubscribe removes sub and closes its channel.
func (h *sessionEventHub) unsubscribe(sub *sessionEventSub) {
	h.mu.Lock()
	delete(h.subs, sub)
	close(sub.ch)
	h.mu.Unlock()
}

// publish offers ev to every subscriber without blocking. A subscriber with
// a full channel drops ev, and the loss is reported by a later resync. A
// subscriber with room always receives ev, even while a resync for an
// earlier loss is still queued: that resync still follows the drop it
// covers, so delivering ev cannot hide a gap.
func (h *sessionEventHub) publish(ev runtime.SessionEvent) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default:
			select {
			case sub.wake <- struct{}{}:
			default:
			}
		}
	}
}

func resyncEvent() runtime.SessionEvent {
	return runtime.SessionEvent{Kind: runtime.SessionEventResync, Time: time.Now()}
}

// sessionEventSource attributes a connection's events to the session and
// process a Provider owns.
type sessionEventSource struct {
	hub     *sessionEventHub
	session string
	ref     string
}

// emit publishes one event of kind for the source's session.
func (s *sessionEventSource) emit(kind runtime.SessionEventKind) {
	if s == nil {
		return
	}
	s.hub.publish(runtime.SessionEvent{Kind: kind, Session: s.session, Ref: s.ref, Time: time.Now()})
}

// attachSessionEvents makes sc publish its events for name. It runs when
// Start commits the connection; if the agent already exited, the exit the
// process monitor could not attribute is published here.
func (p *Provider) attachSessionEvents(name string, sc *sessionConn) {
	src := &sessionEventSource{
		hub:     p.events,
		session: name,
		ref:     fmt.Sprintf("acp:%d", sc.cmd.Process.Pid),
	}
	sc.mu.Lock()
	sc.events = src
	exited := sc.exited
	sc.mu.Unlock()
	if exited {
		src.emit(runtime.SessionEventExited)
	}
}

// emitLocked publishes kind for an attached connection. Caller must hold
// sc.mu, which orders one session's events; the hub never takes sc.mu.
func (sc *sessionConn) emitLocked(kind runtime.SessionEventKind) {
	sc.events.emit(kind)
}

// markExited records that the agent process has exited and publishes it
// for an attached connection, then releases waiters on exitReported.
func (sc *sessionConn) markExited() {
	sc.mu.Lock()
	sc.exited = true
	sc.emitLocked(runtime.SessionEventExited)
	sc.mu.Unlock()
	close(sc.exitReported)
}

// emitClosed publishes closed once the exit has been reported, so a
// subscriber sees exited before closed.
func (sc *sessionConn) emitClosed() {
	<-sc.exitReported
	sc.mu.Lock()
	sc.emitLocked(runtime.SessionEventClosed)
	sc.mu.Unlock()
}
