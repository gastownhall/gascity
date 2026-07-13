package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

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

// waitStreaming polls pump.streaming() until it reports want or times out.
func waitStreaming(t *testing.T, pump *sessionEventPump, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pump.streaming() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("streaming() never became %v", want)
}

func TestSessionEventPumpNoStreamProviderStaysInactive(t *testing.T) {
	pump, pokeCh, cancel := newTestPump(t)
	defer cancel()
	pump.restart(runtime.NewFake()) // plain fake: no SessionEventProvider
	if pump.streaming() {
		t.Fatal("streaming() = true for a provider without an event stream")
	}
	assertNoPoke(t, pokeCh)
}

func TestSessionEventPumpLivenessEventsPoke(t *testing.T) {
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
}

func TestSessionEventPumpResyncPokesAfterTrailingDelay(t *testing.T) {
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
}

func TestSessionEventPumpResyncTrailingExtendsUntilCap(t *testing.T) {
	pump, pokeCh, cancel := newTestPump(t)
	defer cancel()
	pump.resyncDelay = 250 * time.Millisecond
	pump.resyncMaxDefer = 600 * time.Millisecond
	fp := &eventedFake{Fake: runtime.NewFake()}
	pump.restart(fp)
	// Resyncs spaced inside the delay keep extending it (a start wave
	// defers its own poke past its tail)...
	fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventResync})
	for i := 0; i < 3; i++ {
		time.Sleep(150 * time.Millisecond)
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventResync})
	}
	// ...but the cap stops the extension: past resyncMaxDefer the armed
	// timer runs out undisturbed even under continuous resyncs.
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case <-pokeCh:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("capped resync deferral never poked")
		}
		fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventResync})
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSessionEventPumpUnattributedDeathsDoNotPoke(t *testing.T) {
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
}

func TestSessionEventPumpIgnoresAgentEvents(t *testing.T) {
	pump, pokeCh, cancel := newTestPump(t)
	defer cancel()
	fp := &eventedFake{Fake: runtime.NewFake()}
	pump.restart(fp)
	fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventAgentStatus, Session: "crew-1", AgentStatus: "working"})
	fp.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventAgentDetected, Session: "crew-1"})
	assertNoPoke(t, pokeCh)
}

func TestSessionEventPumpBurstCoalesces(t *testing.T) {
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
	deadline := time.Now().Add(2 * time.Second)
	for {
		fp.mu.Lock()
		pending := len(fp.ch)
		fp.mu.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pump never drained the event burst")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the last in-flight forward land
	waitPoke(t, pokeCh)
	assertNoPoke(t, pokeCh)
}

func TestSessionEventPumpSubscribeErrorStaysInactive(t *testing.T) {
	pump, pokeCh, cancel := newTestPump(t)
	defer cancel()
	fp := &eventedFake{Fake: runtime.NewFake(), subscribeErr: errors.New("boom")}
	pump.restart(fp)
	if pump.streaming() {
		t.Fatal("streaming() = true after subscribe error")
	}
	assertNoPoke(t, pokeCh)
}

func TestSessionEventPumpRestartSwitchesProviders(t *testing.T) {
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
	// The old stream's close must not clear the new stream's liveness.
	time.Sleep(50 * time.Millisecond)
	if !pump.streaming() {
		t.Fatal("streaming() = false after restart onto a streaming provider")
	}
	b.emit(t, runtime.SessionEvent{Kind: runtime.SessionEventClosed, Session: "crew-2"})
	waitPoke(t, pokeCh)
}

func TestSessionEventPumpRestartToNonStreamingProviderDeactivates(t *testing.T) {
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
}

func TestSessionEventPumpStreamCloseDeactivates(t *testing.T) {
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
}

func TestSessionEventPumpParentCancelDeactivates(t *testing.T) {
	pump, _, cancel := newTestPump(t)
	fp := &eventedFake{Fake: runtime.NewFake()}
	pump.restart(fp)
	cancel()
	waitStreaming(t, pump, false)
}

// --- session-phase stretch gate ---

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
	pump.restart(&eventedFake{Fake: runtime.NewFake()})
	if !pump.streaming() {
		t.Fatal("test pump failed to stream")
	}
	return pump, cancel
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
