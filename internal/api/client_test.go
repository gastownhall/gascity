package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

func TestClientReadOnlyError(t *testing.T) {
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
	if got := err.Error(); got != "mutations disabled: server bound to non-localhost address" {
		t.Errorf("error = %q, want read-only message", got)
	}
	if IsConnError(err) {
		t.Errorf("IsConnError = true for read-only rejection (should be false)")
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

func TestClientResubscribesEventsAfterReconnect(t *testing.T) {
	type socketRequest struct {
		Type    string         `json:"type"`
		ID      string         `json:"id"`
		Action  string         `json:"action"`
		Payload map[string]any `json:"payload"`
	}

	readRequest := func(conn *websocket.Conn) socketRequest {
		t.Helper()
		var req socketRequest
		if err := conn.ReadJSON(&req); err != nil {
			t.Errorf("read request: %v", err)
		}
		return req
	}

	var connections atomic.Int32
	stopSeen := make(chan string, 1)
	srv := newRawWSServer(t, func(conn *websocket.Conn) {
		switch connections.Add(1) {
		case 1:
			req := readRequest(conn)
			if req.Action != "subscription.start" {
				t.Errorf("first action = %q, want subscription.start", req.Action)
				return
			}
			if got := req.Payload["kind"]; got != "events" {
				t.Errorf("first kind = %v, want events", got)
			}
			if _, ok := req.Payload["after_seq"]; ok {
				t.Errorf("first payload unexpectedly had after_seq: %#v", req.Payload)
			}
			if err := conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     req.ID,
				"result": map[string]any{"subscription_id": "server-sub-1", "status": "ok"},
			}); err != nil {
				t.Errorf("write subscribe response: %v", err)
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type":            "event",
				"subscription_id": "server-sub-1",
				"event_type":      "bead.created",
				"index":           41,
				"payload":         map[string]any{"id": "bead-41"},
			}); err != nil {
				t.Errorf("write first event: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			_ = conn.Close()
		case 2:
			req := readRequest(conn)
			if req.Action != "subscription.start" {
				t.Errorf("second action = %q, want subscription.start", req.Action)
				return
			}
			if got := req.Payload["kind"]; got != "events" {
				t.Errorf("second kind = %v, want events", got)
			}
			if got := req.Payload["after_seq"]; got != float64(41) {
				t.Errorf("second after_seq = %#v, want 41", got)
			}
			if err := conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     req.ID,
				"result": map[string]any{"subscription_id": "server-sub-2", "status": "ok"},
			}); err != nil {
				t.Errorf("write reconnect subscribe response: %v", err)
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type":            "event",
				"subscription_id": "server-sub-2",
				"event_type":      "bead.created",
				"index":           42,
				"payload":         map[string]any{"id": "bead-42"},
			}); err != nil {
				t.Errorf("write second event: %v", err)
				return
			}
			stopReq := readRequest(conn)
			if stopReq.Action != "subscription.stop" {
				t.Errorf("stop action = %q, want subscription.stop", stopReq.Action)
				return
			}
			if got := stopReq.Payload["subscription_id"]; got != "server-sub-2" {
				t.Errorf("stop subscription_id = %#v, want server-sub-2", got)
			}
			select {
			case stopSeen <- stopReq.Payload["subscription_id"].(string):
			default:
			}
			_ = conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     stopReq.ID,
				"result": map[string]any{"subscription_id": "server-sub-2", "status": "ok"},
			})
		default:
			t.Errorf("unexpected extra websocket connection")
		}
	})
	defer srv.Close()

	c := NewClient(srv.URL)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eventsCh := make(chan SubscriptionEvent, 4)
	subID, err := c.SubscribeEvents(ctx, 0, func(evt SubscriptionEvent) {
		eventsCh <- evt
	})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	if subID != "server-sub-1" {
		t.Fatalf("subscription_id = %q, want stable server-sub-1", subID)
	}

	var first, second SubscriptionEvent
	select {
	case first = <-eventsCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first event")
	}
	select {
	case second = <-eventsCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for resumed event")
	}

	if first.SubscriptionID != subID {
		t.Fatalf("first event subscription_id = %q, want %q", first.SubscriptionID, subID)
	}
	if second.SubscriptionID != subID {
		t.Fatalf("second event subscription_id = %q, want stable %q", second.SubscriptionID, subID)
	}
	if second.Index != 42 {
		t.Fatalf("second event index = %d, want 42", second.Index)
	}

	if err := c.Unsubscribe(subID); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}

	select {
	case stopped := <-stopSeen:
		if stopped != "server-sub-2" {
			t.Fatalf("stop routed to %q, want server-sub-2", stopped)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for unsubscribe request")
	}
}

func TestClientResubscribesEventsWithCursorAfterReconnect(t *testing.T) {
	type socketRequest struct {
		Type    string         `json:"type"`
		ID      string         `json:"id"`
		Action  string         `json:"action"`
		Payload map[string]any `json:"payload"`
	}

	readRequest := func(conn *websocket.Conn) socketRequest {
		t.Helper()
		var req socketRequest
		if err := conn.ReadJSON(&req); err != nil {
			t.Errorf("read request: %v", err)
		}
		return req
	}

	var connections atomic.Int32
	srv := newRawWSServer(t, func(conn *websocket.Conn) {
		switch connections.Add(1) {
		case 1:
			req := readRequest(conn)
			if req.Action != "subscription.start" {
				t.Errorf("first action = %q, want subscription.start", req.Action)
				return
			}
			if got := req.Payload["kind"]; got != "events" {
				t.Errorf("first kind = %v, want events", got)
			}
			if _, ok := req.Payload["after_cursor"]; ok {
				t.Errorf("first payload unexpectedly had after_cursor: %#v", req.Payload)
			}
			if err := conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     req.ID,
				"result": map[string]any{"subscription_id": "cursor-sub-1", "status": "ok"},
			}); err != nil {
				t.Errorf("write subscribe response: %v", err)
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type":            "event",
				"subscription_id": "cursor-sub-1",
				"event_type":      "bead.created",
				"index":           41,
				"cursor":          "alpha=41",
				"payload":         map[string]any{"id": "bead-41"},
			}); err != nil {
				t.Errorf("write first event: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			_ = conn.Close()
		case 2:
			req := readRequest(conn)
			if req.Action != "subscription.start" {
				t.Errorf("second action = %q, want subscription.start", req.Action)
				return
			}
			if got := req.Payload["kind"]; got != "events" {
				t.Errorf("second kind = %v, want events", got)
			}
			if got := req.Payload["after_cursor"]; got != "alpha=41" {
				t.Errorf("second after_cursor = %#v, want alpha=41", got)
			}
			if _, ok := req.Payload["after_seq"]; ok {
				t.Errorf("second payload unexpectedly had after_seq: %#v", req.Payload)
			}
			if err := conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     req.ID,
				"result": map[string]any{"subscription_id": "cursor-sub-2", "status": "ok"},
			}); err != nil {
				t.Errorf("write reconnect subscribe response: %v", err)
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type":            "event",
				"subscription_id": "cursor-sub-2",
				"event_type":      "bead.created",
				"index":           42,
				"cursor":          "alpha=42",
				"payload":         map[string]any{"id": "bead-42"},
			}); err != nil {
				t.Errorf("write second event: %v", err)
				return
			}
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		default:
			t.Errorf("unexpected extra websocket connection")
		}
	})
	defer srv.Close()

	c := NewClient(srv.URL)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eventsCh := make(chan SubscriptionEvent, 4)
	subID, err := c.SubscribeEvents(ctx, 0, func(evt SubscriptionEvent) {
		eventsCh <- evt
	})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	if subID != "cursor-sub-1" {
		t.Fatalf("subscription_id = %q, want stable cursor-sub-1", subID)
	}

	select {
	case <-eventsCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first event")
	}
	select {
	case <-eventsCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for resumed event")
	}
}

func TestClientResubscribesSessionStreamAfterReconnect(t *testing.T) {
	type socketRequest struct {
		Type    string         `json:"type"`
		ID      string         `json:"id"`
		Action  string         `json:"action"`
		Payload map[string]any `json:"payload"`
	}

	readRequest := func(conn *websocket.Conn) socketRequest {
		t.Helper()
		var req socketRequest
		if err := conn.ReadJSON(&req); err != nil {
			t.Errorf("read request: %v", err)
		}
		return req
	}

	var connections atomic.Int32
	srv := newRawWSServer(t, func(conn *websocket.Conn) {
		switch connections.Add(1) {
		case 1:
			req := readRequest(conn)
			if req.Action != "subscription.start" {
				t.Errorf("first action = %q, want subscription.start", req.Action)
				return
			}
			if got := req.Payload["kind"]; got != "session.stream" {
				t.Errorf("first kind = %v, want session.stream", got)
			}
			if got := req.Payload["target"]; got != "sess-abc" {
				t.Errorf("first target = %v, want sess-abc", got)
			}
			if got := req.Payload["format"]; got != "jsonl" {
				t.Errorf("first format = %v, want jsonl", got)
			}
			if got := req.Payload["turns"]; got != float64(2) {
				t.Errorf("first turns = %#v, want 2", got)
			}
			_ = conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     req.ID,
				"result": map[string]any{"subscription_id": "stream-sub-1", "status": "ok"},
			})
			_ = conn.WriteJSON(map[string]any{
				"type":            "event",
				"subscription_id": "stream-sub-1",
				"event_type":      "session.turn",
				"cursor":          "turn-1",
				"payload":         map[string]any{"text": "first"},
			})
			time.Sleep(100 * time.Millisecond)
			_ = conn.Close()
		case 2:
			req := readRequest(conn)
			if req.Action != "subscription.start" {
				t.Errorf("second action = %q, want subscription.start", req.Action)
				return
			}
			if got := req.Payload["kind"]; got != "session.stream" {
				t.Errorf("second kind = %v, want session.stream", got)
			}
			if got := req.Payload["target"]; got != "sess-abc" {
				t.Errorf("second target = %v, want sess-abc", got)
			}
			if got := req.Payload["format"]; got != "jsonl" {
				t.Errorf("second format = %v, want jsonl", got)
			}
			if got := req.Payload["turns"]; got != float64(2) {
				t.Errorf("second turns = %#v, want 2", got)
			}
			if got := req.Payload["after_cursor"]; got != "turn-1" {
				t.Errorf("second after_cursor = %#v, want turn-1", got)
			}
			if _, ok := req.Payload["after_seq"]; ok {
				t.Errorf("session stream payload unexpectedly had after_seq: %#v", req.Payload)
			}
			if err := conn.WriteJSON(map[string]any{
				"type":   "response",
				"id":     req.ID,
				"result": map[string]any{"subscription_id": "stream-sub-2", "status": "ok"},
			}); err != nil {
				t.Errorf("write reconnect subscribe response: %v", err)
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type":            "event",
				"subscription_id": "stream-sub-2",
				"event_type":      "session.turn",
				"cursor":          "turn-2",
				"payload":         map[string]any{"text": "second"},
			}); err != nil {
				t.Errorf("write second stream event: %v", err)
				return
			}
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		default:
			t.Errorf("unexpected extra websocket connection")
		}
	})
	defer srv.Close()

	c := NewClient(srv.URL)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eventsCh := make(chan SubscriptionEvent, 4)
	subID, err := c.SubscribeSessionStream(ctx, "sess-abc", "jsonl", 2, func(evt SubscriptionEvent) {
		eventsCh <- evt
	})
	if err != nil {
		t.Fatalf("SubscribeSessionStream: %v", err)
	}
	if subID != "stream-sub-1" {
		t.Fatalf("subscription_id = %q, want stable stream-sub-1", subID)
	}

	var first, second SubscriptionEvent
	select {
	case first = <-eventsCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first stream event")
	}
	select {
	case second = <-eventsCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for resumed stream event")
	}

	if first.SubscriptionID != subID {
		t.Fatalf("first stream event subscription_id = %q, want %q", first.SubscriptionID, subID)
	}
	if second.SubscriptionID != subID {
		t.Fatalf("second stream event subscription_id = %q, want stable %q", second.SubscriptionID, subID)
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

func TestClientBackoffSchedule(t *testing.T) {
	// wsBackoffDuration should produce: 1s, 2s, 4s, 8s, 16s, 30s, 30s
	expected := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for i, want := range expected {
		got := wsBackoffDuration(i + 1)
		if got != want {
			t.Errorf("wsBackoffDuration(%d) = %s, want %s", i+1, got, want)
		}
	}
	// Zero failures should produce minimum backoff.
	if got := wsBackoffDuration(0); got != 1*time.Second {
		t.Errorf("wsBackoffDuration(0) = %s, want 1s", got)
	}
}
