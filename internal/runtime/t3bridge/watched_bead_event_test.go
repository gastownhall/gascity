package t3bridge

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

func TestWatchedBeadEventCanonicalizesIdentity(t *testing.T) {
	payload := json.RawMessage(`{"id":"ga-1","title":"work","status":"closed"}`)
	for _, tc := range []struct {
		name    string
		event   events.Event
		wantOK  bool
		wantSub string
	}{
		{name: "matching subject", event: events.Event{Type: events.BeadClosed, Subject: "ga-1", Payload: payload}, wantOK: true, wantSub: "ga-1"},
		{name: "subjectless event takes payload id", event: events.Event{Type: events.BeadUpdated, Payload: payload}, wantOK: true, wantSub: "ga-1"},
		{name: "mismatched subject dropped", event: events.Event{Type: events.BeadClosed, Subject: "ga-2", Payload: payload}},
		{name: "no identity dropped", event: events.Event{Type: events.BeadCreated, Payload: json.RawMessage(`{`)}},
		{name: "non-projected type dropped", event: events.Event{Type: events.BeadDeleted, Subject: "ga-1", Payload: payload}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := watchedBeadEvent(tc.event)
			if ok != tc.wantOK {
				t.Fatalf("watchedBeadEvent ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Subject != tc.wantSub {
				t.Fatalf("watchedBeadEvent subject = %q, want %q", got.Subject, tc.wantSub)
			}
		})
	}
}
