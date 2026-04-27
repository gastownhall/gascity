package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestEventList(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "worker"})
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "worker", Subject: "gc-1"})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []events.Event `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2", resp.Total)
	}
}

func TestEventListFilterByType(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "worker"})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events?type=bead.created"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []events.Event `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1", resp.Total)
	}
}

func TestEventListIncludesCustomEventTypes(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: "custom.untyped", Actor: "tester", Payload: json.RawMessage(`{"source":"test"}`)})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 || len(resp.Items) != 2 {
		t.Fatalf("response = %+v, want custom and registered events", resp)
	}
	custom := eventListItemByType(t, resp.Items, "custom.untyped")
	payload := assertJSONPayloadObject(t, custom["payload"])
	if payload["source"] != "test" {
		t.Fatalf("custom payload = %v, want source=test", payload)
	}
}

func TestEventListRejectsInvalidSince(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events?since=notaduration"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid since duration") {
		t.Fatalf("body = %q, want invalid since duration", rec.Body.String())
	}
}

func TestEventStream(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	h := newTestCityHandler(t, state)

	// Create a context with timeout to avoid hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(state, "/events/stream"), nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Run the handler in a goroutine since it blocks.
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Give the handler time to set up.
	time.Sleep(50 * time.Millisecond)

	// Record an event.
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "worker"})

	// Wait for event to be delivered or timeout.
	time.Sleep(100 * time.Millisecond)
	cancel() // Stop the stream.
	<-done

	body := rec.Body.String()
	// Event name is now "event" (documented in OpenAPI spec via sse.Register).
	// The actual event type is in the JSON body's "type" field.
	if !strings.Contains(body, "event: event") {
		t.Errorf("SSE body missing event: event line, got: %s", body)
	}
	if !strings.Contains(body, `"type":"session.woke"`) {
		t.Errorf("SSE body missing event type in JSON body, got: %s", body)
	}
	if !strings.Contains(body, "id: 1") {
		t.Errorf("SSE body missing event id, got: %s", body)
	}

	// Check SSE headers.
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}
}

func TestEventStreamProjectsWorkflowMetadata(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	root, err := store.Create(beads.Bead{
		Title: "Workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf_123",
			"gc.scope_kind":  "city",
			"gc.scope_ref":   "test-city",
		},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	payload, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal root: %v", err)
	}

	h := newTestCityHandler(t, state)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(state, "/events/stream"), nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	state.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.BeadUpdated,
		Actor:   "worker",
		Subject: root.ID,
		Payload: payload,
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, `"workflow":{"type":"workflow:event"`) {
		t.Fatalf("SSE body missing workflow projection: %s", body)
	}
	if !strings.Contains(body, `"workflow_id":"wf_123"`) {
		t.Fatalf("SSE body missing workflow id: %s", body)
	}
	if !strings.Contains(body, `"scope_kind":"city"`) {
		t.Fatalf("SSE body missing logical scope: %s", body)
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

func TestEventStreamNoEvents(t *testing.T) {
	state := newFakeState(t)
	state.eventProv = nil
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events/stream"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleEventEmit(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	body := `{"type":"deploy.completed","actor":"ci","subject":"myapp","message":"v2.3.1"}`
	req := newPostRequest(cityURL(state, "/events"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
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
	h := newTestCityHandler(t, state)

	body := `{"actor":"ci"}`
	req := newPostRequest(cityURL(state, "/events"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleEventEmit_MissingActor(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	body := `{"type":"test.event"}`
	req := newPostRequest(cityURL(state, "/events"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleEventEmit_NoEventsProvider(t *testing.T) {
	state := newFakeState(t)
	state.eventProv = nil
	h := newTestCityHandler(t, state)

	body := `{"type":"test.event","actor":"ci"}`
	req := newPostRequest(cityURL(state, "/events"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
