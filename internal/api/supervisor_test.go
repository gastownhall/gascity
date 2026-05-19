package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// fakeCityResolver implements CityResolver for testing.
type fakeCityResolver struct {
	cities             map[string]*fakeState // keyed by city name
	listed             []CityInfo
	pending            map[string]string
	supervisorRecorder events.Recorder
}

func (f *fakeCityResolver) ListCities() []CityInfo {
	out := append([]CityInfo(nil), f.listed...)
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

func (f *fakeCityResolver) StorePendingRequestID(cityPath, requestID string) error {
	if f.pending == nil {
		f.pending = make(map[string]string)
	}
	if _, exists := f.pending[cityPath]; exists {
		return ErrPendingRequestExists
	}
	f.pending[cityPath] = requestID
	return nil
}

func (f *fakeCityResolver) ConsumePendingRequestID(cityPath string) (string, bool, error) {
	id, ok := f.pending[cityPath]
	delete(f.pending, cityPath)
	return id, ok, nil
}

func (f *fakeCityResolver) SupervisorEventRecorder() events.Recorder {
	return f.supervisorRecorder
}

func newTestSupervisorMux(t *testing.T, cities map[string]*fakeState) *SupervisorMux {
	t.Helper()
	resolver := &fakeCityResolver{cities: cities}
	return NewSupervisorMux(resolver, nil, false, "test", time.Now())
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

	req := httptest.NewRequest("GET", "/v0/cities", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []CityInfo `json:"items"`
		Total int        `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2", resp.Total)
	}
	// Sorted by name.
	if resp.Items[0].Name != "alpha" || resp.Items[1].Name != "beta" {
		t.Errorf("items = %v, want alpha then beta", resp.Items)
	}
}

func TestSupervisorCityServiceProxy404sUntilCityRunning(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	req := httptest.NewRequest(http.MethodGet, "/v0/city/starting/svc/review-intake/healthz", nil)
	rec := httptest.NewRecorder()

	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	const want = `{"status":404,"title":"Not Found","detail":"not_found: city not found or not running"}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("body = %s, want %s", rec.Body.String(), want)
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

func TestSupervisorCityNamespacedRoute(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "bright-lights"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"bright-lights": s,
	})

	req := httptest.NewRequest("GET", "/v0/city/bright-lights/agents", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Should return the agent list from the city's state.
	var resp struct {
		Items []json.RawMessage `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1 (one agent in fakeState)", resp.Total)
	}
}

func TestSupervisorCityScopedRoute404sUntilCityRunning(t *testing.T) {
	resolver := &fakeCityResolver{
		cities: map[string]*fakeState{},
		listed: []CityInfo{{
			Name:    "bright-lights",
			Path:    "/tmp/bright-lights",
			Running: false,
			Status:  "starting_agents",
		}},
	}
	sm := NewSupervisorMux(resolver, nil, false, "test", time.Now())

	req := httptest.NewRequest("GET", "/v0/city/bright-lights/agents", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), CityNotFoundOrNotRunningDetail("bright-lights")) {
		t.Fatalf("body missing not-running detail: %s", rec.Body.String())
	}
}

func TestSupervisorCityDetail(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "bright-lights"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"bright-lights": s,
	})

	// /v0/city/{name} with no suffix should return status.
	req := httptest.NewRequest("GET", "/v0/city/bright-lights", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "bright-lights" {
		t.Errorf("Name = %q, want %q", resp.Name, "bright-lights")
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

	// "/v0/city/" is not a registered route — every per-city operation
	// is registered at a specific scoped path like /v0/city/{cityName}/foo,
	// and the /svc pass-through requires /v0/city/{cityName}/svc/... . A
	// bare "/v0/city/" correctly 404s.
	req := httptest.NewRequest("GET", "/v0/city/", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestSupervisorPerCityEventStream verifies that per-city event stream
// requests (/v0/city/{name}/events/stream) are correctly routed to the
// city's event handler. This is a regression test for #287 where the
// supervisor returned 404 for valid per-city event stream requests.
func TestSupervisorPerCityEventStream(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "gc-work"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"gc-work": s,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/city/gc-work/events/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestSupervisorEventStreamsFlushHeadersBeforeFirstEvent(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "gc-work"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"gc-work": s,
	})
	srv := httptest.NewServer(sm)
	t.Cleanup(srv.Close)

	for _, path := range []string{
		"/v0/events/stream",
		"/v0/city/gc-work/events/stream",
	} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Accept", "text/event-stream")

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close() //nolint:errcheck

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Fatalf("Content-Type = %q, want text/event-stream", ct)
			}
		})
	}
}

func TestSupervisorPerCityEventStreamEmitsTypedEnvelopePayloadObject(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "gc-work"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"gc-work": s,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/city/gc-work/events/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	payload, err := json.Marshal(MailEventPayload{Rig: "myrig"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	s.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.MailSent,
		Actor:   "tester",
		Subject: "mail-1",
		Payload: payload,
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	frame := firstSSETestFrame(t, rec.Body.String(), "event")
	if frame.ID != "1" {
		t.Fatalf("SSE id = %q, want 1; body=%s", frame.ID, rec.Body.String())
	}
	data := decodeSSETestData(t, frame)
	if data["type"] != events.MailSent {
		t.Fatalf("data.type = %v, want %s; data=%v", data["type"], events.MailSent, data)
	}
	if _, ok := data["city"]; ok {
		t.Fatalf("per-city event data unexpectedly includes city: %v", data)
	}
	payloadObject, ok := data["payload"].(map[string]any)
	if !ok {
		t.Fatalf("data.payload = %#v, want JSON object", data["payload"])
	}
	if payloadObject["rig"] != "myrig" {
		t.Fatalf("payload.rig = %v, want myrig; payload=%v", payloadObject["rig"], payloadObject)
	}
}

func TestSupervisorPerCityEventStreamEmitsNoPayloadObject(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "gc-work"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"gc-work": s,
	})

	frame := firstSSEFrameAfterRecord(t, sm, "/v0/city/gc-work/events/stream", "event", func() {
		s.eventProv.(*events.Fake).Record(events.Event{
			Type:    events.SessionWoke,
			Actor:   "tester",
			Subject: "session-1",
		})
	})
	data := decodeSSETestData(t, frame)
	if data["type"] != events.SessionWoke {
		t.Fatalf("data.type = %v, want %s; data=%v", data["type"], events.SessionWoke, data)
	}
	payloadObject := assertJSONPayloadObject(t, data["payload"])
	if len(payloadObject) != 0 {
		t.Fatalf("data.payload = %v, want empty object for NoPayload", payloadObject)
	}
}

func TestSupervisorPerCityEventStreamWithoutCursorStartsAtHead(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "gc-work"
	ep := s.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "old"})

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"gc-work": s,
	})

	frame := firstSSEFrameAfterRecord(t, sm, "/v0/city/gc-work/events/stream", "event", func() {
		ep.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "new"})
	})
	if frame.ID != "2" {
		t.Fatalf("SSE id = %q, want 2; body=%s", frame.ID, frame.Data)
	}
	data := decodeSSETestData(t, frame)
	if data["subject"] != "new" {
		t.Fatalf("data.subject = %v, want new; data=%v", data["subject"], data)
	}
}

func TestSupervisorGlobalEventList(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	s2 := newFakeState(t)
	s2.cityName = "beta"

	// Record events in each city's event provider.
	s1.eventProv.(*events.Fake).Record(events.Event{Type: events.SessionWoke, Actor: "a1"})
	s2.eventProv.(*events.Fake).Record(events.Event{Type: events.SessionStopped, Actor: "b1"})

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s1,
		"beta":  s2,
	})

	req := httptest.NewRequest("GET", "/v0/events", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		EventCursor string               `json:"event_cursor"`
		Items       []events.TaggedEvent `json:"items"`
		Total       int                  `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	if resp.EventCursor != "alpha:1,beta:1" {
		t.Fatalf("event_cursor = %q, want alpha:1,beta:1", resp.EventCursor)
	}

	// Verify events are tagged with city names.
	cities := make(map[string]bool)
	for _, e := range resp.Items {
		cities[e.City] = true
	}
	if !cities["alpha"] || !cities["beta"] {
		t.Errorf("expected events from both cities, got: %v", cities)
	}
}

func TestSupervisorEventListsEmitTypedPayloadObjects(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "alpha"
	payload, err := json.Marshal(MailEventPayload{Rig: "myrig"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	s.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.MailSent,
		Actor:   "tester",
		Subject: "mail-1",
		Payload: payload,
	})
	s.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.SessionWoke,
		Actor:   "tester",
		Subject: "session-1",
	})

	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": s})

	for _, tt := range []struct {
		name     string
		path     string
		wantCity string
	}{
		{name: "per-city", path: "/v0/city/alpha/events"},
		{name: "supervisor", path: "/v0/events", wantCity: "alpha"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			sm.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}

			var resp struct {
				Items []map[string]any `json:"items"`
				Total int              `json:"total"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Total != 2 {
				t.Fatalf("total = %d, want 2; items=%v", resp.Total, resp.Items)
			}

			mail := eventListItemByType(t, resp.Items, events.MailSent)
			if tt.wantCity != "" && mail["city"] != tt.wantCity {
				t.Fatalf("mail city = %v, want %s; item=%v", mail["city"], tt.wantCity, mail)
			}
			mailPayload := assertJSONPayloadObject(t, mail["payload"])
			if mailPayload["rig"] != "myrig" {
				t.Fatalf("mail payload.rig = %v, want myrig; payload=%v", mailPayload["rig"], mailPayload)
			}

			noPayload := assertJSONPayloadObject(t, eventListItemByType(t, resp.Items, events.SessionWoke)["payload"])
			if len(noPayload) != 0 {
				t.Fatalf("session.woke payload = %v, want empty object", noPayload)
			}
		})
	}
}

func TestSupervisorEventListsIncludeCustomEventTypes(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "alpha"
	s.eventProv.(*events.Fake).Record(events.Event{Type: "custom.untyped", Actor: "tester", Payload: json.RawMessage(`{"source":"test"}`)})
	s.eventProv.(*events.Fake).Record(events.Event{Type: events.SessionWoke, Actor: "tester"})

	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": s})

	req := httptest.NewRequest("GET", "/v0/events", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
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
	if custom["city"] != "alpha" {
		t.Fatalf("custom city = %v, want alpha; item=%v", custom["city"], custom)
	}
	payload := assertJSONPayloadObject(t, custom["payload"])
	if payload["source"] != "test" {
		t.Fatalf("custom payload = %v, want source=test", payload)
	}
}

func TestSupervisorEventListFilterIsEmptyMatchesEventsFilterZeroValue(t *testing.T) {
	if !supervisorEventListFilterIsEmpty(events.Filter{}) {
		t.Fatal("zero-value filter reported non-empty")
	}

	tests := []struct {
		name   string
		filter events.Filter
	}{
		{name: "type", filter: events.Filter{Type: events.BeadCreated}},
		{name: "actor", filter: events.Filter{Actor: "human"}},
		{name: "subject", filter: events.Filter{Subject: "gc-1"}},
		{name: "since", filter: events.Filter{Since: time.Unix(1, 0)}},
		{name: "until", filter: events.Filter{Until: time.Unix(1, 0)}},
		{name: "after_seq", filter: events.Filter{AfterSeq: 1}},
		{name: "limit", filter: events.Filter{Limit: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if supervisorEventListFilterIsEmpty(tt.filter) {
				t.Fatalf("filter %+v reported empty", tt.filter)
			}
		})
	}
}

func TestSupervisorGlobalEventListWithFilter(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	s1.eventProv.(*events.Fake).Record(events.Event{Type: events.SessionWoke, Actor: "a1"})
	s1.eventProv.(*events.Fake).Record(events.Event{Type: events.SessionStopped, Actor: "a1"})

	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": s1})

	req := httptest.NewRequest("GET", "/v0/events?type=session.woke", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if resp.Items[0].Type != events.SessionWoke {
		t.Errorf("type = %q, want %q", resp.Items[0].Type, events.SessionWoke)
	}
}

func TestSupervisorGlobalEventListLimitReturnsTail(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	ep := s1.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "a1", Subject: "old"})
	ep.Record(events.Event{Type: events.SessionStopped, Actor: "a1", Subject: "middle"})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "a1", Subject: "new"})

	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": s1})

	req := httptest.NewRequest("GET", "/v0/events?limit=1", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 3 {
		t.Fatalf("total = %d, want 3", resp.Total)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].Subject != "new" {
		t.Fatalf("subject = %q, want new", resp.Items[0].Subject)
	}
}

func TestSupervisorGlobalEventListLimitReturnsTailAcrossCitiesWithHeadTotal(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	alpha := s1.eventProv.(*events.Fake)
	alpha.Record(events.Event{Type: events.SessionWoke, Actor: "a1", Subject: "alpha-old", Ts: time.Unix(1, 0)})
	alpha.Record(events.Event{Type: events.SessionWoke, Actor: "a1", Subject: "alpha-new", Ts: time.Unix(4, 0)})

	s2 := newFakeState(t)
	s2.cityName = "beta"
	beta := s2.eventProv.(*events.Fake)
	beta.Record(events.Event{Type: events.SessionWoke, Actor: "b1", Subject: "beta-old", Ts: time.Unix(2, 0)})
	beta.Record(events.Event{Type: events.SessionStopped, Actor: "b1", Subject: "beta-middle", Ts: time.Unix(3, 0)})
	beta.Record(events.Event{Type: events.SessionWoke, Actor: "b1", Subject: "beta-new", Ts: time.Unix(5, 0)})

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s1,
		"beta":  s2,
	})

	req := httptest.NewRequest("GET", "/v0/events?limit=2", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 5 {
		t.Fatalf("total = %d, want 5", resp.Total)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(resp.Items))
	}
	if resp.Items[0].Subject != "alpha-new" || resp.Items[1].Subject != "beta-new" {
		t.Fatalf("subjects = [%s %s], want [alpha-new beta-new]", resp.Items[0].Subject, resp.Items[1].Subject)
	}
}

func TestSupervisorGlobalEventListLimitWithFilterReportsFilteredTotal(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	ep := s1.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "a1", Subject: "old", Ts: time.Unix(1, 0)})
	ep.Record(events.Event{Type: events.SessionStopped, Actor: "a1", Subject: "ignored", Ts: time.Unix(2, 0)})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "a1", Subject: "new", Ts: time.Unix(3, 0)})

	sm := newTestSupervisorMux(t, map[string]*fakeState{"alpha": s1})

	req := httptest.NewRequest("GET", "/v0/events?type=session.woke&limit=1", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("total = %d, want 2 filtered matches", resp.Total)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].Subject != "new" {
		t.Fatalf("subject = %q, want new", resp.Items[0].Subject)
	}
}

func TestSupervisorGlobalEventListRejectsInvalidSince(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})

	req := httptest.NewRequest("GET", "/v0/events?since=notaduration", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid since duration") {
		t.Fatalf("body = %q, want invalid since duration", rec.Body.String())
	}
}

func TestSupervisorGlobalEventListEmpty(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})

	req := httptest.NewRequest("GET", "/v0/events", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
}

// TestSupervisorGlobalEventStreamNoProviders guards the Codex-flagged
// precheck bug: when no running city has an event provider, the
// supervisor must reject /v0/events/stream with 503 Problem Details
// *before* committing 200 text/event-stream headers. Otherwise clients
// see "stream opened, then immediate EOF" and can't distinguish it
// from a dropped connection.
func TestSupervisorGlobalEventStreamNoProviders(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})

	req := httptest.NewRequest("GET", "/v0/events/stream", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "problem+json") && !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want Problem Details", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no_providers") {
		t.Errorf("body missing no_providers code: %s", body)
	}
}

func TestSupervisorGlobalEventStreamCompositeCursor(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	s2 := newFakeState(t)
	s2.cityName = "beta"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s1,
		"beta":  s2,
	})

	// Use a cancellable context so we can stop the SSE stream.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/events/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Run ServeHTTP in a goroutine since it blocks.
	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.ServeHTTP(rec, req)
	}()

	// Record events after the stream handler starts.
	time.Sleep(50 * time.Millisecond)
	s1.eventProv.(*events.Fake).Record(events.Event{Type: events.SessionWoke, Actor: "a1"})
	s2.eventProv.(*events.Fake).Record(events.Event{Type: events.SessionWoke, Actor: "b1"})

	// Give events time to propagate through the stream.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	// Parse SSE events from the response body.
	body := rec.Body.String()
	scanner := bufio.NewScanner(strings.NewReader(body))
	var sseIDs []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			sseIDs = append(sseIDs, strings.TrimPrefix(line, "id: "))
		}
	}

	if len(sseIDs) == 0 {
		t.Fatalf("expected SSE events with id lines, got body: %s", body)
	}

	// Each id should be a composite cursor (containing ":" for city:seq format).
	for _, id := range sseIDs {
		if !strings.Contains(id, ":") {
			t.Errorf("SSE id %q is not a composite cursor (expected city:seq format)", id)
		}
		// Verify round-trip: ParseCursor should produce a non-empty map.
		cursors := events.ParseCursor(id)
		if len(cursors) == 0 {
			t.Errorf("ParseCursor(%q) returned empty map", id)
		}
	}

	// The last cursor should contain both cities (once both have emitted events).
	lastID := sseIDs[len(sseIDs)-1]
	lastCursors := events.ParseCursor(lastID)
	if _, ok := lastCursors["alpha"]; !ok {
		t.Errorf("last cursor %q missing city 'alpha'", lastID)
	}
	if _, ok := lastCursors["beta"]; !ok {
		t.Errorf("last cursor %q missing city 'beta'", lastID)
	}
}

func TestSupervisorGlobalEventStreamEmitsTypedTaggedEnvelopePayloadObject(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "alpha"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/events/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	payload, err := json.Marshal(MailEventPayload{Rig: "myrig"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	s.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.MailSent,
		Actor:   "tester",
		Subject: "mail-1",
		Payload: payload,
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	frame := firstSSETestFrame(t, rec.Body.String(), "tagged_event")
	if frame.ID != "alpha:1" {
		t.Fatalf("SSE id = %q, want alpha:1; body=%s", frame.ID, rec.Body.String())
	}
	data := decodeSSETestData(t, frame)
	if data["type"] != events.MailSent {
		t.Fatalf("data.type = %v, want %s; data=%v", data["type"], events.MailSent, data)
	}
	if data["city"] != "alpha" {
		t.Fatalf("data.city = %v, want alpha; data=%v", data["city"], data)
	}
	payloadObject, ok := data["payload"].(map[string]any)
	if !ok {
		t.Fatalf("data.payload = %#v, want JSON object", data["payload"])
	}
	if payloadObject["rig"] != "myrig" {
		t.Fatalf("payload.rig = %v, want myrig; payload=%v", payloadObject["rig"], payloadObject)
	}
}

func TestSupervisorGlobalEventStreamEmitsNoPayloadObject(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "alpha"

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s,
	})

	frame := firstSSEFrameAfterRecord(t, sm, "/v0/events/stream", "tagged_event", func() {
		s.eventProv.(*events.Fake).Record(events.Event{
			Type:    events.SessionWoke,
			Actor:   "tester",
			Subject: "session-1",
		})
	})
	if frame.ID != "alpha:1" {
		t.Fatalf("SSE id = %q, want alpha:1", frame.ID)
	}
	data := decodeSSETestData(t, frame)
	if data["type"] != events.SessionWoke {
		t.Fatalf("data.type = %v, want %s; data=%v", data["type"], events.SessionWoke, data)
	}
	if data["city"] != "alpha" {
		t.Fatalf("data.city = %v, want alpha; data=%v", data["city"], data)
	}
	payloadObject := assertJSONPayloadObject(t, data["payload"])
	if len(payloadObject) != 0 {
		t.Fatalf("data.payload = %v, want empty object for NoPayload", payloadObject)
	}
}

func TestSupervisorGlobalEventStreamWithoutCursorStartsAtHead(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "alpha"
	ep := s.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "old"})

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s,
	})

	frame := firstSSEFrameAfterRecord(t, sm, "/v0/events/stream", "tagged_event", func() {
		ep.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "new"})
	})
	if frame.ID != "alpha:2" {
		t.Fatalf("SSE id = %q, want alpha:2; body=%s", frame.ID, frame.Data)
	}
	data := decodeSSETestData(t, frame)
	if data["subject"] != "new" {
		t.Fatalf("data.subject = %v, want new; data=%v", data["subject"], data)
	}
}

func TestSupervisorGlobalEventStreamAfterCursorReplaysFromCursor(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "alpha"
	ep := s.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "old"})

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s,
	})

	frame := firstSSEFrameAfterRecord(t, sm, "/v0/events/stream?after_cursor=alpha:0", "tagged_event", func() {})
	if frame.ID != "alpha:1" {
		t.Fatalf("SSE id = %q, want alpha:1; body=%s", frame.ID, frame.Data)
	}
	data := decodeSSETestData(t, frame)
	if data["subject"] != "old" {
		t.Fatalf("data.subject = %v, want old; data=%v", data["subject"], data)
	}
}

func TestCurrentSupervisorEventCursorReturnsProviderErrors(t *testing.T) {
	s := newFakeState(t)
	s.cityName = "alpha"
	s.eventProv = events.NewFailFake()

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s,
	})

	if got, err := sm.currentSupervisorEventCursor(); err == nil {
		t.Fatalf("currentSupervisorEventCursor() = %q, nil error; want provider error", got)
	}
}

func TestCurrentSupervisorEventCursorIsStrictOnPartialProviderFailure(t *testing.T) {
	healthy := newFakeState(t)
	healthy.cityName = "alpha"
	healthy.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.SessionWoke,
		Actor:   "tester",
		Subject: "healthy",
	})
	broken := newFakeState(t)
	broken.cityName = "bravo"
	broken.eventProv = events.NewFailFake()

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": healthy,
		"bravo": broken,
	})

	got, err := sm.currentSupervisorEventCursor()
	if err == nil {
		t.Fatalf("currentSupervisorEventCursor() = %q, nil error; want strict partial-provider failure", got)
	}
	if got != "" {
		t.Fatalf("currentSupervisorEventCursor() returned partial cursor %q with error; want empty cursor", got)
	}
	if !strings.Contains(err.Error(), "bravo") {
		t.Fatalf("currentSupervisorEventCursor() error = %v, want broken city name", err)
	}
}

func TestSupervisorGlobalEventStreamProjectsWorkflowMetadata(t *testing.T) {
	s1 := newFakeState(t)
	s1.cityName = "alpha"
	store := s1.stores["myrig"]
	root, err := store.Create(beads.Bead{
		Title: "Workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf_123",
			"gc.scope_kind":  "city",
			"gc.scope_ref":   "alpha",
		},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	payload, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal root: %v", err)
	}

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": s1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/events/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	s1.eventProv.(*events.Fake).Record(events.Event{
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
		t.Fatalf("global SSE body missing workflow projection: %s", body)
	}
	if !strings.Contains(body, `"city":"alpha"`) {
		t.Fatalf("global SSE body missing city tag: %s", body)
	}
}

type sseTestFrame struct {
	Event string
	ID    string
	Data  string
}

func firstSSETestFrame(t *testing.T, body, eventName string) sseTestFrame {
	t.Helper()

	for _, frame := range parseSSETestFrames(body) {
		if frame.Event == eventName {
			return frame
		}
	}
	t.Fatalf("SSE event %q not found in body: %s", eventName, body)
	return sseTestFrame{}
}

func firstSSEFrameAfterRecord(t *testing.T, h http.Handler, path, eventName string, record func()) sseTestFrame {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	record()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	return firstSSETestFrame(t, rec.Body.String(), eventName)
}

func parseSSETestFrames(body string) []sseTestFrame {
	var frames []sseTestFrame
	var current sseTestFrame
	flush := func() {
		if current.Event != "" || current.ID != "" || current.Data != "" {
			frames = append(frames, current)
			current = sseTestFrame{}
		}
	}

	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event: "):
			current.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			current.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			current.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	flush()
	return frames
}

func decodeSSETestData(t *testing.T, frame sseTestFrame) map[string]any {
	t.Helper()

	var data map[string]any
	if err := json.Unmarshal([]byte(frame.Data), &data); err != nil {
		t.Fatalf("decode SSE data for event %q: %v; data=%s", frame.Event, err, frame.Data)
	}
	return data
}

func assertJSONPayloadObject(t *testing.T, raw any) map[string]any {
	t.Helper()

	payloadObject, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("payload = %#v, want JSON object", raw)
	}
	return payloadObject
}

func eventListItemByType(t *testing.T, items []map[string]any, eventType string) map[string]any {
	t.Helper()

	for _, item := range items {
		if item["type"] == eventType {
			return item
		}
	}
	t.Fatalf("event type %s not found in items: %v", eventType, items)
	return nil
}
