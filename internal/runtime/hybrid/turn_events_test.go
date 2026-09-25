package hybrid

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
		name   string
		local  runtime.Provider
		remote runtime.Provider
		want   func(l, r runtime.Provider) chan runtime.TurnEvent
	}{
		{
			name:   "remote only",
			local:  runtime.NewFake(),
			remote: newTurnEventFake(),
			want:   func(_, r runtime.Provider) chan runtime.TurnEvent { return r.(*turnEventFake).ch },
		},
		{
			name:   "local only",
			local:  newTurnEventFake(),
			remote: runtime.NewFake(),
			want:   func(l, _ runtime.Provider) chan runtime.TurnEvent { return l.(*turnEventFake).ch },
		},
		{
			name:   "both: local first, like session events",
			local:  newTurnEventFake(),
			remote: newTurnEventFake(),
			want:   func(l, _ runtime.Provider) chan runtime.TurnEvent { return l.(*turnEventFake).ch },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p runtime.TurnEventProvider = New(tc.local, tc.remote, func(string) bool { return false })
			got, err := p.SubscribeTurnEvents(context.Background())
			if err != nil {
				t.Fatalf("SubscribeTurnEvents: %v", err)
			}
			want := tc.want(tc.local, tc.remote)
			want <- runtime.TurnEvent{Kind: runtime.TurnEventStarted, TurnID: "t1"}
			if ev := <-got; ev.TurnID != "t1" {
				t.Fatalf("forwarded event = %+v, want turn t1", ev)
			}
		})
	}
}

func TestSubscribeTurnEventsErrorsWithoutAnImplementingBackend(t *testing.T) {
	p := New(runtime.NewFake(), runtime.NewFake(), func(string) bool { return false })
	if _, err := p.SubscribeTurnEvents(context.Background()); err == nil {
		t.Fatal("SubscribeTurnEvents succeeded with no turn-event backend, want an error")
	}
}
