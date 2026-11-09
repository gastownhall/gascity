package main

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

func sessionListEventPayload(t *testing.T, b beads.Bead) json.RawMessage {
	t.Helper()
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatalf("encode %s snapshot: %v", b.ID, err)
	}
	return payload
}

func sessionListEventsOf(t *testing.T, ep *events.Fake) []events.Event {
	t.Helper()
	var out []events.Event
	for _, e := range ep.Events {
		if e.Type == events.SessionCreated || e.Type == events.SessionClosed {
			out = append(out, e)
		}
	}
	return out
}

func sessionListTestController(t *testing.T) (*controllerState, *events.Fake) {
	t.Helper()
	countBeadCloseAutocloseDispatches(t)
	cityStore, _ := primedCache(t)
	cs := identityTestController(cityStore, nil)
	ep := events.NewFake()
	cs.eventProv = ep
	return cs, ep
}

// The controller's bead event watcher turns a session bead's create and close
// into exactly one session.created and one session.closed, with typed
// payloads, however many times the ledger repeats the mutation.
func TestBeadEventWatcherDerivesSessionListEvents(t *testing.T) {
	cs, ep := sessionListTestController(t)
	open := beads.Bead{
		ID: "ct-1", Title: "worker", Type: session.BeadType, Status: "open",
		Labels:   []string{session.LabelSession, "agent:rig/worker"},
		Metadata: map[string]string{"template": "rig/worker", "alias": "w1", "agent_name": "rig/worker-1"},
	}
	closed := open
	closed.Status = "closed"
	closed.Metadata = map[string]string{"template": "rig/worker", "alias": "w1", "agent_name": "rig/worker-1", "close_reason": "drained"}

	// The cache's own create, then the same create inferred by a refresh.
	cs.applyBeadEventToStores(events.Event{Type: events.BeadCreated, Actor: cacheLocalActor, Subject: "ct-1", Payload: sessionListEventPayload(t, open)})
	cs.applyBeadEventToStores(events.Event{Type: events.BeadCreated, Actor: cacheReconcileActor, Subject: "ct-1", Payload: sessionListEventPayload(t, open)})
	cs.applyBeadEventToStores(events.Event{Type: events.BeadUpdated, Actor: cacheLocalActor, Subject: "ct-1", Payload: sessionListEventPayload(t, open)})
	// A double close: the writer's event and the cache's.
	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: "gc", Subject: "ct-1", Payload: sessionListEventPayload(t, closed)})
	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: cacheReconcileActor, Subject: "ct-1", Payload: sessionListEventPayload(t, closed)})

	got := sessionListEventsOf(t, ep)
	if len(got) != 2 {
		t.Fatalf("session list events = %+v, want one session.created then one session.closed", got)
	}
	if got[0].Type != events.SessionCreated || got[1].Type != events.SessionClosed {
		t.Fatalf("session list event types = [%s %s], want [%s %s]", got[0].Type, got[1].Type, events.SessionCreated, events.SessionClosed)
	}
	for _, e := range got {
		if e.Subject != "ct-1" || e.SessionID != "ct-1" {
			t.Fatalf("%s envelope subject=%q session_id=%q, want ct-1", e.Type, e.Subject, e.SessionID)
		}
	}
	created, _, err := events.DecodePayload(events.SessionCreated, got[0].Payload)
	if err != nil {
		t.Fatalf("decode session.created: %v", err)
	}
	if want := (api.SessionCreatedPayload{SessionID: "ct-1", Template: "rig/worker", Title: "worker", Alias: "w1", AgentName: "rig/worker-1"}); created != want {
		t.Fatalf("session.created payload = %+v, want %+v", created, want)
	}
	closedPayload, _, err := events.DecodePayload(events.SessionClosed, got[1].Payload)
	if err != nil {
		t.Fatalf("decode session.closed: %v", err)
	}
	if want := (api.SessionClosedPayload{SessionID: "ct-1", Template: "rig/worker", Title: "worker", Alias: "w1", AgentName: "rig/worker-1", Reason: "drained"}); closedPayload != want {
		t.Fatalf("session.closed payload = %+v, want %+v", closedPayload, want)
	}
}

// Bead events for beads that are not sessions derive nothing.
func TestBeadEventWatcherIgnoresNonSessionBeads(t *testing.T) {
	cs, ep := sessionListTestController(t)
	task := beads.Bead{ID: "ct-2", Title: "task", Type: "task", Status: "open"}
	cs.applyBeadEventToStores(events.Event{Type: events.BeadCreated, Actor: cacheLocalActor, Subject: "ct-2", Payload: sessionListEventPayload(t, task)})
	task.Status = "closed"
	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: cacheLocalActor, Subject: "ct-2", Payload: sessionListEventPayload(t, task)})
	if got := sessionListEventsOf(t, ep); len(got) != 0 {
		t.Fatalf("session list events for a task bead = %+v, want none", got)
	}
}
