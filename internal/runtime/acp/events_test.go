package acp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

// sessionEventWait bounds every fact-based wait on a session-event stream.
// Events arrive in microseconds; the bound only turns a hang into a failure.
const sessionEventWait = 10 * time.Second

// subscribe opens a session-event stream that lives for the test.
func subscribe(t *testing.T, p *Provider) <-chan runtime.SessionEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, err := p.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	return ch
}

// nextEvent receives one event, failing on a closed stream or a hang.
func nextEvent(t *testing.T, ch <-chan runtime.SessionEvent) runtime.SessionEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("session-event stream closed, want an event")
		}
		return ev
	case <-time.After(sessionEventWait):
		t.Fatalf("no session event within %s", sessionEventWait)
	}
	return runtime.SessionEvent{}
}

// expectEvent receives one event and checks its kind and session.
func expectEvent(t *testing.T, ch <-chan runtime.SessionEvent, kind runtime.SessionEventKind, session string) runtime.SessionEvent {
	t.Helper()
	ev := nextEvent(t, ch)
	if ev.Kind != kind || ev.Session != session {
		t.Fatalf("event = %s(%q), want %s(%q)", ev.Kind, ev.Session, kind, session)
	}
	if ev.Time.IsZero() {
		t.Fatalf("event %s(%q) has a zero Time", ev.Kind, ev.Session)
	}
	return ev
}

// expectResync receives the stream-opening resync.
func expectResync(t *testing.T, ch <-chan runtime.SessionEvent) {
	t.Helper()
	expectEvent(t, ch, runtime.SessionEventResync, "")
}

// startFake starts an in-process ACP agent and registers its cleanup.
func startFake(t *testing.T, p *Provider, command string) string {
	t.Helper()
	name := testName()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: command,
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	return name
}

// agentRef is the Ref the provider stamps on events for name.
func agentRef(t *testing.T, p *Provider, name string) string {
	t.Helper()
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if !ok || sc.cmd == nil {
		t.Fatalf("no in-process connection for %q", name)
	}
	return fmt.Sprintf("acp:%d", sc.cmd.Process.Pid)
}

// fakeACPExitOnPromptCommand is fakeACPShellCommand whose agent exits with
// status 3 when it receives a prompt, without answering it.
func fakeACPExitOnPromptCommand(t *testing.T) string {
	t.Helper()
	base := fakeACPShellCommand()
	const answer = "respond(msg_id, {})\n'"
	if !strings.HasSuffix(base, answer) {
		t.Fatal("fakeACPShellCommand no longer ends with the prompt answer")
	}
	return strings.TrimSuffix(base, answer) + "sys.exit(3)\n'"
}

func TestSessionEventsStreamOpensWithResync(t *testing.T) {
	p := newTestProvider(t)
	ch := subscribe(t, p)
	expectResync(t, ch)
}

func TestSessionEventsBracketAPromptTurn(t *testing.T) {
	p := newTestProvider(t)
	name := startFake(t, p, fakeACPShellCommand())
	ch := subscribe(t, p)
	expectResync(t, ch)

	if err := p.Nudge(name, runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	ref := agentRef(t, p, name)
	started := expectEvent(t, ch, runtime.SessionEventAgentStateChanged, name)
	idle := expectEvent(t, ch, runtime.SessionEventAgentIdle, name)
	for _, ev := range []runtime.SessionEvent{started, idle} {
		if ev.Ref != ref {
			t.Fatalf("%s Ref = %q, want %q", ev.Kind, ev.Ref, ref)
		}
	}
	if idle.Time.Before(started.Time) {
		t.Fatalf("agent_idle at %s precedes agent_state_changed at %s", idle.Time, started.Time)
	}
}

func TestSessionEventsReportAgentExitWithoutIdle(t *testing.T) {
	p := newTestProvider(t)
	name := startFake(t, p, fakeACPExitOnPromptCommand(t))
	ch := subscribe(t, p)
	expectResync(t, ch)

	if err := p.Nudge(name, runtime.TextContent("die")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	expectEvent(t, ch, runtime.SessionEventAgentStateChanged, name)
	// A drained connection is gone, never idle: the failed turn does not
	// announce an idle agent before the exit.
	expectEvent(t, ch, runtime.SessionEventExited, name)
	if p.IsRunning(name) {
		t.Fatal("IsRunning = true after the exited event")
	}
}

func TestSessionEventsReportStopAsExitedThenClosed(t *testing.T) {
	p := newTestProvider(t)
	name := startFake(t, p, fakeACPShellCommand())
	ref := agentRef(t, p, name)
	ch := subscribe(t, p)
	expectResync(t, ch)

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if ev := expectEvent(t, ch, runtime.SessionEventExited, name); ev.Ref != ref {
		t.Fatalf("exited Ref = %q, want %q", ev.Ref, ref)
	}
	if ev := expectEvent(t, ch, runtime.SessionEventClosed, name); ev.Ref != ref {
		t.Fatalf("closed Ref = %q, want %q", ev.Ref, ref)
	}
}

func TestSessionEventsReachEverySubscriber(t *testing.T) {
	p := newTestProvider(t)
	name := startFake(t, p, fakeACPShellCommand())
	first := subscribe(t, p)
	second := subscribe(t, p)
	expectResync(t, first)
	expectResync(t, second)

	if err := p.Nudge(name, runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	for _, ch := range []<-chan runtime.SessionEvent{first, second} {
		expectEvent(t, ch, runtime.SessionEventAgentStateChanged, name)
		expectEvent(t, ch, runtime.SessionEventAgentIdle, name)
	}
}

func TestSessionEventsIgnoreConnectionsTheProviderDoesNotOwn(t *testing.T) {
	p := newTestProvider(t)
	ch := subscribe(t, p)
	expectResync(t, ch)

	// injectConn never went through Start, so no events are attributed to it.
	_, sc := injectConn(t, p)
	sc.setActivePrompt(7)
	respondTo(sc, 7, `{"stopReason":"end_turn"}`)

	p.events.publish(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: "marker", Time: time.Now()})
	expectEvent(t, ch, runtime.SessionEventAgentIdle, "marker")
}

func TestSessionEventsCancelClosesStream(t *testing.T) {
	p := newTestProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	expectResync(t, ch)
	cancel()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("received %s after cancel, want a closed stream", ev.Kind)
		}
	case <-time.After(sessionEventWait):
		t.Fatalf("stream still open %s after cancel", sessionEventWait)
	}
	// Publishing after the subscriber left must not panic on the closed
	// channel.
	p.events.publish(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: "late", Time: time.Now()})
}

func TestSessionEventsSlowSubscriberGetsResyncAfterDrops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSessionEventHub()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := h.subscribe(ctx)

		// The subscriber reads nothing while far more events than the
		// buffer holds are published.
		const published = 3 * sessionEventBuffer
		for i := range published {
			h.publish(runtime.SessionEvent{
				Kind:    runtime.SessionEventAgentStateChanged,
				Session: fmt.Sprintf("s-%d", i),
				Time:    time.Now(),
			})
		}

		expectResync(t, ch)
		// The buffer held the opening resync plus the first events in
		// order; the rest were dropped and coalesced into a resync that
		// arrives as soon as the reader makes room.
		for i := range sessionEventBuffer - 1 {
			expectEvent(t, ch, runtime.SessionEventAgentStateChanged, fmt.Sprintf("s-%d", i))
		}
		expectResync(t, ch)

		// Let the resync goroutine settle: a drop that raced the first
		// resync may have queued one more, which is allowed.
		synctest.Wait()
		h.publish(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: "after", Time: time.Now()})
		ev := nextEvent(t, ch)
		for ev.Kind == runtime.SessionEventResync {
			ev = nextEvent(t, ch)
		}
		if ev.Kind != runtime.SessionEventAgentIdle || ev.Session != "after" {
			t.Fatalf("event after the loss = %s(%q), want agent_idle(\"after\")", ev.Kind, ev.Session)
		}
	})
}

// TestSessionEventHubDeliversWhileResyncPending pins that a subscriber with
// room receives an event even while the resync for an earlier drop has not
// been sent yet: the hub drops only under backpressure.
func TestSessionEventHubDeliversWhileResyncPending(t *testing.T) {
	h := newSessionEventHub()
	// No serve goroutine: the pending wake stands for a loss whose resync
	// has not been queued yet.
	sub := &sessionEventSub{
		ch:   make(chan runtime.SessionEvent, sessionEventBuffer),
		wake: make(chan struct{}, 1),
	}
	sub.wake <- struct{}{}
	h.subs[sub] = struct{}{}

	h.publish(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: "roomy", Time: time.Now()})
	select {
	case ev := <-sub.ch:
		if ev.Kind != runtime.SessionEventAgentIdle || ev.Session != "roomy" {
			t.Fatalf("event = %s(%q), want agent_idle(\"roomy\")", ev.Kind, ev.Session)
		}
	default:
		t.Fatal("event dropped although the subscriber had room")
	}
}

func TestSessionEventsStalledSubscriberDoesNotBlockPrompts(t *testing.T) {
	p := newTestProvider(t)
	name := startFake(t, p, fakeACPShellCommand())
	_ = subscribe(t, p) // never read

	// Every turn publishes two events, so this overflows the buffer twice.
	for i := range sessionEventBuffer {
		if err := p.Nudge(name, runtime.TextContent(fmt.Sprintf("turn %d", i))); err != nil {
			t.Fatalf("Nudge %d: %v", i, err)
		}
		if err := p.WaitForIdle(context.Background(), name, sessionEventWait); err != nil {
			t.Fatalf("WaitForIdle after turn %d: %v", i, err)
		}
	}
	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	_, last := sc.turns()
	if last == nil || last.State != turnCompleted {
		t.Fatalf("last turn = %+v, want a completed turn", last)
	}
}

func TestSessionEventsSurviveProductionWrapping(t *testing.T) {
	seam := NewSeamBackedWithDir(shortTempDir(t), Config{})
	sep, ok := seam.(runtime.SessionEventProvider)
	if !ok {
		t.Fatal("NewSeamBackedWithDir does not implement runtime.SessionEventProvider")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := sep.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("seam SubscribeSessionEvents: %v", err)
	}
	expectResync(t, ch)

	router := auto.New(runtime.NewFake(), seam)
	routed, err := router.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("auto SubscribeSessionEvents: %v", err)
	}
	expectResync(t, routed)
}

// expectNoEvent fails if an event is already waiting on ch.
func expectNoEvent(t *testing.T, ch <-chan runtime.SessionEvent) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		t.Fatalf("unexpected event %s(%q) (open=%v), want none", ev.Kind, ev.Session, ok)
	default:
	}
}

// attachTestSource makes sc publish its events on h as session name.
func attachTestSource(sc *sessionConn, h *sessionEventHub, name string) {
	sc.mu.Lock()
	sc.events = &sessionEventSource{hub: h, session: name, ref: "acp:test"}
	sc.mu.Unlock()
}

// TestSessionEventsStopAfterAgentDeathReportsClosedOnce pins Stop's dead
// path, the normal production path after an agent dies on its own: the
// reconciler's Stop reports closed exactly once after the exit.
func TestSessionEventsStopAfterAgentDeathReportsClosedOnce(t *testing.T) {
	p := newTestProvider(t)
	name := startFake(t, p, fakeACPExitOnPromptCommand(t))
	ch := subscribe(t, p)
	expectResync(t, ch)

	if err := p.Nudge(name, runtime.TextContent("die")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	expectEvent(t, ch, runtime.SessionEventAgentStateChanged, name)
	expectEvent(t, ch, runtime.SessionEventExited, name)

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	expectEvent(t, ch, runtime.SessionEventClosed, name)
	if err := p.Stop(name); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	p.events.publish(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: "marker", Time: time.Now()})
	expectEvent(t, ch, runtime.SessionEventAgentIdle, "marker")
}

// TestSessionEventsClosedWaitsForExited pins that closed never precedes
// exited: emitClosed publishes nothing until the exit has been reported.
func TestSessionEventsClosedWaitsForExited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSessionEventHub()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := h.subscribe(ctx)
		expectResync(t, ch)

		sc := newSessionConn(nil, nil, nil, 10, nil)
		attachTestSource(sc, h, "s")
		closedDone := make(chan struct{})
		go func() {
			sc.emitClosed()
			close(closedDone)
		}()
		synctest.Wait()
		expectNoEvent(t, ch)

		sc.markExited()
		<-closedDone
		expectEvent(t, ch, runtime.SessionEventExited, "s")
		expectEvent(t, ch, runtime.SessionEventClosed, "s")
	})
}

// TestSessionEventsAttachAfterExitReportsExited pins that an agent that
// exited before Start attached its events still reports exited.
func TestSessionEventsAttachAfterExitReportsExited(t *testing.T) {
	p := newTestProvider(t)
	ch := subscribe(t, p)
	expectResync(t, ch)

	sc := newSessionConn(&exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}, nil, nil, 10, nil)
	sc.markExited()
	expectNoEvent(t, ch)

	p.attachSessionEvents("early", sc)
	ev := expectEvent(t, ch, runtime.SessionEventExited, "early")
	if want := fmt.Sprintf("acp:%d", os.Getpid()); ev.Ref != want {
		t.Fatalf("exited Ref = %q, want %q", ev.Ref, want)
	}
}

// TestSessionEventsAbandonedPromptReportsIdle pins that a prompt that never
// reached the agent still closes its turn with agent_idle: the connection
// is alive and can take the next turn.
func TestSessionEventsAbandonedPromptReportsIdle(t *testing.T) {
	p := newTestProvider(t)
	ch := subscribe(t, p)
	expectResync(t, ch)

	name, sc := injectConn(t, p)
	attachTestSource(sc, p.events, name)
	if err := p.Nudge(name, runtime.TextContent("hello")); err == nil {
		t.Fatal("Nudge succeeded over an erroring stdin, want an error")
	}
	expectEvent(t, ch, runtime.SessionEventAgentStateChanged, name)
	expectEvent(t, ch, runtime.SessionEventAgentIdle, name)
}
