package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/workspacesvc"
)

func TestClientSuspendCity(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.SuspendCity(); err != nil {
		t.Fatalf("SuspendCity: %v", err)
	}
	if gotMethod != "PATCH" {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/v0/city/alpha" {
		t.Errorf("path = %q, want /v0/city/alpha", gotPath)
	}
	if gotBody["suspended"] != true {
		t.Errorf("body suspended = %v, want true", gotBody["suspended"])
	}
}

func TestClientWaitForEventRequestsReplayCursorForCityStream(t *testing.T) {
	seen := make(chan url.Values, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/events/stream" {
			t.Fatalf("path = %q, want /v0/city/alpha/events/stream", r.URL.Path)
		}
		seen <- r.URL.Query()
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, _ = c.waitForEvent(t.Context(), "req-never", "request.result.session.message", RequestOperationSessionMessage)

	query := <-seen
	if got := query.Get("after_seq"); got != "0" {
		t.Fatalf("after_seq = %q, want 0", got)
	}
}

func TestClientWaitForEventRequestsReplayCursorForSupervisorStream(t *testing.T) {
	seen := make(chan url.Values, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/events/stream" {
			t.Fatalf("path = %q, want /v0/events/stream", r.URL.Path)
		}
		seen <- r.URL.Query()
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	_, _ = c.waitForEvent(t.Context(), "req-never", "request.result.city.create", RequestOperationCityCreate)

	query := <-seen
	if got := query.Get("after_cursor"); got != "0" {
		t.Fatalf("after_cursor = %q, want 0", got)
	}
}

func TestClientWaitForEventReportsNonOKSSEStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"stream unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	_, err := c.waitForEvent(t.Context(), "req-never", "request.result.city.create", RequestOperationCityCreate)
	if err == nil {
		t.Fatal("waitForEvent succeeded for non-OK SSE response")
	}
	if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "stream unavailable") {
		t.Fatalf("error = %q, want status and response detail", err.Error())
	}
}

func TestClientWaitForEventReportsScannerError(t *testing.T) {
	largePayload := strings.Repeat("x", 5*1024*1024)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + largePayload + "\n\n"))
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	_, err := c.waitForEvent(t.Context(), "req-never", "request.result.city.create", RequestOperationCityCreate)
	if err == nil {
		t.Fatal("waitForEvent succeeded after scanner failure")
	}
	if !strings.Contains(err.Error(), "SSE scan") {
		t.Fatalf("error = %q, want scanner error context", err.Error())
	}
}

func TestClientWaitForEventHandlesMultiLineDataFrames(t *testing.T) {
	frame := "event: tagged_event\n" +
		`data: {"type":"request.result.session.message","payload":` + "\n" +
		`data: {"request_id":"req-1","session_id":"gc-1"}}` + "\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(frame))
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	env, err := c.waitForEvent(t.Context(), "req-1", "request.result.session.message", RequestOperationSessionMessage)
	if err != nil {
		t.Fatalf("waitForEvent: %v", err)
	}
	if env.Type != "request.result.session.message" {
		t.Fatalf("event type = %q, want request.result.session.message", env.Type)
	}
}

func TestClientWaitForEventReportsMalformedMatchingSuccessPayload(t *testing.T) {
	frame := `data: {"type":"request.result.session.message","payload":"not an object"}` + "\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(frame))
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.waitForEvent(t.Context(), "req-1", "request.result.session.message", RequestOperationSessionMessage)
	if err == nil {
		t.Fatal("waitForEvent succeeded with malformed matching success payload")
	}
	if !strings.Contains(err.Error(), "decode request.result.session.message payload") {
		t.Fatalf("error = %q, want malformed success payload context", err.Error())
	}
}

func TestClientWaitForEventReportsMalformedRequestFailedPayload(t *testing.T) {
	frame := `data: {"type":"request.failed","payload":"not an object"}` + "\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(frame))
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.waitForEvent(t.Context(), "req-1", "request.result.session.message", RequestOperationSessionMessage)
	if err == nil {
		t.Fatal("waitForEvent succeeded with malformed request.failed payload")
	}
	if !strings.Contains(err.Error(), "decode request.failed payload") {
		t.Fatalf("error = %q, want malformed failure payload context", err.Error())
	}
}

func TestClientWaitForEventHonorsContextCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		<-r.Context().Done()
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := NewClient(ts.URL)
	_, err := c.waitForEvent(ctx, "req-never", "request.result.city.create", RequestOperationCityCreate)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestClientResumeCity(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.ResumeCity(); err != nil {
		t.Fatalf("ResumeCity: %v", err)
	}
	if gotBody["suspended"] != false {
		t.Errorf("body suspended = %v, want false", gotBody["suspended"])
	}
}

func TestClientSuspendAgent(t *testing.T) {
	var gotMethod, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	// Generated client targets the scoped path natively.
	if gotPath != "/v0/city/alpha/agent/worker/suspend" {
		t.Errorf("path = %q, want /v0/city/alpha/agent/worker/suspend", gotPath)
	}
}

func TestClientResumeAgent(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.ResumeAgent("worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	if gotPath != "/v0/city/alpha/agent/worker/resume" {
		t.Errorf("path = %q, want /v0/city/alpha/agent/worker/resume", gotPath)
	}
}

func TestClientSuspendRig(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.SuspendRig("myrig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}
	if gotPath != "/v0/city/alpha/rig/myrig/suspend" {
		t.Errorf("path = %q, want /v0/city/alpha/rig/myrig/suspend", gotPath)
	}
}

func TestClientResumeRig(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.ResumeRig("myrig"); err != nil {
		t.Fatalf("ResumeRig: %v", err)
	}
	if gotPath != "/v0/city/alpha/rig/myrig/resume" {
		t.Errorf("path = %q, want /v0/city/alpha/rig/myrig/resume", gotPath)
	}
}

func TestClientErrorResponse(t *testing.T) {
	// The server speaks RFC 9457 Problem Details on every error. The
	// generated client decodes the body into a typed ErrorModel and the
	// adapter reads the Detail field directly — there's no hand-written
	// JSON parsing or legacy format fallback anywhere in the path.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Not Found",
			"status": http.StatusNotFound,
			"detail": "not_found: agent 'nope' not found",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	err := c.SuspendAgent("nope")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "API error: not_found: agent 'nope' not found" {
		t.Errorf("error = %q", got)
	}
}

func TestClientQualifiedAgentName(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.SuspendAgent("myrig/worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}
	// Qualified agent names now map to explicit {dir}/{base}/{action}
	// route segments — the slash between dir and base must arrive
	// unescaped so the server's ServeMux routes to the qualified variant.
	if gotPath != "/v0/city/alpha/agent/myrig/worker/suspend" {
		t.Errorf("path = %q, want /v0/city/alpha/agent/myrig/worker/suspend", gotPath)
	}
}

func TestClientConnError(t *testing.T) {
	// Client targeting a port with nothing listening → connection refused.
	c := NewCityScopedClient("http://127.0.0.1:1", "alpha") // port 1 is never listening
	err := c.SuspendCity()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsConnError(err) {
		t.Errorf("IsConnError = false for connection refused error: %v", err)
	}
}

func TestClientAPIErrorNotConnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Bad Request",
			"status": http.StatusBadRequest,
			"detail": "bad_request: malformed payload",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	err := c.SuspendCity()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if IsConnError(err) {
		t.Errorf("IsConnError = true for API error response: %v", err)
	}
}

func TestClientReadOnlyFallback(t *testing.T) {
	// Server returns 403 Problem Details with a `read_only:` prefix in
	// detail — should trigger ShouldFallback.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Forbidden",
			"status": http.StatusForbidden,
			"detail": "read_only: mutations disabled: server bound to non-localhost address",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
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
	c := NewCityScopedClient("http://127.0.0.1:1", "alpha")
	err := c.SuspendCity()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(err) {
		t.Errorf("ShouldFallback = false for connection error: %v", err)
	}
}

func TestClientBusinessErrorNoFallback(t *testing.T) {
	// A 404 not_found is a business error — should NOT trigger fallback.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Not Found",
			"status": http.StatusNotFound,
			"detail": "not_found: agent 'nope' not found",
		})
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	err := c.SuspendAgent("nope")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if ShouldFallback(err) {
		t.Errorf("ShouldFallback = true for business error: %v", err)
	}
}

func TestClientRestartRig(t *testing.T) {
	var gotMethod, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.RestartRig("myrig"); err != nil {
		t.Fatalf("RestartRig: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v0/city/alpha/rig/myrig/restart" {
		t.Errorf("path = %q, want /v0/city/alpha/rig/myrig/restart", gotPath)
	}
}

func TestClientListServices(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/services" {
			t.Fatalf("path = %q, want /v0/city/alpha/services", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []workspacesvc.Status{{
				ServiceName:      "healthz",
				Kind:             "workflow",
				MountPath:        "/svc/healthz",
				PublishMode:      "private",
				State:            "ready",
				LocalState:       "ready",
				PublicationState: "private",
			}},
			"total": 1,
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	items, err := c.ListServices()
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(items) != 1 || items[0].ServiceName != "healthz" {
		t.Fatalf("items = %#v, want one healthz service", items)
	}
}

func TestClientGetService(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/service/healthz" {
			t.Fatalf("path = %q, want /v0/city/alpha/service/healthz", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(workspacesvc.Status{ //nolint:errcheck
			ServiceName:      "healthz",
			Kind:             "workflow",
			MountPath:        "/svc/healthz",
			PublishMode:      "private",
			State:            "ready",
			LocalState:       "ready",
			PublicationState: "private",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	status, err := c.GetService("healthz")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if status.ServiceName != "healthz" {
		t.Fatalf("ServiceName = %q, want healthz", status.ServiceName)
	}
}

func TestClientListCities(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/cities" {
			t.Fatalf("path = %q, want /v0/cities", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []CityInfo{{
				Name:    "bright-lights",
				Path:    "/tmp/bright-lights",
				Running: true,
			}},
			"total": 1,
		})
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	items, err := c.ListCities()
	if err != nil {
		t.Fatalf("ListCities: %v", err)
	}
	if len(items) != 1 || items[0].Name != "bright-lights" || !items[0].Running {
		t.Fatalf("items = %#v, want one running bright-lights city", items)
	}
}

func TestCityScopedClientRewritesPaths(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []workspacesvc.Status{},
			"total": 0,
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "bright-lights")
	if _, err := c.ListServices(); err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if gotPath != "/v0/city/bright-lights/services" {
		t.Fatalf("path = %q, want /v0/city/bright-lights/services", gotPath)
	}
}

func TestClientKillSession(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.KillSession("sess-123"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if gotPath != "/v0/city/alpha/session/sess-123/kill" {
		t.Errorf("path = %q, want /v0/city/alpha/session/sess-123/kill", gotPath)
	}
}

func TestClientCSRFHeader(t *testing.T) {
	var gotHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-GC-Request")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	c.SuspendAgent("worker") //nolint:errcheck
	if gotHeader != "true" {
		t.Errorf("X-GC-Request = %q, want %q", gotHeader, "true")
	}
}
