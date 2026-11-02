package auto

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// eventedBackend is a fake backend with a session-event stream.
type eventedBackend struct {
	*runtime.Fake
	ch chan runtime.SessionEvent
}

func (b *eventedBackend) SubscribeSessionEvents(context.Context) (<-chan runtime.SessionEvent, error) {
	return b.ch, nil
}

// TestAutoSessionEventsSkipNestedHybridWithoutEvents pins the production
// shape auto(hybrid(tmux, k8s), acp): the nested hybrid has the method but
// no event stream, which must not hide or fail the ACP stream.
func TestAutoSessionEventsSkipNestedHybridWithoutEvents(t *testing.T) {
	evented := &eventedBackend{Fake: runtime.NewFake(), ch: make(chan runtime.SessionEvent)}
	nested := hybrid.New(runtime.NewFake(), runtime.NewFake(), func(string) bool { return false })
	ch, err := New(nested, evented).SubscribeSessionEvents(context.Background())
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	if ch != (<-chan runtime.SessionEvent)(evented.ch) {
		t.Fatal("ACP stream was wrapped or replaced, want it unchanged")
	}
}

func TestAutoSessionEventsNoBackendIsSentinel(t *testing.T) {
	nested := hybrid.New(runtime.NewFake(), runtime.NewFake(), func(string) bool { return false })
	_, err := New(nested, runtime.NewFake()).SubscribeSessionEvents(context.Background())
	if !errors.Is(err, runtime.ErrNoSessionEventSource) {
		t.Fatalf("err = %v, want ErrNoSessionEventSource", err)
	}
}

func TestAutoSessionEventsMergeBothBackends(t *testing.T) {
	a := &eventedBackend{Fake: runtime.NewFake(), ch: make(chan runtime.SessionEvent, 1)}
	b := &eventedBackend{Fake: runtime.NewFake(), ch: make(chan runtime.SessionEvent, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := New(a, b).SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	a.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "on-default"}
	b.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "on-acp"}
	seen := map[string]bool{}
	for range 2 {
		seen[(<-ch).Session] = true
	}
	if !seen["on-default"] || !seen["on-acp"] {
		t.Fatalf("merged events = %v, want both backends' events", seen)
	}
}
