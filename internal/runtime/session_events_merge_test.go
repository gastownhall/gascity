package runtime

import (
	"bytes"
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
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-time.After(mergeWait):
			t.Fatalf("merged stream still open after %s", mergeWait)
		}
	}
}

func TestSubscribeSessionEventSourcesMergesBothStreams(t *testing.T) {
	a := &streamFake{Fake: NewFake()}
	b := &streamFake{Fake: NewFake()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	ch, err := SubscribeSessionEventSources(ctx, &stderr,
		SessionEventSource{Name: "a", Provider: a},
		SessionEventSource{Name: "b", Provider: b})
	if err != nil {
		t.Fatalf("SubscribeSessionEventSources: %v", err)
	}

	a.send(t, SessionEvent{Kind: SessionEventExited, Session: "from-a"})
	if ev := recvEvent(t, ch); ev.Session != "from-a" {
		t.Fatalf("first event session = %q, want from-a", ev.Session)
	}
	b.send(t, SessionEvent{Kind: SessionEventExited, Session: "from-b"})
	if ev := recvEvent(t, ch); ev.Session != "from-b" {
		t.Fatalf("second event session = %q, want from-b", ev.Session)
	}

	// One input ending keeps the other flowing.
	a.end()
	b.send(t, SessionEvent{Kind: SessionEventClosed, Session: "still-b"})
	if ev := recvEvent(t, ch); ev.Session != "still-b" {
		t.Fatalf("event after one input closed = %q, want still-b", ev.Session)
	}
	// Both inputs ending closes the merged stream.
	b.end()
	expectClosed(t, ch)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing logged", stderr.String())
	}
}

func TestSubscribeSessionEventSourcesCancelClosesMergedStream(t *testing.T) {
	a := &streamFake{Fake: NewFake()}
	b := &streamFake{Fake: NewFake()}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := SubscribeSessionEventSources(ctx, &bytes.Buffer{},
		SessionEventSource{Name: "a", Provider: a},
		SessionEventSource{Name: "b", Provider: b})
	if err != nil {
		t.Fatalf("SubscribeSessionEventSources: %v", err)
	}
	cancel()
	expectClosed(t, ch)
}

func TestSubscribeSessionEventSourcesOneFailureReturnsTheOther(t *testing.T) {
	a := &streamFake{Fake: NewFake(), subscribeErr: errors.New("socket gone")}
	b := &streamFake{Fake: NewFake()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	ch, err := SubscribeSessionEventSources(ctx, &stderr,
		SessionEventSource{Name: "local", Provider: a},
		SessionEventSource{Name: "remote", Provider: b})
	if err != nil {
		t.Fatalf("SubscribeSessionEventSources: %v", err)
	}
	b.send(t, SessionEvent{Kind: SessionEventExited, Session: "from-b"})
	if ev := recvEvent(t, ch); ev.Session != "from-b" {
		t.Fatalf("event session = %q, want from-b", ev.Session)
	}
	got := stderr.String()
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "local") || !strings.Contains(got, "socket gone") {
		t.Fatalf("stderr = %q, want one line naming the local backend and its error", got)
	}
}

func TestSubscribeSessionEventSourcesBothFailuresError(t *testing.T) {
	a := &streamFake{Fake: NewFake(), subscribeErr: errors.New("a down")}
	b := &streamFake{Fake: NewFake(), subscribeErr: errors.New("b down")}
	_, err := SubscribeSessionEventSources(context.Background(), &bytes.Buffer{},
		SessionEventSource{Name: "a", Provider: a},
		SessionEventSource{Name: "b", Provider: b})
	if err == nil || !strings.Contains(err.Error(), "a down") || !strings.Contains(err.Error(), "b down") {
		t.Fatalf("err = %v, want both subscribe failures", err)
	}
}

func TestSubscribeSessionEventSourcesSingleImplementerIsUnwrapped(t *testing.T) {
	a := &streamFake{Fake: NewFake()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := SubscribeSessionEventSources(ctx, &bytes.Buffer{},
		SessionEventSource{Name: "plain", Provider: NewFake()},
		SessionEventSource{Name: "evented", Provider: a})
	if err != nil {
		t.Fatalf("SubscribeSessionEventSources: %v", err)
	}
	a.mu.Lock()
	direct := a.ch
	a.mu.Unlock()
	if (<-chan SessionEvent)(direct) != ch {
		t.Fatal("single implementer's stream was wrapped, want it returned unchanged")
	}

	failing := &streamFake{Fake: NewFake(), subscribeErr: errors.New("only one")}
	if _, err := SubscribeSessionEventSources(ctx, &bytes.Buffer{},
		SessionEventSource{Name: "plain", Provider: NewFake()},
		SessionEventSource{Name: "evented", Provider: failing}); err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("single failing implementer err = %v, want its error", err)
	}
}

func TestSubscribeSessionEventSourcesNoImplementerErrors(t *testing.T) {
	if _, err := SubscribeSessionEventSources(context.Background(), &bytes.Buffer{},
		SessionEventSource{Name: "a", Provider: NewFake()},
		SessionEventSource{Name: "b", Provider: NewFake()}); err == nil {
		t.Fatal("err = nil, want an error when no backend implements SessionEventProvider")
	}
}
