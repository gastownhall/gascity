package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// turnStreamFake is a Provider with a turn-event stream it owns.
type turnStreamFake struct {
	*Fake
	subscribeErr error
	ch           chan TurnEvent
}

func newTurnStreamFake() *turnStreamFake {
	return &turnStreamFake{Fake: NewFake(), ch: make(chan TurnEvent, 4)}
}

func (p *turnStreamFake) SubscribeTurnEvents(context.Context) (<-chan TurnEvent, error) {
	if p.subscribeErr != nil {
		return nil, p.subscribeErr
	}
	return p.ch, nil
}

func turnSrc(name string, p Provider) TurnEventSource {
	return TurnEventSource{Name: name, Provider: p}
}

func recvTurnEvent(t *testing.T, ch <-chan TurnEvent) TurnEvent {
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
	return TurnEvent{}
}

func TestSubscribeTurnEventSourcesMergesBoth(t *testing.T) {
	a, b := newTurnStreamFake(), newTurnStreamFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := SubscribeTurnEventSources(ctx, turnSrc("a", a), turnSrc("b", b))
	if err != nil {
		t.Fatalf("SubscribeTurnEventSources: %v", err)
	}
	a.ch <- TurnEvent{Kind: TurnEventStarted, TurnID: "on-a"}
	if ev := recvTurnEvent(t, ch); ev.TurnID != "on-a" {
		t.Fatalf("turn = %q, want on-a", ev.TurnID)
	}
	b.ch <- TurnEvent{Kind: TurnEventStarted, TurnID: "on-b"}
	if ev := recvTurnEvent(t, ch); ev.TurnID != "on-b" {
		t.Fatalf("turn = %q, want on-b", ev.TurnID)
	}
}

func TestSubscribeTurnEventSourcesSingleStreamUnchanged(t *testing.T) {
	b := newTurnStreamFake()
	ch, err := SubscribeTurnEventSources(context.Background(), turnSrc("a", NewFake()), turnSrc("b", b))
	if err != nil {
		t.Fatalf("SubscribeTurnEventSources: %v", err)
	}
	if ch != (<-chan TurnEvent)(b.ch) {
		t.Fatal("single stream was wrapped, want it unchanged")
	}
}

func TestSubscribeTurnEventSourcesNoSourceIsSentinel(t *testing.T) {
	_, err := SubscribeTurnEventSources(context.Background(), turnSrc("a", NewFake()), turnSrc("b", NewFake()))
	if !errors.Is(err, ErrNoTurnEventSource) {
		t.Fatalf("err = %v, want ErrNoTurnEventSource", err)
	}
}

// TestSubscribeTurnEventSourcesSkipsNestedNonImplementer pins that a nested
// composite with no turn-event backend does not hide the other backend's
// stream.
func TestSubscribeTurnEventSourcesSkipsNestedNonImplementer(t *testing.T) {
	nested := newTurnStreamFake()
	nested.subscribeErr = ErrNoTurnEventSource
	b := newTurnStreamFake()
	ch, err := SubscribeTurnEventSources(context.Background(), turnSrc("a", nested), turnSrc("b", b))
	if err != nil {
		t.Fatalf("SubscribeTurnEventSources: %v", err)
	}
	if ch != (<-chan TurnEvent)(b.ch) {
		t.Fatal("stream of the only real source was wrapped, want it unchanged")
	}
}

// TestSubscribeTurnEventSourcesFailsOnBackendError pins that a real
// subscribe failure fails the whole subscribe rather than returning a stream
// that silently lacks one backend's turns.
func TestSubscribeTurnEventSourcesFailsOnBackendError(t *testing.T) {
	broken := newTurnStreamFake()
	broken.subscribeErr = errors.New("transport down")
	_, err := SubscribeTurnEventSources(context.Background(), turnSrc("a", newTurnStreamFake()), turnSrc("b", broken))
	if err == nil || errors.Is(err, ErrNoTurnEventSource) {
		t.Fatalf("err = %v, want the backend's subscribe failure", err)
	}
}

func TestMergeTurnEventsClosesWhenBothInputsClose(t *testing.T) {
	a, b := make(chan TurnEvent), make(chan TurnEvent)
	out := MergeTurnEvents(context.Background(), a, b)
	close(a)
	close(b)
	select {
	case ev, ok := <-out:
		if ok {
			t.Fatalf("received %+v, want a closed stream", ev)
		}
	case <-time.After(mergeWait):
		t.Fatalf("stream still open after %s", mergeWait)
	}
}
