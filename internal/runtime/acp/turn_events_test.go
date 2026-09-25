package acp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// subscribeTurns opens a turn-event stream that lives for the test.
func subscribeTurns(t *testing.T, p runtime.TurnEventProvider) <-chan runtime.TurnEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, err := p.SubscribeTurnEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeTurnEvents: %v", err)
	}
	return ch
}

// nextTurnEvent receives one turn event, failing on a closed stream or a hang.
func nextTurnEvent(t *testing.T, ch <-chan runtime.TurnEvent) runtime.TurnEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("turn-event stream closed, want an event")
		}
		return ev
	case <-time.After(sessionEventWait):
		t.Fatalf("no turn event within %s", sessionEventWait)
	}
	return runtime.TurnEvent{}
}

// expectTurnPair receives a started and a completed event for one turn in
// session and returns the completed event.
func expectTurnPair(t *testing.T, ch <-chan runtime.TurnEvent, session, sessionID string) runtime.TurnEvent {
	t.Helper()
	started := nextTurnEvent(t, ch)
	if started.Kind != runtime.TurnEventStarted || started.Session != session || started.SessionID != sessionID {
		t.Fatalf("first event = %+v, want started for %q/%q", started, session, sessionID)
	}
	if !uuidV4Pattern.MatchString(started.TurnID) || started.Time.IsZero() {
		t.Fatalf("started = %+v, want a UUIDv4 TurnID and a Time", started)
	}
	if started.Status != "" || started.Usage != nil || started.Error != "" || started.StopReason != "" {
		t.Fatalf("started = %+v, want no outcome fields", started)
	}
	completed := nextTurnEvent(t, ch)
	if completed.Kind != runtime.TurnEventCompleted || completed.Session != session || completed.SessionID != sessionID {
		t.Fatalf("second event = %+v, want completed for %q/%q", completed, session, sessionID)
	}
	if completed.TurnID != started.TurnID {
		t.Fatalf("completed TurnID = %q, want the started TurnID %q", completed.TurnID, started.TurnID)
	}
	if completed.Time.Before(started.Time) {
		t.Fatalf("completed at %s precedes started at %s", completed.Time, started.Time)
	}
	return completed
}

// expectNoTurnEvent fails if a turn event is already waiting on ch.
func expectNoTurnEvent(t *testing.T, ch <-chan runtime.TurnEvent) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected turn event %+v", ev)
		}
		t.Fatal("turn-event stream closed unexpectedly")
	default:
	}
}

// attachTestTurns makes sc publish turn events for name through p's hub.
func attachTestTurns(p *Provider, sc *sessionConn, name, sessionID string) {
	sc.mu.Lock()
	sc.turnEvents = &turnEventSource{hub: &p.turnEvents, session: name, sessionID: sessionID}
	sc.mu.Unlock()
}

func TestTurnEventsPairAPromptWithUsageAndSessionID(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	command := fakeACPResultCommand(t, `{"stopReason": "end_turn", "usage": {"inputTokens": 3, "outputTokens": 4, "totalTokens": 9, "thoughtTokens": 2, "cachedReadTokens": 1, "cachedWriteTokens": 5}}`)
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: command,
		WorkDir: t.TempDir(),
		Env:     map[string]string{"GC_SESSION_ID": "gc-sess-1"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	ch := subscribeTurns(t, p)

	if err := p.Nudge(name, runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	completed := expectTurnPair(t, ch, name, "gc-sess-1")
	if completed.Status != runtime.TurnStatusCompleted || completed.StopReason != "end_turn" || completed.Error != "" {
		t.Fatalf("completed = %+v, want completed/end_turn", completed)
	}
	want := runtime.TurnUsage{InputTokens: 3, OutputTokens: 4, TotalTokens: 9, ThoughtTokens: 2, CachedReadTokens: 1, CachedWriteTokens: 5}
	if completed.Usage == nil || *completed.Usage != want {
		t.Fatalf("usage = %+v, want %+v", completed.Usage, want)
	}
}

func TestTurnEventsWithoutSessionIDOrUsage(t *testing.T) {
	p := newTestProvider(t)
	name := startFake(t, p, fakeACPShellCommand())
	ch := subscribeTurns(t, p)

	if err := p.Nudge(name, runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	completed := expectTurnPair(t, ch, name, "")
	if completed.Status != runtime.TurnStatusCompleted || completed.Usage != nil {
		t.Fatalf("completed = %+v, want completed without usage", completed)
	}
}

func TestTurnEventOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		settle     func(sc *sessionConn, id int64)
		status     runtime.TurnStatus
		stopReason string
		err        string
	}{
		{
			name:       "end_turn",
			settle:     func(sc *sessionConn, id int64) { respondTo(sc, id, `{"stopReason":"end_turn"}`) },
			status:     runtime.TurnStatusCompleted,
			stopReason: "end_turn",
		},
		{
			name:       "other stop reason",
			settle:     func(sc *sessionConn, id int64) { respondTo(sc, id, `{"stopReason":"max_tokens"}`) },
			status:     runtime.TurnStatusCompleted,
			stopReason: "max_tokens",
		},
		{
			name:       "cancel",
			settle:     func(sc *sessionConn, id int64) { respondTo(sc, id, `{"stopReason":"`+acpStopReasonCancelled+`"}`) },
			status:     runtime.TurnStatusCancelled,
			stopReason: acpStopReasonCancelled,
		},
		{
			name: "json-rpc error",
			settle: func(sc *sessionConn, id int64) {
				sc.dispatch(JSONRPCMessage{JSONRPC: "2.0", ID: &id, Error: &JSONRPCError{Code: -32603, Message: "boom"}})
			},
			status: runtime.TurnStatusFailed,
			err:    "boom",
		},
		{
			name:   "agent exit mid-turn",
			settle: func(sc *sessionConn, _ int64) { sc.drainPending(nil) },
			status: runtime.TurnStatusFailed,
			err:    turnFailureConnClosed,
		},
		{
			name:   "prompt never sent",
			settle: func(sc *sessionConn, id int64) { sc.abandonPrompt(id, errors.New("broken pipe")) },
			status: runtime.TurnStatusFailed,
			err:    "sending prompt: broken pipe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProvider(t)
			sc := newSessionConn(nil, nil, nil, 10, nil)
			attachTestTurns(p, sc, "s", "sid")
			ch := subscribeTurns(t, p)

			sc.setActivePrompt(41)
			tc.settle(sc, 41)
			completed := expectTurnPair(t, ch, "s", "sid")
			if completed.Status != tc.status || completed.StopReason != tc.stopReason || completed.Error != tc.err {
				t.Fatalf("completed = %+v, want status %q stop %q error %q", completed, tc.status, tc.stopReason, tc.err)
			}
			_, last := sc.turns()
			if last == nil || last.ID != completed.TurnID {
				t.Fatalf("last turn = %+v, want the record behind TurnID %q", last, completed.TurnID)
			}
			// A turn ends once: a later drain publishes nothing more.
			sc.drainPending(nil)
			expectNoTurnEvent(t, ch)
		})
	}
}

func TestTurnEventsSkipTurnsStartedBeforeSubscribing(t *testing.T) {
	p := newTestProvider(t)
	sc := newSessionConn(nil, nil, nil, 10, nil)
	attachTestTurns(p, sc, "s", "")

	sc.setActivePrompt(1)
	ch := subscribeTurns(t, p)
	respondTo(sc, 1, `{"stopReason":"end_turn"}`)
	expectNoTurnEvent(t, ch)

	// The next turn is reported in full.
	sc.setActivePrompt(2)
	respondTo(sc, 2, `{"stopReason":"end_turn"}`)
	expectTurnPair(t, ch, "s", "")
}

func TestTurnEventsIgnoreConnectionsTheProviderDoesNotOwn(t *testing.T) {
	p := newTestProvider(t)
	ch := subscribeTurns(t, p)
	sc := newSessionConn(nil, nil, nil, 10, nil)
	sc.setActivePrompt(1)
	respondTo(sc, 1, `{"stopReason":"end_turn"}`)
	expectNoTurnEvent(t, ch)
}

func TestTurnEventsReachEverySubscriber(t *testing.T) {
	p := newTestProvider(t)
	sc := newSessionConn(nil, nil, nil, 10, nil)
	attachTestTurns(p, sc, "s", "")
	first := subscribeTurns(t, p)
	second := subscribeTurns(t, p)

	sc.setActivePrompt(1)
	respondTo(sc, 1, `{"stopReason":"end_turn"}`)
	a := expectTurnPair(t, first, "s", "")
	b := expectTurnPair(t, second, "s", "")
	if a.TurnID != b.TurnID {
		t.Fatalf("subscribers saw different turns: %q vs %q", a.TurnID, b.TurnID)
	}
}

func TestTurnEventsCancelClosesStream(t *testing.T) {
	p := newTestProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.SubscribeTurnEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeTurnEvents: %v", err)
	}
	cancel()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("received %+v after cancel, want a closed stream", ev)
		}
	case <-time.After(sessionEventWait):
		t.Fatalf("stream still open %s after cancel", sessionEventWait)
	}
	// Publishing after the subscriber left must not panic on the closed
	// channel.
	sc := newSessionConn(nil, nil, nil, 10, nil)
	attachTestTurns(p, sc, "late", "")
	sc.setActivePrompt(1)
	respondTo(sc, 1, `{"stopReason":"end_turn"}`)
}

func TestTurnEventsRequireAContext(t *testing.T) {
	p := newTestProvider(t)
	//nolint:staticcheck // SA1012: the nil context is the case under test.
	if _, err := p.SubscribeTurnEvents(nil); err == nil {
		t.Fatal("SubscribeTurnEvents(nil) succeeded, want an error")
	}
}

// TestTurnEventsSlowSubscriberDropsAndCounts pins the overflow contract: a
// subscriber that stops reading keeps the first turnEventBuffer events,
// later events are dropped and counted without blocking the publisher, and
// delivery resumes once the subscriber has room.
func TestTurnEventsSlowSubscriberDropsAndCounts(t *testing.T) {
	h := &turnEventHub{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.subscribe(ctx)
	src := &turnEventSource{hub: h, session: "s"}

	const turns = turnEventBuffer // two events per turn: overflows by half
	for i := range turns {
		rec := &turnRecord{ID: fmt.Sprintf("turn-%d", i), StartedAt: time.Now()}
		src.started(rec)
		src.completed(rec, turnOutcome{state: turnCompleted, stopReason: "end_turn"}, time.Now())
	}
	if got := h.dropped(); got != turns {
		t.Fatalf("dropped = %d, want %d", got, turns)
	}
	for i := range turnEventBuffer / 2 {
		for _, kind := range []runtime.TurnEventKind{runtime.TurnEventStarted, runtime.TurnEventCompleted} {
			ev := nextTurnEvent(t, ch)
			if ev.Kind != kind || ev.TurnID != fmt.Sprintf("turn-%d", i) {
				t.Fatalf("event = %s(%s), want %s(turn-%d)", ev.Kind, ev.TurnID, kind, i)
			}
		}
	}
	expectNoTurnEvent(t, ch)

	rec := &turnRecord{ID: "after", StartedAt: time.Now()}
	src.started(rec)
	if ev := nextTurnEvent(t, ch); ev.Kind != runtime.TurnEventStarted || ev.TurnID != "after" {
		t.Fatalf("event after the loss = %s(%s), want started(after)", ev.Kind, ev.TurnID)
	}
	if got := h.dropped(); got != 0 {
		t.Fatalf("dropped after recovery = %d, want the count reset once logged", got)
	}
}

func TestTurnEventsStalledSubscriberDoesNotBlockPrompts(t *testing.T) {
	p := newTestProvider(t)
	name := startFake(t, p, fakeACPShellCommand())
	_ = subscribeTurns(t, p) // never read

	// Every turn publishes two events, so this overflows the buffer.
	for i := range turnEventBuffer/2 + 2 {
		if err := p.Nudge(name, runtime.TextContent(fmt.Sprintf("turn %d", i))); err != nil {
			t.Fatalf("Nudge %d: %v", i, err)
		}
		if err := p.WaitForIdle(context.Background(), name, sessionEventWait); err != nil {
			t.Fatalf("WaitForIdle after turn %d: %v", i, err)
		}
	}
	if got := p.turnEvents.dropped(); got == 0 {
		t.Fatal("no turn events dropped, want the overflow counted")
	}
}

func TestTurnEventsSurviveProductionWrapping(t *testing.T) {
	seam := NewSeamBackedWithDir(shortTempDir(t), Config{})
	tep, ok := seam.(runtime.TurnEventProvider)
	if !ok {
		t.Fatal("NewSeamBackedWithDir does not implement runtime.TurnEventProvider")
	}
	raw := seam.(*seamBackedProvider).raw
	sc := newSessionConn(nil, nil, nil, 10, nil)
	attachTestTurns(raw, sc, "s", "")

	direct := subscribeTurns(t, tep)
	viaAuto := subscribeTurns(t, auto.New(runtime.NewFake(), seam))
	viaHybrid := subscribeTurns(t, hybrid.New(auto.New(runtime.NewFake(), seam), runtime.NewFake(), func(string) bool { return false }))

	sc.setActivePrompt(1)
	respondTo(sc, 1, `{"stopReason":"end_turn"}`)
	for _, ch := range []<-chan runtime.TurnEvent{direct, viaAuto, viaHybrid} {
		expectTurnPair(t, ch, "s", "")
	}
}
