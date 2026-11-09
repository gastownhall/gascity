package api

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSessionListEventPayloadsAreRegistered(t *testing.T) {
	for eventType, want := range map[string]events.Payload{
		events.SessionCreated: SessionCreatedPayload{},
		events.SessionClosed:  SessionClosedPayload{},
	} {
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %s sample: %v", eventType, err)
		}
		got, registered, err := events.DecodePayload(eventType, raw)
		if err != nil || !registered {
			t.Fatalf("DecodePayload(%s) registered=%v err=%v, want a registered payload", eventType, registered, err)
		}
		if _, ok := got.(SessionCreatedPayload); eventType == events.SessionCreated && !ok {
			t.Fatalf("DecodePayload(%s) = %T, want SessionCreatedPayload", eventType, got)
		}
		if _, ok := got.(SessionClosedPayload); eventType == events.SessionClosed && !ok {
			t.Fatalf("DecodePayload(%s) = %T, want SessionClosedPayload", eventType, got)
		}
	}
}

func TestSessionListEventPayloadsProjectInfo(t *testing.T) {
	info := session.Info{ID: "gc-1", Template: "rig/worker", Title: "worker", Alias: "w1", AgentName: "rig/worker-1", Provider: "secret-provider", Command: "run --token x"}
	created := SessionCreatedPayloadFor(info)
	if created != (SessionCreatedPayload{SessionID: "gc-1", Template: "rig/worker", Title: "worker", Alias: "w1", AgentName: "rig/worker-1"}) {
		t.Fatalf("SessionCreatedPayloadFor = %+v", created)
	}
	closed := SessionClosedPayloadFor(info, "drained")
	if closed != (SessionClosedPayload{SessionID: "gc-1", Template: "rig/worker", Title: "worker", Alias: "w1", AgentName: "rig/worker-1", Reason: "drained"}) {
		t.Fatalf("SessionClosedPayloadFor = %+v", closed)
	}
}

// session.created and session.closed reach the city event stream with their
// typed payloads, so a client can maintain the session list without polling.
func TestCityEventStreamCarriesTypedSessionListEvents(t *testing.T) {
	info := session.Info{ID: "gc-1", Template: "rig/worker", Title: "worker", Alias: "w1", AgentName: "rig/worker-1"}
	cases := []struct {
		eventType string
		payload   events.Payload
		want      map[string]any
	}{
		{events.SessionCreated, SessionCreatedPayloadFor(info), map[string]any{
			"session_id": "gc-1", "template": "rig/worker", "title": "worker", "alias": "w1", "agent_name": "rig/worker-1",
		}},
		{events.SessionClosed, SessionClosedPayloadFor(info, "drained"), map[string]any{
			"session_id": "gc-1", "template": "rig/worker", "title": "worker", "alias": "w1", "agent_name": "rig/worker-1", "reason": "drained",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			s := newFakeState(t)
			s.cityName = "gc-work"
			sm := newTestSupervisorMux(t, map[string]*fakeState{"gc-work": s})
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			frame := firstSSEFrameAfterRecord(t, sm, "/v0/city/gc-work/events/stream", "event", func() {
				s.eventProv.(*events.Fake).Record(events.Event{
					Type: tc.eventType, Actor: "gc", Subject: "gc-1", SessionID: "gc-1", Payload: raw,
				})
			})
			data := decodeSSETestData(t, frame)
			if data["type"] != tc.eventType {
				t.Fatalf("data.type = %v, want %s; data=%v", data["type"], tc.eventType, data)
			}
			payload := assertJSONPayloadObject(t, data["payload"])
			if len(payload) != len(tc.want) {
				t.Fatalf("payload = %v, want exactly %v", payload, tc.want)
			}
			for k, v := range tc.want {
				if payload[k] != v {
					t.Fatalf("payload[%q] = %v, want %v; payload=%v", k, payload[k], v, payload)
				}
			}
		})
	}
}
