package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workspacesvc"
	"github.com/gorilla/websocket"
)

type stateResolver struct {
	cities map[string]State
}

func (r *stateResolver) ListCities() []CityInfo {
	items := make([]CityInfo, 0, len(r.cities))
	for name, state := range r.cities {
		items = append(items, CityInfo{
			Name:    name,
			Path:    state.CityPath(),
			Running: true,
		})
	}
	return items
}

func (r *stateResolver) CityState(name string) State {
	return r.cities[name]
}

func newTestSupervisorMuxWithStates(t *testing.T, cities map[string]State) *SupervisorMux {
	t.Helper()
	return NewSupervisorMux(&stateResolver{cities: cities}, false, "test", time.Now())
}

func expectClientSocketAction(t *testing.T, conn *websocket.Conn, wantAction string, wantPayload map[string]any) {
	t.Helper()
	var req struct {
		Type    string         `json:"type"`
		ID      string         `json:"id"`
		Action  string         `json:"action"`
		Payload map[string]any `json:"payload"`
	}
	if err := conn.ReadJSON(&req); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if req.Type != "request" {
		t.Fatalf("request type = %q, want request", req.Type)
	}
	if req.Action != wantAction {
		t.Fatalf("request action = %q, want %q", req.Action, wantAction)
	}
	for key, want := range wantPayload {
		if got := req.Payload[key]; got != want {
			t.Fatalf("payload[%q] = %#v, want %#v", key, got, want)
		}
	}
}

func TestClientSuspendCity(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "city.patch" {
			t.Errorf("action = %q, want city.patch", action)
		}
		payload, _ := req["payload"].(map[string]any)
		if payload["suspended"] != true {
			t.Errorf("suspended = %v, want true", payload["suspended"])
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]string{"status": "ok"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.SuspendCity(); err != nil {
		t.Fatalf("SuspendCity: %v", err)
	}
}

func TestClientResumeCity(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		payload, _ := req["payload"].(map[string]any)
		if payload["suspended"] != false {
			t.Errorf("suspended = %v, want false", payload["suspended"])
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]string{"status": "ok"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.ResumeCity(); err != nil {
		t.Fatalf("ResumeCity: %v", err)
	}
}

func TestClientSuspendAgent(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "agent.suspend" {
			t.Errorf("action = %q, want agent.suspend", action)
		}
		payload, _ := req["payload"].(map[string]any)
		if payload["name"] != "worker" {
			t.Errorf("name = %v, want worker", payload["name"])
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]string{"status": "ok"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}
}

func TestClientResumeAgent(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "agent.resume" {
			t.Errorf("action = %q, want agent.resume", action)
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]string{"status": "ok"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.ResumeAgent("worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
}

func TestClientSuspendRig(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "rig.suspend" {
			t.Errorf("action = %q, want rig.suspend", action)
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]string{"status": "ok"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.SuspendRig("myrig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}
}

func TestClientResumeRig(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "rig.resume" {
			t.Errorf("action = %q, want rig.resume", action)
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]string{"status": "ok"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.ResumeRig("myrig"); err != nil {
		t.Fatalf("ResumeRig: %v", err)
	}
}

func TestClientErrorResponse(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		_ = conn.WriteJSON(map[string]any{"type": "error", "id": id, "code": "not_found", "message": "agent 'nope' not found"})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	err := c.SuspendAgent("nope")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "API error: agent 'nope' not found" {
		t.Errorf("error = %q", got)
	}
}

func TestClientQualifiedAgentName(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		payload, _ := req["payload"].(map[string]any)
		if payload["name"] != "myrig/worker" {
			t.Errorf("name = %v, want myrig/worker", payload["name"])
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]string{"status": "ok"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.SuspendAgent("myrig/worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}
}

func TestClientConnError(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	err := c.SuspendCity()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsConnError(err) {
		t.Errorf("IsConnError = false for connection refused error: %v", err)
	}
}

func TestClientAPIErrorNotConnError(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		_ = conn.WriteJSON(map[string]any{"type": "error", "id": id, "code": "bad_request", "message": "invalid"})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	err := c.SuspendCity()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if IsConnError(err) {
		t.Errorf("IsConnError = true for API error response: %v", err)
	}
}

func TestClientReadOnlyFallback(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		_ = conn.WriteJSON(map[string]any{"type": "error", "id": id, "code": "read_only", "message": "mutations disabled: server bound to non-localhost address"})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	err := c.SuspendCity()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(err) {
		t.Errorf("ShouldFallback = false for read-only rejection: %v", err)
	}
	if IsConnError(err) {
		t.Errorf("IsConnError = true for read-only rejection (should be false)")
	}
}

func TestClientConnErrorShouldFallback(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	err := c.SuspendCity()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(err) {
		t.Errorf("ShouldFallback = false for connection error: %v", err)
	}
}

func TestClientBusinessErrorNoFallback(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		_ = conn.WriteJSON(map[string]any{"type": "error", "id": id, "code": "not_found", "message": "agent 'nope' not found"})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	err := c.SuspendAgent("nope")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if ShouldFallback(err) {
		t.Errorf("ShouldFallback = true for business error: %v", err)
	}
}

func TestClientRestartRig(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "rig.restart" {
			t.Errorf("action = %q, want rig.restart", action)
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]string{"status": "ok"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.RestartRig("myrig"); err != nil {
		t.Fatalf("RestartRig: %v", err)
	}
}

func TestClientListServices(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "services.list" {
			t.Errorf("action = %q, want services.list", action)
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]any{
			"items": []map[string]any{{"service_name": "healthz", "kind": "workflow", "state": "ready"}},
			"total": 1,
		}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	items, err := c.ListServices()
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(items) != 1 || items[0].ServiceName != "healthz" {
		t.Fatalf("items = %#v, want one healthz service", items)
	}
}

func TestClientGetService(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "service.get" {
			t.Errorf("action = %q, want service.get", action)
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]any{"service_name": "healthz", "kind": "workflow", "state": "ready"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	status, err := c.GetService("healthz")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if status.ServiceName != "healthz" {
		t.Fatalf("ServiceName = %q, want healthz", status.ServiceName)
	}
}

func TestClientListCities(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "cities.list" {
			t.Errorf("action = %q, want cities.list", action)
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]any{
			"items": []map[string]any{{"name": "bright-lights", "path": "/tmp/bright-lights", "running": true}},
			"total": 1,
		}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	items, err := c.ListCities()
	if err != nil {
		t.Fatalf("ListCities: %v", err)
	}
	if len(items) != 1 || items[0].Name != "bright-lights" || !items[0].Running {
		t.Fatalf("items = %#v, want one running bright-lights city", items)
	}
}

func TestCityScopedClientRewritesPaths(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		scope, _ := req["scope"].(map[string]any)
		if scope["city"] != "bright-lights" {
			t.Errorf("scope.city = %v, want bright-lights", scope["city"])
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]any{"items": []any{}, "total": 0}})
	})
	defer srv.Close()
	c := NewCityScopedClient(srv.URL, "bright-lights")
	defer c.Close()
	if _, err := c.ListServices(); err != nil {
		t.Fatalf("ListServices: %v", err)
	}
}

func TestClientKillSession(t *testing.T) {
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		if action != "session.kill" {
			t.Errorf("action = %q, want session.kill", action)
		}
		payload, _ := req["payload"].(map[string]any)
		if payload["id"] != "sess-123" {
			t.Errorf("payload.id = %v, want sess-123", payload["id"])
		}
		_ = conn.WriteJSON(map[string]any{"type": "response", "id": id, "result": map[string]string{"status": "ok"}})
	})
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	if err := c.KillSession("sess-123"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
}

func TestClientListCitiesUsesWebSocketWhenAvailable(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	s2 := newFakeState(t)
	s2.cityName = "beta"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s1,
		"beta":  s2,
	})
	base := sm.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	items, err := c.ListCities()
	if err != nil {
		t.Fatalf("ListCities: %v", err)
	}
	if len(items) != 2 || items[0].Name != "alpha" || items[1].Name != "beta" {
		t.Fatalf("items = %#v, want alpha then beta", items)
	}
}

func TestClientSuspendCityUsesWebSocketWhenAvailable(t *testing.T) {
	state := newFakeMutatorState(t)
	base := New(state).handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	if err := c.SuspendCity(); err != nil {
		t.Fatalf("SuspendCity: %v", err)
	}
	if !state.cfg.Workspace.Suspended {
		t.Fatal("city suspended = false, want true")
	}
	if err := c.ResumeCity(); err != nil {
		t.Fatalf("ResumeCity: %v", err)
	}
	if state.cfg.Workspace.Suspended {
		t.Fatal("city suspended = true after resume, want false")
	}
}

func TestClientCityScopedServicesUseWebSocketWhenAvailable(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName:      "healthz",
			Kind:             "workflow",
			MountPath:        "/svc/healthz",
			PublishMode:      "private",
			State:            "ready",
			LocalState:       "ready",
			PublicationState: "private",
		}},
	}
	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"bright-lights": state,
	})
	base := sm.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "bright-lights")
	items, err := c.ListServices()
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(items) != 1 || items[0].ServiceName != "healthz" {
		t.Fatalf("ListServices items = %#v, want one healthz service", items)
	}

	status, err := c.GetService("healthz")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if status.ServiceName != "healthz" {
		t.Fatalf("GetService service = %#v, want healthz", status)
	}
}

func TestClientRestartServiceUsesWebSocketWhenAvailable(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName:      "healthz",
			Kind:             "workflow",
			MountPath:        "/svc/healthz",
			PublishMode:      "private",
			State:            "ready",
			LocalState:       "ready",
			PublicationState: "private",
		}},
	}
	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"bright-lights": state,
	})
	base := sm.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "bright-lights")
	if err := c.RestartService("healthz"); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
}

func TestClientAgentAndRigActionsUseWebSocketWhenAvailable(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityName = "bright-lights"
	sm := newTestSupervisorMuxWithStates(t, map[string]State{
		"bright-lights": state,
	})
	base := sm.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "bright-lights")
	if err := c.SuspendAgent("myrig/worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}
	if !state.suspended["myrig/worker"] {
		t.Fatal("agent suspended = false, want true")
	}
	if err := c.ResumeAgent("myrig/worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	if state.suspended["myrig/worker"] {
		t.Fatal("agent suspended = true after resume, want false")
	}
	if err := c.SuspendRig("myrig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}
	if err := c.ResumeRig("myrig"); err != nil {
		t.Fatalf("ResumeRig: %v", err)
	}
	if err := c.RestartRig("myrig"); err != nil {
		t.Fatalf("RestartRig: %v", err)
	}
}

func TestClientSessionActionsUseWebSocketWhenAvailable(t *testing.T) {
	state := newSessionFakeState(t)
	state.cityName = "bright-lights"
	info := createTestSession(t, state.cityBeadStore, state.sp, "Socket Session")

	sm := newTestSupervisorMuxWithStates(t, map[string]State{
		"bright-lights": state,
	})
	base := sm.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "bright-lights")
	resp, err := c.SubmitSession(info.ID, "please summarize city status", "")
	if err != nil {
		t.Fatalf("SubmitSession: %v", err)
	}
	if resp.Status != "accepted" {
		t.Fatalf("SubmitSession status = %q, want accepted", resp.Status)
	}
	if resp.ID != info.ID {
		t.Fatalf("SubmitSession id = %q, want %q", resp.ID, info.ID)
	}

	if err := c.KillSession(info.ID); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
}

func TestClientGetJSONUsesWebSocketForMailAndSessionReads(t *testing.T) {
	state := newSessionFakeState(t)
	state.cityName = "bright-lights"
	pendingInfo := createTestSession(t, state.cityBeadStore, state.sp, "Pending Session")
	state.sp.SetPendingInteraction(pendingInfo.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	transcriptInfo := createTestSession(t, state.cityBeadStore, state.sp, "Transcript Session")
	msg, err := state.cityMailProv.Send("mayor", "worker", "Review needed", "Please review")
	if err != nil {
		t.Fatalf("Send(msg): %v", err)
	}
	searchBase := t.TempDir()
	srv := New(state)
	srv.sessionLogSearchPaths = []string{searchBase}
	writeNamedSessionJSONL(t, searchBase, transcriptInfo.WorkDir, transcriptInfo.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := session.NewManager(state.cityBeadStore, state.sp).Close(transcriptInfo.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		srv.handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "bright-lights")

	mailBody, err := c.GetJSON("/v0/mail/" + msg.ID)
	if err != nil {
		t.Fatalf("GetJSON(mail): %v", err)
	}
	if !bytes.Contains(mailBody, []byte(`"subject":"Review needed"`)) {
		t.Fatalf("mail body = %s, want Review needed", mailBody)
	}

	pendingBody, err := c.GetJSON("/v0/session/" + pendingInfo.ID + "/pending")
	if err != nil {
		t.Fatalf("GetJSON(pending): %v", err)
	}
	if !bytes.Contains(pendingBody, []byte(`"request_id":"req-1"`)) {
		t.Fatalf("pending body = %s, want req-1", pendingBody)
	}

	transcriptBody, err := c.GetJSON("/v0/session/" + transcriptInfo.ID + "/transcript?tail=1")
	if err != nil {
		t.Fatalf("GetJSON(transcript): %v", err)
	}
	if !bytes.Contains(transcriptBody, []byte(`"world"`)) {
		t.Fatalf("transcript body = %s, want websocket transcript payload", transcriptBody)
	}
}

func TestClientGetJSONUsesWebSocketForConfigAndProviderReads(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cfg.Packs = map[string]config.PackSource{
		"base": {Source: "github", Ref: "main", Path: "packs/base"},
	}
	srv := New(state)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		srv.handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "bright-lights")

	cityBody, err := c.GetJSON("/v0/city")
	if err != nil {
		t.Fatalf("GetJSON(city): %v", err)
	}
	if !bytes.Contains(cityBody, []byte(`"name":"bright-lights"`)) {
		t.Fatalf("city body = %s, want bright-lights", cityBody)
	}

	configBody, err := c.GetJSON("/v0/config")
	if err != nil {
		t.Fatalf("GetJSON(config): %v", err)
	}
	if !bytes.Contains(configBody, []byte(`"workspace"`)) {
		t.Fatalf("config body = %s, want workspace payload", configBody)
	}

	packsBody, err := c.GetJSON("/v0/packs")
	if err != nil {
		t.Fatalf("GetJSON(packs): %v", err)
	}
	if !bytes.Contains(packsBody, []byte(`"base"`)) {
		t.Fatalf("packs body = %s, want base pack", packsBody)
	}

	providersBody, err := c.GetJSON("/v0/providers?view=public")
	if err != nil {
		t.Fatalf("GetJSON(providers): %v", err)
	}
	if !bytes.Contains(providersBody, []byte(`"items"`)) {
		t.Fatalf("providers body = %s, want list response", providersBody)
	}

	providerBody, err := c.GetJSON("/v0/provider/claude")
	if err != nil {
		t.Fatalf("GetJSON(provider): %v", err)
	}
	if !bytes.Contains(providerBody, []byte(`"name":"claude"`)) {
		t.Fatalf("provider body = %s, want claude provider", providerBody)
	}
}

func TestClientGetJSONUsesWebSocketForPaginatedListReads(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			t.Fatalf("unexpected HTTP path %s; expected websocket-only transport", r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{"type": "hello"}); err != nil {
			t.Fatalf("write hello: %v", err)
		}

		expectClientSocketAction(t, conn, "beads.list", map[string]any{
			"status": "open",
			"limit":  float64(50),
			"cursor": "c1",
		})
		if err := conn.WriteJSON(map[string]any{
			"type": "response",
			"id":   "cli-1",
			"result": map[string]any{
				"items": []any{},
				"total": 0,
			},
		}); err != nil {
			t.Fatalf("write beads.list response: %v", err)
		}

		expectClientSocketAction(t, conn, "events.list", map[string]any{
			"since":  "1h",
			"limit":  float64(10),
			"cursor": "c2",
		})
		if err := conn.WriteJSON(map[string]any{
			"type": "response",
			"id":   "cli-2",
			"result": map[string]any{
				"items": []any{},
				"total": 0,
			},
		}); err != nil {
			t.Fatalf("write events.list response: %v", err)
		}

		expectClientSocketAction(t, conn, "mail.list", map[string]any{
			"status": "all",
			"limit":  float64(25),
			"cursor": "c3",
		})
		if err := conn.WriteJSON(map[string]any{
			"type": "response",
			"id":   "cli-3",
			"result": map[string]any{
				"items": []any{},
				"total": 0,
			},
		}); err != nil {
			t.Fatalf("write mail.list response: %v", err)
		}

		expectClientSocketAction(t, conn, "sessions.list", map[string]any{
			"state":  "active",
			"peek":   true,
			"limit":  float64(2),
			"cursor": "c4",
		})
		if err := conn.WriteJSON(map[string]any{
			"type": "response",
			"id":   "cli-4",
			"result": map[string]any{
				"items": []any{},
				"total": 0,
			},
		}); err != nil {
			t.Fatalf("write sessions.list response: %v", err)
		}
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	if _, err := c.GetJSON("/v0/beads?status=open&limit=50&cursor=c1"); err != nil {
		t.Fatalf("GetJSON(beads): %v", err)
	}
	if _, err := c.GetJSON("/v0/events?since=1h&limit=10&cursor=c2"); err != nil {
		t.Fatalf("GetJSON(events): %v", err)
	}
	if _, err := c.GetJSON("/v0/mail?status=all&limit=25&cursor=c3"); err != nil {
		t.Fatalf("GetJSON(mail): %v", err)
	}
	if _, err := c.GetJSON("/v0/sessions?state=active&peek=true&limit=2&cursor=c4"); err != nil {
		t.Fatalf("GetJSON(sessions): %v", err)
	}
}

func TestClientGetJSONAndPostJSONUseWebSocketForAgentRigAndSessionRoutes(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			t.Fatalf("unexpected HTTP path %s; expected websocket-only transport", r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{"type": "hello"}); err != nil {
			t.Fatalf("write hello: %v", err)
		}

		expectClientSocketAction(t, conn, "agents.list", map[string]any{"rig": "myrig", "peek": true})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-1", "result": map[string]any{"items": []any{}, "total": 0}}); err != nil {
			t.Fatalf("write agents.list response: %v", err)
		}

		expectClientSocketAction(t, conn, "agent.get", map[string]any{"name": "myrig/worker"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-2", "result": map[string]any{"name": "myrig/worker"}}); err != nil {
			t.Fatalf("write agent.get response: %v", err)
		}

		expectClientSocketAction(t, conn, "rig.get", map[string]any{"name": "myrig"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-3", "result": map[string]any{"name": "myrig"}}); err != nil {
			t.Fatalf("write rig.get response: %v", err)
		}

		expectClientSocketAction(t, conn, "session.get", map[string]any{"id": "sess-1", "peek": true})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-4", "result": map[string]any{"id": "sess-1"}}); err != nil {
			t.Fatalf("write session.get response: %v", err)
		}

		expectClientSocketAction(t, conn, "session.wake", map[string]any{"id": "sess-1"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-5", "result": map[string]any{"status": "ok"}}); err != nil {
			t.Fatalf("write session.wake response: %v", err)
		}

		expectClientSocketAction(t, conn, "session.rename", map[string]any{"id": "sess-1", "title": "Renamed"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-6", "result": map[string]any{"title": "Renamed"}}); err != nil {
			t.Fatalf("write session.rename response: %v", err)
		}

		expectClientSocketAction(t, conn, "session.respond", map[string]any{"id": "sess-1", "action": "approve"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-7", "result": map[string]any{"status": "accepted"}}); err != nil {
			t.Fatalf("write session.respond response: %v", err)
		}
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	if _, err := c.GetJSON("/v0/agents?rig=myrig&peek=true"); err != nil {
		t.Fatalf("GetJSON(agents): %v", err)
	}
	if _, err := c.GetJSON("/v0/agent/myrig/worker"); err != nil {
		t.Fatalf("GetJSON(agent): %v", err)
	}
	if _, err := c.GetJSON("/v0/rig/myrig"); err != nil {
		t.Fatalf("GetJSON(rig): %v", err)
	}
	if _, err := c.GetJSON("/v0/session/sess-1?peek=true"); err != nil {
		t.Fatalf("GetJSON(session): %v", err)
	}
	if _, err := c.PostJSON("/v0/session/sess-1/wake", nil); err != nil {
		t.Fatalf("PostJSON(session wake): %v", err)
	}
	if _, err := c.PostJSON("/v0/session/sess-1/rename", map[string]any{"title": "Renamed"}); err != nil {
		t.Fatalf("PostJSON(session rename): %v", err)
	}
	if _, err := c.PostJSON("/v0/session/sess-1/respond", map[string]any{"action": "approve"}); err != nil {
		t.Fatalf("PostJSON(session respond): %v", err)
	}
}

func TestClientPostJSONUsesWebSocketForMailMutations(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	seed, err := state.cityMailProv.Send("mayor", "worker", "Review needed", "Please review")
	if err != nil {
		t.Fatalf("Send(seed): %v", err)
	}
	srv := New(state)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		srv.handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "bright-lights")

	readBody, err := c.PostJSON("/v0/mail/"+seed.ID+"/read", nil)
	if err != nil {
		t.Fatalf("PostJSON(read): %v", err)
	}
	if !bytes.Contains(readBody, []byte(`"read"`)) {
		t.Fatalf("read body = %s, want read status", readBody)
	}

	replyBody, err := c.PostJSON("/v0/mail/"+seed.ID+"/reply", map[string]any{
		"from":    "worker",
		"subject": "Re: Review needed",
		"body":    "On it",
	})
	if err != nil {
		t.Fatalf("PostJSON(reply): %v", err)
	}
	if !bytes.Contains(replyBody, []byte(`"Re: Review needed"`)) {
		t.Fatalf("reply body = %s, want reply payload", replyBody)
	}

	sendBody, err := c.PostJSON("/v0/mail", map[string]any{
		"from":    "mayor",
		"to":      "worker",
		"subject": "Another review",
		"body":    "Please review this too",
	})
	if err != nil {
		t.Fatalf("PostJSON(send): %v", err)
	}
	if !bytes.Contains(sendBody, []byte(`"Another review"`)) {
		t.Fatalf("send body = %s, want send payload", sendBody)
	}
}

func TestClientPostJSONUsesWebSocketForBeadMutationsAndSling(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			t.Fatalf("unexpected HTTP path %s; expected websocket-only transport", r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{"type": "hello"}); err != nil {
			t.Fatalf("write hello: %v", err)
		}

		expectClientSocketAction(t, conn, "bead.create", map[string]any{
			"title": "New issue",
			"rig":   "myrig",
		})
		if err := conn.WriteJSON(map[string]any{
			"type": "response",
			"id":   "cli-1",
			"result": map[string]any{
				"id":    "gc-1",
				"title": "New issue",
			},
		}); err != nil {
			t.Fatalf("write bead.create response: %v", err)
		}

		expectClientSocketAction(t, conn, "bead.update", map[string]any{
			"id":       "gc-1",
			"assignee": "worker",
		})
		if err := conn.WriteJSON(map[string]any{
			"type":   "response",
			"id":     "cli-2",
			"result": map[string]any{"status": "updated"},
		}); err != nil {
			t.Fatalf("write bead.update response: %v", err)
		}

		expectClientSocketAction(t, conn, "bead.close", map[string]any{
			"id": "gc-1",
		})
		if err := conn.WriteJSON(map[string]any{
			"type":   "response",
			"id":     "cli-3",
			"result": map[string]any{"status": "closed"},
		}); err != nil {
			t.Fatalf("write bead.close response: %v", err)
		}

		expectClientSocketAction(t, conn, "sling.run", map[string]any{
			"target": "myrig/worker",
			"bead":   "gc-1",
		})
		if err := conn.WriteJSON(map[string]any{
			"type": "response",
			"id":   "cli-4",
			"result": map[string]any{
				"status": "slung",
				"target": "myrig/worker",
				"bead":   "gc-1",
			},
		}); err != nil {
			t.Fatalf("write sling.run response: %v", err)
		}
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	createBody, err := c.PostJSON("/v0/beads", map[string]any{"title": "New issue", "rig": "myrig"})
	if err != nil {
		t.Fatalf("PostJSON(bead create): %v", err)
	}
	if !strings.Contains(string(createBody), `"id":"gc-1"`) {
		t.Fatalf("bead create body = %s, want websocket response", createBody)
	}

	updateBody, err := c.PostJSON("/v0/bead/gc-1/update", map[string]any{"assignee": "worker"})
	if err != nil {
		t.Fatalf("PostJSON(bead update): %v", err)
	}
	if !strings.Contains(string(updateBody), `"status":"updated"`) {
		t.Fatalf("bead update body = %s, want websocket response", updateBody)
	}

	closeBody, err := c.PostJSON("/v0/bead/gc-1/close", nil)
	if err != nil {
		t.Fatalf("PostJSON(bead close): %v", err)
	}
	if !strings.Contains(string(closeBody), `"status":"closed"`) {
		t.Fatalf("bead close body = %s, want websocket response", closeBody)
	}

	slingBody, err := c.PostJSON("/v0/sling", map[string]any{"target": "myrig/worker", "bead": "gc-1"})
	if err != nil {
		t.Fatalf("PostJSON(sling): %v", err)
	}
	if !strings.Contains(string(slingBody), `"status":"slung"`) {
		t.Fatalf("sling body = %s, want websocket response", slingBody)
	}
}

func TestClientGetJSONAndPostJSONUseWebSocketForSupplementalMailAndBeadRoutes(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			t.Fatalf("unexpected HTTP path %s; expected websocket-only transport", r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{"type": "hello"}); err != nil {
			t.Fatalf("write hello: %v", err)
		}

		expectClientSocketAction(t, conn, "mail.count", map[string]any{"agent": "worker", "rig": "myrig"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-1", "result": map[string]any{"total": 2, "unread": 1}}); err != nil {
			t.Fatalf("write mail.count response: %v", err)
		}

		expectClientSocketAction(t, conn, "mail.thread", map[string]any{"id": "thread-1", "rig": "myrig"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-2", "result": map[string]any{"items": []any{map[string]any{"id": "mail-1"}}, "total": 1}}); err != nil {
			t.Fatalf("write mail.thread response: %v", err)
		}

		expectClientSocketAction(t, conn, "bead.deps", map[string]any{"id": "gc-1"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-3", "result": map[string]any{"children": []any{map[string]any{"id": "gc-2"}}}}); err != nil {
			t.Fatalf("write bead.deps response: %v", err)
		}

		expectClientSocketAction(t, conn, "beads.graph", map[string]any{"root_id": "gc-root"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-4", "result": map[string]any{"root": map[string]any{"id": "gc-root"}, "beads": []any{map[string]any{"id": "gc-root"}}, "deps": []any{}}}); err != nil {
			t.Fatalf("write beads.graph response: %v", err)
		}

		expectClientSocketAction(t, conn, "mail.mark_unread", map[string]any{"id": "mail-1"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-5", "result": map[string]any{"status": "unread"}}); err != nil {
			t.Fatalf("write mail.mark_unread response: %v", err)
		}

		expectClientSocketAction(t, conn, "mail.archive", map[string]any{"id": "mail-1"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-6", "result": map[string]any{"status": "archived"}}); err != nil {
			t.Fatalf("write mail.archive response: %v", err)
		}

		expectClientSocketAction(t, conn, "bead.reopen", map[string]any{"id": "gc-1"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-7", "result": map[string]any{"status": "reopened"}}); err != nil {
			t.Fatalf("write bead.reopen response: %v", err)
		}

		expectClientSocketAction(t, conn, "bead.assign", map[string]any{"id": "gc-1", "assignee": "worker"})
		if err := conn.WriteJSON(map[string]any{"type": "response", "id": "cli-8", "result": map[string]any{"status": "assigned", "assignee": "worker"}}); err != nil {
			t.Fatalf("write bead.assign response: %v", err)
		}
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	if _, err := c.GetJSON("/v0/mail/count?agent=worker&rig=myrig"); err != nil {
		t.Fatalf("GetJSON(mail count): %v", err)
	}
	if _, err := c.GetJSON("/v0/mail/thread/thread-1?rig=myrig"); err != nil {
		t.Fatalf("GetJSON(mail thread): %v", err)
	}
	if _, err := c.GetJSON("/v0/bead/gc-1/deps"); err != nil {
		t.Fatalf("GetJSON(bead deps): %v", err)
	}
	if _, err := c.GetJSON("/v0/beads/graph/gc-root"); err != nil {
		t.Fatalf("GetJSON(bead graph): %v", err)
	}
	if _, err := c.PostJSON("/v0/mail/mail-1/mark-unread", nil); err != nil {
		t.Fatalf("PostJSON(mail mark unread): %v", err)
	}
	if _, err := c.PostJSON("/v0/mail/mail-1/archive", nil); err != nil {
		t.Fatalf("PostJSON(mail archive): %v", err)
	}
	if _, err := c.PostJSON("/v0/bead/gc-1/reopen", nil); err != nil {
		t.Fatalf("PostJSON(bead reopen): %v", err)
	}
	if _, err := c.PostJSON("/v0/bead/gc-1/assign", map[string]any{"assignee": "worker"}); err != nil {
		t.Fatalf("PostJSON(bead assign): %v", err)
	}
}

func TestClientSupervisorImplicitSingleCityWebSocketRouting(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityName = "bright-lights"
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName:      "healthz",
			Kind:             "workflow",
			MountPath:        "/svc/healthz",
			PublishMode:      "private",
			State:            "ready",
			LocalState:       "ready",
			PublicationState: "private",
		}},
	}
	sm := newTestSupervisorMuxWithStates(t, map[string]State{
		"bright-lights": state,
	})
	base := sm.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	items, err := c.ListServices()
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(items) != 1 || items[0].ServiceName != "healthz" {
		t.Fatalf("ListServices items = %#v, want one healthz service", items)
	}
	if err := c.SuspendCity(); err != nil {
		t.Fatalf("SuspendCity: %v", err)
	}
	if !state.cfg.Workspace.Suspended {
		t.Fatal("city suspended = false, want true")
	}
}

func TestClientSupervisorWebSocketRequiresCityWhenMultipleRunning(t *testing.T) {
	alpha := newFakeState(t)
	alpha.cityName = "alpha"
	alpha.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{ServiceName: "alpha-healthz"}},
	}
	beta := newFakeState(t)
	beta.cityName = "beta"
	beta.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{ServiceName: "beta-healthz"}},
	}
	sm := newTestSupervisorMuxWithStates(t, map[string]State{
		"alpha": alpha,
		"beta":  beta,
	})
	base := sm.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.Error(w, "http disabled for test", http.StatusGone)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	_, err := c.ListServices()
	if err == nil {
		t.Fatal("ListServices error = nil, want city_required")
	}
	if got := err.Error(); got != "API error: multiple cities running; use scope.city to specify which city" {
		t.Fatalf("ListServices error = %q, want city_required", got)
	}
}

// newRawWSServer creates a WS server with a handler that gets the raw connection.
// The handler receives the conn after the hello envelope is sent.
func newRawWSServer(t *testing.T, handler func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		// Send hello.
		_ = conn.WriteJSON(map[string]any{"type": "hello", "protocol": "gc-ws-v0"})
		handler(conn)
	}))
}

// wsTestServer is a test WS server that responds to any request with a
// configurable handler. It properly loops reading messages.
func wsTestServer(t *testing.T, handler func(action string, req map[string]any, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/ws" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "hello", "protocol": "gc-ws-v0"})
		for {
			var raw map[string]any
			if err := conn.ReadJSON(&raw); err != nil {
				return
			}
			action, _ := raw["action"].(string)
			handler(action, raw, conn)
		}
	}))
}

func TestClientSubscribeEvents(t *testing.T) {
	eventSent := make(chan struct{}, 1)
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		switch action {
		case "subscription.start":
			payload, _ := req["payload"].(map[string]any)
			if payload["kind"] != "events" {
				t.Errorf("kind = %v, want events", payload["kind"])
			}
			_ = conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     id,
				"result": map[string]any{"subscription_id": "sub-1", "status": "ok"},
			})
			_ = conn.WriteJSON(map[string]any{
				"type":            "event",
				"subscription_id": "sub-1",
				"event_type":      "bead.created",
				"index":           42,
				"payload":         map[string]any{"id": "bead-abc"},
			})
		case "subscription.stop":
			_ = conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     id,
				"result": map[string]any{"subscription_id": "sub-1", "status": "ok"},
			})
		}
	})
	defer srv.Close()

	c := NewClient(srv.URL)
	defer c.Close()

	var gotEvent SubscriptionEvent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subID, err := c.SubscribeEvents(ctx, 0, func(evt SubscriptionEvent) {
		gotEvent = evt
		select {
		case eventSent <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	if subID != "sub-1" {
		t.Fatalf("subscription_id = %q, want sub-1", subID)
	}

	select {
	case <-eventSent:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for event")
	}

	if gotEvent.EventType != "bead.created" {
		t.Errorf("event_type = %q, want bead.created", gotEvent.EventType)
	}
	if gotEvent.Index != 42 {
		t.Errorf("index = %d, want 42", gotEvent.Index)
	}
}

func TestClientSubscribeSessionStream(t *testing.T) {
	eventSent := make(chan struct{}, 1)
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		switch action {
		case "subscription.start":
			payload, _ := req["payload"].(map[string]any)
			if payload["kind"] != "session.stream" {
				t.Errorf("kind = %v, want session.stream", payload["kind"])
			}
			if payload["target"] != "sess-abc" {
				t.Errorf("target = %v, want sess-abc", payload["target"])
			}
			if payload["format"] != "jsonl" {
				t.Errorf("format = %v, want jsonl", payload["format"])
			}
			_ = conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     id,
				"result": map[string]any{"subscription_id": "sub-2", "status": "ok"},
			})
			_ = conn.WriteJSON(map[string]any{
				"type":            "event",
				"subscription_id": "sub-2",
				"event_type":      "session.turn",
				"payload":         map[string]any{"role": "assistant", "text": "hello"},
			})
		case "subscription.stop":
			_ = conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     id,
				"result": map[string]any{"subscription_id": "sub-2", "status": "ok"},
			})
		}
	})
	defer srv.Close()

	c := NewClient(srv.URL)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var gotEvent SubscriptionEvent
	subID, err := c.SubscribeSessionStream(ctx, "sess-abc", "jsonl", 0, func(evt SubscriptionEvent) {
		gotEvent = evt
		select {
		case eventSent <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("SubscribeSessionStream: %v", err)
	}
	if subID != "sub-2" {
		t.Fatalf("subscription_id = %q, want sub-2", subID)
	}

	select {
	case <-eventSent:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for session stream event")
	}

	if gotEvent.EventType != "session.turn" {
		t.Errorf("event_type = %q, want session.turn", gotEvent.EventType)
	}
}

func TestClientUnsubscribe(t *testing.T) {
	subStarted := make(chan struct{}, 1)
	unsubDone := make(chan struct{}, 1)
	srv := wsTestServer(t, func(action string, req map[string]any, conn *websocket.Conn) {
		id, _ := req["id"].(string)
		switch action {
		case "subscription.start":
			_ = conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     id,
				"result": map[string]any{"subscription_id": "sub-99", "status": "ok"},
			})
			select {
			case subStarted <- struct{}{}:
			default:
			}
		case "subscription.stop":
			_ = conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     id,
				"result": map[string]any{"subscription_id": "sub-99", "status": "ok"},
			})
			// Send an event AFTER unsubscribe — callback must NOT fire.
			_ = conn.WriteJSON(map[string]any{
				"type":            "event",
				"subscription_id": "sub-99",
				"event_type":      "bead.created",
				"payload":         map[string]any{"id": "should-not-see"},
			})
			select {
			case unsubDone <- struct{}{}:
			default:
			}
		}
	})
	defer srv.Close()

	c := NewClient(srv.URL)
	defer c.Close()

	var callCount atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subID, err := c.SubscribeEvents(ctx, 0, func(evt SubscriptionEvent) {
		callCount.Add(1)
	})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	<-subStarted

	if err := c.Unsubscribe(subID); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}

	<-unsubDone
	// Give time for the post-unsub event to arrive.
	time.Sleep(200 * time.Millisecond)

	if n := callCount.Load(); n != 0 {
		t.Errorf("callback called %d times after unsubscribe, want 0", n)
	}
}

func TestClientCloseDoesNotDeadlock(t *testing.T) {
	srv := newRawWSServer(t, func(conn *websocket.Conn) {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	c := NewClient(srv.URL)
	// Force WS connection in a background goroutine (the server never responds,
	// so this blocks until Close() kills the connection).
	go func() {
		_, _ = c.ListCities()
	}()
	// Give the goroutine time to connect.
	time.Sleep(100 * time.Millisecond)

	// Close must not deadlock. If it does, the test will timeout.
	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() deadlocked")
	}
}
