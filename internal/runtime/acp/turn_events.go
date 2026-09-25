package acp

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file implements [runtime.TurnEventProvider] from the turn records a
// sessionConn keeps: a started event when a turn opens (the session/prompt
// request is about to be written) and a completed event when it ends. The
// hub never blocks the JSON-RPC read loop or Nudge: an event a subscriber
// has no room for is dropped and counted, and the count is logged.

// turnEventBuffer sizes each subscriber channel.
const turnEventBuffer = 256

// sessionIDEnvKey is the environment variable that carries the gc session
// ID into a runtime.
const sessionIDEnvKey = "GC_SESSION_ID"

// acpStopReasonCanceled is the ACP stop reason for a canceled prompt.
const acpStopReasonCanceled = "cancelled" //nolint:misspell // ACP wire value

var _ runtime.TurnEventProvider = (*Provider)(nil)

// SubscribeTurnEvents implements [runtime.TurnEventProvider] for sessions
// this Provider started. Sessions owned by another process are not
// reported. Canceling ctx closes the channel.
func (p *Provider) SubscribeTurnEvents(ctx context.Context) (<-chan runtime.TurnEvent, error) {
	if ctx == nil {
		return nil, fmt.Errorf("acp: SubscribeTurnEvents requires a context")
	}
	return p.turnEvents.subscribe(ctx), nil
}

// turnEventHub fans turn events out to subscribers without blocking the
// publisher. The zero value is ready to use.
type turnEventHub struct {
	mu sync.Mutex
	// seq numbers started turns, so a subscriber skips the end of a turn
	// that started before it subscribed.
	seq  uint64
	subs map[*turnEventSub]struct{}
}

// turnEventSub is one subscriber. ch is sent to only under hub.mu and
// closed under hub.mu after removal, so no send can race the close.
type turnEventSub struct {
	ch chan runtime.TurnEvent
	// from is the hub seq at subscribe time; turns numbered above it are
	// reported.
	from uint64
	// lost counts events dropped since the last delivery or log line.
	lost int64
}

// subscribe registers a subscriber that sees turns started from now on.
func (h *turnEventHub) subscribe(ctx context.Context) <-chan runtime.TurnEvent {
	sub := &turnEventSub{ch: make(chan runtime.TurnEvent, turnEventBuffer)}
	h.mu.Lock()
	sub.from = h.seq
	if h.subs == nil {
		h.subs = make(map[*turnEventSub]struct{})
	}
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.subs, sub)
		close(sub.ch)
		lost := sub.lost
		h.mu.Unlock()
		logLostTurnEvents(lost)
	}()
	return sub.ch
}

// publishStarted numbers a new turn, offers ev to every subscriber, and
// returns the turn's number.
func (h *turnEventHub) publishStarted(ev runtime.TurnEvent) uint64 {
	h.mu.Lock()
	h.seq++
	seq := h.seq
	lost := h.offerLocked(ev, seq)
	h.mu.Unlock()
	logLostTurnEvents(lost)
	return seq
}

// publishCompleted offers ev, the end of turn seq, to every subscriber that
// saw the turn start.
func (h *turnEventHub) publishCompleted(ev runtime.TurnEvent, seq uint64) {
	h.mu.Lock()
	lost := h.offerLocked(ev, seq)
	h.mu.Unlock()
	logLostTurnEvents(lost)
}

// offerLocked delivers ev for turn seq without blocking. A full subscriber
// drops ev and counts the loss; the first delivery after a loss returns the
// count so the caller can log it outside the lock. Caller holds h.mu.
func (h *turnEventHub) offerLocked(ev runtime.TurnEvent, seq uint64) int64 {
	var lost int64
	for sub := range h.subs {
		if seq <= sub.from {
			continue
		}
		select {
		case sub.ch <- ev:
			lost += sub.lost
			sub.lost = 0
		default:
			sub.lost++
		}
	}
	return lost
}

// dropped reports the events dropped and not yet logged across subscribers.
func (h *turnEventHub) dropped() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int64
	for sub := range h.subs {
		n += sub.lost
	}
	return n
}

// logLostTurnEvents writes one stderr line for n dropped events.
func logLostTurnEvents(n int64) {
	if n == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "acp: turn-event subscriber fell behind; dropped %d turn events (the session transcript is the durable record)\n", n)
}

// turnEventSource attributes a connection's turns to the session a Provider
// owns.
type turnEventSource struct {
	hub       *turnEventHub
	session   string
	sessionID string
}

// started publishes the start of rec and remembers its number on rec.
func (s *turnEventSource) started(rec *turnRecord) {
	rec.eventSeq = s.hub.publishStarted(runtime.TurnEvent{
		Kind:      runtime.TurnEventStarted,
		Session:   s.session,
		SessionID: s.sessionID,
		TurnID:    rec.ID,
		Time:      rec.StartedAt,
	})
}

// completed publishes how rec ended.
func (s *turnEventSource) completed(rec *turnRecord, outcome turnOutcome, now time.Time) {
	ev := runtime.TurnEvent{
		Kind:      runtime.TurnEventCompleted,
		Session:   s.session,
		SessionID: s.sessionID,
		TurnID:    rec.ID,
		Time:      now,
	}
	switch {
	case outcome.state == turnFailed:
		ev.Status = runtime.TurnStatusFailed
		ev.Error = outcome.err
	case outcome.stopReason == acpStopReasonCanceled:
		ev.Status = runtime.TurnStatusCancelled
		ev.StopReason = outcome.stopReason
	default:
		ev.Status = runtime.TurnStatusCompleted
		ev.StopReason = outcome.stopReason
	}
	if u := outcome.usage; u != nil {
		ev.Usage = &runtime.TurnUsage{
			InputTokens:       u.InputTokens,
			OutputTokens:      u.OutputTokens,
			TotalTokens:       u.TotalTokens,
			ThoughtTokens:     u.ThoughtTokens,
			CachedReadTokens:  u.CachedReadTokens,
			CachedWriteTokens: u.CachedWriteTokens,
		}
	}
	s.hub.publishCompleted(ev, rec.eventSeq)
}

// attachTurnEvents makes sc publish its turns for name, tagged with the gc
// session ID from the runtime env. It runs when Start commits the
// connection, before any prompt can open a turn.
func (p *Provider) attachTurnEvents(name string, env map[string]string, sc *sessionConn) {
	src := &turnEventSource{hub: &p.turnEvents, session: name, sessionID: env[sessionIDEnvKey]}
	sc.mu.Lock()
	sc.turnEvents = src
	sc.mu.Unlock()
}
