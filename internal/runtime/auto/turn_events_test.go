package auto

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// turnEventFake is a Fake that also serves a turn-event stream it owns.
type turnEventFake struct {
	*runtime.Fake
	ch chan runtime.TurnEvent
}

func newTurnEventFake() *turnEventFake {
	return &turnEventFake{Fake: runtime.NewFake(), ch: make(chan runtime.TurnEvent, 1)}
}

func (f *turnEventFake) SubscribeTurnEvents(context.Context) (<-chan runtime.TurnEvent, error) {
	return f.ch, nil
}

// recvTurn reads one event from ch or fails after a bounded wait.
func recvTurn(t *testing.T, ch <-chan runtime.TurnEvent) runtime.TurnEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("turn-event stream closed, want an event")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no turn event within 10s")
	}
	return runtime.TurnEvent{}
}

func TestSubscribeTurnEventsForwardsTheImplementingBackend(t *testing.T) {
	cases := []struct {
		name             string
		defaultSP, acpSP runtime.Provider
		senders          func(a, b runtime.Provider) []chan runtime.TurnEvent
	}{
		{
			name:      "acp only",
			defaultSP: runtime.NewFake(),
			acpSP:     newTurnEventFake(),
			senders: func(_, b runtime.Provider) []chan runtime.TurnEvent {
				return []chan runtime.TurnEvent{b.(*turnEventFake).ch}
			},
		},
		{
			name:      "default only",
			defaultSP: newTurnEventFake(),
			acpSP:     runtime.NewFake(),
			senders: func(a, _ runtime.Provider) []chan runtime.TurnEvent {
				return []chan runtime.TurnEvent{a.(*turnEventFake).ch}
			},
		},
		{
			name:      "both: streams merged, like session events",
			defaultSP: newTurnEventFake(),
			acpSP:     newTurnEventFake(),
			senders: func(a, b runtime.Provider) []chan runtime.TurnEvent {
				return []chan runtime.TurnEvent{a.(*turnEventFake).ch, b.(*turnEventFake).ch}
			},
		},
		{
			name:      "acp behind a nested composite without turn events",
			defaultSP: New(runtime.NewFake(), runtime.NewFake()),
			acpSP:     newTurnEventFake(),
			senders: func(_, b runtime.Provider) []chan runtime.TurnEvent {
				return []chan runtime.TurnEvent{b.(*turnEventFake).ch}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var p runtime.TurnEventProvider = New(tc.defaultSP, tc.acpSP)
			got, err := p.SubscribeTurnEvents(ctx)
			if err != nil {
				t.Fatalf("SubscribeTurnEvents: %v", err)
			}
			for i, ch := range tc.senders(tc.defaultSP, tc.acpSP) {
				id := fmt.Sprintf("t%d", i)
				ch <- runtime.TurnEvent{Kind: runtime.TurnEventStarted, TurnID: id}
				if ev := recvTurn(t, got); ev.TurnID != id {
					t.Fatalf("forwarded event = %+v, want turn %s", ev, id)
				}
			}
		})
	}
}

func TestSubscribeTurnEventsErrorsWithoutAnImplementingBackend(t *testing.T) {
	p := New(runtime.NewFake(), runtime.NewFake())
	_, err := p.SubscribeTurnEvents(context.Background())
	if !errors.Is(err, runtime.ErrNoTurnEventSource) {
		t.Fatalf("err = %v, want runtime.ErrNoTurnEventSource", err)
	}
}
