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
// and the reconcile poke it earns, and sessionEventResyncPokeMaxDefer caps
// how long consecutive resyncs may keep extending that delay. Resyncs open
// every connection cycle — including the benign cycle the stream starts for
// every newly detected agent pane — so an immediate poke would land a
// reconcile in the middle of the very start wave that triggered it
// (live-verified: the poked tick can race the in-flight create's meta stamp
// and roll it back; a cold provider-server start holds that window open for
// ~10s+). Each further resync re-arms the timer, so a start wave — whose
// every start emits a resubscribe resync — defers the poke past its own
// tail; the cap guarantees a flapping stream still gets its poll-now within
// a bounded time. Deaths are unaffected: attributed exits poke immediately,
// and the patrol scan remains the hard backstop behind everything.
const (
	sessionEventResyncPokeDelay    = 15 * time.Second
	sessionEventResyncPokeMaxDefer = time.Minute
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
// runtime.SessionEventProvider deactivate the pump. Callers serialize
// restarts (startup and config reload both run on the reconciler goroutine).
func (p *sessionEventPump) restart(sp runtime.Provider) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.gen++
	p.streamGen.Store(0)
	sep, ok := sp.(runtime.SessionEventProvider)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(p.parent)
	events, err := sep.SubscribeSessionEvents(ctx)
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

// streaming reports whether a session-event stream is currently established.
func (p *sessionEventPump) streaming() bool {
	return p.streamGen.Load() != 0
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
			return
		case <-resyncTimer.C:
			resyncArmed = false
			p.poke("resync", "")
		case ev, ok := <-events:
			if !ok {
				if p.streamGen.CompareAndSwap(gen, 0) && ctx.Err() == nil {
					fmt.Fprintf(p.stderr, "%s: session-event stream ended; session liveness falls back to patrol polling\n", p.logPrefix) //nolint:errcheck // best-effort stderr
				}
				return
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
				// Trailing-edge with a cap: the first resync arms the timer,
				// later ones re-arm it (a start wave keeps deferring its own
				// poke past its tail) until resyncMaxDefer forces the fire
				// (see sessionEventResyncPokeDelay for why not immediate).
				switch {
				case !resyncArmed:
					resyncTimer.Reset(p.resyncDelay)
					resyncArmed = true
					resyncFirstArm = time.Now()
				case time.Since(resyncFirstArm) < p.resyncMaxDefer:
					if !resyncTimer.Stop() {
						select {
						case <-resyncTimer.C:
						default:
						}
					}
					resyncTimer.Reset(p.resyncDelay)
				}
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
