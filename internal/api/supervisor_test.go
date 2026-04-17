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
		Items []events.TaggedEvent `json:"items"`
		Total int                  `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
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
