package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// mergeWait bounds every fact-based wait on a merged stream.
const mergeWait = 10 * time.Second

// streamFake is a Provider with a controllable session-event stream whose
// channel closes when the subscription ctx ends or the test ends it.
type streamFake struct {
	*Fake
	subscribeErr error

	mu sync.Mutex
	ch chan SessionEvent
}

func newStreamFake() *streamFake { return &streamFake{Fake: NewFake()} }

func (p *streamFake) SubscribeSessionEvents(ctx context.Context) (<-chan SessionEvent, error) {
	if p.subscribeErr != nil {
		return nil, p.subscribeErr
	}
	ch := make(chan SessionEvent, 16)
	p.mu.Lock()
	p.ch = ch
	p.mu.Unlock()
	go func() {
		<-ctx.Done()
		p.end()
	}()
	return ch, nil
}

func (p *streamFake) send(t *testing.T, ev SessionEvent) {
	t.Helper()
	p.mu.Lock()
	ch := p.ch
	p.mu.Unlock()
	if ch == nil {
		t.Fatal("send: no open subscription")
	}
	ch <- ev
}

// end closes the current subscription channel once.
func (p *streamFake) end() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch != nil {
		close(p.ch)
		p.ch = nil
	}
}

func (p *streamFake) stream() <-chan SessionEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ch
}

func recvEvent(t *testing.T, ch <-chan SessionEvent) SessionEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("merged stream closed, want an event")
		}
		return ev
	case <-time.After(mergeWait):
		t.Fatalf("no event within %s", mergeWait)
	}
	return SessionEvent{}
}

func expectClosed(t *testing.T, ch <-chan SessionEvent) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("received %s(%q), want a closed stream", ev.Kind, ev.Session)
		}
	case <-time.After(mergeWait):
		t.Fatalf("stream still open after %s", mergeWait)
	}
}

func src(name string, p Provider) SessionEventSource {
	return SessionEventSource{Name: name, Provider: p}
}

func TestSubscribeSessionEventSourcesMergesBoth(t *testing.T) {
	a, b := newStreamFake(), newStreamFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := SubscribeSessionEventSources(ctx, src("a", a), src("b", b))
	if err != nil {
		t.Fatalf("SubscribeSessionEventSources: %v", err)
	}
	a.send(t, SessionEvent{Kind: SessionEventExited, Session: "on-a"})
	if ev := recvEvent(t, ch); ev.Session != "on-a" {
		t.Fatalf("session = %q, want on-a", ev.Session)
	}
	b.send(t, SessionEvent{Kind: SessionEventExited, Session: "on-b"})
	if ev := recvEvent(t, ch); ev.Session != "on-b" {
		t.Fatalf("session = %q, want on-b", ev.Session)
	}
}

func TestSubscribeSessionEventSourcesSingleStreamUnchanged(t *testing.T) {
	b := newStreamFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := SubscribeSessionEventSources(ctx, src("a", NewFake()), src("b", b))
	if err != nil {
		t.Fatalf("SubscribeSessionEventSources: %v", err)
	}
	if ch != b.stream() {
		t.Fatal("single stream was wrapped, want it unchanged")
	}
}

func TestSubscribeSessionEventSourcesNoSourceIsSentinel(t *testing.T) {
	_, err := SubscribeSessionEventSources(context.Background(), src("a", NewFake()), src("b", NewFake()))
	if !errors.Is(err, ErrNoSessionEventSource) {
		t.Fatalf("err = %v, want ErrNoSessionEventSource", err)
	}
}

// TestSubscribeSessionEventSourcesSkipsNestedNonImplementer pins that a
// nested composite with no event-capable backend is not a failure.
func TestSubscribeSessionEventSourcesSkipsNestedNonImplementer(t *testing.T) {
	nested := newStreamFake()
	nested.subscribeErr = ErrNoSessionEventSource
	b := newStreamFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := SubscribeSessionEventSources(ctx, src("a", nested), src("b", b))
	if err != nil {
		t.Fatalf("SubscribeSessionEventSources: %v", err)
	}
	if ch != b.stream() {
		t.Fatal("stream of the only real source was wrapped, want it unchanged")
	}
}

// TestSubscribeSessionEventSourcesFailsOnBackendError pins that a real
// subscribe failure fails the whole subscribe rather than returning a stream
// that silently lacks one backend's events.
func TestSubscribeSessionEventSourcesFailsOnBackendError(t *testing.T) {
	broken := newStreamFake()
	broken.subscribeErr = errors.New("transport down")
	for _, order := range [][2]SessionEventSource{
		{src("broken", broken), src("ok", newStreamFake())},
		{src("ok", newStreamFake()), src("broken", broken)},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := SubscribeSessionEventSources(ctx, order[0], order[1])
		cancel()
		if err == nil || !strings.Contains(err.Error(), "broken backend") || !strings.Contains(err.Error(), "transport down") {
			t.Fatalf("err = %v, want the broken backend's error", err)
		}
		if errors.Is(err, ErrNoSessionEventSource) {
			t.Fatalf("err = %v wraps ErrNoSessionEventSource, want a real failure", err)
		}
	}
}

func TestMergeSessionEventsSurvivesOneInputClosing(t *testing.T) {
	a, b := make(chan SessionEvent, 1), make(chan SessionEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := MergeSessionEvents(ctx, a, b)
	close(a)
	b <- SessionEvent{Kind: SessionEventClosed, Session: "on-b"}
	if ev := recvEvent(t, out); ev.Session != "on-b" {
		t.Fatalf("session = %q, want on-b", ev.Session)
	}
	close(b)
	expectClosed(t, out)
}

func TestMergeSessionEventsClosesOnCancel(t *testing.T) {
	a, b := make(chan SessionEvent), make(chan SessionEvent)
	ctx, cancel := context.WithCancel(context.Background())
	out := MergeSessionEvents(ctx, a, b)
	cancel()
	expectClosed(t, out)
}
