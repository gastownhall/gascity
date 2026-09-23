package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// sessionEventResyncPokeDelay is the trailing delay between a stream resync
// and the reconcile poke it earns, and sessionEventResyncPokeMaxDefer bounds
// how long consecutive resyncs may defer that poke, measured from the first
// resync of the burst rather than from the last. Resyncs open
// every connection cycle — including the benign cycle the stream starts for
// every newly detected agent pane — so an immediate poke would land a
// reconcile in the middle of the very start wave that triggered it
// (live-verified: the poked tick can race the in-flight create's meta stamp
// and roll it back; a cold provider-server start holds that window open for
// ~10s+). Each further resync re-arms the timer, so a start wave — whose
// every start emits a resubscribe resync — defers the poke past its own
// tail; the cap is the deadline for the poke itself, so a flapping stream
// gets its poll-now no later than one minute after the burst began. Deaths are unaffected: attributed exits poke immediately,
// and the patrol scan remains the hard backstop behind everything.
const (
	sessionEventResyncPokeDelay    = 15 * time.Second
	sessionEventResyncPokeMaxDefer = time.Minute

	// sessionEventFlowingStaleAfter bounds how long flowing() may report true
	// after the last observed event. herdr (the only current
	// SessionEventProvider) retries a broken transport forever, capped at
	// sessionEventMaxBackoff (5s, internal/runtime/herdr/events.go), without
	// ever closing the channel — so a transport outage is invisible to
	// streamGen/observedGen, which only clear on ctx.Done or a channel
	// close. Without a staleness bound, flowing() would keep reporting true
	// through the whole outage, and sessionPhaseStretchActive would keep
	// stretching patrol-driven session scans even though the event path
	// delivering the "real-time work" it stretches against has gone silent.
	// 30s is 6x the max reconnect backoff, giving margin for reconnect
	// latency and scheduling jitter before declaring the stream stale.
	sessionEventFlowingStaleAfter = 30 * time.Second
)

// sessionEventPump bridges a provider's push session-event stream
// (runtime.SessionEventProvider) into the reconciler's poke channel, so a
// session death becomes an immediate reconcile tick instead of waiting for
// the next patrol. Only attributed session deaths (exited, closed with a
// session name) and stream resyncs are forwarded — deaths immediately,
// resyncs on a trailing delay; agent-activity kinds have their own
// consumers, and unattributed pane noise is dropped (see forward).
// Events are level-triggered hints — the poked tick re-reads authoritative
// state (ListRunning et al.), so a replayed event costs at most one
// redundant reconcile. The poke channel's buffered-1 semantics plus the run
// loop's tick debouncer coalesce event bursts.
//
// Providers without an event stream (tmux) leave the pump inactive and every
// polled path untouched.
type sessionEventPump struct {
	parent         context.Context
	pokeCh         chan<- struct{}
	stderr         io.Writer
	logPrefix      string
	resyncDelay    time.Duration
	resyncMaxDefer time.Duration

	mu     sync.Mutex
	gen    int64              // subscription generation counter
	cancel context.CancelFunc // cancels the current subscription

	// streamGen holds the generation of the currently-established stream,
	// 0 when none. Forward goroutines clear only their own generation, so
	// a late close from a replaced subscription cannot mask a live one.
	streamGen atomic.Int64
	// observedGen is set only after the current stream delivers an event. A
	// successful Subscribe call may merely start a disconnected retry loop, so
	// it is not sufficient evidence for stretching patrol liveness checks.
	observedGen atomic.Int64
	// lastEventUnixNano is the receive time (UnixNano) of the most recent
	// event on the current stream. Used by flowing() to detect a transport
	// outage that neither closes the channel nor updates streamGen/
	// observedGen — see sessionEventFlowingStaleAfter.
	lastEventUnixNano atomic.Int64
	// composite holds this subscription's own staleness checker when sp
	// implements runtime.StaleAwareSessionEventProvider (a merged
	// multi-backend stream), nil otherwise. flowing() consults it so one
	// backend's silence is never masked by the other backend's continuing
	// traffic on the merged channel. Scoped to this specific subscription
	// (not shared provider-wide state) so a second, independent subscriber
	// of the same provider — e.g. the nudge-event dispatcher — can never
	// overwrite the pump's own staleness view.
	composite atomic.Pointer[compositeStaleHolder]
}

// compositeStaleHolder wraps this subscription's staleness checker so it
// can live behind an atomic.Pointer (a nil *compositeStaleHolder and "no
// composite reporter for this subscription" must be distinguishable from a
// present-but-nil func value).
type compositeStaleHolder struct {
	checker func() bool
}

// newSessionEventPump returns a pump whose subscriptions live within parent
// and poke pokeCh. Wire a provider with restart.
func newSessionEventPump(parent context.Context, pokeCh chan<- struct{}, stderr io.Writer, logPrefix string) *sessionEventPump {
	return &sessionEventPump{
		parent:         parent,
		pokeCh:         pokeCh,
		stderr:         stderr,
		logPrefix:      logPrefix,
		resyncDelay:    sessionEventResyncPokeDelay,
		resyncMaxDefer: sessionEventResyncPokeMaxDefer,
	}
}

// restart re-points the pump at sp's session-event stream, canceling any
// prior subscription. Providers that do not implement
// runtime.SessionEventProvider deactivate the pump and log the same
// patrol-polling fallback line a subscribe error would, so the degradation
// is never silent. Callers serialize restarts (startup and config reload
// both run on the reconciler goroutine).
func (p *sessionEventPump) restart(sp runtime.Provider) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.gen++
	p.streamGen.Store(0)
	p.observedGen.Store(0)
	p.lastEventUnixNano.Store(0)
	p.composite.Store(nil)
	ctx, cancel := context.WithCancel(p.parent)
	var events <-chan runtime.SessionEvent
	var err error
	if sasep, ok := sp.(runtime.StaleAwareSessionEventProvider); ok {
		var stale func() bool
		events, stale, err = sasep.SubscribeSessionEventsStale(ctx)
		if err == nil && stale != nil {
			p.composite.Store(&compositeStaleHolder{checker: stale})
		}
	} else if sep, ok := sp.(runtime.SessionEventProvider); ok {
		events, err = sep.SubscribeSessionEvents(ctx)
	} else {
		cancel()
		fmt.Fprintf(p.stderr, "%s: provider does not support session events (session liveness stays on patrol polling)\n", p.logPrefix) //nolint:errcheck // best-effort stderr
		return
	}
	if err != nil {
		cancel()
		fmt.Fprintf(p.stderr, "%s: session-event subscribe: %v (session liveness stays on patrol polling)\n", p.logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	p.cancel = cancel
	p.streamGen.Store(p.gen)
	fmt.Fprintf(p.stderr, "%s: session-event stream active: session death pokes the reconciler\n", p.logPrefix) //nolint:errcheck // best-effort stderr
	go p.forward(ctx, p.gen, events)
}

// streaming reports whether a session-event stream is currently established,
// meaning the subscription call succeeded and its forward goroutine is still
// running. It says nothing about whether the stream has ever DELIVERED: a
// provider is free to return a channel and connect behind it (herdr does,
// retrying forever with capped backoff), so a subscription to an unreachable
// provider is indistinguishable here from a healthy one. Do not use this to
// decide to poll less; it is only a report of whether a subscription exists.
func (p *sessionEventPump) streaming() bool {
	return p.streamGen.Load() != 0
}

// flowing reports whether the current subscription has actually delivered at
// least one event (normally its contract-required leading resync) recently.
// A stream stuck retrying a broken transport (herdr never closes the
// channel on a transient failure) stops updating lastEventUnixNano, so
// flowing() goes false once sessionEventFlowingStaleAfter has elapsed since
// the last delivered event — even though streamGen/observedGen still report
// the subscription as established and previously-observed.
func (p *sessionEventPump) flowing() bool {
	gen := p.streamGen.Load()
	if gen == 0 || p.observedGen.Load() != gen {
		return false
	}
	last := p.lastEventUnixNano.Load()
	if last == 0 {
		return false
	}
	if time.Since(time.Unix(0, last)) >= sessionEventFlowingStaleAfter {
		return false
	}
	// A merged multi-backend stream can keep delivering off one healthy
	// backend while the other has gone silent; lastEventUnixNano alone would
	// report that as flowing. Ask the composite reporter (if any) whether
	// either fanned-in backend is individually stale.
	if h := p.composite.Load(); h != nil && h.checker != nil && h.checker() {
		return false
	}
	return true
}

// forward pumps liveness events into the poke channel until the stream ends.
func (p *sessionEventPump) forward(ctx context.Context, gen int64, events <-chan runtime.SessionEvent) {
	resyncTimer := time.NewTimer(p.resyncDelay)
	if !resyncTimer.Stop() {
		<-resyncTimer.C
	}
	defer resyncTimer.Stop()
	resyncArmed := false
	var resyncFirstArm time.Time
	for {
		select {
		case <-ctx.Done():
			p.streamGen.CompareAndSwap(gen, 0)
			p.observedGen.CompareAndSwap(gen, 0)
			return
		case <-resyncTimer.C:
			resyncArmed = false
			p.poke("resync", "")
		case ev, ok := <-events:
			if !ok {
				if p.streamGen.CompareAndSwap(gen, 0) && ctx.Err() == nil {
					fmt.Fprintf(p.stderr, "%s: session-event stream ended; session liveness falls back to patrol polling\n", p.logPrefix) //nolint:errcheck // best-effort stderr
				}
				p.observedGen.CompareAndSwap(gen, 0)
				return
			}
			if p.streamGen.Load() == gen {
				p.observedGen.Store(gen)
				p.lastEventUnixNano.Store(time.Now().UnixNano())
			}
			switch ev.Kind {
			case runtime.SessionEventExited, runtime.SessionEventClosed:
				// Only attributed deaths poke. Unattributed pane events are
				// provider noise — most prominently the stray shell pane the
				// provider closes inside every agent start, which would poke
				// a reconcile into the middle of the very start wave that
				// caused it (live-verified: the poked tick can race the
				// in-flight create's meta stamp and roll it back). A death
				// the provider cannot attribute is covered by the next
				// resync or patrol scan.
				if ev.Session == "" {
					continue
				}
				p.poke(string(ev.Kind), ev.Session)
			case runtime.SessionEventResync:
				// Trailing-edge with a cap: the first resync arms the timer and
				// later ones re-arm it, so a start wave keeps deferring its own
				// poke past its own tail (see sessionEventResyncPokeDelay for
				// why the poke is not immediate). resyncMaxDefer is the
				// deadline for the POKE, measured from the first arm, so the
				// last re-arm is clamped to whatever is left of that window.
				// Re-arming a full delay instead would make the cap only the
				// point after which one more whole delay is allowed to run, and
				// the poke would land at resyncMaxDefer+resyncDelay.
				if !resyncArmed {
					resyncTimer.Reset(p.resyncDelay)
					resyncArmed = true
					resyncFirstArm = time.Now()
					break
				}
				delay := p.resyncDelay
				if remaining := p.resyncMaxDefer - time.Since(resyncFirstArm); remaining < delay {
					delay = max(remaining, 0)
				}
				if !resyncTimer.Stop() {
					select {
					case <-resyncTimer.C:
					default:
					}
				}
				resyncTimer.Reset(delay)
			}
		}
	}
}

// poke signals the reconciler without ever blocking; a full channel means a
// tick is already owed, which covers this event too.
func (p *sessionEventPump) poke(kind, session string) {
	select {
	case p.pokeCh <- struct{}{}:
		// Log only when the send lands: a replayed backlog burst fills the
		// buffer once and stays quiet.
		fmt.Fprintf(p.stderr, "%s: session event %s(%s) → reconcile poke\n", p.logPrefix, kind, session) //nolint:errcheck // best-effort stderr
	default:
	}
}
