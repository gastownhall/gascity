package main

import (
	"bytes"
	"context"
	"errors"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
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

// TestSessionEventPumpNonImplementingProviderLogsFallback covers the failed
// type-assertion path in restart: a provider that does not implement
// runtime.SessionEventProvider at all (the common case for any non-herdr
// provider) must announce the patrol-polling fallback exactly as the
// subscribe-error path does, instead of returning silently. Before this, a
// mixed city got zero benefit from the event-driven poke and zero signal
// that it hadn't.
func TestSessionEventPumpNonImplementingProviderLogsFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stderr bytes.Buffer
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pokeCh := make(chan struct{}, 1)
		pump := newSessionEventPump(ctx, pokeCh, &stderr, "test")
		pump.restart(runtime.NewFake()) // plain fake: no SessionEventProvider
		if pump.streaming() {
			t.Fatal("streaming() = true for a provider without an event stream")
		}
		if got := stderr.String(); !strings.Contains(got, "patrol polling") {
			t.Fatalf("restart onto a non-implementing provider logged %q, want a patrol-polling fallback line", got)
		}
	})
}

// TestSessionEventPumpDeliversThroughAutoProvider covers the composite-
// provider gap: resolveSessionTransportProvider wraps herdr (or any
// event-capable backend) in sessionauto.New whenever a city needs ACP
// routing for some agents, and before auto.Provider implemented
// runtime.SessionEventProvider itself, the pump's type assertion on the
// wrapper failed even though the wrapped backend supported events — silently
// losing the whole event-driven poke in exactly the configuration the
// feature was built for. This asserts an event delivered by the wrapped
// backend still reaches the pump through the auto composite.
func TestSessionEventPumpDeliversThroughAutoProvider(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, pokeCh, cancel := newTestPump(t)
		defer cancel()
		fp := &eventedFake{Fake: runtime.NewFake()}
		wrapped := sessionauto.New(fp, runtime.NewFake()) // fp is the default backend; acp backend has no events
		pump.restart(wrapped)
		if !pump.streaming() {
			t.Fatal("streaming() = false after subscribing through an auto.Provider wrapping an event-capable backend")
		}
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "crew-1", Time: time.Now()})
		waitPoke(t, pokeCh)
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

// A healthy herdr subscription may be silent indefinitely. Once its leading
// resync proves delivery, flowing remains true until the subscription closes.
func TestSessionEventPumpFlowingSurvivesIndefiniteSilence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pump, _, cancel := newTestPump(t)
		defer cancel()
		pump.resyncDelay = time.Hour // keep the trailing poke out of this test's way
		fp := &eventedFake{Fake: runtime.NewFake()}
		pump.restart(fp)
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventResync})
		synctest.Wait()
		if !pump.flowing() {
			t.Fatal("flowing() = false immediately after a delivered event")
		}
		<-time.After(24 * time.Hour)
		synctest.Wait()
		if !pump.flowing() {
			t.Fatal("flowing() = false for an established but quiet stream")
		}
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

func stretchTestRuntime(t *testing.T, stretch string, pump *sessionEventPump) *CityRuntime {
	t.Helper()
	return &CityRuntime{
		cfg: &config.City{
			Daemon: config.DaemonConfig{
				PatrolInterval:        "30s",
				SessionPatrolInterval: stretch,
			},
		},
		sessionEvents: pump,
	}
}

func streamingPump(t *testing.T) (*sessionEventPump, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pump := newSessionEventPump(ctx, make(chan struct{}, 1), &bytes.Buffer{}, "test")
	fp := &eventedFake{Fake: runtime.NewFake()}
	pump.restart(fp)
	if !pump.streaming() {
		t.Fatal("test pump failed to stream")
	}
	fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventResync})
	deadline := time.Now().Add(2 * time.Second)
	for !pump.flowing() && time.Now().Before(deadline) {
		goruntime.Gosched()
	}
	if !pump.flowing() {
		t.Fatal("test pump never observed event flow")
	}
	return pump, cancel
}

func TestSessionPhasesDueDoesNotStretchBeforeEventFlow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pump := newSessionEventPump(ctx, make(chan struct{}, 1), &bytes.Buffer{}, "test")
	pump.restart(&eventedFake{Fake: runtime.NewFake()})
	if !pump.streaming() {
		t.Fatal("precondition: subscription should be established")
	}
	if pump.flowing() {
		t.Fatal("non-emitting subscription must not count as event flow")
	}
	cr := stretchTestRuntime(t, "10m", pump)
	now := time.Now()
	cr.sessionPhasesLast = now
	if !cr.sessionPhasesDue("patrol", false, now.Add(time.Minute)) {
		t.Fatal("patrol was stretched before the event backend proved it could deliver")
	}
}

func TestSessionPhasesDueNonPatrolTriggersAlwaysRun(t *testing.T) {
	pump, cancel := streamingPump(t)
	defer cancel()
	cr := stretchTestRuntime(t, "10m", pump)
	now := time.Now()
	cr.sessionPhasesLast = now // just ran
	for _, trigger := range []string{"poke", "startup-poke"} {
		if !cr.sessionPhasesDue(trigger, false, now) {
			t.Errorf("sessionPhasesDue(%q) = false, want true", trigger)
		}
	}
}

func TestSessionPhasesDuePatrolWithoutStretchRuns(t *testing.T) {
	cr := stretchTestRuntime(t, "", nil)
	cr.sessionPhasesLast = time.Now()
	if !cr.sessionPhasesDue("patrol", false, time.Now()) {
		t.Error("sessionPhasesDue(patrol) = false with stretching unset, want true")
	}
}

func TestSessionPhasesDuePatrolStretchSkipsWithinWindow(t *testing.T) {
	pump, cancel := streamingPump(t)
	defer cancel()
	cr := stretchTestRuntime(t, "10m", pump)
	now := time.Now()
	if !cr.sessionPhasesDue("patrol", false, now) {
		t.Fatal("first patrol tick must run the session phases")
	}
	// sessionPhasesDue is pure (does not stamp sessionPhasesLast); the
	// caller (tick()) stamps it only once the phases actually complete.
	// Simulate that here so the stretch window has a baseline to measure
	// against, matching what a real completed tick would do.
	cr.sessionPhasesLast = now
	if cr.sessionPhasesDue("patrol", false, now.Add(time.Minute)) {
		t.Error("patrol tick inside the stretch window ran the session phases")
	}
	if !cr.sessionPhasesDue("patrol", false, now.Add(11*time.Minute)) {
		t.Error("patrol tick past the stretch window skipped the session phases")
	}
}

func TestSessionPhasesDuePatrolStretchIgnoredWithoutStream(t *testing.T) {
	cr := stretchTestRuntime(t, "10m", nil) // no pump wired
	cr.sessionPhasesLast = time.Now()
	if !cr.sessionPhasesDue("patrol", false, time.Now()) {
		t.Error("stretch honored without a session-event stream")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idle := newSessionEventPump(ctx, make(chan struct{}, 1), &bytes.Buffer{}, "test")
	idle.restart(runtime.NewFake()) // provider without a stream
	cr = stretchTestRuntime(t, "10m", idle)
	cr.sessionPhasesLast = time.Now()
	if !cr.sessionPhasesDue("patrol", false, time.Now()) {
		t.Error("stretch honored while the pump is not streaming")
	}
}

func TestSessionPhasesDueStretchNotLongerThanPatrolIgnored(t *testing.T) {
	pump, cancel := streamingPump(t)
	defer cancel()
	for _, stretch := range []string{"30s", "10s"} {
		cr := stretchTestRuntime(t, stretch, pump)
		cr.sessionPhasesLast = time.Now()
		if !cr.sessionPhasesDue("patrol", false, time.Now()) {
			t.Errorf("stretch %q (not longer than patrol) skipped the session phases", stretch)
		}
	}
}

func TestSessionPhasesDueConfigPendingRuns(t *testing.T) {
	pump, cancel := streamingPump(t)
	defer cancel()
	cr := stretchTestRuntime(t, "10m", pump)
	now := time.Now()
	cr.sessionPhasesLast = now
	if !cr.sessionPhasesDue("patrol", true, now) {
		t.Error("pending config change did not force the session phases")
	}
}
