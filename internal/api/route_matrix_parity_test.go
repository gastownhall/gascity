package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
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

func TestRouteMatrixParity_GET_v0_city_ViaWS(t *testing.T) {
	state := newFakeState(t)
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-city-get",
		Action: "city.get",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-city-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body cityGetResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode city.get: %v", err)
	}
	if body.Name != "test-city" || body.AgentCount != 1 || body.RigCount != 1 {
		t.Fatalf("body = %+v, want test-city with one agent and one rig", body)
	}
}

func TestRouteMatrixParity_PATCH_v0_city_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-city-patch",
		Action: "city.patch",
		Payload: map[string]any{
			"suspended": true,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-city-patch" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if !state.cfg.Workspace.Suspended {
		t.Fatal("city suspended = false, want true after city.patch")
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

func TestRouteMatrixParity_POST_v0_providers_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-provider-create",
		Action: "provider.create",
		Payload: map[string]any{
			"name": "myagent",
			"spec": map[string]any{
				"command":      "myagent-cli",
				"display_name": "My Agent",
			},
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-provider-create" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if spec, ok := state.cfg.Providers["myagent"]; !ok || spec.Command != "myagent-cli" {
		t.Fatalf("providers = %+v, want created myagent provider", state.cfg.Providers)
	}
}

func TestRouteMatrixParity_PATCH_v0_provider_name_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	state.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {Command: "old-cli", DisplayName: "Old Name"},
	}
	cmd := "new-cli"
	dn := "New Name"

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-provider-update",
		Action: "provider.update",
		Payload: map[string]any{
			"name":   "custom",
			"update": map[string]any{"command": &cmd, "display_name": &dn},
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-provider-update" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if spec := state.cfg.Providers["custom"]; spec.Command != "new-cli" {
		t.Fatalf("provider = %+v, want command updated to new-cli", spec)
	}
}

func TestRouteMatrixParity_DELETE_v0_provider_name_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	state.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {Command: "custom-cli"},
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-provider-delete",
		Action: "provider.delete",
		Payload: map[string]any{
			"name": "custom",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-provider-delete" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if _, ok := state.cfg.Providers["custom"]; ok {
		t.Fatalf("providers = %+v, want custom provider removed", state.cfg.Providers)
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

func TestRouteMatrixParity_POST_v0_agents_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-create",
		Action: "agent.create",
		Payload: map[string]any{
			"name":     "coder",
			"provider": "claude",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-create" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	found := false
	for _, a := range state.cfg.Agents {
		if a.Name == "coder" && a.Provider == "claude" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("created agent coder missing from config")
	}
}

func TestRouteMatrixParity_PATCH_v0_agent_name_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-update",
		Action: "agent.update",
		Payload: map[string]any{
			"name":     "myrig/worker",
			"provider": "gemini",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-update" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if got := state.cfg.Agents[0].Provider; got != "gemini" {
		t.Fatalf("provider = %q, want gemini", got)
	}
}

func TestRouteMatrixParity_DELETE_v0_agent_name_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-delete",
		Action: "agent.delete",
		Payload: map[string]any{
			"name": "myrig/worker",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-delete" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Agents) != 0 {
		t.Fatalf("agents = %+v, want worker removed", state.cfg.Agents)
	}
}

func TestRouteMatrixParity_POST_v0_agent_name_suspend_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-suspend",
		Action: "agent.suspend",
		Payload: map[string]any{
			"name": "myrig/worker",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-suspend" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if !state.suspended["myrig/worker"] {
		t.Fatal("agent suspend flag missing after agent.suspend")
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

func TestRouteMatrixParity_POST_v0_events_ViaWS(t *testing.T) {
	state := newFakeState(t)
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[map[string]string](t, conn, "route-events-emit", "event.emit", eventEmitRequest{
		Type:    "route.test",
		Actor:   "tester",
		Subject: "gc-1",
		Message: "hello",
	})

	if body["status"] != "recorded" {
		t.Fatalf("event.emit body = %+v, want recorded", body)
	}
	items, err := state.eventProv.List(events.Filter{Type: "route.test", Actor: "tester"})
	if err != nil {
		t.Fatalf("List(events): %v", err)
	}
	if len(items) != 1 || items[0].Subject != "gc-1" || items[0].Message != "hello" {
		t.Fatalf("recorded events = %+v, want one route.test event", items)
	}
}

func TestRouteMatrixParity_GET_v0_convoys_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	if _, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"}); err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "Task", Type: "task"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-convoys-list",
		Action: "convoys.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-convoys-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode convoys.list: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("body = %+v, want single convoy entry", body)
	}
	if body.Items[0].Type != "convoy" {
		t.Fatalf("items[0] = %#v, want convoy bead", body.Items[0])
	}
}

func TestRouteMatrixParity_GET_v0_convoy_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	pid := convoy.ID
	if _, err := store.Create(beads.Bead{Title: "Child", Type: "task", ParentID: pid}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-convoy-get",
		Action: "convoy.get",
		Payload: map[string]any{
			"id": convoy.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-convoy-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Convoy   beads.Bead   `json:"convoy"`
		Children []beads.Bead `json:"children"`
		Progress struct {
			Total  int `json:"total"`
			Closed int `json:"closed"`
		} `json:"progress"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode convoy.get: %v", err)
	}
	if body.Convoy.ID != convoy.ID || len(body.Children) != 1 || body.Progress.Total != 1 || body.Progress.Closed != 0 {
		t.Fatalf("body = %+v, want convoy snapshot with one open child", body)
	}
}

func TestRouteMatrixParity_POST_v0_convoys_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	item, err := store.Create(beads.Bead{Title: "Task", Type: "task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-convoy-create",
		Action: "convoy.create",
		Payload: map[string]any{
			"rig":   "myrig",
			"title": "Test convoy",
			"items": []string{item.ID},
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-convoy-create" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body beads.Bead
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode convoy.create: %v", err)
	}
	gotItem, err := store.Get(item.ID)
	if err != nil {
		t.Fatalf("get created child: %v", err)
	}
	if body.Type != "convoy" || gotItem.ParentID != body.ID {
		t.Fatalf("convoy/item = %+v / %+v, want item parent linked to created convoy", body, gotItem)
	}
}

func TestRouteMatrixParity_POST_v0_convoy_id_add_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	item, err := store.Create(beads.Bead{Title: "Task", Type: "task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-convoy-add",
		Action: "convoy.add",
		Payload: map[string]any{
			"id":    convoy.ID,
			"items": []string{item.ID},
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-convoy-add" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	gotItem, err := store.Get(item.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if gotItem.ParentID != convoy.ID {
		t.Fatalf("item parent = %q, want convoy %q", gotItem.ParentID, convoy.ID)
	}
}

func TestRouteMatrixParity_POST_v0_convoy_id_remove_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	pid := convoy.ID
	item, err := store.Create(beads.Bead{Title: "Task", Type: "task", ParentID: pid})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-convoy-remove",
		Action: "convoy.remove",
		Payload: map[string]any{
			"id":    convoy.ID,
			"items": []string{item.ID},
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-convoy-remove" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	gotItem, err := store.Get(item.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if gotItem.ParentID != "" {
		t.Fatalf("item parent = %q, want empty after remove", gotItem.ParentID)
	}
}

func TestRouteMatrixParity_GET_v0_convoy_id_check_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	pid := convoy.ID
	done, err := store.Create(beads.Bead{Title: "Done", Type: "task", ParentID: pid})
	if err != nil {
		t.Fatalf("create closed child: %v", err)
	}
	if err := store.Close(done.ID); err != nil {
		t.Fatalf("close child: %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "Open", Type: "task", ParentID: pid}); err != nil {
		t.Fatalf("create open child: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-convoy-check",
		Action: "convoy.check",
		Payload: map[string]any{
			"id": convoy.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-convoy-check" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		ConvoyID string `json:"convoy_id"`
		Total    int    `json:"total"`
		Closed   int    `json:"closed"`
		Complete bool   `json:"complete"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode convoy.check: %v", err)
	}
	if body.ConvoyID != convoy.ID || body.Total != 2 || body.Closed != 1 || body.Complete {
		t.Fatalf("body = %+v, want convoy_id=%q total=2 closed=1 complete=false", body, convoy.ID)
	}
}

func TestRouteMatrixParity_POST_v0_convoy_id_close_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-convoy-close",
		Action: "convoy.close",
		Payload: map[string]any{
			"id": convoy.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-convoy-close" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	got, err := store.Get(convoy.ID)
	if err != nil {
		t.Fatalf("get convoy: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("convoy status = %q, want closed", got.Status)
	}
}

func TestRouteMatrixParity_DELETE_v0_convoy_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	convoy, err := store.Create(beads.Bead{Title: "Convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-convoy-delete",
		Action: "convoy.delete",
		Payload: map[string]any{
			"id": convoy.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-convoy-delete" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	got, err := store.Get(convoy.ID)
	if err != nil {
		t.Fatalf("get convoy: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("convoy status = %q, want closed after delete", got.Status)
	}
}

func TestRouteMatrixParity_GET_v0_workflow_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	root, err := state.cityBeadStore.Create(beads.Bead{
		Title: "Workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-1",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-workflow-get",
		Action: "workflow.get",
		Payload: map[string]any{
			"id": "wf-1",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-workflow-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body workflowSnapshotResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode workflow.get: %v", err)
	}
	if body.WorkflowID != "wf-1" || body.RootBeadID != root.ID || len(body.Beads) == 0 {
		t.Fatalf("body = %+v, want workflow wf-1 rooted at %q", body, root.ID)
	}
}

func TestRouteMatrixParity_DELETE_v0_workflow_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	root, err := state.cityBeadStore.Create(beads.Bead{
		Title: "Workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-1",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	if _, err := state.cityBeadStore.Create(beads.Bead{
		Title: "Workflow child",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	}); err != nil {
		t.Fatalf("create workflow child: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-workflow-delete",
		Action: "workflow.delete",
		Payload: map[string]any{
			"id": "wf-1",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-workflow-delete" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		WorkflowID string `json:"workflow_id"`
		Closed     int    `json:"closed"`
		Deleted    int    `json:"deleted"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode workflow.delete: %v", err)
	}
	if body.WorkflowID != "wf-1" || body.Closed != 2 || body.Deleted != 0 {
		t.Fatalf("body = %+v, want wf-1 closed=2 deleted=0", body)
	}
	gotRoot, err := state.cityBeadStore.Get(root.ID)
	if err != nil {
		t.Fatalf("get workflow root: %v", err)
	}
	if gotRoot.Status != "closed" {
		t.Fatalf("workflow root status = %q, want closed", gotRoot.Status)
	}
}

func TestRouteMatrixParity_GET_v0_orders_ViaWS(t *testing.T) {
	state := newFakeState(t)
	enabled := true
	state.autos = []orders.Order{
		{Name: "daily", Formula: "mol-daily", Gate: "cooldown", Interval: "1h", Enabled: &enabled},
		{Name: "smoke", Exec: "echo ok", Gate: "manual"},
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-orders-list",
		Action: "orders.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-orders-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Orders []orderResponse `json:"orders"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode orders.list: %v", err)
	}
	if len(body.Orders) != 2 {
		t.Fatalf("orders = %+v, want 2 entries", body.Orders)
	}
	if body.Orders[0].Name != "daily" || body.Orders[0].Type != "formula" || !body.Orders[0].Enabled {
		t.Fatalf("orders[0] = %+v, want enabled daily formula order", body.Orders[0])
	}
	if body.Orders[1].Name != "smoke" || body.Orders[1].Type != "exec" || !body.Orders[1].CaptureOutput {
		t.Fatalf("orders[1] = %+v, want exec smoke order with captured output", body.Orders[1])
	}
}

func TestRouteMatrixParity_GET_v0_orders_feed_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.autos = []orders.Order{
		{Name: "daily", Formula: "mol-daily", Gate: "cron", Rig: "myrig", Pool: "reviewers"},
	}
	routeMatrixCreateWorkflowRun(t, state.stores["myrig"], "mol-daily", "wf-order-feed", "myrig/claude", "rig", "myrig")
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-orders-feed",
		Action: "orders.feed",
		Payload: map[string]any{
			"scope_kind": "rig",
			"scope_ref":  "myrig",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-orders-feed" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []monitorFeedItemResponse `json:"items"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode orders.feed: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].WorkflowID != "wf-order-feed" || body.Items[0].Type != "formula" {
		t.Fatalf("items = %+v, want single wf-order-feed formula item", body.Items)
	}
}

func TestRouteMatrixParity_GET_v0_orders_check_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.autos = []orders.Order{{Name: "daily", Exec: "echo ok", Gate: "cooldown"}}
	if _, err := state.cityBeadStore.Create(beads.Bead{
		Title:  "daily run",
		Status: "closed",
		Labels: []string{"order-run:daily", "exec-failed"},
	}); err != nil {
		t.Fatalf("create order run bead: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-orders-check",
		Action: "orders.check",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-orders-check" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Checks []struct {
			ScopedName     string  `json:"scoped_name"`
			LastRunOutcome *string `json:"last_run_outcome"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode orders.check: %v", err)
	}
	if len(body.Checks) != 1 || body.Checks[0].ScopedName != "daily" {
		t.Fatalf("checks = %+v, want single daily check", body.Checks)
	}
	if body.Checks[0].LastRunOutcome == nil || *body.Checks[0].LastRunOutcome != "failed" {
		t.Fatalf("last_run_outcome = %v, want failed", body.Checks[0].LastRunOutcome)
	}
}

func TestRouteMatrixParity_GET_v0_orders_history_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.autos = []orders.Order{{Name: "daily", Exec: "echo ok", Gate: "manual"}}
	bead, err := state.cityBeadStore.Create(beads.Bead{
		Title:  "daily run",
		Status: "closed",
		Labels: []string{"order-run:daily", "exec"},
		Metadata: map[string]string{
			"convergence.gate_duration_ms": "42",
			"convergence.gate_exit_code":   "0",
		},
	})
	if err != nil {
		t.Fatalf("create order history bead: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-orders-history",
		Action: "orders.history",
		Payload: map[string]any{
			"scoped_name": "daily",
			"limit":       10,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-orders-history" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body []struct {
		BeadID     string  `json:"bead_id"`
		Name       string  `json:"name"`
		ScopedName string  `json:"scoped_name"`
		HasOutput  bool    `json:"has_output"`
		ExitCode   *string `json:"exit_code"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode orders.history: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("history = %+v, want single entry", body)
	}
	if body[0].BeadID != bead.ID || body[0].Name != "daily" || body[0].ScopedName != "daily" || !body[0].HasOutput {
		t.Fatalf("history[0] = %+v, want bead=%q daily has_output=true", body[0], bead.ID)
	}
	if body[0].ExitCode == nil || *body[0].ExitCode != "0" {
		t.Fatalf("exit_code = %v, want 0", body[0].ExitCode)
	}
}

func TestRouteMatrixParity_GET_v0_order_history_bead_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	bead, err := state.cityBeadStore.Create(beads.Bead{
		Title:  "daily run",
		Status: "closed",
		Labels: []string{"order-run:daily", "exec"},
		Metadata: map[string]string{
			"convergence.gate_stdout": "hello",
			"convergence.gate_stderr": "world",
		},
	})
	if err != nil {
		t.Fatalf("create order history detail bead: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-order-history-detail",
		Action: "order.history.detail",
		Payload: map[string]any{
			"id": bead.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-order-history-detail" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		BeadID string   `json:"bead_id"`
		Labels []string `json:"labels"`
		Output string   `json:"output"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode order.history.detail: %v", err)
	}
	if body.BeadID != bead.ID {
		t.Fatalf("bead_id = %q, want %q", body.BeadID, bead.ID)
	}
	if !strings.Contains(body.Output, "hello") || !strings.Contains(body.Output, "world") {
		t.Fatalf("output = %q, want merged stdout/stderr", body.Output)
	}
	if len(body.Labels) != 2 {
		t.Fatalf("labels = %+v, want original bead labels", body.Labels)
	}
}

func TestRouteMatrixParity_GET_v0_order_name_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.autos = []orders.Order{{Name: "daily", Formula: "mol-daily", Gate: "cooldown", Interval: "1h"}}
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-order-get",
		Action: "order.get",
		Payload: map[string]any{
			"name": "daily",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-order-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body orderResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode order.get: %v", err)
	}
	if body.Name != "daily" || body.Type != "formula" || body.Interval != "1h" {
		t.Fatalf("body = %+v, want daily formula order with interval 1h", body)
	}
}

func TestRouteMatrixParity_POST_v0_order_name_enable_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	state.autos = []orders.Order{{Name: "daily", Exec: "echo ok", Gate: "cooldown"}}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-order-enable",
		Action: "order.enable",
		Payload: map[string]any{
			"name": "daily",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-order-enable" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Orders.Overrides) != 1 || state.cfg.Orders.Overrides[0].Enabled == nil || !*state.cfg.Orders.Overrides[0].Enabled {
		t.Fatalf("order overrides = %+v, want single enabled daily override", state.cfg.Orders.Overrides)
	}
}

func TestRouteMatrixParity_POST_v0_order_name_disable_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	state.autos = []orders.Order{{Name: "daily", Exec: "echo ok", Gate: "cooldown"}}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-order-disable",
		Action: "order.disable",
		Payload: map[string]any{
			"name": "daily",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-order-disable" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Orders.Overrides) != 1 || state.cfg.Orders.Overrides[0].Enabled == nil || *state.cfg.Orders.Overrides[0].Enabled {
		t.Fatalf("order overrides = %+v, want single disabled daily override", state.cfg.Orders.Overrides)
	}
}

func TestRouteMatrixParity_GET_v0_formulas_ViaWS(t *testing.T) {
	state := newFakeState(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	routeMatrixWriteWorkflowFormula(t, formulaDir, "daily")
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-formulas-list",
		Action: "formulas.list",
		Payload: map[string]any{
			"scope_kind": "city",
			"scope_ref":  "test-city",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-formulas-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items   []formulaSummaryResponse `json:"items"`
		Partial bool                     `json:"partial"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode formulas.list: %v", err)
	}
	if body.Partial {
		t.Fatal("partial = true, want false")
	}
	if len(body.Items) != 1 || body.Items[0].Name != "daily" {
		t.Fatalf("items = %+v, want single daily formula", body.Items)
	}
}

func TestRouteMatrixParity_GET_v0_formulas_feed_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	routeMatrixCreateWorkflowRun(t, state.cityBeadStore, "daily", "wf-formula-feed", "mayor", "city", "test-city")
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-formulas-feed",
		Action: "formulas.feed",
		Payload: map[string]any{
			"scope_kind": "city",
			"scope_ref":  "test-city",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-formulas-feed" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Items []monitorFeedItemResponse `json:"items"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode formulas.feed: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].WorkflowID != "wf-formula-feed" {
		t.Fatalf("items = %+v, want single wf-formula-feed item", body.Items)
	}
}

func TestRouteMatrixParity_GET_v0_formulas_name_runs_ViaWS(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	routeMatrixWriteWorkflowFormula(t, formulaDir, "daily")
	routeMatrixCreateWorkflowRun(t, state.cityBeadStore, "daily", "wf-daily-run", "mayor", "city", "test-city")
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-formula-runs",
		Action: "formula.runs",
		Payload: map[string]any{
			"name":       "daily",
			"scope_kind": "city",
			"scope_ref":  "test-city",
			"limit":      2,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-formula-runs" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body formulaRunsResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode formula.runs: %v", err)
	}
	if body.Formula != "daily" || body.RunCount != 1 || len(body.RecentRuns) != 1 {
		t.Fatalf("body = %+v, want daily run_count=1 recent_runs=1", body)
	}
	if body.RecentRuns[0].WorkflowID != "wf-daily-run" {
		t.Fatalf("recent_runs[0] = %+v, want wf-daily-run", body.RecentRuns[0])
	}
}

func TestRouteMatrixParity_GET_v0_formulas_name_ViaWS(t *testing.T) {
	detail := routeMatrixFormulaDetail(t)
	if detail.Name != "daily" || detail.Description != "Preview BD-123" {
		t.Fatalf("detail = %+v, want daily preview formula with substituted description", detail)
	}
}

func TestRouteMatrixParity_GET_v0_formula_name_ViaWS(t *testing.T) {
	detail := routeMatrixFormulaDetail(t)
	if len(detail.Steps) != 1 || detail.Steps[0]["title"] != "Prep BD-123" {
		t.Fatalf("steps = %+v, want single substituted Prep BD-123 step", detail.Steps)
	}
	if len(detail.Preview.Nodes) != 1 || detail.Preview.Nodes[0].ID != "daily.prep" {
		t.Fatalf("preview.nodes = %+v, want single daily.prep node", detail.Preview.Nodes)
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

func TestRouteMatrixParity_POST_v0_sessions_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-create",
		Action: "session.create",
		Payload: map[string]any{
			"kind": "agent",
			"name": "myrig/worker",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-create" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.create: %v", err)
	}
	if body.Template != "myrig/worker" || body.Title != "myrig/worker" || body.Running {
		t.Fatalf("body = %+v, want async-created myrig/worker session with running=false", body)
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

func TestRouteMatrixParity_GET_v0_session_id_transcript_ViaWS(t *testing.T) {
	conn, info := openRouteMatrixAgentOutputSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-transcript",
		Action: "session.transcript",
		Payload: map[string]any{
			"id":    info.ID,
			"turns": 0,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-transcript" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionTranscriptResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.transcript: %v", err)
	}
	if body.ID != info.ID || len(body.Turns) != 2 {
		t.Fatalf("body = %+v, want closed transcript with 2 turns", body)
	}
}

func TestRouteMatrixParity_GET_v0_session_id_stream_ViaWS(t *testing.T) {
	conn, info := openRouteMatrixAgentOutputSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-stream",
		Action: "subscription.start",
		Payload: map[string]any{
			"kind":   "session.stream",
			"target": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-stream" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var turnEvt wsEventEnvelope
	readWSJSON(t, conn, &turnEvt)
	if turnEvt.Type != "event" || turnEvt.EventType != "turn" {
		t.Fatalf("turn event = %#v, want turn event", turnEvt)
	}
	var activityEvt wsEventEnvelope
	readWSJSON(t, conn, &activityEvt)
	if activityEvt.Type != "event" || activityEvt.EventType != "activity" {
		t.Fatalf("activity event = %#v, want activity event", activityEvt)
	}
	if !strings.Contains(string(activityEvt.Payload), `"idle"`) {
		t.Fatalf("activity payload = %s, want idle state", activityEvt.Payload)
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

func TestRouteMatrixParity_PATCH_v0_session_id_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-patch",
		Action: "session.patch",
		Payload: map[string]any{
			"id":    info.ID,
			"title": "Updated Title",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-patch" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.patch: %v", err)
	}
	if body.Title != "Updated Title" {
		t.Fatalf("body = %+v, want updated title", body)
	}
}

func TestRouteMatrixParity_POST_v0_session_id_submit_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit Me")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-submit",
		Action: "session.submit",
		Payload: map[string]any{
			"id":      info.ID,
			"message": "hello",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-submit" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.submit: %v", err)
	}
	if body["status"] != "accepted" || body["id"] != info.ID || body["queued"] != false {
		t.Fatalf("body = %+v, want accepted non-queued submit for %q", body, info.ID)
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

func TestRouteMatrixParity_POST_v0_session_id_stop_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "helper", "Codex", "codex", t.TempDir(), "codex", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := fs.cityBeadStore.Update(info.ID, beads.UpdateOpts{Metadata: map[string]string{"pool_managed": "true"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-stop",
		Action: "session.stop",
		Payload: map[string]any{
			"id": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-stop" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	sawEscape := false
	for _, call := range fs.sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
			break
		}
	}
	if !sawEscape {
		t.Fatalf("calls = %#v, want SendKeys(Escape)", fs.sp.Calls)
	}
}

func TestRouteMatrixParity_POST_v0_session_id_kill_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Kill Me")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-kill",
		Action: "session.kill",
		Payload: map[string]any{
			"id": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-kill" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if fs.sp.IsRunning(info.SessionName) {
		t.Fatalf("session %q should not be running after session.kill", info.SessionName)
	}
}

func TestRouteMatrixParity_POST_v0_session_id_suspend_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-suspend",
		Action: "session.suspend",
		Payload: map[string]any{
			"id": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-suspend" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != session.StateSuspended {
		t.Fatalf("state = %q, want suspended", got.State)
	}
}

func TestRouteMatrixParity_POST_v0_session_id_close_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Close")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-close",
		Action: "session.close",
		Payload: map[string]any{
			"id": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-close" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	sessions, err := mgr.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions after close = %d, want 0", len(sessions))
	}
}

func TestRouteMatrixParity_POST_v0_session_id_wake_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held Session")
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"held_until":   "9999-12-31T23:59:59Z",
		"wait_hold":    "true",
		"sleep_intent": "wait-hold",
		"sleep_reason": "wait-hold",
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-wake",
		Action: "session.wake",
		Payload: map[string]any{
			"id": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-wake" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Metadata["held_until"] != "" || b.Metadata["wait_hold"] != "" || b.Metadata["sleep_intent"] != "" || b.Metadata["sleep_reason"] != "" {
		t.Fatalf("metadata after wake = %+v, want hold fields cleared", b.Metadata)
	}
}

func TestRouteMatrixParity_POST_v0_session_id_rename_ViaWS(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-rename",
		Action: "session.rename",
		Payload: map[string]any{
			"id":    info.ID,
			"title": "Renamed",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-rename" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.rename: %v", err)
	}
	if body.Title != "Renamed" {
		t.Fatalf("body = %+v, want renamed title", body)
	}
}

func TestRouteMatrixParity_GET_v0_session_id_agents_ViaWS(t *testing.T) {
	conn, info := openRouteMatrixSessionAgentsSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-agents-list",
		Action: "session.agents.list",
		Payload: map[string]any{
			"id": info.ID,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-agents-list" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Agents []struct {
			AgentID         string `json:"agent_id"`
			ParentToolUseID string `json:"parent_tool_use_id"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.agents.list: %v", err)
	}
	if len(body.Agents) != 1 || body.Agents[0].AgentID != "myagent" || body.Agents[0].ParentToolUseID != "toolu_111" {
		t.Fatalf("agents = %+v, want single myagent/toolu_111 mapping", body.Agents)
	}
}

func TestRouteMatrixParity_GET_v0_session_id_agents_agent_id_ViaWS(t *testing.T) {
	conn, info := openRouteMatrixSessionAgentsSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-session-agent-get",
		Action: "session.agent.get",
		Payload: map[string]any{
			"id":       info.ID,
			"agent_id": "myagent",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-session-agent-get" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body struct {
		Messages []json.RawMessage `json:"messages"`
		Status   string            `json:"status"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode session.agent.get: %v", err)
	}
	if body.Status != "completed" || len(body.Messages) != 4 {
		t.Fatalf("body = %+v, want completed agent transcript with 4 messages", body)
	}
}

func TestRouteMatrixParity_POST_v0_sling_ViaWS(t *testing.T) {
	state := newFakeState(t)
	_, _, conn := wsSetup(t, state)

	oldRunner := slingCommandRunner
	defer func() { slingCommandRunner = oldRunner }()

	var gotArgs []string
	slingCommandRunner = func(_ context.Context, _ string, args []string) (string, string, error) {
		gotArgs = append([]string(nil), args...)
		return "Slung BD-42 → myrig/worker\n", "", nil
	}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-sling",
		Action: "sling.run",
		Payload: map[string]any{
			"target": "myrig/worker",
			"bead":   "BD-42",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-sling" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var body slingResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode sling.run: %v", err)
	}
	if body.Status != "slung" || body.Target != "myrig/worker" || body.Bead != "BD-42" {
		t.Fatalf("body = %+v, want direct sling response for BD-42", body)
	}
	wantArgs := []string{"--city", state.CityPath(), "sling", "myrig/worker", "BD-42"}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
		}
	}
}

func TestRouteMatrixParity_GET_v0_beads_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	if _, err := store.Create(beads.Bead{Title: "First", Type: "task"}); err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "Second", Type: "message"}); err != nil {
		t.Fatalf("Create(second): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}](t, conn, "route-beads-list", "beads.list", socketBeadsListPayload{})

	if body.Total != 2 || len(body.Items) != 2 {
		t.Fatalf("beads.list = %+v, want two beads", body)
	}
}

func TestRouteMatrixParity_GET_v0_beads_graph_rootID_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	root := createBeadWithMeta(t, store, "Workflow Root", map[string]string{"gc.kind": "workflow"})
	child1 := createBeadWithMeta(t, store, "Step 1", map[string]string{"gc.root_bead_id": root.ID})
	child2 := createBeadWithMeta(t, store, "Step 2", map[string]string{"gc.root_bead_id": root.ID})
	if err := store.DepAdd(child2.ID, child1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[beadGraphResponse](t, conn, "route-beads-graph", "beads.graph", socketBeadGraphPayload{
		RootID: root.ID,
	})

	if body.Root.ID != root.ID || len(body.Beads) != 3 || len(body.Deps) != 1 {
		t.Fatalf("beads.graph = %+v, want root + 2 children + 1 dep", body)
	}
}

func TestRouteMatrixParity_GET_v0_beads_ready_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	if _, err := store.Create(beads.Bead{Title: "Open"}); err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	closed, err := store.Create(beads.Bead{Title: "Closed"})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close(closed): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}](t, conn, "route-beads-ready", "beads.ready", nil)

	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].Title != "Open" {
		t.Fatalf("beads.ready = %+v, want one open bead", body)
	}
}

func TestRouteMatrixParity_POST_v0_beads_ViaWS(t *testing.T) {
	state := newFakeState(t)
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[beads.Bead](t, conn, "route-bead-create", "bead.create", beadCreateRequest{
		Rig:   "myrig",
		Title: "Fix login bug",
		Type:  "task",
	})

	if body.ID == "" || body.Title != "Fix login bug" {
		t.Fatalf("bead.create = %+v, want created Fix login bug bead", body)
	}
	got, err := state.stores["myrig"].Get(body.ID)
	if err != nil {
		t.Fatalf("Get(created): %v", err)
	}
	if got.Title != "Fix login bug" {
		t.Fatalf("created bead in store = %+v, want Fix login bug", got)
	}
}

func TestRouteMatrixParity_GET_v0_bead_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	bead, err := state.stores["myrig"].Create(beads.Bead{Title: "Inspect me", Type: "task"})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[beads.Bead](t, conn, "route-bead-get", "bead.get", socketIDPayload{ID: bead.ID})

	if body.ID != bead.ID || body.Title != "Inspect me" {
		t.Fatalf("bead.get = %+v, want %q Inspect me", body, bead.ID)
	}
}

func TestRouteMatrixParity_GET_v0_bead_id_deps_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	parent, err := store.Create(beads.Bead{Title: "Parent", Type: "task"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "Child", Type: "task", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[struct {
		Children []beads.Bead `json:"children"`
	}](t, conn, "route-bead-deps", "bead.deps", socketIDPayload{ID: parent.ID})

	if len(body.Children) != 1 || body.Children[0].ID != child.ID {
		t.Fatalf("bead.deps = %+v, want child %q", body, child.ID)
	}
}

func TestRouteMatrixParity_POST_v0_bead_id_close_ViaWS(t *testing.T) {
	state := newFakeState(t)
	bead, err := state.stores["myrig"].Create(beads.Bead{Title: "Close me", Type: "task"})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[map[string]string](t, conn, "route-bead-close", "bead.close", socketIDPayload{ID: bead.ID})

	if body["status"] != "closed" {
		t.Fatalf("bead.close body = %+v, want closed", body)
	}
	got, err := state.stores["myrig"].Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(closed): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("closed bead status = %q, want closed", got.Status)
	}
}

func TestRouteMatrixParity_POST_v0_bead_id_reopen_ViaWS(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	bead, err := store.Create(beads.Bead{Title: "Reopen me", Type: "task"})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close(bead): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[map[string]string](t, conn, "route-bead-reopen", "bead.reopen", socketIDPayload{ID: bead.ID})

	if body["status"] != "reopened" {
		t.Fatalf("bead.reopen body = %+v, want reopened", body)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(reopened): %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("reopened bead status = %q, want open", got.Status)
	}
}

func TestRouteMatrixParity_POST_v0_bead_id_update_ViaWS(t *testing.T) {
	assertRouteMatrixBeadUpdateViaWS(t, "route-bead-update-post")
}

func TestRouteMatrixParity_PATCH_v0_bead_id_ViaWS(t *testing.T) {
	assertRouteMatrixBeadUpdateViaWS(t, "route-bead-update-patch")
}

func TestRouteMatrixParity_POST_v0_bead_id_assign_ViaWS(t *testing.T) {
	state := newFakeState(t)
	bead, err := state.stores["myrig"].Create(beads.Bead{Title: "Assign me", Type: "task"})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[map[string]string](t, conn, "route-bead-assign", "bead.assign", socketBeadAssignPayload{
		ID:       bead.ID,
		Assignee: "worker-1",
	})

	if body["status"] != "assigned" {
		t.Fatalf("bead.assign body = %+v, want assigned", body)
	}
	got, err := state.stores["myrig"].Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(assigned): %v", err)
	}
	if got.Assignee != "worker-1" {
		t.Fatalf("assigned bead = %+v, want worker-1", got)
	}
}

func TestRouteMatrixParity_DELETE_v0_bead_id_ViaWS(t *testing.T) {
	state := newFakeState(t)
	bead, err := state.stores["myrig"].Create(beads.Bead{Title: "Delete me", Type: "task"})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[map[string]string](t, conn, "route-bead-delete", "bead.delete", socketIDPayload{ID: bead.ID})

	if body["status"] != "deleted" {
		t.Fatalf("bead.delete body = %+v, want deleted", body)
	}
	got, err := state.stores["myrig"].Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(deleted): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("deleted bead status = %q, want closed", got.Status)
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

func TestRouteMatrixParity_POST_v0_rigs_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rig-create",
		Action: "rig.create",
		Payload: map[string]any{
			"name": "new-rig",
			"path": "/tmp/new-rig",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-rig-create" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	found := false
	for _, rig := range state.cfg.Rigs {
		if rig.Name == "new-rig" && rig.Path == "/tmp/new-rig" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("rigs = %+v, want created new-rig", state.cfg.Rigs)
	}
}

func TestRouteMatrixParity_PATCH_v0_rig_name_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rig-update",
		Action: "rig.update",
		Payload: map[string]any{
			"name": "myrig",
			"path": "/tmp/updated",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-rig-update" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if got := state.cfg.Rigs[0].Path; got != "/tmp/updated" {
		t.Fatalf("path = %q, want /tmp/updated", got)
	}
}

func TestRouteMatrixParity_DELETE_v0_rig_name_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rig-delete",
		Action: "rig.delete",
		Payload: map[string]any{
			"name": "myrig",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-rig-delete" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Rigs) != 0 {
		t.Fatalf("rigs = %+v, want myrig removed", state.cfg.Rigs)
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

func TestRouteMatrixParity_PUT_v0_patches_agents_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	suspended := true

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-patch-set",
		Action: "patches.agents.set",
		Payload: config.AgentPatch{
			Dir:       "rig1",
			Name:      "worker",
			Suspended: &suspended,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-patch-set" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Patches.Agents) != 1 {
		t.Fatalf("agent patches = %+v, want one patch", state.cfg.Patches.Agents)
	}
}

func TestRouteMatrixParity_DELETE_v0_patches_agent_name_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	suspended := true
	state.cfg.Patches.Agents = []config.AgentPatch{{Dir: "rig1", Name: "worker", Suspended: &suspended}}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-agent-patch-delete",
		Action: "patches.agent.delete",
		Payload: map[string]any{
			"name": "rig1/worker",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-agent-patch-delete" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Patches.Agents) != 0 {
		t.Fatalf("agent patches = %+v, want empty after delete", state.cfg.Patches.Agents)
	}
}

func TestRouteMatrixParity_PUT_v0_patches_rigs_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	suspended := true

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rig-patch-set",
		Action: "patches.rigs.set",
		Payload: config.RigPatch{
			Name:      "myrig",
			Suspended: &suspended,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-rig-patch-set" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Patches.Rigs) != 1 {
		t.Fatalf("rig patches = %+v, want one patch", state.cfg.Patches.Rigs)
	}
}

func TestRouteMatrixParity_DELETE_v0_patches_rig_name_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	suspended := true
	state.cfg.Patches.Rigs = []config.RigPatch{{Name: "myrig", Suspended: &suspended}}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-rig-patch-delete",
		Action: "patches.rig.delete",
		Payload: map[string]any{
			"name": "myrig",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-rig-patch-delete" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Patches.Rigs) != 0 {
		t.Fatalf("rig patches = %+v, want empty after delete", state.cfg.Patches.Rigs)
	}
}

func TestRouteMatrixParity_PUT_v0_patches_providers_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	cmd := "my-claude"

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-provider-patch-set",
		Action: "patches.providers.set",
		Payload: config.ProviderPatch{
			Name:    "claude",
			Command: &cmd,
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-provider-patch-set" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Patches.Providers) != 1 {
		t.Fatalf("provider patches = %+v, want one patch", state.cfg.Patches.Providers)
	}
}

func TestRouteMatrixParity_DELETE_v0_patches_provider_name_ViaWS(t *testing.T) {
	state, _, conn := openRouteMatrixMutatorSocket(t)
	cmd := "my-claude"
	state.cfg.Patches.Providers = []config.ProviderPatch{{Name: "claude", Command: &cmd}}

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-provider-patch-delete",
		Action: "patches.provider.delete",
		Payload: map[string]any{
			"name": "claude",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-provider-patch-delete" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if len(state.cfg.Patches.Providers) != 0 {
		t.Fatalf("provider patches = %+v, want empty after delete", state.cfg.Patches.Providers)
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

func TestRouteMatrixParity_POST_v0_extmsg_inbound_ViaWS(t *testing.T) {
	state, conn, conversation, sessionID := openRouteMatrixExtMsgSocket(t)
	routeMatrixBindExtMsgConversation(t, state, conversation, sessionID)

	body := routeMatrixWSRequestResult[struct {
		Message         extmsg.ExternalInboundMessage
		Binding         *extmsg.SessionBindingRecord
		TranscriptEntry *extmsg.ConversationTranscriptRecord
		TargetSessionID string
	}](t, conn, "route-extmsg-inbound", "extmsg.inbound", extmsgInboundRequest{
		Message: &extmsg.ExternalInboundMessage{
			ProviderMessageID: "msg-in-1",
			Conversation:      conversation,
			Actor:             extmsg.ExternalActor{ID: "user-1", DisplayName: "User 1"},
			Text:              "hello from extmsg",
			ReceivedAt:        time.Now().UTC(),
		},
	})

	if body.TargetSessionID != sessionID {
		t.Fatalf("target session id = %q, want %q", body.TargetSessionID, sessionID)
	}
	if body.Binding == nil || body.Binding.SessionID != sessionID {
		t.Fatalf("binding = %+v, want session %q", body.Binding, sessionID)
	}
	if body.TranscriptEntry == nil || body.TranscriptEntry.ProviderMessageID != "msg-in-1" {
		t.Fatalf("transcript entry = %+v, want msg-in-1", body.TranscriptEntry)
	}
}

func TestRouteMatrixParity_POST_v0_extmsg_outbound_ViaWS(t *testing.T) {
	state, conn, conversation, sessionID := openRouteMatrixExtMsgSocket(t)
	routeMatrixBindExtMsgConversation(t, state, conversation, sessionID)
	state.adapterReg.Register(extmsg.AdapterKey{Provider: conversation.Provider, AccountID: conversation.AccountID}, &routeMatrixExtMsgAdapter{
		name: "discord-main",
		receipt: &extmsg.PublishReceipt{
			MessageID:    "msg-out-1",
			Conversation: conversation,
			Delivered:    true,
		},
	})

	body := routeMatrixWSRequestResult[struct {
		Receipt         extmsg.PublishReceipt
		DeliveryContext *extmsg.DeliveryContextRecord
		TranscriptEntry *extmsg.ConversationTranscriptRecord
	}](t, conn, "route-extmsg-outbound", "extmsg.outbound", extmsgOutboundRequest{
		SessionID:    sessionID,
		Conversation: conversation,
		Text:         "reply from session",
	})

	if !body.Receipt.Delivered || body.Receipt.MessageID != "msg-out-1" {
		t.Fatalf("receipt = %+v, want delivered msg-out-1", body.Receipt)
	}
	if body.DeliveryContext == nil || body.DeliveryContext.SessionID != sessionID {
		t.Fatalf("delivery context = %+v, want session %q", body.DeliveryContext, sessionID)
	}
	if body.TranscriptEntry == nil || body.TranscriptEntry.SourceSessionID != sessionID {
		t.Fatalf("transcript entry = %+v, want source session %q", body.TranscriptEntry, sessionID)
	}
}

func TestRouteMatrixParity_GET_v0_extmsg_bindings_ViaWS(t *testing.T) {
	state, conn, conversation, sessionID := openRouteMatrixExtMsgSocket(t)
	routeMatrixBindExtMsgConversation(t, state, conversation, sessionID)

	body := routeMatrixWSRequestResult[struct {
		Items []extmsg.SessionBindingRecord `json:"items"`
		Total int                           `json:"total"`
	}](t, conn, "route-extmsg-bindings", "extmsg.bindings.list", socketExtMsgBindingsPayload{
		SessionID: sessionID,
	})

	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("bindings list = %+v, want one item", body)
	}
	if body.Items[0].SessionID != sessionID || body.Items[0].Conversation.ConversationID != conversation.ConversationID {
		t.Fatalf("binding item = %+v, want session %q conversation %q", body.Items[0], sessionID, conversation.ConversationID)
	}
}

func TestRouteMatrixParity_POST_v0_extmsg_bind_ViaWS(t *testing.T) {
	state, conn, conversation, sessionID := openRouteMatrixExtMsgSocket(t)

	body := routeMatrixWSRequestResult[extmsg.SessionBindingRecord](t, conn, "route-extmsg-bind", "extmsg.bind", extmsgBindRequest{
		Conversation: conversation,
		SessionID:    sessionID,
		Metadata:     map[string]string{"topic": "ops"},
	})

	if body.SessionID != sessionID || body.Conversation.ConversationID != conversation.ConversationID {
		t.Fatalf("binding = %+v, want session %q conversation %q", body, sessionID, conversation.ConversationID)
	}
	if body.Metadata["topic"] != "ops" {
		t.Fatalf("binding metadata = %+v, want topic=ops", body.Metadata)
	}
	if got, err := state.extmsgSvc.Bindings.ListBySession(context.Background(), sessionID); err != nil || len(got) != 1 {
		t.Fatalf("ListBySession after bind = %+v, %v, want one binding", got, err)
	}
}

func TestRouteMatrixParity_POST_v0_extmsg_unbind_ViaWS(t *testing.T) {
	state, conn, conversation, sessionID := openRouteMatrixExtMsgSocket(t)
	routeMatrixBindExtMsgConversation(t, state, conversation, sessionID)

	body := routeMatrixWSRequestResult[struct {
		Unbound []extmsg.SessionBindingRecord `json:"unbound"`
	}](t, conn, "route-extmsg-unbind", "extmsg.unbind", extmsgUnbindRequest{
		Conversation: &conversation,
		SessionID:    sessionID,
	})

	if len(body.Unbound) != 1 || body.Unbound[0].SessionID != sessionID {
		t.Fatalf("unbind body = %+v, want one unbound binding for %q", body, sessionID)
	}
	got, err := state.extmsgSvc.Bindings.ListBySession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListBySession after unbind: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("bindings after unbind = %+v, want empty", got)
	}
}

func TestRouteMatrixParity_GET_v0_extmsg_groups_ViaWS(t *testing.T) {
	state, conn, conversation, _ := openRouteMatrixExtMsgSocket(t)
	group := routeMatrixEnsureExtMsgGroup(t, state, conversation, "alpha")

	body := routeMatrixWSRequestResult[extmsg.ConversationGroupRecord](t, conn, "route-extmsg-group-lookup", "extmsg.groups.lookup", extmsgGroupLookupRequest{
		ScopeID:        conversation.ScopeID,
		Provider:       conversation.Provider,
		AccountID:      conversation.AccountID,
		ConversationID: conversation.ConversationID,
		Kind:           string(conversation.Kind),
	})

	if body.ID != group.ID || body.RootConversation.ConversationID != conversation.ConversationID {
		t.Fatalf("group lookup = %+v, want %q for %q", body, group.ID, conversation.ConversationID)
	}
}

func TestRouteMatrixParity_POST_v0_extmsg_groups_ViaWS(t *testing.T) {
	_, conn, conversation, _ := openRouteMatrixExtMsgSocket(t)

	body := routeMatrixWSRequestResult[extmsg.ConversationGroupRecord](t, conn, "route-extmsg-group-ensure", "extmsg.groups.ensure", extmsgGroupEnsureRequest{
		RootConversation: conversation,
		Mode:             extmsg.GroupModeLauncher,
		DefaultHandle:    "alpha",
		Metadata:         map[string]string{"topic": "ops"},
	})

	if body.ID == "" || body.RootConversation.ConversationID != conversation.ConversationID {
		t.Fatalf("group ensure = %+v, want created group for %q", body, conversation.ConversationID)
	}
	if body.DefaultHandle != "alpha" || body.Metadata["topic"] != "ops" {
		t.Fatalf("group ensure = %+v, want default handle alpha + topic ops", body)
	}
}

func TestRouteMatrixParity_POST_v0_extmsg_participants_ViaWS(t *testing.T) {
	state, conn, conversation, sessionID := openRouteMatrixExtMsgSocket(t)
	group := routeMatrixEnsureExtMsgGroup(t, state, conversation, "alpha")

	body := routeMatrixWSRequestResult[extmsg.ConversationGroupParticipant](t, conn, "route-extmsg-participant-upsert", "extmsg.participant.upsert", extmsgParticipantUpsertRequest{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessionID,
		Public:    true,
		Metadata:  map[string]string{"team": "ops"},
	})

	if body.GroupID != group.ID || body.SessionID != sessionID || body.Handle != "alpha" {
		t.Fatalf("participant = %+v, want alpha/%q in %q", body, sessionID, group.ID)
	}
	if !body.Public || body.Metadata["team"] != "ops" {
		t.Fatalf("participant = %+v, want public ops participant", body)
	}
}

func TestRouteMatrixParity_DELETE_v0_extmsg_participants_ViaWS(t *testing.T) {
	state, conn, conversation, sessionID := openRouteMatrixExtMsgSocket(t)
	group := routeMatrixEnsureExtMsgGroup(t, state, conversation, "alpha")
	if _, err := state.extmsgSvc.Groups.UpsertParticipant(context.Background(), routeMatrixExtMsgCaller(), extmsg.UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessionID,
	}); err != nil {
		t.Fatalf("UpsertParticipant(setup): %v", err)
	}

	body := routeMatrixWSRequestResult[map[string]string](t, conn, "route-extmsg-participant-remove", "extmsg.participant.remove", extmsgParticipantRemoveRequest{
		GroupID: group.ID,
		Handle:  "alpha",
	})

	if body["status"] != "removed" {
		t.Fatalf("participant remove body = %+v, want removed", body)
	}
}

func TestRouteMatrixParity_GET_v0_extmsg_transcript_ViaWS(t *testing.T) {
	state, conn, conversation, _ := openRouteMatrixExtMsgSocket(t)
	routeMatrixAppendExtMsgTranscript(t, state, conversation, "msg-1", "first")
	routeMatrixAppendExtMsgTranscript(t, state, conversation, "msg-2", "second")

	body := routeMatrixWSRequestResult[struct {
		Items []extmsg.ConversationTranscriptRecord `json:"items"`
		Total int                                   `json:"total"`
	}](t, conn, "route-extmsg-transcript-list", "extmsg.transcript.list", extmsgTranscriptListRequest{
		ScopeID:        conversation.ScopeID,
		Provider:       conversation.Provider,
		AccountID:      conversation.AccountID,
		ConversationID: conversation.ConversationID,
		Kind:           string(conversation.Kind),
	})

	if body.Total != 2 || len(body.Items) != 2 {
		t.Fatalf("transcript list = %+v, want two items", body)
	}
	if body.Items[0].ProviderMessageID != "msg-1" || body.Items[1].ProviderMessageID != "msg-2" {
		t.Fatalf("transcript items = %+v, want msg-1,msg-2", body.Items)
	}
}

func TestRouteMatrixParity_POST_v0_extmsg_transcript_ack_ViaWS(t *testing.T) {
	state, conn, conversation, sessionID := openRouteMatrixExtMsgSocket(t)
	first := routeMatrixAppendExtMsgTranscript(t, state, conversation, "msg-1", "first")
	routeMatrixAppendExtMsgTranscript(t, state, conversation, "msg-2", "second")
	if _, err := state.extmsgSvc.Transcript.EnsureMembership(context.Background(), extmsg.EnsureMembershipInput{
		Caller:         routeMatrixExtMsgCaller(),
		Conversation:   conversation,
		SessionID:      sessionID,
		BackfillPolicy: extmsg.MembershipBackfillAll,
		Now:            time.Now(),
	}); err != nil {
		t.Fatalf("EnsureMembership(setup): %v", err)
	}

	body := routeMatrixWSRequestResult[map[string]string](t, conn, "route-extmsg-transcript-ack", "extmsg.transcript.ack", extmsgTranscriptAckRequest{
		Conversation: conversation,
		SessionID:    sessionID,
		Sequence:     first.Sequence,
	})

	if body["status"] != "acked" {
		t.Fatalf("transcript ack body = %+v, want acked", body)
	}
	backfill, err := state.extmsgSvc.Transcript.ListBackfill(context.Background(), extmsg.ListBackfillInput{
		Caller:       routeMatrixExtMsgCaller(),
		Conversation: conversation,
		SessionID:    sessionID,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ListBackfill(after ack): %v", err)
	}
	if len(backfill) != 1 || backfill[0].ProviderMessageID != "msg-2" {
		t.Fatalf("backfill after ack = %+v, want only msg-2", backfill)
	}
}

func TestRouteMatrixParity_GET_v0_extmsg_adapters_ViaWS(t *testing.T) {
	state, conn, conversation, _ := openRouteMatrixExtMsgSocket(t)
	state.adapterReg.Register(extmsg.AdapterKey{Provider: conversation.Provider, AccountID: conversation.AccountID}, &routeMatrixExtMsgAdapter{name: "discord-main"})

	body := routeMatrixWSRequestResult[struct {
		Items []adapterInfo `json:"items"`
		Total int           `json:"total"`
	}](t, conn, "route-extmsg-adapters-list", "extmsg.adapters.list", nil)

	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("adapter list = %+v, want one item", body)
	}
	if body.Items[0].Provider != conversation.Provider || body.Items[0].Name != "discord-main" {
		t.Fatalf("adapter item = %+v, want %s discord-main", body.Items[0], conversation.Provider)
	}
}

func TestRouteMatrixParity_POST_v0_extmsg_adapters_ViaWS(t *testing.T) {
	state, conn, conversation, _ := openRouteMatrixExtMsgSocket(t)

	body := routeMatrixWSRequestResult[map[string]string](t, conn, "route-extmsg-adapter-register", "extmsg.adapters.register", extmsgAdapterRegisterRequest{
		Provider:    conversation.Provider,
		AccountID:   conversation.AccountID,
		Name:        "discord-main",
		CallbackURL: "https://example.invalid/extmsg",
	})

	if body["status"] != "registered" || body["name"] != "discord-main" {
		t.Fatalf("adapter register body = %+v, want registered discord-main", body)
	}
	got := state.adapterReg.Lookup(extmsg.AdapterKey{Provider: conversation.Provider, AccountID: conversation.AccountID})
	if got == nil || got.Name() != "discord-main" {
		t.Fatalf("registered adapter = %#v, want discord-main", got)
	}
}

func TestRouteMatrixParity_DELETE_v0_extmsg_adapters_ViaWS(t *testing.T) {
	state, conn, conversation, _ := openRouteMatrixExtMsgSocket(t)
	state.adapterReg.Register(extmsg.AdapterKey{Provider: conversation.Provider, AccountID: conversation.AccountID}, &routeMatrixExtMsgAdapter{name: "discord-main"})

	body := routeMatrixWSRequestResult[map[string]string](t, conn, "route-extmsg-adapter-unregister", "extmsg.adapters.unregister", extmsgAdapterUnregisterRequest{
		Provider:  conversation.Provider,
		AccountID: conversation.AccountID,
	})

	if body["status"] != "unregistered" {
		t.Fatalf("adapter unregister body = %+v, want unregistered", body)
	}
	if got := state.adapterReg.Lookup(extmsg.AdapterKey{Provider: conversation.Provider, AccountID: conversation.AccountID}); got != nil {
		t.Fatalf("adapter after unregister = %#v, want nil", got)
	}
}

func assertRouteMatrixBeadUpdateViaWS(t *testing.T, requestID string) {
	t.Helper()

	state := newFakeState(t)
	store := state.stores["myrig"]
	bead, err := store.Create(beads.Bead{Title: "Update me", Type: "task"})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	_, _, conn := wsSetup(t, state)

	body := routeMatrixWSRequestResult[map[string]string](t, conn, requestID, "bead.update", map[string]any{
		"id":          bead.ID,
		"description": "updated description",
		"labels":      []string{"urgent"},
	})

	if body["status"] != "updated" {
		t.Fatalf("bead.update body = %+v, want updated", body)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(updated): %v", err)
	}
	if got.Description != "updated description" || len(got.Labels) != 1 || got.Labels[0] != "urgent" {
		t.Fatalf("updated bead = %+v, want description+label applied", got)
	}
}

func routeMatrixWSRequestResult[T any](t *testing.T, conn *websocket.Conn, id, action string, payload any) T {
	t.Helper()

	req := wsRequestEnvelope{Type: "request", ID: id, Action: action}
	if payload != nil {
		req.Payload = payload
	}
	writeWSJSON(t, conn, req)

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != id {
		t.Fatalf("%s response = %#v, want correlated response", action, resp)
	}

	var body T
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("decode %s: %v", action, err)
	}
	return body
}

func openRouteMatrixExtMsgSocket(t *testing.T) (*fakeState, *websocket.Conn, extmsg.ConversationRef, string) {
	t.Helper()

	state := newFakeState(t)
	conversation, sessionID := routeMatrixEnableExtMsg(t, state)
	_, _, conn := wsSetup(t, state)
	return state, conn, conversation, sessionID
}

func routeMatrixEnableExtMsg(t *testing.T, state *fakeState) (extmsg.ConversationRef, string) {
	t.Helper()

	store := beads.NewMemStore()
	state.cityBeadStore = store
	services := extmsg.NewServices(store)
	state.extmsgSvc = &services
	state.adapterReg = extmsg.NewAdapterRegistry()
	return extmsg.ConversationRef{
		ScopeID:        state.cityName,
		Provider:       "discord",
		AccountID:      "acct-1",
		ConversationID: "conv-1",
		Kind:           extmsg.ConversationRoom,
	}, "sess-extmsg"
}

func routeMatrixExtMsgCaller() extmsg.Caller {
	return extmsg.Caller{Kind: extmsg.CallerController, ID: "route-matrix"}
}

func routeMatrixBindExtMsgConversation(t *testing.T, state *fakeState, conversation extmsg.ConversationRef, sessionID string) extmsg.SessionBindingRecord {
	t.Helper()

	binding, err := state.extmsgSvc.Bindings.Bind(context.Background(), routeMatrixExtMsgCaller(), extmsg.BindInput{
		Conversation: conversation,
		SessionID:    sessionID,
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("Bind(setup): %v", err)
	}
	return binding
}

func routeMatrixEnsureExtMsgGroup(t *testing.T, state *fakeState, conversation extmsg.ConversationRef, defaultHandle string) extmsg.ConversationGroupRecord {
	t.Helper()

	group, err := state.extmsgSvc.Groups.EnsureGroup(context.Background(), routeMatrixExtMsgCaller(), extmsg.EnsureGroupInput{
		RootConversation: conversation,
		Mode:             extmsg.GroupModeLauncher,
		DefaultHandle:    defaultHandle,
	})
	if err != nil {
		t.Fatalf("EnsureGroup(setup): %v", err)
	}
	return group
}

func routeMatrixAppendExtMsgTranscript(t *testing.T, state *fakeState, conversation extmsg.ConversationRef, messageID, text string) extmsg.ConversationTranscriptRecord {
	t.Helper()

	entry, err := state.extmsgSvc.Transcript.Append(context.Background(), extmsg.AppendTranscriptInput{
		Caller:            routeMatrixExtMsgCaller(),
		Conversation:      conversation,
		Kind:              extmsg.TranscriptMessageInbound,
		Provenance:        extmsg.TranscriptProvenanceLive,
		ProviderMessageID: messageID,
		Text:              text,
		CreatedAt:         time.Now(),
	})
	if err != nil {
		t.Fatalf("AppendTranscript(setup): %v", err)
	}
	return entry
}

type routeMatrixExtMsgAdapter struct {
	name    string
	caps    extmsg.AdapterCapabilities
	receipt *extmsg.PublishReceipt
}

func (a *routeMatrixExtMsgAdapter) Name() string { return a.name }

func (a *routeMatrixExtMsgAdapter) Capabilities() extmsg.AdapterCapabilities { return a.caps }

func (a *routeMatrixExtMsgAdapter) VerifyAndNormalizeInbound(context.Context, extmsg.InboundPayload) (*extmsg.ExternalInboundMessage, error) {
	return nil, extmsg.ErrAdapterUnsupported
}

func (a *routeMatrixExtMsgAdapter) Publish(_ context.Context, req extmsg.PublishRequest) (*extmsg.PublishReceipt, error) {
	if a.receipt != nil {
		return a.receipt, nil
	}
	return &extmsg.PublishReceipt{Conversation: req.Conversation, Delivered: true}, nil
}

func (a *routeMatrixExtMsgAdapter) EnsureChildConversation(context.Context, extmsg.ConversationRef, string) (*extmsg.ConversationRef, error) {
	return nil, extmsg.ErrAdapterUnsupported
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

func openRouteMatrixMutatorSocket(t *testing.T) (*fakeMutatorState, *httptest.Server, *websocket.Conn) {
	t.Helper()

	state := newFakeMutatorState(t)
	srv := New(state)
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	t.Cleanup(func() { _ = conn.Close() })
	drainWSHello(t, conn)
	return state, ts, conn
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

func routeMatrixCreateWorkflowRun(t *testing.T, store beads.Store, formulaName, workflowID, target, scopeKind, scopeRef string) {
	t.Helper()

	root, err := store.Create(beads.Bead{
		Title: "Workflow root",
		Ref:   formulaName,
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      workflowID,
			"gc.run_target":       target,
			"gc.scope_kind":       scopeKind,
			"gc.scope_ref":        scopeRef,
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	inProgress := "in_progress"
	assignee := target
	if err := store.Update(root.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("set workflow in_progress: %v", err)
	}
}

func routeMatrixWriteWorkflowFormula(t *testing.T, dir, name string) {
	t.Helper()
	writeTestFormula(t, dir, name, `
description = "Preview {{issue}}"
formula = "`+name+`"
version = 2

[vars]
[vars.issue]
description = "Issue bead ID"
required = true

[[steps]]
id = "prep"
title = "Prep {{issue}}"
`)
}

func routeMatrixFormulaDetail(t *testing.T) formulaDetailResponse {
	t.Helper()

	state := newFakeState(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	routeMatrixWriteWorkflowFormula(t, formulaDir, "daily")
	_, _, conn := wsSetup(t, state)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "route-formula-detail",
		Action: "formula.get",
		Payload: map[string]any{
			"name":       "daily",
			"scope_kind": "city",
			"scope_ref":  "test-city",
			"target":     "worker",
			"vars": map[string]string{
				"issue": "BD-123",
			},
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "route-formula-detail" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	var detail formulaDetailResponse
	if err := json.Unmarshal(resp.Result, &detail); err != nil {
		t.Fatalf("decode formula.get: %v", err)
	}
	return detail
}

func openRouteMatrixSessionAgentsSocket(t *testing.T) (*websocket.Conn, session.Info) {
	t.Helper()

	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "myrig/worker", "Agents", "claude", t.TempDir(), "claude", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, info.WorkDir, info.SessionKey+".jsonl",
		`{"uuid":"1","type":"assistant","message":{"role":"assistant","content":"parent"}}`,
	)

	projectDir := filepath.Join(searchBase, sessionlog.ProjectSlug(info.WorkDir))
	subagentsDir := filepath.Join(projectDir, info.SessionKey, "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(subagents): %v", err)
	}
	agentPath := filepath.Join(subagentsDir, "agent-myagent.jsonl")
	content := strings.Join([]string{
		`{"uuid":"a1","type":"system","parentToolUseId":"toolu_111"}`,
		`{"uuid":"a2","parentUuid":"a1","type":"user","message":{"role":"user","content":"do task"}}`,
		`{"uuid":"a3","parentUuid":"a2","type":"assistant","message":{"role":"assistant","content":"done"}}`,
		`{"uuid":"a4","parentUuid":"a3","type":"result","message":{"role":"result"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(agentPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(agent transcript): %v", err)
	}

	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	t.Cleanup(func() { _ = conn.Close() })
	drainWSHello(t, conn)
	return conn, info
}
