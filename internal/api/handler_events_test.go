package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func TestEventList(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "worker"})
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "worker", Subject: "gc-1"})
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "events-1",
		Action: "events.list",
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)

	var list struct {
		Items []events.Event `json:"items"`
		Total int            `json:"total"`
	}
	json.Unmarshal(resp.Result, &list) //nolint:errcheck
	if list.Total != 2 {
		t.Errorf("Total = %d, want 2", list.Total)
	}
}

func TestEventListFilterByType(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "worker"})
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "events-filter",
		Action: "events.list",
		Payload: map[string]any{
			"type": "bead.created",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)

	var list struct {
		Items []events.Event `json:"items"`
		Total int            `json:"total"`
	}
	json.Unmarshal(resp.Result, &list) //nolint:errcheck
	if list.Total != 1 {
		t.Errorf("Total = %d, want 1", list.Total)
	}
}

func TestWatcherCloseUnblocksNext(t *testing.T) {
	ep := events.NewFake()
	watcher, err := ep.Watch(context.Background(), 0)
	if err != nil {
		t.Fatalf("Watch() error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := watcher.Next()
		done <- err
	}()

	// Give Next time to block.
	time.Sleep(50 * time.Millisecond)

	// Close should unblock the blocked Next call.
	if err := watcher.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Error("Next() returned nil error after Close(); expected error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next() did not unblock after Close() — goroutine leak")
	}
}

func TestHandleEventEmit(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "emit-1",
		Action: "event.emit",
		Payload: map[string]any{
			"type":    "deploy.completed",
			"actor":   "ci",
			"subject": "myapp",
			"message": "v2.3.1",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" {
		t.Fatalf("emit response type = %q, want response", resp.Type)
	}

	ep := state.eventProv.(*events.Fake)
	evts, err := ep.List(events.Filter{Type: "deploy.completed"})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if evts[0].Actor != "ci" {
		t.Errorf("actor = %q, want %q", evts[0].Actor, "ci")
	}
	if evts[0].Subject != "myapp" {
		t.Errorf("subject = %q, want %q", evts[0].Subject, "myapp")
	}
}

func TestHandleEventEmit_MissingType(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "emit-notype",
		Action: "event.emit",
		Payload: map[string]any{
			"actor": "ci",
		},
	})
	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" {
		t.Fatalf("expected error response, got type = %q", errResp.Type)
	}
}

func TestHandleEventEmit_MissingActor(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "emit-noactor",
		Action: "event.emit",
		Payload: map[string]any{
			"type": "test.event",
		},
	})
	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" {
		t.Fatalf("expected error response, got type = %q", errResp.Type)
	}
}

func TestHandleEventEmit_NoEventsProvider(t *testing.T) {
	state := newFakeState(t)
	state.eventProv = nil
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "emit-noep",
		Action: "event.emit",
		Payload: map[string]any{
			"type":  "test.event",
			"actor": "ci",
		},
	})
	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" {
		t.Fatalf("expected error response, got type = %q", errResp.Type)
	}
}
