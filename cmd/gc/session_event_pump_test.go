package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Every test in this file runs inside a testing/synctest bubble, so its timers
// are the bubble's virtual clock rather than wall time. Two consequences worth
// knowing before editing: a deadline like time.After(100ms) is exact instead of
// approximate, which makes the "no poke yet" assertions deterministic; and a
// receive on a timer is how the test ADVANCES that clock, costing no real time.
// Nothing here waits on elapsed wall time, and nothing should be rewritten to.

// eventedFake wraps the fake provider with a controllable session-event
// stream, mimicking the SessionEventProvider contract: the subscription
// channel closes when its ctx is canceled.
type eventedFake struct {
	*runtime.Fake
	subscribeErr error

	mu        sync.Mutex
	ch        chan runtime.SessionEvent
	closeOnce *sync.Once
	subCtx    context.Context
}

func (p *eventedFake) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	if p.subscribeErr != nil {
		return nil, p.subscribeErr
	}
	ch := make(chan runtime.SessionEvent, 16)
	once := &sync.Once{}
	p.mu.Lock()
	p.ch = ch
	p.closeOnce = once
	p.subCtx = ctx
	p.mu.Unlock()
	go func() {
		<-ctx.Done()
		once.Do(func() { close(ch) })
	}()
	return ch, nil
}

// emit sends ev on the current subscription. Fails the test if the send
// does not complete promptly (subscription buffer full or missing).
func (p *eventedFake) emit(t *testing.T, ev runtime.SessionEvent) {
	t.Helper()
	p.mu.Lock()
	ch := p.ch
	p.mu.Unlock()
	if ch == nil {
		t.Fatal("emit: no active subscription")
	}
	select {
	case ch <- ev:
	case <-time.After(2 * time.Second):
		t.Fatal("emit: subscription buffer full")
	}
}

func (p *eventedFake) subscriptionCtx() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.subCtx
}

func newTestPump(t *testing.T) (*sessionEventPump, chan struct{}, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pokeCh := make(chan struct{}, 1)
	pump := newSessionEventPump(ctx, pokeCh, &bytes.Buffer{}, "test")
	return pump, pokeCh, cancel
}

func waitPoke(t *testing.T, pokeCh chan struct{}) {
	t.Helper()
	select {
	case <-pokeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a reconcile poke, got none")
	}
}

func assertNoPoke(t *testing.T, pokeCh chan struct{}) {
	t.Helper()
	select {
	case <-pokeCh:
		t.Fatal("unexpected reconcile poke")
	case <-time.After(100 * time.Millisecond):
	}
}

// waitStreaming settles the bubble and then asserts the stream state. Inside a
// synctest bubble "every other goroutine is blocked" is an observable fact, so
// this needs no polling: if the forward goroutine were going to flip the flag,
// it already has by the time Wait returns.
func waitStreaming(t *testing.T, pump *sessionEventPump, want bool) {
	t.Helper()
	synctest.Wait()
	if pump.streaming() != want {
		t.Fatalf("streaming() = %v, want %v", pump.streaming(), want)
	}
}

func TestSessionEventPumpNoStreamProviderStaysInactive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		pump.restart(runtime.NewFake()) // plain fake: no SessionEventProvider
		if pump.streaming() {
			t.Fatal("streaming() = true for a provider without an event stream")
		}
		assertNoPoke(t, pokeCh)
	})
}

func TestSessionEventPumpLivenessEventsPoke(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		fp := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(fp)
		if !pump.streaming() {
			t.Fatal("streaming() = false after subscribing")
		}
		for _, kind := range []runtime.SessionEventKind{
			runtime.SessionEventExited,
			runtime.SessionEventClosed,
		} {
			fp.emit(t, runtime.SessionEvent{Kind: kind, Session: "crew-1", Time: time.Now()})
			waitPoke(t, pokeCh)
		}
	})
}

func TestSessionEventPumpResyncPokesAfterTrailingDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		pump.resyncDelay = 300 * time.Millisecond
		fp := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(fp)
		// A resync burst (initial attach + per-new-agent resubscribe cycles)
		// must collapse into one delayed poke, not poke per cycle — an
		// immediate poke would land a reconcile inside the start wave that
		// triggered the resubscribe.
		for i := 0; i < 5; i++ {
			fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventResync, Time: time.Now()})
		}
		assertNoPoke(t, pokeCh) // 100ms window: still inside the trailing delay
		waitPoke(t, pokeCh)
		assertNoPoke(t, pokeCh) // burst coalesced: exactly one poke

		// A later resync (e.g. server bounce reconnect) earns its own poke.
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventResync, Time: time.Now()})
		waitPoke(t, pokeCh)
	})
}

func TestSessionEventPumpResyncDeferralLandsExactlyAtTheCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		pump.resyncDelay = 250 * time.Millisecond
		pump.resyncMaxDefer = 600 * time.Millisecond
		fp := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(fp)
		// Resyncs spaced inside the delay keep extending it (a start wave defers
		// its own poke past its tail), and resyncMaxDefer is the deadline for the
		// POKE, not merely the point after which one more whole delay may run. So
		// the landing time is asserted exactly, and every emit is followed by
		// synctest.Wait so the pump has consumed it before the clock moves: a
		// bracketed assertion here stays green with the cap deleted outright,
		// because the pump can be descheduled long enough for an unclamped timer
		// to expire between two resyncs.
		armed := time.Now()
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventResync})
		synctest.Wait()
		const step = 100 * time.Millisecond
		const steps = 20
		for i := 0; i < steps; i++ {
			select {
			case <-pokeCh:
				if elapsed := time.Since(armed); elapsed != pump.resyncMaxDefer {
					t.Fatalf("poke landed %v after the first resync, want exactly the %v cap", elapsed, pump.resyncMaxDefer)
				}
				return
			default:
			}
			<-time.After(step)
			fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventResync})
			synctest.Wait()
		}
		t.Fatalf("continuous resyncs deferred the poke past %v of bubble time: the cap never fired", steps*step)
	})
}

func TestSessionEventPumpUnattributedDeathsDoNotPoke(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		fp := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(fp)
		// Stray-pane lifecycle noise (e.g. the shell pane the provider closes
		// during every agent start) must not poke a reconcile into the start
		// wave that produced it; only attributed deaths and resyncs poke.
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventExited, Ref: "%42", Time: time.Now()})
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventClosed, Ref: "w4:p1", Time: time.Now()})
		assertNoPoke(t, pokeCh)
	})
}

func TestSessionEventPumpIgnoresAgentEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		fp := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(fp)
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventAgentStateChanged, Session: "crew-1"})
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventAgentDetected, Session: "crew-1"})
		assertNoPoke(t, pokeCh)
	})
}

func TestSessionEventPumpBurstCoalesces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		fp := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(fp)
		// A replayed backlog burst must collapse into the poke channel's
		// buffered-1 semantics, not queue one tick per event. Nothing drains
		// pokeCh during the burst (the reconciler is "busy"), so after the pump
		// digests the whole burst exactly one poke may be buffered.
		for i := 0; i < 100; i++ {
			fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "crew-1"})
		}
		// Wait settles the bubble, which covers both halves of the drain: the
		// forward goroutine has consumed the burst and has finished acting on
		// the last event, because it is blocked again.
		synctest.Wait()
		fp.mu.Lock()
		pending := len(fp.ch)
		fp.mu.Unlock()
		if pending != 0 {
			t.Fatalf("pump left %d events undrained", pending)
		}
		waitPoke(t, pokeCh)
		assertNoPoke(t, pokeCh)
	})
}

func TestSessionEventPumpSubscribeErrorStaysInactive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		fp := &eventedFake{Fake: runtime.NewFake(), subscribeErr: errors.New("boom")}
		pump.restart(fp)
		if pump.streaming() {
			t.Fatal("streaming() = true after subscribe error")
		}
		assertNoPoke(t, pokeCh)
	})
}

func TestSessionEventPumpRestartSwitchesProviders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		a := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(a)
		aCtx := a.subscriptionCtx()

		b := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(b)
		select {
		case <-aCtx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("restart did not cancel the previous subscription")
		}
		// The old stream's close must not clear the new stream's liveness, so
		// settle the bubble first: by the time Wait returns, the replaced
		// subscription's forward goroutine has observed its closed channel and
		// done whatever it was going to do to the flag.
		synctest.Wait()
		if !pump.streaming() {
			t.Fatal("streaming() = false after restart onto a streaming provider")
		}
		b.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventClosed, Session: "crew-2"})
		waitPoke(t, pokeCh)
	})
}

func TestSessionEventPumpRestartToNonStreamingProviderDeactivates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, _, cancel := newTestPump(t)
		defer cancel()
		a := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(a)
		aCtx := a.subscriptionCtx()
		pump.restart(runtime.NewFake())
		if pump.streaming() {
			t.Fatal("streaming() = true after restart onto a non-streaming provider")
		}
		select {
		case <-aCtx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("restart did not cancel the previous subscription")
		}
	})
}

func TestSessionEventPumpStreamCloseDeactivates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, _, cancel := newTestPump(t)
		defer cancel()
		fp := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(fp)
		fp.mu.Lock()
		ch, once := fp.ch, fp.closeOnce
		fp.ch = nil
		fp.mu.Unlock()
		once.Do(func() { close(ch) }) // provider ends the stream outside ctx cancellation
		waitStreaming(t, pump, false)
	})
}

func TestSessionEventPumpParentCancelDeactivates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, _, cancel := newTestPump(t)
		fp := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(fp)
		cancel()
		waitStreaming(t, pump, false)
	})
}
