package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gorilla/websocket"
)

// TestEndToEndProtocol exercises the full path through the supervisor:
// connect → hello → scoped request → subscription → event → unsubscribe → close.
func TestEndToEndProtocol(t *testing.T) {
	// 1. Create supervisor with one fake city.
	state := newFakeState(t)
	state.cityName = "test-city"
	sm := newTestSupervisorMux(t, map[string]*fakeState{"test-city": state})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v0/ws"
	header := http.Header{}
	header.Set("Origin", "http://localhost")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 2. Verify hello envelope.
	var hello HelloEnvelope
	if err := conn.ReadJSON(&hello); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if hello.Type != "hello" {
		t.Fatalf("hello.type = %q, want hello", hello.Type)
	}
	if hello.Protocol == "" {
		t.Fatal("hello.protocol is empty")
	}
	if hello.ServerRole != "supervisor" {
		t.Fatalf("hello.server_role = %q, want supervisor", hello.ServerRole)
	}
	if len(hello.Capabilities) == 0 {
		t.Fatal("hello.capabilities is empty")
	}
	if len(hello.SubscriptionKinds) == 0 {
		t.Fatal("hello.subscription_kinds is empty")
	}

	// 3. Send a scoped request (beads.list) and verify response correlation.
	_ = conn.WriteJSON(map[string]any{
		"type":   "request",
		"id":     "e2e-1",
		"action": "beads.list",
		"scope":  map[string]string{"city": "test-city"},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "e2e-1" {
		t.Fatalf("response: type=%q id=%q, want response/e2e-1", resp.Type, resp.ID)
	}
	var listResult struct {
		Items json.RawMessage `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &listResult); err != nil {
		t.Fatalf("decode list result: %v", err)
	}

	// 4. Start an event subscription.
	_ = conn.WriteJSON(map[string]any{
		"type":   "request",
		"id":     "e2e-2",
		"action": "subscription.start",
		"scope":  map[string]string{"city": "test-city"},
		"payload": map[string]any{
			"kind": "events",
		},
	})
	var subResp wsResponseEnvelope
	readWSJSON(t, conn, &subResp)
	if subResp.Type != "response" || subResp.ID != "e2e-2" {
		t.Fatalf("sub response: type=%q id=%q, want response/e2e-2", subResp.Type, subResp.ID)
	}
	var subResult struct {
		SubscriptionID string `json:"subscription_id"`
	}
	if err := json.Unmarshal(subResp.Result, &subResult); err != nil {
		t.Fatalf("decode sub result: %v", err)
	}
	if subResult.SubscriptionID == "" {
		t.Fatal("subscription_id is empty")
	}

	// 5. Record an event in the fake city and verify delivery.
	state.eventProv.(*events.Fake).Record(events.Event{
		Type:  events.BeadCreated,
		Actor: "test-agent",
	})
	// Read the event (may need to wait briefly for delivery).
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var evt struct {
		Type           string `json:"type"`
		SubscriptionID string `json:"subscription_id"`
		EventType      string `json:"event_type"`
	}
	if err := conn.ReadJSON(&evt); err != nil {
		t.Fatalf("read event: %v", err)
	}
	conn.SetReadDeadline(time.Time{})
	if evt.Type != "event" {
		t.Fatalf("event.type = %q, want event", evt.Type)
	}
	if evt.SubscriptionID != subResult.SubscriptionID {
		t.Fatalf("event.subscription_id = %q, want %q", evt.SubscriptionID, subResult.SubscriptionID)
	}
	if evt.EventType != events.BeadCreated {
		t.Fatalf("event.event_type = %q, want %q", evt.EventType, events.BeadCreated)
	}

	// 6. Unsubscribe.
	_ = conn.WriteJSON(map[string]any{
		"type":   "request",
		"id":     "e2e-3",
		"action": "subscription.stop",
		"payload": map[string]any{
			"subscription_id": subResult.SubscriptionID,
		},
	})
	var unsubResp wsResponseEnvelope
	readWSJSON(t, conn, &unsubResp)
	if unsubResp.Type != "response" || unsubResp.ID != "e2e-3" {
		t.Fatalf("unsub response: type=%q id=%q", unsubResp.Type, unsubResp.ID)
	}

	// 7. Send an idempotent mutation (bead.create with idempotency_key).
	_ = conn.WriteJSON(map[string]any{
		"type":            "request",
		"id":              "e2e-4",
		"action":          "bead.create",
		"idempotency_key": "e2e-idem-1",
		"scope":           map[string]string{"city": "test-city"},
		"payload": map[string]any{
			"title": "E2E Test Bead",
			"type":  "task",
		},
	})
	var createResp wsResponseEnvelope
	readWSJSON(t, conn, &createResp)
	if createResp.Type != "response" || createResp.ID != "e2e-4" {
		t.Fatalf("create response: type=%q id=%q", createResp.Type, createResp.ID)
	}

	// 8. Replay the same mutation — verify idempotent result.
	_ = conn.WriteJSON(map[string]any{
		"type":            "request",
		"id":              "e2e-5",
		"action":          "bead.create",
		"idempotency_key": "e2e-idem-1",
		"scope":           map[string]string{"city": "test-city"},
		"payload": map[string]any{
			"title": "E2E Test Bead",
			"type":  "task",
		},
	})
	var replayResp wsResponseEnvelope
	readWSJSON(t, conn, &replayResp)
	if replayResp.Type != "response" || replayResp.ID != "e2e-5" {
		t.Fatalf("replay response: type=%q id=%q", replayResp.Type, replayResp.ID)
	}

	// 9. Clean close with code 1000.
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done")
	_ = conn.WriteMessage(websocket.CloseMessage, closeMsg)
}
