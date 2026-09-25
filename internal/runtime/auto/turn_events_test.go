package auto

import (
	"context"
	"testing"

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

func TestSubscribeTurnEventsForwardsTheImplementingBackend(t *testing.T) {
	cases := []struct {
		name      string
		defaultSP runtime.Provider
		acpSP     runtime.Provider
		want      func(d, a runtime.Provider) chan runtime.TurnEvent
	}{
		{
			name:      "acp only",
			defaultSP: runtime.NewFake(),
			acpSP:     newTurnEventFake(),
			want:      func(_, a runtime.Provider) chan runtime.TurnEvent { return a.(*turnEventFake).ch },
		},
		{
			name:      "default only",
			defaultSP: newTurnEventFake(),
			acpSP:     runtime.NewFake(),
			want:      func(d, _ runtime.Provider) chan runtime.TurnEvent { return d.(*turnEventFake).ch },
		},
		{
			name:      "both: default first, like session events",
			defaultSP: newTurnEventFake(),
			acpSP:     newTurnEventFake(),
			want:      func(d, _ runtime.Provider) chan runtime.TurnEvent { return d.(*turnEventFake).ch },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p runtime.TurnEventProvider = New(tc.defaultSP, tc.acpSP)
			got, err := p.SubscribeTurnEvents(context.Background())
			if err != nil {
				t.Fatalf("SubscribeTurnEvents: %v", err)
			}
			want := tc.want(tc.defaultSP, tc.acpSP)
			want <- runtime.TurnEvent{Kind: runtime.TurnEventStarted, TurnID: "t1"}
			if ev := <-got; ev.TurnID != "t1" {
				t.Fatalf("forwarded event = %+v, want turn t1", ev)
			}
		})
	}
}

func TestSubscribeTurnEventsErrorsWithoutAnImplementingBackend(t *testing.T) {
	p := New(runtime.NewFake(), runtime.NewFake())
	if _, err := p.SubscribeTurnEvents(context.Background()); err == nil {
		t.Fatal("SubscribeTurnEvents succeeded with no turn-event backend, want an error")
	}
}
