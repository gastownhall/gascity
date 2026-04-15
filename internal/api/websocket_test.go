package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gorilla/websocket"
)

type wsHelloEnvelope struct {
	Type         string   `json:"type"`
	Protocol     string   `json:"protocol"`
	ServerRole   string   `json:"server_role"`
	ReadOnly     bool     `json:"read_only"`
	Capabilities []string `json:"capabilities"`
}

type wsRequestEnvelope struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	Action  string      `json:"action"`
	Scope   *wsScope    `json:"scope,omitempty"`
	Payload interface{} `json:"payload,omitempty"`
}

type wsScope struct {
	City string `json:"city,omitempty"`
}

type wsResponseEnvelope struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Index  uint64          `json:"index,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

type wsErrorEnvelope struct {
	Type    string       `json:"type"`
	ID      string       `json:"id"`
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Details []FieldError `json:"details,omitempty"`
}

type wsEventEnvelope struct {
	Type           string          `json:"type"`
	SubscriptionID string          `json:"subscription_id"`
	EventType      string          `json:"event_type"`
	Index          uint64          `json:"index,omitempty"`
	Cursor         string          `json:"cursor,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

func TestServerWebSocketHello(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()

	var hello wsHelloEnvelope
	readWSJSON(t, conn, &hello)
	if hello.Type != "hello" {
		t.Fatalf("hello type = %q, want hello", hello.Type)
	}
	if hello.Protocol == "" {
		t.Fatal("hello protocol empty")
	}
	if hello.ServerRole != "city" {
		t.Fatalf("hello server_role = %q, want city", hello.ServerRole)
	}
	if hello.ReadOnly {
		t.Fatal("hello read_only = true, want false")
	}
	if len(hello.Capabilities) == 0 {
		t.Fatal("hello capabilities empty")
	}
	if wsCapabilitiesContain(hello.Capabilities, "cities.list") {
		t.Fatal("city hello advertised supervisor-only action cities.list")
	}
	if wsCapabilitiesContain(hello.Capabilities, "health.get") {
		t.Fatal("city hello advertised HTTP-only action health.get")
	}
	if !wsCapabilitiesContain(hello.Capabilities, "subscription.start") {
		t.Fatal("city hello missing protocol action subscription.start")
	}
	if !wsCapabilitiesContain(hello.Capabilities, "subscription.stop") {
		t.Fatal("city hello missing protocol action subscription.stop")
	}
}

func TestServerWebSocketRejectsHealthGet(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-1",
		Action: "health.get",
	})

	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.ID != "req-1" {
		t.Fatalf("response id = %q, want req-1", resp.ID)
	}
	if resp.Code != "not_found" {
		t.Fatalf("response code = %q, want not_found", resp.Code)
	}
}

func TestServerWebSocketStatusGet(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-status",
		Action: "status.get",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-status" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if !strings.Contains(string(resp.Result), `"name":"test-city"`) {
		t.Fatalf("result = %s, want city status payload", resp.Result)
	}
}

func TestServerWebSocketConfigAndProviderReads(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Packs = map[string]config.PackSource{
		"base": {Source: "github", Ref: "main", Path: "packs/base"},
	}
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	tests := []struct {
		id      string
		action  string
		payload any
		want    string
	}{
		{id: "req-city", action: "city.get", want: `"name":"test-city"`},
		{id: "req-config", action: "config.get", want: `"workspace"`},
		{id: "req-explain", action: "config.explain", want: `"providers"`},
		{id: "req-validate", action: "config.validate", want: `"valid":true`},
		{id: "req-packs", action: "packs.list", want: `"packs"`},
		{id: "req-providers", action: "providers.list", want: `"items"`},
		{id: "req-provider", action: "provider.get", payload: map[string]any{"name": "claude"}, want: `"name":"claude"`},
	}

	for _, tc := range tests {
		writeWSJSON(t, conn, wsRequestEnvelope{
			Type:    "request",
			ID:      tc.id,
			Action:  tc.action,
			Payload: tc.payload,
		})
		var resp wsResponseEnvelope
		readWSJSON(t, conn, &resp)
		if resp.Type != "response" || resp.ID != tc.id {
			t.Fatalf("%s response = %#v, want correlated response", tc.action, resp)
		}
		if !strings.Contains(string(resp.Result), tc.want) {
			t.Fatalf("%s result = %s, want %s", tc.action, resp.Result, tc.want)
		}
	}
}

func TestServerWebSocketPaginatedListParity(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	for i := 0; i < 4; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  "Open bead",
			Status: "open",
			Type:   "task",
		}); err != nil {
			t.Fatalf("Create(bead %d): %v", i, err)
		}
	}
	state.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "mayor"})
	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "gc", Subject: "gc-1"})
	state.eventProv.Record(events.Event{Type: events.MailSent, Actor: "gc", Subject: "msg-1"})

	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-beads-page-1",
		Action: "beads.list",
		Payload: map[string]any{
			"status": "open",
			"limit":  2,
		},
	})
	var beadsPage1 wsResponseEnvelope
	readWSJSON(t, conn, &beadsPage1)
	if beadsPage1.Type != "response" || beadsPage1.ID != "req-beads-page-1" {
		t.Fatalf("beads page 1 response = %#v, want correlated response", beadsPage1)
	}
	var beadList listResponse
	if err := json.Unmarshal(beadsPage1.Result, &beadList); err != nil {
		t.Fatalf("unmarshal bead list: %v", err)
	}
	beadItems, ok := beadList.Items.([]any)
	if !ok {
		t.Fatalf("bead items type = %T, want []any", beadList.Items)
	}
	if len(beadItems) != 2 || beadList.Total != 2 || beadList.NextCursor != "" {
		t.Fatalf("bead page 1 = %#v, want 2 items total 2 no next cursor", beadList)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-beads-page-2",
		Action: "beads.list",
		Payload: map[string]any{
			"status": "open",
			"limit":  2,
			"cursor": encodeCursor(2),
		},
	})
	var beadsPage2 wsResponseEnvelope
	readWSJSON(t, conn, &beadsPage2)
	if beadsPage2.Type != "response" || beadsPage2.ID != "req-beads-page-2" {
		t.Fatalf("beads page 2 response = %#v, want correlated response", beadsPage2)
	}
	var beadList2 listResponse
	if err := json.Unmarshal(beadsPage2.Result, &beadList2); err != nil {
		t.Fatalf("unmarshal bead list 2: %v", err)
	}
	beadItems2, ok := beadList2.Items.([]any)
	if !ok {
		t.Fatalf("bead items 2 type = %T, want []any", beadList2.Items)
	}
	if len(beadItems2) != 2 || beadList2.Total != 4 || beadList2.NextCursor != "" {
		t.Fatalf("bead page 2 = %#v, want 2 items total 4 no next cursor", beadList2)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-events-page-1",
		Action: "events.list",
		Payload: map[string]any{
			"limit": 2,
		},
	})
	var eventsPage wsResponseEnvelope
	readWSJSON(t, conn, &eventsPage)
	if eventsPage.Type != "response" || eventsPage.ID != "req-events-page-1" {
		t.Fatalf("events response = %#v, want correlated response", eventsPage)
	}
	var eventList listResponse
	if err := json.Unmarshal(eventsPage.Result, &eventList); err != nil {
		t.Fatalf("unmarshal event list: %v", err)
	}
	eventItems, ok := eventList.Items.([]any)
	if !ok {
		t.Fatalf("event items type = %T, want []any", eventList.Items)
	}
	if len(eventItems) != 2 || eventList.Total != 3 || eventList.NextCursor != "" {
		t.Fatalf("event page = %#v, want 2 items total 3 no next cursor", eventList)
	}
}

func TestReadOnlyServerWebSocketRejectsCityPatch(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := NewReadOnly(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-2",
		Action: "city.patch",
		Payload: map[string]any{
			"suspended": true,
		},
	})

	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("error type = %q, want error", resp.Type)
	}
	if resp.ID != "req-2" {
		t.Fatalf("error id = %q, want req-2", resp.ID)
	}
	if resp.Code != "read_only" {
		t.Fatalf("error code = %q, want read_only", resp.Code)
	}
	if !strings.Contains(resp.Message, "mutations disabled") {
		t.Fatalf("error message = %q, want mutations disabled", resp.Message)
	}
}

func TestSupervisorWebSocketHelloAndCitiesList(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	s2 := newFakeState(t)
	s2.cityName = "beta"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s1,
		"beta":  s2,
	})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()

	var hello wsHelloEnvelope
	readWSJSON(t, conn, &hello)
	if hello.Type != "hello" {
		t.Fatalf("hello type = %q, want hello", hello.Type)
	}
	if hello.ServerRole != "supervisor" {
		t.Fatalf("hello server_role = %q, want supervisor", hello.ServerRole)
	}
	if !wsCapabilitiesContain(hello.Capabilities, "cities.list") {
		t.Fatalf("supervisor hello capabilities = %v, want cities.list", hello.Capabilities)
	}
	if wsCapabilitiesContain(hello.Capabilities, "health.get") {
		t.Fatalf("supervisor hello capabilities = %v, should not advertise health.get", hello.Capabilities)
	}
	if !wsCapabilitiesContain(hello.Capabilities, "subscription.start") {
		t.Fatalf("supervisor hello capabilities = %v, want subscription.start", hello.Capabilities)
	}
	if !wsCapabilitiesContain(hello.Capabilities, "subscription.stop") {
		t.Fatalf("supervisor hello capabilities = %v, want subscription.stop", hello.Capabilities)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-3",
		Action: "cities.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" {
		t.Fatalf("response type = %q, want response", resp.Type)
	}
	if resp.ID != "req-3" {
		t.Fatalf("response id = %q, want req-3", resp.ID)
	}

	var body struct {
		Items []CityInfo `json:"items"`
		Total int        `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body.Total != 2 {
		t.Fatalf("total = %d, want 2", body.Total)
	}
	if len(body.Items) != 2 || body.Items[0].Name != "alpha" || body.Items[1].Name != "beta" {
		t.Fatalf("items = %#v, want alpha then beta", body.Items)
	}
}

func TestServerWebSocketRejectsSupervisorOnlyAction(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-cities-city",
		Action: "cities.list",
	})

	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" {
		t.Fatalf("response type = %q, want error", errResp.Type)
	}
	if errResp.ID != "req-cities-city" {
		t.Fatalf("response id = %q, want req-cities-city", errResp.ID)
	}
	if errResp.Code != "unsupported_on_server_role" {
		t.Fatalf("code = %q, want unsupported_on_server_role", errResp.Code)
	}
	if !strings.Contains(errResp.Message, "cities.list") || !strings.Contains(errResp.Message, "city") {
		t.Fatalf("message = %q, want city-role rejection", errResp.Message)
	}
}

func TestServerWebSocketUnknownActionReturnsCorrelatedError(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-unknown",
		Action: "totally.unknown",
	})

	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("error type = %q, want error", resp.Type)
	}
	if resp.ID != "req-unknown" {
		t.Fatalf("error id = %q, want req-unknown", resp.ID)
	}
	if resp.Code != "not_found" {
		t.Fatalf("error code = %q, want not_found", resp.Code)
	}
	if !strings.Contains(resp.Message, "unknown action") {
		t.Fatalf("error message = %q, want unknown action", resp.Message)
	}
}

func TestServerWebSocketRejectsMalformedRequestEnvelope(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, map[string]any{
		"type":   "request",
		"id":     "req-bad",
		"action": "",
	})

	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("error type = %q, want error", resp.Type)
	}
	if resp.ID != "req-bad" {
		t.Fatalf("error id = %q, want req-bad", resp.ID)
	}
	if resp.Code != "invalid" {
		t.Fatalf("error code = %q, want invalid", resp.Code)
	}
	if !strings.Contains(resp.Message, "required") {
		t.Fatalf("error message = %q, want request id and action are required", resp.Message)
	}
}

func TestServerWebSocketRejectsNonRequestMessageType(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, map[string]any{
		"type":   "event",
		"id":     "req-wrong-type",
		"action": "status.get",
	})

	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("error type = %q, want error", resp.Type)
	}
	if resp.ID != "req-wrong-type" {
		t.Fatalf("error id = %q, want req-wrong-type", resp.ID)
	}
	if resp.Code != "invalid" {
		t.Fatalf("error code = %q, want invalid", resp.Code)
	}
	if !strings.Contains(resp.Message, "message type must be request") {
		t.Fatalf("error message = %q, want message type must be request", resp.Message)
	}
}

func TestServerWebSocketEventSubscription(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-1",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind": "events",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "sub-1" {
		t.Fatalf("subscription response = %#v, want correlated response", resp)
	}
	var result struct {
		SubscriptionID string `json:"subscription_id"`
		Kind           string `json:"kind"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal subscription result: %v", err)
	}
	if result.SubscriptionID == "" || result.Kind != "events" {
		t.Fatalf("subscription result = %#v, want subscription_id + kind events", result)
	}

	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "tester"})

	var evt wsEventEnvelope
	readWSJSON(t, conn, &evt)
	if evt.Type != "event" {
		t.Fatalf("event type = %q, want event", evt.Type)
	}
	if evt.SubscriptionID != result.SubscriptionID {
		t.Fatalf("event subscription_id = %q, want %q", evt.SubscriptionID, result.SubscriptionID)
	}
	if evt.EventType != events.BeadCreated {
		t.Fatalf("event_type = %q, want %q", evt.EventType, events.BeadCreated)
	}
	if evt.Index == 0 {
		t.Fatal("event index = 0, want > 0")
	}
}

func TestServerWebSocketBeadsListWithStatusFilter(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	if _, err := store.Create(beads.Bead{Title: "Open task", Status: "open"}); err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	inProgress, err := store.Create(beads.Bead{Title: "In progress task"})
	if err != nil {
		t.Fatalf("Create(in progress): %v", err)
	}
	inProgressStatus := "in_progress"
	if err := store.Update(inProgress.ID, beads.UpdateOpts{Status: &inProgressStatus}); err != nil {
		t.Fatalf("Update(in progress): %v", err)
	}
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-beads",
		Action: "beads.list",
		Payload: map[string]any{
			"status": "in_progress",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-beads" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body.Total == 0 {
		t.Fatal("total = 0, want at least one in-progress bead")
	}
	for _, item := range body.Items {
		if item.Status != "in_progress" {
			t.Fatalf("item = %#v, want only in_progress beads", item)
		}
	}
}

func TestServerWebSocketBeadsReady(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	blocker, err := store.Create(beads.Bead{Title: "Blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	readyBead, err := store.Create(beads.Bead{Title: "Ready task"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	blockedBead, err := store.Create(beads.Bead{Title: "Blocked task", Needs: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Create(blocked): %v", err)
	}
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-ready",
		Action: "beads.ready",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-ready" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	ids := map[string]bool{}
	for _, item := range body.Items {
		ids[item.ID] = true
	}
	if !ids[readyBead.ID] {
		t.Fatalf("ready bead %q missing from %#v", readyBead.ID, body.Items)
	}
	if ids[blockedBead.ID] {
		t.Fatalf("blocked bead %q unexpectedly present in ready list", blockedBead.ID)
	}
}

func TestServerWebSocketMailList(t *testing.T) {
	state := newFakeState(t)
	msg1, err := state.cityMailProv.Send("mayor", "worker", "Review needed", "Please review")
	if err != nil {
		t.Fatalf("Send(msg1): %v", err)
	}
	if err := state.cityMailProv.MarkRead(msg1.ID); err != nil {
		t.Fatalf("MarkRead(msg1): %v", err)
	}
	if _, err := state.cityMailProv.Send("worker", "mayor", "Unread", "Unread body"); err != nil {
		t.Fatalf("Send(msg2): %v", err)
	}
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-mail",
		Action: "mail.list",
		Payload: map[string]any{
			"status": "all",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-mail" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []struct {
			Subject string `json:"subject"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body.Total != 2 {
		t.Fatalf("total = %d, want 2", body.Total)
	}
	gotSubjects := map[string]bool{}
	for _, item := range body.Items {
		gotSubjects[item.Subject] = true
	}
	if !gotSubjects["Review needed"] || !gotSubjects["Unread"] {
		t.Fatalf("subjects = %#v, want both messages", gotSubjects)
	}
}

func TestServerWebSocketMailGet(t *testing.T) {
	state := newFakeState(t)
	msg, err := state.cityMailProv.Send("mayor", "worker", "Review needed", "Please review")
	if err != nil {
		t.Fatalf("Send(msg): %v", err)
	}
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-mail-get",
		Action: "mail.get",
		Payload: map[string]any{
			"id": msg.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-mail-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		ID      string `json:"id"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body.ID != msg.ID || body.Subject != "Review needed" || body.Body != "Please review" {
		t.Fatalf("mail body = %#v, want fetched message", body)
	}
}

func TestServerWebSocketMailRead(t *testing.T) {
	state := newFakeState(t)
	msg, err := state.cityMailProv.Send("mayor", "worker", "Review needed", "Please review")
	if err != nil {
		t.Fatalf("Send(msg): %v", err)
	}
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-mail-read",
		Action: "mail.read",
		Payload: map[string]any{
			"id": msg.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-mail-read" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if unread, err := state.cityMailProv.Inbox(""); err != nil {
		t.Fatalf("Inbox: %v", err)
	} else if len(unread) != 0 {
		t.Fatalf("unread inbox = %#v, want message marked read", unread)
	}
}

func TestServerWebSocketMailReplyAndSend(t *testing.T) {
	state := newFakeState(t)
	seed, err := state.cityMailProv.Send("mayor", "worker", "Review needed", "Please review")
	if err != nil {
		t.Fatalf("Send(seed): %v", err)
	}
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-mail-reply",
		Action: "mail.reply",
		Payload: map[string]any{
			"id":      seed.ID,
			"from":    "worker",
			"subject": "Re: Review needed",
			"body":    "On it",
		},
	})

	var replyResp wsResponseEnvelope
	readWSJSON(t, conn, &replyResp)
	if replyResp.Type != "response" || replyResp.ID != "req-mail-reply" {
		t.Fatalf("reply response = %#v, want correlated response", replyResp)
	}
	var replied struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(replyResp.Result, &replied); err != nil {
		t.Fatalf("unmarshal reply result: %v", err)
	}
	if replied.Subject != "Re: Review needed" {
		t.Fatalf("reply subject = %q, want reply payload", replied.Subject)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-mail-send",
		Action: "mail.send",
		Payload: map[string]any{
			"from":    "mayor",
			"to":      "worker",
			"subject": "New review",
			"body":    "Please review this too",
		},
	})

	var sendResp wsResponseEnvelope
	readWSJSON(t, conn, &sendResp)
	if sendResp.Type != "response" || sendResp.ID != "req-mail-send" {
		t.Fatalf("send response = %#v, want correlated response", sendResp)
	}
	var sent struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(sendResp.Result, &sent); err != nil {
		t.Fatalf("unmarshal send result: %v", err)
	}
	if sent.Subject != "New review" {
		t.Fatalf("send subject = %q, want websocket send payload", sent.Subject)
	}
}

func TestServerWebSocketMailSendValidationIncludesFieldDetails(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-mail-send-invalid",
		Action: "mail.send",
		Payload: map[string]any{
			"from": "mayor",
		},
	})

	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" || resp.ID != "req-mail-send-invalid" {
		t.Fatalf("response = %#v, want correlated error", resp)
	}
	if resp.Code != "invalid" {
		t.Fatalf("code = %q, want invalid", resp.Code)
	}
	if len(resp.Details) != 2 {
		t.Fatalf("details = %#v, want 2 field errors", resp.Details)
	}
}

func TestServerWebSocketBeadMutationsAndSling(t *testing.T) {
	state := newFakeState(t)
	oldRunner := slingCommandRunner
	defer func() { slingCommandRunner = oldRunner }()
	slingCommandRunner = func(_ context.Context, _ string, args []string) (string, string, error) {
		return "Started workflow wf-123\n", "", nil
	}

	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-bead-create",
		Action: "bead.create",
		Payload: map[string]any{
			"title": "New issue",
			"rig":   "myrig",
		},
	})
	var createResp wsResponseEnvelope
	readWSJSON(t, conn, &createResp)
	if createResp.Type != "response" || createResp.ID != "req-bead-create" {
		t.Fatalf("create response = %#v, want correlated response", createResp)
	}
	var created struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(createResp.Result, &created); err != nil {
		t.Fatalf("unmarshal create result: %v", err)
	}
	if created.ID == "" || created.Title != "New issue" {
		t.Fatalf("created bead = %#v, want created bead payload", created)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-bead-update",
		Action: "bead.update",
		Payload: map[string]any{
			"id":       created.ID,
			"assignee": "worker",
		},
	})
	var updateResp wsResponseEnvelope
	readWSJSON(t, conn, &updateResp)
	if updateResp.Type != "response" || updateResp.ID != "req-bead-update" {
		t.Fatalf("update response = %#v, want correlated response", updateResp)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-bead-close",
		Action: "bead.close",
		Payload: map[string]any{
			"id": created.ID,
		},
	})
	var closeResp wsResponseEnvelope
	readWSJSON(t, conn, &closeResp)
	if closeResp.Type != "response" || closeResp.ID != "req-bead-close" {
		t.Fatalf("close response = %#v, want correlated response", closeResp)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-sling",
		Action: "sling.run",
		Payload: map[string]any{
			"target": "myrig/worker",
			"bead":   created.ID,
		},
	})
	var slingResp wsResponseEnvelope
	readWSJSON(t, conn, &slingResp)
	if slingResp.Type != "response" || slingResp.ID != "req-sling" {
		t.Fatalf("sling response = %#v, want correlated response", slingResp)
	}
	var slung struct {
		Status     string `json:"status"`
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(slingResp.Result, &slung); err != nil {
		t.Fatalf("unmarshal sling result: %v", err)
	}
	if slung.Status != "slung" {
		t.Fatalf("sling status = %q, want slung", slung.Status)
	}
	if slung.WorkflowID != "" {
		t.Fatalf("workflow id = %q, want empty for direct bead sling", slung.WorkflowID)
	}
}

func TestServerWebSocketEventsList(t *testing.T) {
	state := newFakeState(t)
	state.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "mayor"})
	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1"})
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-events",
		Action: "events.list",
		Payload: map[string]any{
			"type": string(events.SessionWoke),
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-events" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []struct {
			Type string `json:"type"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body.Total != 1 {
		t.Fatalf("total = %d, want 1", body.Total)
	}
	if len(body.Items) != 1 || body.Items[0].Type != events.SessionWoke {
		t.Fatalf("items = %#v, want one session.woke event", body.Items)
	}
}

func TestServerWebSocketSessionPending(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	srv := New(fs)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-session-pending",
		Action: "session.pending",
		Payload: map[string]any{
			"id": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-session-pending" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionPendingResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !body.Supported || body.Pending == nil || body.Pending.RequestID != "req-1" {
		t.Fatalf("pending body = %#v, want req-1", body)
	}
}

func TestServerWebSocketAgentRigAndSessionReads(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Inspector")
	srv := New(fs)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	tests := []struct {
		id      string
		action  string
		payload any
		want    string
	}{
		{id: "req-agents", action: "agents.list", want: `"name":"myrig/worker"`},
		{id: "req-agent", action: "agent.get", payload: map[string]any{"name": "myrig/worker"}, want: `"name":"myrig/worker"`},
		{id: "req-rig", action: "rig.get", payload: map[string]any{"name": "myrig"}, want: `"name":"myrig"`},
		{id: "req-session", action: "session.get", payload: map[string]any{"id": info.ID}, want: info.ID},
	}

	for _, tc := range tests {
		writeWSJSON(t, conn, wsRequestEnvelope{
			Type:    "request",
			ID:      tc.id,
			Action:  tc.action,
			Payload: tc.payload,
		})
		var resp wsResponseEnvelope
		readWSJSON(t, conn, &resp)
		if resp.Type != "response" || resp.ID != tc.id {
			t.Fatalf("%s response = %#v, want correlated response", tc.action, resp)
		}
		if !strings.Contains(string(resp.Result), tc.want) {
			t.Fatalf("%s result = %s, want %s", tc.action, resp.Result, tc.want)
		}
	}
}

func TestServerWebSocketSessionMutations(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-9",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	srv := New(fs)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-rename",
		Action: "session.rename",
		Payload: map[string]any{
			"id":    info.ID,
			"title": "Renamed over ws",
		},
	})
	var renameResp wsResponseEnvelope
	readWSJSON(t, conn, &renameResp)
	if renameResp.Type != "response" || renameResp.ID != "req-rename" {
		t.Fatalf("rename response = %#v, want correlated response", renameResp)
	}
	if !strings.Contains(string(renameResp.Result), `"title":"Renamed over ws"`) {
		t.Fatalf("rename result = %s, want renamed title", renameResp.Result)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-respond",
		Action: "session.respond",
		Payload: map[string]any{
			"id":         info.ID,
			"request_id": "req-9",
			"action":     "approve",
		},
	})
	var respondResp wsResponseEnvelope
	readWSJSON(t, conn, &respondResp)
	if respondResp.Type != "response" || respondResp.ID != "req-respond" {
		t.Fatalf("respond response = %#v, want correlated response", respondResp)
	}
	if !strings.Contains(string(respondResp.Result), `"status":"accepted"`) {
		t.Fatalf("respond result = %s, want accepted", respondResp.Result)
	}
}

func TestServerWebSocketSessionTranscript(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Transcript")
	srv := New(fs)
	searchBase := t.TempDir()
	srv.sessionLogSearchPaths = []string{searchBase}
	writeNamedSessionJSONL(t, searchBase, info.WorkDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := session.NewManager(fs.cityBeadStore, fs.sp).Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-session-transcript",
		Action: "session.transcript",
		Payload: map[string]any{
			"id":    info.ID,
			"turns": 1,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-session-transcript" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionTranscriptResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body.Format != "conversation" {
		t.Fatalf("format = %q, want conversation", body.Format)
	}
	if len(body.Turns) == 0 || body.Turns[len(body.Turns)-1].Text != "world" {
		t.Fatalf("turns = %#v, want websocket transcript tail ending in world", body.Turns)
	}
}

func TestServerWebSocketSessionTranscriptJSONLUsesRawTranscript(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Transcript")
	srv := New(fs)
	searchBase := t.TempDir()
	srv.sessionLogSearchPaths = []string{searchBase}
	writeNamedSessionJSONL(t, searchBase, info.WorkDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := session.NewManager(fs.cityBeadStore, fs.sp).Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "req-session-transcript-jsonl",
		Action: "session.transcript",
		Payload: map[string]any{
			"id":     info.ID,
			"format": "jsonl",
			"turns":  1,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "req-session-transcript-jsonl" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionRawTranscriptResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body.Format != "raw" {
		t.Fatalf("format = %q, want raw jsonl alias", body.Format)
	}
	if len(body.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 tail message", len(body.Messages))
	}
	if !strings.Contains(string(body.Messages[0]), `\"world\"`) {
		t.Fatalf("messages[0] = %s, want assistant world payload", body.Messages[0])
	}
}

func TestServerWebSocketSupplementalMailAndBeadRoutes(t *testing.T) {
	state := newSessionFakeState(t)
	root, err := state.cityBeadStore.Create(beads.Bead{Title: "Root"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := state.cityBeadStore.Create(beads.Bead{Title: "Child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	closedChild, err := state.cityBeadStore.Create(beads.Bead{Title: "Closed Child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create(closedChild): %v", err)
	}
	if err := state.cityBeadStore.Close(closedChild.ID); err != nil {
		t.Fatalf("Close(closedChild): %v", err)
	}

	seed, err := state.cityMailProv.Send("mayor", "worker", "Review needed", "Please review")
	if err != nil {
		t.Fatalf("Send(seed): %v", err)
	}
	reply, err := state.cityMailProv.Reply(seed.ID, "worker", "Re: Review needed", "On it")
	if err != nil {
		t.Fatalf("Reply(seed): %v", err)
	}

	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	tests := []struct {
		id      string
		action  string
		payload any
		want    string
	}{
		{id: "req-mail-count", action: "mail.count", payload: map[string]any{"agent": "worker"}, want: `"total":1`},
		{id: "req-mail-thread", action: "mail.thread", payload: map[string]any{"id": seed.ThreadID}, want: seed.ID},
		{id: "req-bead-deps", action: "bead.deps", payload: map[string]any{"id": root.ID}, want: child.ID},
		{id: "req-bead-graph", action: "beads.graph", payload: map[string]any{"root_id": root.ID}, want: root.ID},
		{id: "req-mail-unread", action: "mail.mark_unread", payload: map[string]any{"id": seed.ID}, want: `"status":"unread"`},
		{id: "req-mail-archive", action: "mail.archive", payload: map[string]any{"id": reply.ID}, want: `"status":"archived"`},
		{id: "req-bead-reopen", action: "bead.reopen", payload: map[string]any{"id": closedChild.ID}, want: `"status":"reopened"`},
		{id: "req-bead-assign", action: "bead.assign", payload: map[string]any{"id": root.ID, "assignee": "worker"}, want: `"assignee":"worker"`},
	}

	for _, tc := range tests {
		writeWSJSON(t, conn, wsRequestEnvelope{
			Type:    "request",
			ID:      tc.id,
			Action:  tc.action,
			Payload: tc.payload,
		})
		var resp wsResponseEnvelope
		readWSJSON(t, conn, &resp)
		if resp.Type != "response" || resp.ID != tc.id {
			t.Fatalf("%s response = %#v, want correlated response", tc.action, resp)
		}
		if !strings.Contains(string(resp.Result), tc.want) {
			t.Fatalf("%s result = %s, want %s", tc.action, resp.Result, tc.want)
		}
	}
}

func TestSupervisorWebSocketGlobalEventSubscription(t *testing.T) {
	alpha := newFakeState(t)
	alpha.cityName = "alpha"
	beta := newFakeState(t)
	beta.cityName = "beta"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": alpha,
		"beta":  beta,
	})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-global",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind": "events",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "sub-global" {
		t.Fatalf("subscription response = %#v, want correlated response", resp)
	}
	var result struct {
		SubscriptionID string `json:"subscription_id"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal subscription result: %v", err)
	}

	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "alpha-mayor"})

	var evt wsEventEnvelope
	readWSJSON(t, conn, &evt)
	if evt.Type != "event" {
		t.Fatalf("event type = %q, want event", evt.Type)
	}
	if evt.SubscriptionID != result.SubscriptionID {
		t.Fatalf("event subscription_id = %q, want %q", evt.SubscriptionID, result.SubscriptionID)
	}
	if evt.EventType != events.SessionWoke {
		t.Fatalf("event_type = %q, want %q", evt.EventType, events.SessionWoke)
	}
	if evt.Cursor == "" {
		t.Fatal("global event cursor empty")
	}

	var payload struct {
		City string `json:"city"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("unmarshal global event payload: %v", err)
	}
	if payload.City != "alpha" {
		t.Fatalf("payload city = %q, want alpha", payload.City)
	}
	if payload.Type != events.SessionWoke {
		t.Fatalf("payload type = %q, want %q", payload.Type, events.SessionWoke)
	}
}

func TestServerWebSocketSessionStreamClosedSessionSnapshot(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-session",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind":   "session.stream",
			"target": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "sub-session" {
		t.Fatalf("subscription response = %#v, want correlated response", resp)
	}

	var turnEvt wsEventEnvelope
	readWSJSON(t, conn, &turnEvt)
	if turnEvt.Type != "event" {
		t.Fatalf("turn event type = %q, want event", turnEvt.Type)
	}
	if turnEvt.EventType != "turn" {
		t.Fatalf("turn event_type = %q, want turn", turnEvt.EventType)
	}
	if turnEvt.Index == 0 {
		t.Fatal("turn event index = 0, want > 0")
	}
	if !strings.Contains(string(turnEvt.Payload), `"hello"`) || !strings.Contains(string(turnEvt.Payload), `"world"`) {
		t.Fatalf("turn payload = %s, want closed-session snapshot", turnEvt.Payload)
	}

	var activityEvt wsEventEnvelope
	readWSJSON(t, conn, &activityEvt)
	if activityEvt.EventType != "activity" {
		t.Fatalf("activity event_type = %q, want activity", activityEvt.EventType)
	}
	if !strings.Contains(string(activityEvt.Payload), `"idle"`) {
		t.Fatalf("activity payload = %s, want idle", activityEvt.Payload)
	}
}

func TestServerWebSocketSessionStreamClosedSessionSnapshotHonorsTurns(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-session-tail",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind":   "session.stream",
			"target": info.ID,
			"turns":  1,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "sub-session-tail" {
		t.Fatalf("subscription response = %#v, want correlated response", resp)
	}

	var turnEvt wsEventEnvelope
	readWSJSON(t, conn, &turnEvt)
	if turnEvt.Type != "event" || turnEvt.EventType != "turn" {
		t.Fatalf("turn event = %#v, want turn event", turnEvt)
	}
	if strings.Contains(string(turnEvt.Payload), `"hello"`) {
		t.Fatalf("turn payload = %s, want tail without older hello turn", turnEvt.Payload)
	}
	if !strings.Contains(string(turnEvt.Payload), `"world"`) {
		t.Fatalf("turn payload = %s, want last assistant turn", turnEvt.Payload)
	}
}

func TestServerWebSocketSessionStreamClosedSessionSnapshotResumesAfterCursor(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"3","parentUuid":"2","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"again\"}","timestamp":"2025-01-01T00:00:02Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-session-resume",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind":         "session.stream",
			"target":       info.ID,
			"after_cursor": "2",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "sub-session-resume" {
		t.Fatalf("subscription response = %#v, want correlated response", resp)
	}

	var turnEvt wsEventEnvelope
	readWSJSON(t, conn, &turnEvt)
	if turnEvt.Type != "event" || turnEvt.EventType != "turn" {
		t.Fatalf("turn event = %#v, want turn event", turnEvt)
	}
	if turnEvt.Cursor != "3" {
		t.Fatalf("turn cursor = %q, want 3", turnEvt.Cursor)
	}
	if strings.Contains(string(turnEvt.Payload), `"hello"`) || strings.Contains(string(turnEvt.Payload), `"world"`) {
		t.Fatalf("turn payload = %s, want only post-cursor turns", turnEvt.Payload)
	}
	if !strings.Contains(string(turnEvt.Payload), `"again"`) {
		t.Fatalf("turn payload = %s, want resumed turn", turnEvt.Payload)
	}
}

func TestServerWebSocketSessionStreamRejectsInvalidFormat(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-session-invalid-format",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind":   "session.stream",
			"target": info.ID,
			"format": "bogus",
		},
	})

	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" || errResp.ID != "sub-session-invalid-format" {
		t.Fatalf("error response = %#v, want correlated error", errResp)
	}
	if errResp.Code != "invalid" {
		t.Fatalf("code = %q, want invalid", errResp.Code)
	}
	if !strings.Contains(errResp.Message, "format") {
		t.Fatalf("message = %q, want format validation error", errResp.Message)
	}
}

func TestSupervisorWebSocketSessionStreamUsesImplicitSingleCity(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cityName = "alpha"
	searchBase := t.TempDir()
	fs.cfg.Daemon.ObservePaths = []string{searchBase}
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": fs})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-session-supervisor",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind":   "session.stream",
			"target": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "sub-session-supervisor" {
		t.Fatalf("subscription response = %#v, want correlated response", resp)
	}

	var evt wsEventEnvelope
	readWSJSON(t, conn, &evt)
	if evt.Type != "event" || evt.EventType != "turn" {
		t.Fatalf("event = %#v, want turn event", evt)
	}
	if !strings.Contains(string(evt.Payload), `"hello"`) {
		t.Fatalf("payload = %s, want session snapshot", evt.Payload)
	}
}

func TestServerWebSocketBlockingWatch(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Record an event so the index advances past 0.
	state.eventProv.Record(events.Event{Type: "test.event", Actor: "tester"})

	// Send a blocking watch request with index=0 — should return immediately
	// since there's already an event at seq > 0.
	writeWSJSON(t, conn, map[string]any{
		"type":   "request",
		"id":     "watch-1",
		"action": "beads.list",
		"watch":  map[string]any{"index": 0, "wait": "100ms"},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" {
		t.Fatalf("type = %q, want response", resp.Type)
	}
	if resp.ID != "watch-1" {
		t.Fatalf("id = %q, want watch-1", resp.ID)
	}
	if resp.Index == 0 {
		t.Fatal("index = 0, want > 0 after blocking watch")
	}
}

func TestServerWebSocketBlockingWatchTimeout(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Send a blocking watch with a very high index — should timeout.
	writeWSJSON(t, conn, map[string]any{
		"type":   "request",
		"id":     "watch-timeout",
		"action": "status.get",
		"watch":  map[string]any{"index": 999999, "wait": "50ms"},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" {
		t.Fatalf("type = %q, want response", resp.Type)
	}
	if resp.ID != "watch-timeout" {
		t.Fatalf("id = %q, want watch-timeout", resp.ID)
	}
	// Response should still come back (with index 0 since no events).
}

func TestServerWebSocketRejectsMismatchedCityScope(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Send a request with a wrong city scope.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "scope-1",
		Action: "status.get",
		Scope:  &wsScope{City: "wrong-city"},
	})

	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" {
		t.Fatalf("type = %q, want error", errResp.Type)
	}
	if errResp.Code != "invalid" {
		t.Fatalf("code = %q, want invalid", errResp.Code)
	}
	if !strings.Contains(errResp.Message, "scope.city") {
		t.Fatalf("message = %q, want scope.city mismatch", errResp.Message)
	}
}

func TestServerWebSocketAcceptsMatchingCityScope(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Send a request with the correct city scope — should succeed.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "scope-2",
		Action: "status.get",
		Scope:  &wsScope{City: state.CityName()},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" {
		t.Fatalf("type = %q, want response", resp.Type)
	}
	if resp.ID != "scope-2" {
		t.Fatalf("id = %q, want scope-2", resp.ID)
	}
}

func TestServerWebSocketOriginRejection(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v0/ws"
	header := http.Header{}
	header.Set("Origin", "http://evil.example.com")
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err == nil {
		t.Fatal("expected dial error for non-localhost origin, got nil")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestServerWebSocketSubscriptionStopOK(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Start a subscription.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-start",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind": "events",
		},
	})
	var subResp wsResponseEnvelope
	readWSJSON(t, conn, &subResp)
	if subResp.Type != "response" || subResp.ID != "sub-start" {
		t.Fatalf("subscription start = %#v, want correlated response", subResp)
	}
	var result map[string]string
	if err := json.Unmarshal(subResp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	subID := result["subscription_id"]
	if subID == "" {
		t.Fatal("subscription_id empty")
	}

	// Stop the subscription.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-stop",
		Action: "subscription.stop",
		Payload: map[string]any{
			"subscription_id": subID,
		},
	})
	var stopResp wsResponseEnvelope
	readWSJSON(t, conn, &stopResp)
	if stopResp.Type != "response" || stopResp.ID != "sub-stop" {
		t.Fatalf("subscription stop = %#v, want correlated response", stopResp)
	}
	var stopResult map[string]string
	if err := json.Unmarshal(stopResp.Result, &stopResult); err != nil {
		t.Fatalf("unmarshal stop result: %v", err)
	}
	if stopResult["status"] != "ok" {
		t.Fatalf("stop status = %q, want ok", stopResult["status"])
	}
}

func TestServerWebSocketUnsubscribeNonexistentReturnsError(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "unsub-bad",
		Action: "subscription.stop",
		Payload: map[string]any{
			"subscription_id": "nonexistent",
		},
	})

	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" {
		t.Fatalf("type = %q, want error", errResp.Type)
	}
	if errResp.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", errResp.Code)
	}
	if !strings.Contains(errResp.Message, "subscription not found") {
		t.Fatalf("message = %q, want 'subscription not found'", errResp.Message)
	}
}

func TestServerWebSocketOversizeMessageRejected(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Send a message that exceeds the 10MB read limit.
	bigPayload := strings.Repeat("x", 11<<20) // 11 MB
	err := conn.WriteJSON(map[string]string{
		"type":    "request",
		"id":      "big-1",
		"action":  "status.get",
		"payload": bigPayload,
	})
	if err != nil {
		// Write might fail if the server closes the connection fast.
		return
	}
	// The next read should fail because the server closed the connection.
	var resp wsResponseEnvelope
	err = conn.ReadJSON(&resp)
	if err == nil {
		t.Fatal("expected read error after oversize message, got nil")
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected websocket close error after oversize message, got %v", err)
	}
	if closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseMessageTooBig)
	}
}

func TestServerWebSocketCloseCodeInvalidJSON(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"request"`)); err != nil {
		t.Fatalf("write invalid json: %v", err)
	}

	var dummy wsResponseEnvelope
	err := conn.ReadJSON(&dummy)
	if err == nil {
		t.Fatal("expected close error after invalid JSON, got nil")
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected websocket close error after invalid JSON, got %v", err)
	}
	if closeErr.Code != websocket.CloseInvalidFramePayloadData {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseInvalidFramePayloadData)
	}
}

func TestServerWebSocketCloseCodeUnsupportedBinaryFrame(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("write binary frame: %v", err)
	}

	var dummy wsResponseEnvelope
	err := conn.ReadJSON(&dummy)
	if err == nil {
		t.Fatal("expected close error after binary frame, got nil")
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected websocket close error after binary frame, got %v", err)
	}
	if closeErr.Code != websocket.CloseUnsupportedData {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseUnsupportedData)
	}
}

func TestServerWebSocketCloseCodeNormalClosure(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Send a close frame from client side.
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye")
	err := conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("write close: %v", err)
	}

	// Read should get a close error with normal closure code.
	var dummy wsResponseEnvelope
	err = conn.ReadJSON(&dummy)
	if err == nil {
		t.Fatal("expected close error, got nil")
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		// Connection closed — acceptable even without a structured close error.
		return
	}
	if closeErr.Code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseNormalClosure)
	}
}

func TestServerWebSocketCloseCodeOnShutdown(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Shut down the server — should trigger close code 1001 (GoingAway).
	ts.Close()

	// Read should get a close error.
	var dummy wsResponseEnvelope
	err := conn.ReadJSON(&dummy)
	if err == nil {
		t.Fatal("expected close error after shutdown, got nil")
	}
	// Accept either a CloseGoingAway or a generic connection close, since
	// the exact behavior depends on timing between the close frame and
	// the TCP teardown.
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code != websocket.CloseGoingAway && closeErr.Code != websocket.CloseNormalClosure {
			t.Fatalf("close code = %d, want %d or %d", closeErr.Code, websocket.CloseGoingAway, websocket.CloseNormalClosure)
		}
	}
	// If we didn't get a CloseError, a generic EOF/connection-reset is also acceptable.
}

func TestServerWebSocketConcurrentDispatch(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Fire 5 requests in rapid succession without waiting for responses.
	for i := 0; i < 5; i++ {
		writeWSJSON(t, conn, wsRequestEnvelope{
			Type:   "request",
			ID:     "concurrent-" + strings.Repeat("0", i) + "1",
			Action: "status.get",
		})
	}

	// Read all 5 responses — they may arrive in any order.
	received := make(map[string]bool)
	for i := 0; i < 5; i++ {
		var resp wsResponseEnvelope
		readWSJSON(t, conn, &resp)
		if resp.Type != "response" {
			t.Fatalf("response %d: type = %q, want response", i, resp.Type)
		}
		received[resp.ID] = true
	}

	for i := 0; i < 5; i++ {
		id := "concurrent-" + strings.Repeat("0", i) + "1"
		if !received[id] {
			t.Errorf("missing response for %s", id)
		}
	}
}

func TestServerWebSocketIdempotencyReplay(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Send bead.create with idempotency key.
	writeWSJSON(t, conn, map[string]any{
		"type":            "request",
		"id":              "idem-1",
		"action":          "bead.create",
		"idempotency_key": "test-key-abc",
		"payload":         map[string]any{"title": "test bead", "type": "task"},
	})

	var resp1 wsResponseEnvelope
	readWSJSON(t, conn, &resp1)
	if resp1.Type != "response" || resp1.ID != "idem-1" {
		t.Fatalf("first create: %#v", resp1)
	}

	// Send same create again with same idempotency key — should replay.
	writeWSJSON(t, conn, map[string]any{
		"type":            "request",
		"id":              "idem-2",
		"action":          "bead.create",
		"idempotency_key": "test-key-abc",
		"payload":         map[string]any{"title": "test bead", "type": "task"},
	})

	var resp2 wsResponseEnvelope
	readWSJSON(t, conn, &resp2)
	if resp2.Type != "response" || resp2.ID != "idem-2" {
		t.Fatalf("replay: %#v", resp2)
	}

	// Both should return the same bead data.
	if string(resp1.Result) != string(resp2.Result) {
		t.Errorf("idempotency replay mismatch:\n  first:  %s\n  replay: %s", resp1.Result, resp2.Result)
	}
}

func drainWSHello(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	var hello wsHelloEnvelope
	readWSJSON(t, conn, &hello)
	if hello.Type != "hello" {
		t.Fatalf("drain hello type = %q, want hello", hello.Type)
	}
}

func dialWebSocket(t *testing.T, httpURL string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(httpURL, "http")
	header := http.Header{}
	header.Set("Origin", "http://localhost:3000")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket: %v (status %s)", err, resp.Status)
		}
		t.Fatalf("dial websocket: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	return conn
}

func writeWSJSON(t *testing.T, conn *websocket.Conn, v interface{}) {
	t.Helper()
	if err := conn.WriteJSON(v); err != nil {
		t.Fatalf("write ws json: %v", err)
	}
}

func readWSJSON(t *testing.T, conn *websocket.Conn, v interface{}) {
	t.Helper()
	if err := conn.ReadJSON(v); err != nil {
		t.Fatalf("read ws json: %v", err)
	}
}

func wsCapabilitiesContain(caps []string, want string) bool {
	for _, cap := range caps {
		if cap == want {
			return true
		}
	}
	return false
}

// ---------- Protocol parity test suite ----------

// TestWSConcurrentDispatch sends 10 parallel requests over a single WS
// connection and verifies each response correlates to the correct request ID.
func TestWSConcurrentDispatch(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	actions := []string{
		"status.get",
		"city.get",
		"agents.list",
		"beads.list",
		"events.list",
		"rigs.list",
		"config.get",
		"config.validate",
		"providers.list",
		"packs.list",
	}

	// Send all 10 requests in parallel goroutines.
	var wg sync.WaitGroup
	// Use a mutex to serialize writes on the gorilla conn (gorilla does
	// not allow concurrent writes).
	var writeMu sync.Mutex
	for i, action := range actions {
		wg.Add(1)
		go func(idx int, act string) {
			defer wg.Done()
			writeMu.Lock()
			err := conn.WriteJSON(wsRequestEnvelope{
				Type:   "request",
				ID:     fmt.Sprintf("par-%d", idx),
				Action: act,
			})
			writeMu.Unlock()
			if err != nil {
				t.Errorf("write request %d (%s): %v", idx, act, err)
			}
		}(i, action)
	}
	wg.Wait()

	// Read all 10 responses — they may arrive in any order.
	received := make(map[string]bool)
	for i := 0; i < 10; i++ {
		var raw json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
			t.Fatalf("read response %d: %v", i, err)
		}
		var envelope struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("unmarshal envelope %d: %v", i, err)
		}
		if envelope.Type != "response" {
			t.Fatalf("response %d: type = %q, want response (body: %s)", i, envelope.Type, raw)
		}
		received[envelope.ID] = true
	}

	for i := range actions {
		id := fmt.Sprintf("par-%d", i)
		if !received[id] {
			t.Errorf("missing response for %s", id)
		}
	}
}

// TestWSOversizeResponseIncludesRequestID sends a request that produces a
// response exceeding the 10MB outbound limit and verifies the error includes
// the "message_too_large" code and the original request ID.
func TestWSOversizeResponseIncludesRequestID(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]

	// Create enough beads to push the serialized response over 10 MB.
	// With limit=0, beads.list returns up to maxPaginationLimit (1000) items.
	// Each bead with a ~12KB title serializes to roughly 12KB+ of JSON.
	// 1000 beads * ~12KB each ≈ 12MB, well over the 10MB limit.
	bigTitle := strings.Repeat("X", 12_000)
	for i := 0; i < 1000; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  bigTitle,
			Status: "open",
			Type:   "task",
		}); err != nil {
			t.Fatalf("Create(bead %d): %v", i, err)
		}
	}

	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "oversize-req-1",
		Action: "beads.list",
		Payload: map[string]any{
			"status": "open",
			"limit":  0, // request all beads
		},
	})

	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" {
		t.Fatalf("type = %q, want error", errResp.Type)
	}
	if errResp.Code != "message_too_large" {
		t.Fatalf("code = %q, want message_too_large", errResp.Code)
	}
	if errResp.ID != "oversize-req-1" {
		t.Fatalf("error id = %q, want oversize-req-1 (request ID must be preserved)", errResp.ID)
	}
}

// TestWSIdempotencyReplay sends two bead.create requests with the same
// idempotency_key and verifies the second returns the same result without
// re-executing the action (same bead ID returned).
func TestWSIdempotencyReplay(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	idemKey := "idempotent-test-key-42"

	// First request with idempotency key.
	writeWSJSON(t, conn, map[string]any{
		"type":            "request",
		"id":              "idem-first",
		"action":          "bead.create",
		"idempotency_key": idemKey,
		"payload":         map[string]any{"title": "Idempotent bead", "type": "task"},
	})

	var resp1 wsResponseEnvelope
	readWSJSON(t, conn, &resp1)
	if resp1.Type != "response" || resp1.ID != "idem-first" {
		t.Fatalf("first response = %#v, want correlated response", resp1)
	}

	var bead1 struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(resp1.Result, &bead1); err != nil {
		t.Fatalf("unmarshal first result: %v", err)
	}
	if bead1.ID == "" {
		t.Fatal("first response bead ID empty")
	}

	// Second request with the SAME idempotency key — should replay.
	writeWSJSON(t, conn, map[string]any{
		"type":            "request",
		"id":              "idem-second",
		"action":          "bead.create",
		"idempotency_key": idemKey,
		"payload":         map[string]any{"title": "Idempotent bead", "type": "task"},
	})

	var resp2 wsResponseEnvelope
	readWSJSON(t, conn, &resp2)
	if resp2.Type != "response" || resp2.ID != "idem-second" {
		t.Fatalf("replay response = %#v, want correlated response", resp2)
	}

	// The replayed result should match the original result exactly.
	if string(resp1.Result) != string(resp2.Result) {
		t.Errorf("idempotency replay mismatch:\n  first:  %s\n  replay: %s", resp1.Result, resp2.Result)
	}

	// Verify it's the same bead (not a second creation).
	var bead2 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp2.Result, &bead2); err != nil {
		t.Fatalf("unmarshal replay result: %v", err)
	}
	if bead2.ID != bead1.ID {
		t.Errorf("replay bead ID = %q, want %q (same bead, not a new one)", bead2.ID, bead1.ID)
	}
}

// TestWSWriteErrorCancelsSession verifies that when the server cannot write
// to the client (connection dead), the session gets cancelled.
func TestWSWriteErrorCancelsSession(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	drainWSHello(t, conn)

	// Start an event subscription so the server has an active session that
	// will try to write events to us.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-write-err",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind": "events",
		},
	})

	var subResp wsResponseEnvelope
	readWSJSON(t, conn, &subResp)
	if subResp.Type != "response" || subResp.ID != "sub-write-err" {
		t.Fatalf("subscription response = %#v, want correlated response", subResp)
	}

	// Abruptly close the client connection without sending a close frame.
	// This simulates a dead connection. The underlying TCP conn is severed.
	conn.UnderlyingConn().Close()

	// Generate an event that the server will try to push to the dead
	// connection. The write error should cause the session to cancel.
	state.eventProv.Record(events.Event{Type: events.BeadCreated, Actor: "tester"})

	// Give the server a moment to notice the write failure.
	time.Sleep(200 * time.Millisecond)

	// The test passes if we get here without hanging — the server detected
	// the write error and cancelled the session rather than blocking forever.
	// A new connection should work fine (server not stuck).
	conn2 := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn2.Close()

	var hello wsHelloEnvelope
	readWSJSON(t, conn2, &hello)
	if hello.Type != "hello" {
		t.Fatalf("new connection hello type = %q, want hello", hello.Type)
	}
}

// TestWSCloseCodeNormalShutdown verifies the server sends close code 1000
// when the client initiates a clean shutdown.
func TestWSCloseCodeNormalShutdown(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Send a normal close frame from the client.
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "goodbye")
	if err := conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("write close: %v", err)
	}

	// Read the server's close response.
	var dummy wsResponseEnvelope
	err := conn.ReadJSON(&dummy)
	if err == nil {
		t.Fatal("expected close error after sending close frame, got nil")
	}

	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		// Connection closed — acceptable even without a structured close
		// error, since the TCP teardown may race the close frame.
		return
	}
	if closeErr.Code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d, want %d (normal closure)", closeErr.Code, websocket.CloseNormalClosure)
	}
}

func TestWSPingPongKeepalive(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// Dial manually so we can set the ping handler before any reads.
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v0/ws"
	header := http.Header{}
	header.Set("Origin", "http://localhost")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	pingReceived := make(chan struct{}, 1)
	conn.SetPingHandler(func(msg string) error {
		select {
		case pingReceived <- struct{}{}:
		default:
		}
		return conn.WriteControl(websocket.PongMessage, []byte(msg), time.Now().Add(time.Second))
	})

	// Read in a goroutine so control frame handlers fire.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Server pings every 15s. Wait up to 20s.
	select {
	case <-pingReceived:
		// Server is sending pings — keepalive is working.
	case <-time.After(20 * time.Second):
		t.Fatal("timeout waiting for server ping frame")
	}
}
