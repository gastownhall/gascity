package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// fakeCityResolver implements CityResolver for testing.
type fakeCityResolver struct {
	cities map[string]*fakeState // keyed by city name
}

func (f *fakeCityResolver) ListCities() []CityInfo {
	var out []CityInfo
	for name := range f.cities {
		s := f.cities[name]
		out = append(out, CityInfo{
			Name:    name,
			Path:    s.CityPath(),
			Running: true,
		})
	}
	return out
}

func (f *fakeCityResolver) CityState(name string) State {
	if s, ok := f.cities[name]; ok {
		return s
	}
	return nil
}

func newTestSupervisorMux(t *testing.T, cities map[string]*fakeState) *SupervisorMux {
	t.Helper()
	resolver := &fakeCityResolver{cities: cities}
	return NewSupervisorMux(resolver, false, "test", time.Now())
}

func TestSupervisorCitiesList(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	s2 := newFakeState(t)
	s2.cityName = "beta"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s1,
		"beta":  s2,
	})
	base := sm.Handler()
	ts := httptest.NewServer(base)
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{Type: "request", ID: "c1", Action: "cities.list"})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "c1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	var result struct {
		Items []CityInfo `json:"items"`
		Total int        `json:"total"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}
	if result.Items[0].Name != "alpha" || result.Items[1].Name != "beta" {
		t.Errorf("items = %v, want alpha then beta", result.Items)
	}
}

func TestSupervisorProviderReadinessRoute(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "codex", "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".codex", "auth.json"),
		[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"token"}}`),
		0o600,
	); err != nil {
		t.Fatalf("write codex auth: %v", err)
	}

	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	req := httptest.NewRequest("GET", "/v0/provider-readiness?providers=codex", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp providerReadinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Providers["codex"].Status; got != probeStatusConfigured {
		t.Errorf("codex status = %q, want %q", got, probeStatusConfigured)
	}
}

func TestSupervisorReadinessRoute(t *testing.T) {
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeExecutable(t, binDir, "gh", "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(homeDir, ".config", "gh"), 0o755); err != nil {
		t.Fatalf("mkdir gh config dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(homeDir, ".config", "gh", "hosts.yml"),
		[]byte("github.com:\n    user: octocat\n    oauth_token: token\n"),
		0o600,
	); err != nil {
		t.Fatalf("write gh hosts: %v", err)
	}

	unsetGitHubCLITokenEnv(t)
	t.Setenv("HOME", homeDir)
	originalPathEnv := providerProbePathEnv
	providerProbePathEnv = binDir
	defer func() {
		providerProbePathEnv = originalPathEnv
	}()

	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	req := httptest.NewRequest("GET", "/v0/readiness?items=github_cli", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Items["github_cli"].Status; got != probeStatusConfigured {
		t.Errorf("github_cli status = %q, want %q", got, probeStatusConfigured)
	}
}

func TestSupervisorLegacyHTTPAPIRoutesReturnNotFound(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": s1})

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "cities", method: http.MethodGet, path: "/v0/cities"},
		{name: "status", method: http.MethodGet, path: "/v0/status"},
		{name: "events", method: http.MethodGet, path: "/v0/events"},
		{name: "event stream", method: http.MethodGet, path: "/v0/events/stream"},
		{name: "city namespaced status", method: http.MethodGet, path: "/v0/city/alpha/v0/status"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			sm.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s status = %d, want %d", tt.method, tt.path, rec.Code, http.StatusNotFound)
			}
		})
	}
}

func TestSupervisorCityNamespacedRoute(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "bright-lights"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"bright-lights": s,
	})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "city-agents",
		Action: "agents.list",
		Scope:  &wsScope{City: "bright-lights"},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "city-agents" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var listResp struct {
		Items []json.RawMessage `json:"items"`
		Total int               `json:"total"`
	}
	json.Unmarshal(resp.Result, &listResp)
	if listResp.Total != 1 {
		t.Errorf("Total = %d, want 1 (one agent in fakeState)", listResp.Total)
	}
}

func TestSupervisorCityDetail(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "bright-lights"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"bright-lights": s,
	})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "city-detail",
		Action: "status.get",
		Scope:  &wsScope{City: "bright-lights"},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "city-detail" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var status statusResponse
	json.Unmarshal(resp.Result, &status)
	if status.Name != "bright-lights" {
		t.Errorf("Name = %q, want %q", status.Name, "bright-lights")
	}
}

func TestSupervisorCityNotFound(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})

	req := httptest.NewRequest("GET", "/v0/city/unknown/agents", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestSupervisorBarePathSingleCity(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "sole-city"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"sole-city": s,
	})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Bare status.get without scope should route to the sole running city.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "bare-status",
		Action: "status.get",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "bare-status" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var status statusResponse
	json.Unmarshal(resp.Result, &status)
	if status.Name != "sole-city" {
		t.Errorf("Name = %q, want %q", status.Name, "sole-city")
	}
}

func TestSupervisorBareServicePathSingleCity(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "sole-city"
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName: "github-webhook",
			PublishMode: "private",
		}},
		serve: func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/svc/github-webhook/v0/github/webhook" {
				t.Fatalf("path = %q, want /svc/github-webhook/v0/github/webhook", r.URL.Path)
			}
			if r.Header.Get("X-GC-Request") != "1" {
				t.Fatalf("X-GC-Request = %q, want 1", r.Header.Get("X-GC-Request"))
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("proxied"))
			return true
		},
	}

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"sole-city": state,
	})

	req := httptest.NewRequest(http.MethodPost, "/svc/github-webhook/v0/github/webhook", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:9000"
	req.Header.Set("X-GC-Request", "1")
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if strings.TrimSpace(rec.Body.String()) != "proxied" {
		t.Fatalf("body = %q, want proxied", rec.Body.String())
	}
}

func TestSupervisorCityScopedServicePath(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName: "github-webhook",
			PublishMode: "private",
		}},
		serve: func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/svc/github-webhook/v0/github/webhook" {
				t.Fatalf("path = %q, want /svc/github-webhook/v0/github/webhook", r.URL.Path)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("proxied"))
			return true
		},
	}

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"bright-lights": state,
	})

	req := httptest.NewRequest(http.MethodPost, "/v0/city/bright-lights/svc/github-webhook/v0/github/webhook", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:9000"
	req.Header.Set("X-GC-Request", "1")
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if strings.TrimSpace(rec.Body.String()) != "proxied" {
		t.Fatalf("body = %q, want proxied", rec.Body.String())
	}
}

func TestSupervisorHandlerAllowsDirectServiceMutationWithoutCSRF(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "sole-city"
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName: "github-webhook",
			PublishMode: "direct",
		}},
		serve: func(w http.ResponseWriter, _ *http.Request) bool {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("proxied"))
			return true
		},
	}

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"sole-city": state,
	})

	req := httptest.NewRequest(http.MethodPost, "/svc/github-webhook/v0/github/webhook", strings.NewReader(`{}`))
	req.RemoteAddr = "198.51.100.10:9000"
	rec := httptest.NewRecorder()
	sm.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if strings.TrimSpace(rec.Body.String()) != "proxied" {
		t.Fatalf("body = %q, want proxied", rec.Body.String())
	}
}

func TestSupervisorHandlerAllowsCityScopedDirectServiceMutationWithoutCSRF(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName: "github-webhook",
			PublishMode: "direct",
		}},
		serve: func(w http.ResponseWriter, _ *http.Request) bool {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("proxied"))
			return true
		},
	}

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"bright-lights": state,
	})

	req := httptest.NewRequest(http.MethodPost, "/v0/city/bright-lights/svc/github-webhook/v0/github/webhook", strings.NewReader(`{}`))
	req.RemoteAddr = "198.51.100.10:9000"
	rec := httptest.NewRecorder()
	sm.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if strings.TrimSpace(rec.Body.String()) != "proxied" {
		t.Fatalf("body = %q, want proxied", rec.Body.String())
	}
}

func TestSupervisorHandlerReadOnlyAllowsDirectServiceMutationWithoutCSRF(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "sole-city"
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName: "github-webhook",
			PublishMode: "direct",
		}},
		serve: func(w http.ResponseWriter, _ *http.Request) bool {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("proxied"))
			return true
		},
	}

	sm := NewSupervisorMux(&fakeCityResolver{cities: map[string]*fakeState{
		"sole-city": state,
	}}, true, "test", time.Now())

	req := httptest.NewRequest(http.MethodPost, "/svc/github-webhook/v0/github/webhook", strings.NewReader(`{}`))
	req.RemoteAddr = "198.51.100.10:9000"
	rec := httptest.NewRecorder()
	sm.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if strings.TrimSpace(rec.Body.String()) != "proxied" {
		t.Fatalf("body = %q, want proxied", rec.Body.String())
	}
}

func TestSupervisorHandlerReadOnlyStillBlocksPrivateServiceMutation(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "sole-city"
	state.services = &fakeServiceRegistry{
		items: []workspacesvc.Status{{
			ServiceName: "github-webhook",
			PublishMode: "private",
		}},
		serve: func(http.ResponseWriter, *http.Request) bool {
			t.Fatal("private service mutation should not be invoked through read-only supervisor")
			return false
		},
	}

	sm := NewSupervisorMux(&fakeCityResolver{cities: map[string]*fakeState{
		"sole-city": state,
	}}, true, "test", time.Now())

	req := httptest.NewRequest(http.MethodPost, "/svc/github-webhook/v0/github/webhook", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:9000"
	req.Header.Set("X-GC-Request", "1")
	rec := httptest.NewRecorder()
	sm.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestSupervisorBarePathNoCities(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// status.get with no scope and no cities should return no_cities error.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "no-cities",
		Action: "status.get",
	})

	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" {
		t.Fatalf("type = %q, want error", errResp.Type)
	}
	if errResp.Code != "no_cities" {
		t.Errorf("code = %q, want no_cities", errResp.Code)
	}
}

func TestSupervisorBarePathMultipleCities(t *testing.T) {
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
	drainWSHello(t, conn)

	// status.get with no scope and multiple cities should return city_required.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "multi-cities",
		Action: "status.get",
	})

	var errResp wsErrorEnvelope
	readWSJSON(t, conn, &errResp)
	if errResp.Type != "error" {
		t.Fatalf("type = %q, want error", errResp.Type)
	}
	if errResp.Code != "city_required" {
		t.Errorf("code = %q, want city_required", errResp.Code)
	}
}

func TestSupervisorBareServicePathMultipleCities(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	s2 := newFakeState(t)
	s2.cityName = "beta"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s1,
		"beta":  s2,
	})

	req := httptest.NewRequest(http.MethodPost, "/svc/github-webhook/v0/github/webhook", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:9000"
	req.Header.Set("X-GC-Request", "1")
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "city_required") {
		t.Fatalf("body = %q, want city_required error", rec.Body.String())
	}
}

func TestSupervisorHealth(t *testing.T) {
	s := newFakeState(t)
	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"test-city": s,
	})

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want %q", resp["status"], "ok")
	}
	if resp["cities_total"] != float64(1) {
		t.Errorf("cities_total = %v, want 1", resp["cities_total"])
	}
	if resp["cities_running"] != float64(1) {
		t.Errorf("cities_running = %v, want 1", resp["cities_running"])
	}
}

func TestSupervisorEmptyCityName(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})

	req := httptest.NewRequest("GET", "/v0/city/", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestSupervisorPerCityEventSubscription verifies that city-scoped event
// subscriptions work via WS. Regression test for #287.
func TestSupervisorPerCityEventSubscription(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "gc-work"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"gc-work": s,
	})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	// Subscribe to city-scoped events.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "sub-city",
		Action: "subscription.start",
		Scope:  &wsScope{City: "gc-work"},
		Payload: map[string]any{
			"kind": "events",
		},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "sub-city" {
		t.Fatalf("subscription response = %#v, want correlated response", resp)
	}

	// Record an event and verify it arrives.
	s.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester"})

	var evt wsEventEnvelope
	readWSJSON(t, conn, &evt)
	if evt.Type != "event" || evt.EventType != events.SessionWoke {
		t.Fatalf("event = %#v, want session.woke event", evt)
	}
}

func TestSupervisorGlobalEventList(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	s2 := newFakeState(t)
	s2.cityName = "beta"

	s1.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "a1"})
	s2.eventProv.Record(events.Event{Type: events.SessionStopped, Actor: "b1"})

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s1,
		"beta":  s2,
	})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "global-events",
		Action: "events.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "global-events" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var listResp struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	json.Unmarshal(resp.Result, &listResp)
	if listResp.Total != 2 {
		t.Errorf("total = %d, want 2", listResp.Total)
	}

	cities := make(map[string]bool)
	for _, e := range listResp.Items {
		cities[e.City] = true
	}
	if !cities["alpha"] || !cities["beta"] {
		t.Errorf("expected events from both cities, got: %v", cities)
	}
}

func TestSupervisorGlobalEventListWithFilter(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	s1.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "a1"})
	s1.eventProv.Record(events.Event{Type: events.SessionStopped, Actor: "a1"})

	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": s1})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:    "request",
		ID:      "filtered-events",
		Action:  "events.list",
		Payload: map[string]any{"type": events.SessionWoke},
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "filtered-events" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var listResp struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	json.Unmarshal(resp.Result, &listResp)
	if listResp.Total != 1 {
		t.Errorf("total = %d, want 1", listResp.Total)
	}
	if len(listResp.Items) > 0 && listResp.Items[0].Type != events.SessionWoke {
		t.Errorf("type = %q, want %q", listResp.Items[0].Type, events.SessionWoke)
	}
}

func TestSupervisorGlobalEventListEmpty(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	ts := httptest.NewServer(sm.Handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "empty-events",
		Action: "events.list",
	})

	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "empty-events" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var listResp struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	json.Unmarshal(resp.Result, &listResp)
	if listResp.Total != 0 {
		t.Errorf("total = %d, want 0", listResp.Total)
	}
}
