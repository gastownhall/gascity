package auto

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// eventWait bounds every fact-based wait on a session-event stream.
const eventWait = 10 * time.Second

// eventedBackend is a fake backend whose session-event stream the test
// drives; it closes when the subscription ctx ends.
type eventedBackend struct {
	*runtime.Fake
	ch chan runtime.SessionEvent
}

func newEventedBackend() *eventedBackend {
	return &eventedBackend{Fake: runtime.NewFake(), ch: make(chan runtime.SessionEvent, 4)}
}

func (b *eventedBackend) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	go func() {
		<-ctx.Done()
		close(b.ch)
	}()
	return b.ch, nil
}

func receive(t *testing.T, ch <-chan runtime.SessionEvent) runtime.SessionEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("session-event stream closed, want an event")
		}
		return ev
	case <-time.After(eventWait):
		t.Fatalf("no session event within %s", eventWait)
	}
	return runtime.SessionEvent{}
}

// TestAutoSessionEventsMergeBothBackends pins that a composite whose
// backends both publish session events forwards both streams instead of
// silently dropping one backend's deaths.
func TestAutoSessionEventsMergeBothBackends(t *testing.T) {
	a, b := newEventedBackend(), newEventedBackend()
	p := New(a, b)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	a.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "on-first"}
	if ev := receive(t, ch); ev.Session != "on-first" {
		t.Fatalf("event session = %q, want on-first", ev.Session)
	}
	b.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "on-second"}
	if ev := receive(t, ch); ev.Session != "on-second" {
		t.Fatalf("event session = %q, want on-second", ev.Session)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("event after cancel, want a closed stream")
		}
	case <-time.After(eventWait):
		t.Fatalf("stream still open %s after cancel", eventWait)
	}
}

// TestAutoSessionEventsSingleBackendUnchanged pins that a composite with
// one event-capable backend returns that backend's stream as is.
func TestAutoSessionEventsSingleBackendUnchanged(t *testing.T) {
	b := newEventedBackend()
	p := New(runtime.NewFake(), b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	if ch != (<-chan runtime.SessionEvent)(b.ch) {
		t.Fatal("single event-capable backend's stream was wrapped, want it unchanged")
	}
}

// TestAutoSessionEventsNoBackendErrors pins the error when neither backend
// publishes session events.
func TestAutoSessionEventsNoBackendErrors(t *testing.T) {
	a, b := runtime.NewFake(), runtime.NewFake()
	p := New(a, b)
	if _, err := p.SubscribeSessionEvents(context.Background()); err == nil {
		t.Fatal("err = nil, want an error when no backend publishes session events")
	}
}
