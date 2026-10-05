package api

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// Supervisor-stream membership properties (Hardening P0-2): the client
// reconnects with its composite id across city-set changes. The tests wait for
// facts (frames delivered), never for wall-clock time.

// Reconnecting with the last composite id after the city set changed while the
// client was away delivers exactly the events the client missed for the cities
// in its id, and starts a city that joined meanwhile at its head.
func TestSupervisorGlobalEventStreamReconnectAcrossMembershipChange(t *testing.T) {
	alpha := newNamedFakeState(t, "alpha", events.NewFake())
	beta := newNamedFakeState(t, "beta", events.NewFake())
	resolver := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha, "beta": beta})
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())

	first := openSSEStream(t, sm, "/v0/events/stream", "")
	live := &liveGlobalStream{frames: first.frames}
	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "alpha-1"})
	frame, data := live.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "alpha", "alpha-1", "alpha:1,beta:0")
	lastID := frame.ID
	first.stop()

	// While away: both cities keep writing and gamma joins with history the
	// client never asked for.
	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "alpha-2"})
	beta.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-1"})
	gammaProv := events.NewFake()
	gammaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "gamma-old"})
	resolver.addCity(newNamedFakeState(t, "gamma", gammaProv))

	second := openLiveGlobalStream(t, sm, "", lastID)
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		_, data := second.nextTaggedEvent(t)
		subject, _ := data["subject"].(string)
		if seen[subject] {
			t.Fatalf("event %s delivered twice after reconnect", subject)
		}
		seen[subject] = true
	}
	if !seen["alpha-2"] || !seen["beta-1"] {
		t.Fatalf("after reconnect delivered %v, want alpha-2 and beta-1", seen)
	}
	gammaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "gamma-new"})
	frame, data = second.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "gamma", "gamma-new", "alpha:2,beta:1,gamma:2")
}
