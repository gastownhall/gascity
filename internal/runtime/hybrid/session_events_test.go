package hybrid

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// eventedBackend is a fake backend with a session-event stream.
type eventedBackend struct {
	*runtime.Fake
	ch chan runtime.SessionEvent
}

func (b *eventedBackend) SubscribeSessionEvents(context.Context) (<-chan runtime.SessionEvent, error) {
	return b.ch, nil
}

// TestHybridSessionEventsMergeBothBackends pins that an event-capable local
// backend no longer hides the remote backend's session events.
func TestHybridSessionEventsMergeBothBackends(t *testing.T) {
	local := &eventedBackend{Fake: runtime.NewFake(), ch: make(chan runtime.SessionEvent, 1)}
	remote := &eventedBackend{Fake: runtime.NewFake(), ch: make(chan runtime.SessionEvent, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := New(local, remote, func(string) bool { return false }).SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	local.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "on-local"}
	remote.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "on-remote"}
	seen := map[string]bool{}
	for range 2 {
		seen[(<-ch).Session] = true
	}
	if !seen["on-local"] || !seen["on-remote"] {
		t.Fatalf("merged events = %v, want both backends' events", seen)
	}
}
