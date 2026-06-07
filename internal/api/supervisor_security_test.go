package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func TestSupervisorHostAllowlistRejectsUnexpectedHost(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/v0/cities", nil)
	rec := httptest.NewRecorder()

	sm.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusMisdirectedRequest, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	if !strings.Contains(rec.Body.String(), "host_not_allowed") {
		t.Fatalf("body = %q, want host_not_allowed problem detail", rec.Body.String())
	}
}

func TestSupervisorHostAllowlistRejectsEmptyHost(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8372/v0/cities", nil)
	req.Host = ""
	rec := httptest.NewRecorder()

	sm.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusMisdirectedRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "host_not_allowed") {
		t.Fatalf("body = %q, want host_not_allowed problem detail", rec.Body.String())
	}
}

func TestSupervisorHostAllowlistRejectsMutationWithPrivateIPOrBadHost(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{"private ip", "http://192.168.1.58:8372/v0/city/thriva/bead/th-123/update"},
		{"bad host", "http://evil.example:8372/v0/city/thriva/bead/th-123/update"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := newTestSupervisorMux(t, map[string]*fakeState{})
			req := httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			sm.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusMisdirectedRequest {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusMisdirectedRequest, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "csrf") {
				t.Fatalf("body = %q, host rejection must happen before CSRF handling", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "host_not_allowed") {
				t.Fatalf("body = %q, want host_not_allowed problem detail", rec.Body.String())
			}
		})
	}
}

func TestSupervisorHostAllowlistAcceptsLoopbackAndConfiguredHost(t *testing.T) {
	cases := []struct {
		name         string
		target       string
		allowedHosts []string
	}{
		{"localhost", "http://localhost:8372/v0/cities", nil},
		{"ipv4 loopback", "http://127.0.0.1:8372/v0/cities", nil},
		{"ipv6 loopback", "http://[::1]:8372/v0/cities", nil},
		{"configured hostname", "http://thriva-dev:8372/v0/cities", []string{"thriva-dev"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := newTestSupervisorMux(t, map[string]*fakeState{})
			if len(tc.allowedHosts) > 0 {
				sm.WithAllowedHosts(tc.allowedHosts)
			}
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			rec := httptest.NewRecorder()

			sm.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
		})
	}
}

func TestSupervisorRequestAuditRecordsBoundedPayload(t *testing.T) {
	recorder := events.NewFake()
	resolver := &fakeCityResolver{
		cities:             map[string]*fakeState{},
		supervisorRecorder: recorder,
	}
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	longPath := "/v0/" + strings.Repeat("x", 400)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8372"+longPath+"?secret=not-recorded", nil)
	req.RemoteAddr = "192.168.1.20:49152"
	req.Header.Set("Origin", "http://localhost:8080")
	rec := httptest.NewRecorder()

	sm.Handler().ServeHTTP(rec, req)

	if len(recorder.Events) != 1 {
		t.Fatalf("recorded events = %d, want 1", len(recorder.Events))
	}
	event := recorder.Events[0]
	if event.Type != events.SupervisorRequest {
		t.Fatalf("event type = %q, want %q", event.Type, events.SupervisorRequest)
	}
	if event.Actor != "api" {
		t.Fatalf("event actor = %q, want api", event.Actor)
	}
	var payload SupervisorRequestPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Method != http.MethodGet {
		t.Fatalf("method = %q, want %q", payload.Method, http.MethodGet)
	}
	if payload.Status != rec.Code {
		t.Fatalf("status = %d, want response code %d", payload.Status, rec.Code)
	}
	if len([]rune(payload.Path)) > 256 {
		t.Fatalf("path length = %d, want <= 256", len([]rune(payload.Path)))
	}
	if strings.Contains(payload.Path, "secret") {
		t.Fatalf("path = %q, must not include query string", payload.Path)
	}
	if payload.RemoteAddrClass != "private" {
		t.Fatalf("remote addr class = %q, want private", payload.RemoteAddrClass)
	}
	if payload.Host != "127.0.0.1" {
		t.Fatalf("host = %q, want 127.0.0.1", payload.Host)
	}
	if !payload.OriginAllowed {
		t.Fatal("origin_allowed = false, want true")
	}
	if payload.Phase != supervisorRequestPhaseComplete {
		t.Fatalf("phase = %q, want %q", payload.Phase, supervisorRequestPhaseComplete)
	}
}

func TestSupervisorRequestAuditRecordsEventStreamStartBeforeClose(t *testing.T) {
	recorder := events.NewFake()
	resolver := &fakeCityResolver{
		cities:             map[string]*fakeState{},
		supervisorRecorder: recorder,
	}
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8372/v0/events/stream", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sm.Handler().ServeHTTP(rec, req)
	}()

	start := waitForSupervisorRequestPhase(t, recorder, supervisorRequestPhaseStart)
	if start.Status != 0 {
		t.Fatalf("start status = %d, want 0 before response status is known", start.Status)
	}
	if start.DurationMs != 0 {
		t.Fatalf("start duration_ms = %d, want 0", start.DurationMs)
	}
	if start.Path != "/v0/events/stream" {
		t.Fatalf("start path = %q, want /v0/events/stream", start.Path)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not close after context cancellation")
	}
	complete := waitForSupervisorRequestPhase(t, recorder, supervisorRequestPhaseComplete)
	if complete.Status != http.StatusOK {
		t.Fatalf("complete status = %d, want %d", complete.Status, http.StatusOK)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func waitForSupervisorRequestPhase(t *testing.T, provider events.Provider, phase string) SupervisorRequestPayload {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		evts, err := provider.List(events.Filter{Type: events.SupervisorRequest})
		if err != nil {
			t.Fatalf("list supervisor request events: %v", err)
		}
		for _, event := range evts {
			var payload SupervisorRequestPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if payload.Phase == phase {
				return payload
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for supervisor.request phase %q", phase)
	return SupervisorRequestPayload{}
}
