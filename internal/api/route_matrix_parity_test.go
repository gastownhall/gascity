package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workspacesvc"
	"github.com/gorilla/websocket"
)

// Route-matrix parity tests use the former HTTP/SSE route names in the test
// names and exercise the canonical WS replacements described in #646.

func TestRouteMatrixParity_GET_v0_status_ViaWS(t *testing.T) {
	state := newFakeState(t)
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-status-get",
		Action: "status.get",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-status-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body statusResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if body.Name != "test-city" {
		t.Fatalf("status name = %q, want test-city", body.Name)
	}
	if body.AgentCount != 1 || body.RigCount != 1 || body.Running != 1 {
		t.Fatalf("status counts = %+v, want agent_count=1 rig_count=1 running=1", body)
	}
}

func TestRouteMatrixParity_GET_v0_config_ViaWS(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Workspace.Provider = "claude"
	fs.cfg.Agents[0].MinActiveSessions = intPtr(0)
	fs.cfg.Agents[0].MaxActiveSessions = intPtr(3)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {DisplayName: "Custom", Command: "custom-cli"},
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-config-get",
		Action: "config.get",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-config-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body configResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode config.get: %v", err)
	}
	if body.Workspace.Name != "test-city" || body.Workspace.Provider != "claude" {
		t.Fatalf("workspace = %+v, want test-city + claude", body.Workspace)
	}
	if len(body.Agents) != 1 || !body.Agents[0].IsPool {
		t.Fatalf("agents = %+v, want one pooled agent", body.Agents)
	}
	if _, ok := body.Providers["custom"]; !ok {
		t.Fatalf("providers = %+v, want custom provider", body.Providers)
	}
}

func TestRouteMatrixParity_GET_v0_config_explain_ViaWS(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Agents[0].MinActiveSessions = intPtr(0)
	fs.cfg.Agents[0].MaxActiveSessions = intPtr(3)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"claude": {DisplayName: "My Claude", Command: "my-claude"},
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-config-explain",
		Action: "config.explain",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-config-explain" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode config.explain: %v", err)
	}
	agents, ok := body["agents"].([]any)
	if !ok || len(agents) == 0 {
		t.Fatalf("agents = %#v, want non-empty array", body["agents"])
	}
	agent0, ok := agents[0].(map[string]any)
	if !ok || agent0["origin"] != "inline" || agent0["is_pool"] != true {
		t.Fatalf("agent explain = %#v, want inline pooled agent", agent0)
	}
}

func TestRouteMatrixParity_GET_v0_config_validate_ViaWS(t *testing.T) {
	fs := newFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-config-validate",
		Action: "config.validate",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-config-validate" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode config.validate: %v", err)
	}
	if body["valid"] != true {
		t.Fatalf("body = %+v, want valid=true", body)
	}
}

func TestRouteMatrixParity_GET_v0_providers_ViaWS(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {DisplayName: "Custom Agent", Command: "custom-cli"},
		"claude": {DisplayName: "My Claude", Command: "my-claude"},
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-providers-list",
		Action: "providers.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-providers-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []providerResponse `json:"items"`
		Total int                `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode providers.list: %v", err)
	}
	if body.Total < 10 {
		t.Fatalf("total = %d, want builtins plus overrides", body.Total)
	}
}

func TestRouteMatrixParity_GET_v0_provider_name_ViaWS(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {DisplayName: "Custom Agent", Command: "custom-cli"},
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-provider-get",
		Action: "provider.get",
		Payload: map[string]any{
			"name": "custom",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-provider-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body providerResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode provider.get: %v", err)
	}
	if body.Name != "custom" || !body.CityLevel {
		t.Fatalf("body = %+v, want custom city-level provider", body)
	}
}

func TestRouteMatrixParity_GET_v0_agents_ViaWS(t *testing.T) {
	state := newFakeState(t)
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agents-list",
		Action: "agents.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agents-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []agentResponse `json:"items"`
		Total int             `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode agents.list: %v", err)
	}
	if body.Total != 1 {
		t.Fatalf("total = %d, want 1", body.Total)
	}
	if len(body.Items) != 1 || body.Items[0].Name != "myrig/worker" {
		t.Fatalf("items = %+v, want myrig/worker", body.Items)
	}
}

func TestRouteMatrixParity_GET_v0_agent_name_ViaWS(t *testing.T) {
	state := newSessionFakeState(t)
	sessionName := "myrig--worker"
	if err := state.sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := state.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:myrig/worker"},
		Metadata: map[string]string{
			"template":     "myrig/worker",
			"session_name": sessionName,
			"state":        "awake",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-get",
		Action: "agent.get",
		Payload: map[string]any{
			"name": "myrig/worker",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body agentResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode agent.get: %v", err)
	}
	if body.Name != "myrig/worker" {
		t.Fatalf("name = %q, want myrig/worker", body.Name)
	}
	if body.Session == nil || body.Session.ID != info.ID || body.Session.Name != sessionName {
		t.Fatalf("session = %+v, want id=%q name=%q", body.Session, info.ID, sessionName)
	}
}

func TestRouteMatrixParity_GET_v0_agent_name_output_ViaWS(t *testing.T) {
	conn, info := openRouteMatrixAgentOutputSocket(t)

	sessionID := resolveAgentSessionID(t, conn, "myrig/worker")
	if sessionID != info.ID {
		t.Fatalf("resolved session id = %q, want %q", sessionID, info.ID)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-output",
		Action: "session.transcript",
		Payload: map[string]any{
			"id":    sessionID,
			"turns": 0,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-output" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionTranscriptResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	if body.ID != info.ID {
		t.Fatalf("transcript id = %q, want %q", body.ID, info.ID)
	}
	if body.Format != "conversation" {
		t.Fatalf("transcript format = %q, want conversation", body.Format)
	}
	if len(body.Turns) != 2 {
		t.Fatalf("turn count = %d, want 2", len(body.Turns))
	}
	if body.Turns[0].Text != "hello" || body.Turns[1].Text != "world" {
		t.Fatalf("turns = %+v, want hello/world transcript", body.Turns)
	}
}

func TestRouteMatrixParity_GET_v0_agent_name_output_stream_ViaWS(t *testing.T) {
	conn, info := openRouteMatrixAgentOutputSocket(t)

	sessionID := resolveAgentSessionID(t, conn, "myrig/worker")
	if sessionID != info.ID {
		t.Fatalf("resolved session id = %q, want %q", sessionID, info.ID)
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-output-stream",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind":   "session.stream",
			"target": sessionID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-output-stream" {
		t.Fatalf("subscription response = %#v, want correlated response", resp)
	}

	var turnEvt wsEventEnvelope
	readWSJSON(t, conn, &turnEvt)
	if turnEvt.Type != "event" || turnEvt.EventType != "turn" {
		t.Fatalf("turn event = %#v, want turn event", turnEvt)
	}
	if !strings.Contains(string(turnEvt.Payload), `"hello"`) || !strings.Contains(string(turnEvt.Payload), `"world"`) {
		t.Fatalf("turn payload = %s, want transcript snapshot", turnEvt.Payload)
	}

	var activityEvt wsEventEnvelope
	readWSJSON(t, conn, &activityEvt)
	if activityEvt.Type != "event" || activityEvt.EventType != "activity" {
		t.Fatalf("activity event = %#v, want activity event", activityEvt)
	}
	if !strings.Contains(string(activityEvt.Payload), `"idle"`) {
		t.Fatalf("activity payload = %s, want closed-session idle state", activityEvt.Payload)
	}
}

func TestRouteMatrixParity_GET_v0_events_ViaWS(t *testing.T) {
	alpha := newFakeState(t)
	alpha.cityName = "alpha"
	beta := newFakeState(t)
	beta.cityName = "beta"

	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "alpha-mayor"})
	beta.eventProv.Record(events.Event{Type: events.SessionStopped, Actor: "beta-mayor"})

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
		ID:     "route-events-list",
		Action: "events.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-events-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode events list: %v", err)
	}
	if body.Total != 2 {
		t.Fatalf("total = %d, want 2", body.Total)
	}
	cities := map[string]bool{}
	for _, item := range body.Items {
		cities[item.City] = true
	}
	if !cities["alpha"] || !cities["beta"] {
		t.Fatalf("events cities = %v, want alpha and beta", cities)
	}
}

func TestRouteMatrixParity_GET_v0_events_stream_ViaWS(t *testing.T) {
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
		ID:     "route-events-stream",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind": "events",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-events-stream" {
		t.Fatalf("subscription response = %#v, want correlated response", resp)
	}

	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "alpha-mayor"})

	var evt wsEventEnvelope
	readWSJSON(t, conn, &evt)
	if evt.Type != "event" || evt.EventType != events.SessionWoke {
		t.Fatalf("event = %#v, want session.woke event", evt)
	}
	if evt.Cursor == "" {
		t.Fatal("global event cursor empty")
	}

	var payload struct {
		City string `json:"city"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if payload.City != "alpha" || payload.Type != events.SessionWoke {
		t.Fatalf("payload = %+v, want city alpha type %q", payload, events.SessionWoke)
	}
}

func TestRouteMatrixParity_GET_v0_sessions_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session B")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-sessions-list",
		Action: "sessions.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-sessions-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []sessionResponse `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode sessions.list: %v", err)
	}
	if body.Total != 2 || len(body.Items) != 2 {
		t.Fatalf("body = %+v, want total/items=2", body)
	}
}

func TestRouteMatrixParity_GET_v0_session_id_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-get",
		Action: "session.get",
		Payload: map[string]any{
			"id": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.get: %v", err)
	}
	if body.ID != info.ID || body.Title != "Session A" {
		t.Fatalf("body = %+v, want id=%q title=Session A", body, info.ID)
	}
}

func TestRouteMatrixParity_GET_v0_session_id_pending_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-pending",
		Action: "session.pending",
		Payload: map[string]any{
			"id": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-pending" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionPendingResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.pending: %v", err)
	}
	if !body.Supported || body.Pending == nil || body.Pending.RequestID != "req-1" {
		t.Fatalf("body = %+v, want supported pending req-1", body)
	}
}

func TestRouteMatrixParity_POST_v0_session_id_messages_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Resume Me")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-messages",
		Action: "session.messages",
		Payload: map[string]any{
			"id":      info.ID,
			"message": "hello",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-messages" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body map[string]string
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.messages: %v", err)
	}
	if body["status"] != "accepted" || body["id"] != info.ID {
		t.Fatalf("body = %+v, want accepted id=%q", body, info.ID)
	}
	if !fs.sp.IsRunning(info.SessionName) {
		t.Fatalf("session %q should be running after session.messages", info.SessionName)
	}
	found := false
	for _, call := range fs.sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want provider-default nudge hello", fs.sp.Calls)
	}
}

func TestRouteMatrixParity_POST_v0_session_id_respond_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-respond",
		Action: "session.respond",
		Payload: map[string]any{
			"id":     info.ID,
			"action": "approve",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-respond" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if got := fs.sp.Responses[info.SessionName]; len(got) != 1 || got[0].Action != "approve" {
		t.Fatalf("responses = %#v, want single approve", got)
	}
}

func TestRouteMatrixParity_GET_v0_beads_index_wait_ViaWSWatch(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	go func() {
		time.Sleep(75 * time.Millisecond)
		state.eventProv.Record(events.Event{Type: "bead.changed", Actor: "tester"})
	}()

	start := time.Now()
	writeWSJSON(t, conn, map[string]any{
		"type":   "request",
		"id":     "route-beads-watch",
		"action": "beads.list",
		"payload": map[string]any{
			"limit": 10,
		},
		"watch": map[string]any{
			"index": 0,
			"wait":  "1s",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	elapsed := time.Since(start)

	if resp.Type != "response" || resp.ID != "route-beads-watch" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if resp.Index == 0 {
		t.Fatal("watch response index = 0, want event-driven index > 0")
	}
	if elapsed < 50*time.Millisecond || elapsed > 900*time.Millisecond {
		t.Fatalf("watch elapsed = %v, want delayed unblock before timeout", elapsed)
	}
}

func TestRouteMatrixParity_GET_v0_mail_ViaWS(t *testing.T) {
	state := newFakeState(t)
	if _, err := state.cityMailProv.Send("mayor", "worker", "First", "body1"); err != nil {
		t.Fatalf("Send(first): %v", err)
	}
	if _, err := state.cityMailProv.Send("mayor", "worker", "Second", "body2"); err != nil {
		t.Fatalf("Send(second): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	items := routeMatrixListMail(t, conn, "worker", "")
	if len(items) != 2 {
		t.Fatalf("mail items = %d, want 2 unread messages", len(items))
	}
}

func TestRouteMatrixParity_POST_v0_mail_ViaWS(t *testing.T) {
	state := newFakeState(t)
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-mail-send",
		Action: "mail.send",
		Payload: map[string]any{
			"from":    "mayor",
			"to":      "worker",
			"subject": "Review needed",
			"body":    "Please check gc-456",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-send" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body mail.Message
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode mail.send: %v", err)
	}
	if body.Subject != "Review needed" || body.To != "myrig/worker" {
		t.Fatalf("body = %+v, want subject Review needed to myrig/worker", body)
	}
}

func TestRouteMatrixParity_GET_v0_mail_count_ViaWS(t *testing.T) {
	state := newFakeState(t)
	if _, err := state.cityMailProv.Send("a", "b", "msg1", "body1"); err != nil {
		t.Fatalf("Send(msg1): %v", err)
	}
	if _, err := state.cityMailProv.Send("a", "b", "msg2", "body2"); err != nil {
		t.Fatalf("Send(msg2): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-mail-count",
		Action: "mail.count",
		Payload: map[string]any{
			"agent": "b",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-count" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body map[string]int
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode mail.count: %v", err)
	}
	if body["unread"] != 2 || body["total"] != 2 {
		t.Fatalf("body = %+v, want unread=2 total=2", body)
	}
}

func TestRouteMatrixParity_GET_v0_mail_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	msg, err := state.cityMailProv.Send("mayor", "worker", "Subject", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-mail-get",
		Action: "mail.get",
		Payload: map[string]any{
			"id": msg.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body mail.Message
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode mail.get: %v", err)
	}
	if body.ID != msg.ID || body.Subject != "Subject" {
		t.Fatalf("body = %+v, want id=%q subject=Subject", body, msg.ID)
	}
}

func TestRouteMatrixParity_POST_v0_mail_id_read_ViaWS(t *testing.T) {
	state := newFakeState(t)
	msg, err := state.cityMailProv.Send("mayor", "worker", "Read me", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-mail-read",
		Action: "mail.read",
		Payload: map[string]any{
			"id": msg.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-read" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if got := routeMatrixListMail(t, conn, "worker", ""); len(got) != 0 {
		t.Fatalf("unread mail after read = %d, want 0", len(got))
	}
}

func TestRouteMatrixParity_POST_v0_mail_id_mark_unread_ViaWS(t *testing.T) {
	state := newFakeState(t)
	msg, err := state.cityMailProv.Send("mayor", "worker", "Unread me", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := state.cityMailProv.MarkRead(msg.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-mail-mark-unread",
		Action: "mail.mark_unread",
		Payload: map[string]any{
			"id": msg.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-mark-unread" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if got := routeMatrixListMail(t, conn, "worker", ""); len(got) != 1 {
		t.Fatalf("unread mail after mark_unread = %d, want 1", len(got))
	}
}

func TestRouteMatrixParity_POST_v0_mail_id_archive_ViaWS(t *testing.T) {
	state := newFakeState(t)
	msg, err := state.cityMailProv.Send("mayor", "worker", "Archive me", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-mail-archive",
		Action: "mail.archive",
		Payload: map[string]any{
			"id": msg.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-archive" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if got := routeMatrixListMail(t, conn, "worker", "all"); len(got) != 0 {
		t.Fatalf("mail after archive = %d, want 0", len(got))
	}
}

func TestRouteMatrixParity_GET_v0_mail_thread_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	msg, err := state.cityMailProv.Send("alice", "bob", "Thread test", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := state.cityMailProv.Reply(msg.ID, "bob", "Re: Thread test", "reply body"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-mail-thread",
		Action: "mail.thread",
		Payload: map[string]any{
			"id": msg.ThreadID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-thread" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode mail.thread: %v", err)
	}
	if body.Total != 2 || len(body.Items) != 2 {
		t.Fatalf("body = %+v, want thread size 2", body)
	}
}

func TestRouteMatrixParity_POST_v0_mail_id_reply_ViaWS(t *testing.T) {
	state := newFakeState(t)
	msg, err := state.cityMailProv.Send("mayor", "worker", "Initial", "content")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-mail-reply",
		Action: "mail.reply",
		Payload: map[string]any{
			"id":      msg.ID,
			"from":    "worker",
			"subject": "Re: Initial",
			"body":    "Done!",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-reply" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body mail.Message
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode mail.reply: %v", err)
	}
	if body.ThreadID == "" || body.ReplyTo != msg.ID {
		t.Fatalf("body = %+v, want thread id + reply_to=%q", body, msg.ID)
	}
}

func TestRouteMatrixParity_DELETE_v0_mail_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	msg, err := state.cityMailProv.Send("mayor", "worker", "Delete me", "content")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-mail-delete",
		Action: "mail.delete",
		Payload: map[string]any{
			"id": msg.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-delete" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body map[string]string
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode mail.delete: %v", err)
	}
	if body["status"] != "deleted" {
		t.Fatalf("body = %+v, want status=deleted", body)
	}
	if got := routeMatrixListMail(t, conn, "worker", "all"); len(got) != 0 {
		t.Fatalf("mail after delete = %d, want 0", len(got))
	}
}

func TestRouteMatrixParity_GET_v0_rigs_ViaWS(t *testing.T) {
	state := newFakeState(t)
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rigs-list",
		Action: "rigs.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-rigs-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []rigResponse `json:"items"`
		Total int           `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode rigs.list: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].Name != "myrig" {
		t.Fatalf("body = %+v, want single myrig entry", body)
	}
}

func TestRouteMatrixParity_GET_v0_rig_name_ViaWS(t *testing.T) {
	state := newFakeState(t)
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rig-get",
		Action: "rig.get",
		Payload: map[string]any{
			"name": "myrig",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-rig-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body rigResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode rig.get: %v", err)
	}
	if body.Name != "myrig" || body.Path != "/tmp/myrig" {
		t.Fatalf("body = %+v, want rig myrig /tmp/myrig", body)
	}
}

func TestRouteMatrixParity_POST_v0_rig_name_suspend_ViaWS(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rig-suspend",
		Action: "rig.suspend",
		Payload: map[string]any{
			"name": "myrig",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-rig-suspend" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	conn2 := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn2.Close()
	drainWSHello(t, conn2)
	writeWSJSON(t, conn2, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rig-check",
		Action: "rig.get",
		Payload: map[string]any{
			"name": "myrig",
		},
	})
	readWSJSON(t, conn2, &resp)

	var body rigResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode rig.get after suspend: %v", err)
	}
	if !body.Suspended {
		t.Fatalf("body = %+v, want suspended rig", body)
	}
}

func TestRouteMatrixParity_GET_v0_patches_agents_ViaWS(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	fs.cfg.Patches.Agents = []config.AgentPatch{{Dir: "rig1", Name: "worker", Suspended: &suspended}}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-patches-list",
		Action: "patches.agents.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-patches-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []config.AgentPatch `json:"items"`
		Total int                 `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode patches.agents.list: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("body = %+v, want single agent patch", body)
	}
}

func TestRouteMatrixParity_GET_v0_patches_agent_name_ViaWS(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	fs.cfg.Patches.Agents = []config.AgentPatch{{Dir: "rig1", Name: "worker", Suspended: &suspended}}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-patch-get",
		Action: "patches.agent.get",
		Payload: map[string]any{
			"name": "rig1/worker",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-patch-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body config.AgentPatch
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode patches.agent.get: %v", err)
	}
	if body.Dir != "rig1" || body.Name != "worker" || body.Suspended == nil || !*body.Suspended {
		t.Fatalf("body = %+v, want suspended rig1/worker patch", body)
	}
}

func TestRouteMatrixParity_GET_v0_patches_rigs_ViaWS(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	fs.cfg.Patches.Rigs = []config.RigPatch{{Name: "myrig", Suspended: &suspended}}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rig-patches-list",
		Action: "patches.rigs.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-rig-patches-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []config.RigPatch `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode patches.rigs.list: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].Name != "myrig" {
		t.Fatalf("body = %+v, want single myrig rig patch", body)
	}
}

func TestRouteMatrixParity_GET_v0_patches_providers_ViaWS(t *testing.T) {
	fs := newFakeState(t)
	cmd := "new-cmd"
	fs.cfg.Patches.Providers = []config.ProviderPatch{{Name: "claude", Command: &cmd}}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-provider-patches-list",
		Action: "patches.providers.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-provider-patches-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []config.ProviderPatch `json:"items"`
		Total int                    `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode patches.providers.list: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].Name != "claude" {
		t.Fatalf("body = %+v, want single claude provider patch", body)
	}
}

func TestRouteMatrixParity_GET_v0_packs_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Packs = map[string]config.PackSource{
		"gastown": {
			Source: "https://github.com/example/gastown-pack",
			Ref:    "v1.0.0",
			Path:   "packs/gastown",
		},
	}
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-packs-list",
		Action: "packs.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-packs-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Packs []packResponse `json:"packs"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode packs list: %v", err)
	}
	if len(body.Packs) != 1 || body.Packs[0].Name != "gastown" {
		t.Fatalf("packs = %+v, want gastown pack", body.Packs)
	}
}

func TestRouteMatrixParity_GET_v0_services_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName:      "review-intake",
			Kind:             "workflow",
			WorkflowContract: "pack.gc/review-intake.v1",
			MountPath:        "/svc/review-intake",
			PublishMode:      "private",
			StateRoot:        ".gc/services/review-intake",
			State:            "ready",
			LocalState:       "ready",
			PublicationState: "private",
		}},
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-services-list",
		Action: "services.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-services-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []workspacesvc.Status `json:"items"`
		Total int                   `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode services.list: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].ServiceName != "review-intake" {
		t.Fatalf("body = %+v, want single review-intake service", body)
	}
}

func TestRouteMatrixParity_GET_v0_service_name_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName:      "review-intake",
			Kind:             "workflow",
			WorkflowContract: "pack.gc/review-intake.v1",
			MountPath:        "/svc/review-intake",
			PublishMode:      "private",
			StateRoot:        ".gc/services/review-intake",
			State:            "ready",
			LocalState:       "ready",
			PublicationState: "private",
		}},
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-service-get",
		Action: "service.get",
		Payload: map[string]any{
			"name": "review-intake",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-service-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body workspacesvc.Status
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode service.get: %v", err)
	}
	if body.ServiceName != "review-intake" || body.Kind != "workflow" {
		t.Fatalf("body = %+v, want review-intake workflow service", body)
	}
}

func TestRouteMatrixParity_POST_v0_service_name_restart_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName:      "review-intake",
			Kind:             "workflow",
			WorkflowContract: "pack.gc/review-intake.v1",
			MountPath:        "/svc/review-intake",
			PublishMode:      "private",
			StateRoot:        ".gc/services/review-intake",
			State:            "ready",
			LocalState:       "ready",
			PublicationState: "private",
		}},
	}
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-service-restart",
		Action: "service.restart",
		Payload: map[string]any{
			"name": "review-intake",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-service-restart" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body map[string]string
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode restart response: %v", err)
	}
	if body["status"] != "ok" || body["action"] != "restart" || body["service"] != "review-intake" {
		t.Fatalf("restart body = %+v, want status ok action restart service review-intake", body)
	}
}

func openRouteMatrixAgentOutputSocket(t *testing.T) (*websocket.Conn, session.Info) {
	t.Helper()

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
	t.Cleanup(ts.Close)

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	t.Cleanup(func() { _ = conn.Close() })
	drainWSHello(t, conn)
	return conn, info
}

func resolveAgentSessionID(t *testing.T, conn *websocket.Conn, agentName string) string {
	t.Helper()

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-get",
		Action: "agent.get",
		Payload: map[string]any{
			"name": agentName,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-get" {
		t.Fatalf("agent.get response = %#v, want correlated response", resp)
	}

	var body agentResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode agent.get: %v", err)
	}
	if body.Session == nil || body.Session.ID == "" {
		t.Fatalf("agent.get session = %+v, want canonical session id", body.Session)
	}
	return body.Session.ID
}

func routeMatrixListMail(t *testing.T, conn *websocket.Conn, agent, status string) []mail.Message {
	t.Helper()

	payload := map[string]any{}
	if agent != "" {
		payload["agent"] = agent
	}
	if status != "" {
		payload["status"] = status
	}
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:    "request",
		ID:      "route-mail-list-helper",
		Action:  "mail.list",
		Payload: payload,
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-mail-list-helper" {
		t.Fatalf("mail.list response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode mail.list: %v", err)
	}
	if body.Total != len(body.Items) {
		t.Fatalf("mail.list total/items mismatch = %d/%d", body.Total, len(body.Items))
	}
	return body.Items
}
