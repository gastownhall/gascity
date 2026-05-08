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

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

func writeSSEEnvelope(t *testing.T, w http.ResponseWriter, typ string, payload any) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Type    string `json:"type"`
		Payload any    `json:"payload"`
	}{
		Type:    typ,
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("marshal SSE envelope: %v", err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(raw)
	_, _ = w.Write([]byte("\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

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
	_, _ = c.waitForEvent(t.Context(), "req-never", "request.result.session.message", RequestOperationSessionMessage, "")

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
	_, _ = c.waitForEvent(t.Context(), "req-never", "request.result.city.create", RequestOperationCityCreate, "")

	query := <-seen
	if got := query.Get("after_cursor"); got != "0" {
		t.Fatalf("after_cursor = %q, want 0", got)
	}
}

func TestClientWaitForEventUsesAcceptedCursorForCityStream(t *testing.T) {
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
	_, _ = c.waitForEvent(t.Context(), "req-never", "request.result.session.message", RequestOperationSessionMessage, "42")

	query := <-seen
	if got := query.Get("after_seq"); got != "42" {
		t.Fatalf("after_seq = %q, want 42", got)
	}
}

func TestClientWaitForEventUsesAcceptedCursorForSupervisorStream(t *testing.T) {
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
	_, _ = c.waitForEvent(t.Context(), "req-never", "request.result.city.create", RequestOperationCityCreate, "alpha:7,__supervisor__:9")

	query := <-seen
	if got := query.Get("after_cursor"); got != "alpha:7,__supervisor__:9" {
		t.Fatalf("after_cursor = %q, want alpha:7,__supervisor__:9", got)
	}
}

func TestClientWaitForEventReportsNonOKSSEStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"stream unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	_, err := c.waitForEvent(t.Context(), "req-never", "request.result.city.create", RequestOperationCityCreate, "")
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
	_, err := c.waitForEvent(t.Context(), "req-never", "request.result.city.create", RequestOperationCityCreate, "")
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
	env, err := c.waitForEvent(t.Context(), "req-1", "request.result.session.message", RequestOperationSessionMessage, "")
	if err != nil {
		t.Fatalf("waitForEvent: %v", err)
	}
	if env.Type != "request.result.session.message" {
		t.Fatalf("event type = %q, want request.result.session.message", env.Type)
	}
}

func TestClientWaitForEventHandlesEventFieldWithoutSpace(t *testing.T) {
	frame := "event:tagged_event\n" +
		`data: {"type":"request.result.session.message","payload":{"request_id":"req-1","session_id":"gc-1"}}` + "\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(frame))
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	env, err := c.waitForEvent(t.Context(), "req-1", "request.result.session.message", RequestOperationSessionMessage, "")
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
	_, err := c.waitForEvent(t.Context(), "req-1", "request.result.session.message", RequestOperationSessionMessage, "")
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
	_, err := c.waitForEvent(t.Context(), "req-1", "request.result.session.message", RequestOperationSessionMessage, "")
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
	_, err := c.waitForEvent(ctx, "req-never", "request.result.city.create", RequestOperationCityCreate, "")
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

func TestClientSendSessionMessageWaitsForResultEvent(t *testing.T) {
	var gotBody struct {
		Message string `json:"message"`
	}
	var gotHeader string
	var sawPost bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/city/alpha/session/sess-123/messages":
			gotHeader = r.Header.Get("X-GC-Request")
			sawPost = true
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatalf("decode message body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"request_id": "req-msg", "event_cursor": "17"}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == "/v0/city/alpha/events/stream":
			if !sawPost {
				t.Fatal("event stream opened before message POST")
			}
			if got := r.URL.Query().Get("after_seq"); got != "17" {
				t.Fatalf("after_seq = %q, want 17", got)
			}
			writeSSEEnvelope(t, w, events.RequestResultSessionMessage, SessionMessageSucceededPayload{
				RequestID: "req-msg",
				SessionID: "sess-123",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	if err := c.SendSessionMessage("sess-123", "wake up"); err != nil {
		t.Fatalf("SendSessionMessage: %v", err)
	}
	if gotBody.Message != "wake up" {
		t.Fatalf("message = %q, want wake up", gotBody.Message)
	}
	if gotHeader != "true" {
		t.Fatalf("X-GC-Request = %q, want true", gotHeader)
	}
}

func TestClientSendSessionMessageReportsAsyncFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/city/alpha/session/sess-123/messages":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"request_id": "req-msg", "event_cursor": "18"}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == "/v0/city/alpha/events/stream":
			writeSSEEnvelope(t, w, events.RequestFailed, RequestFailedPayload{
				RequestID:    "req-msg",
				Operation:    RequestOperationSessionMessage,
				ErrorCode:    "delivery_failed",
				ErrorMessage: "session is gone",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	err := c.SendSessionMessage("sess-123", "wake up")
	if err == nil {
		t.Fatal("SendSessionMessage succeeded after request.failed")
	}
	if !strings.Contains(err.Error(), "message failed: delivery_failed: session is gone") {
		t.Fatalf("error = %q, want async failure detail", err.Error())
	}
}

func TestClientSubmitSessionWaitsForResultEvent(t *testing.T) {
	var gotBody struct {
		Message string `json:"message"`
		Intent  string `json:"intent"`
	}
	var sawPost bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/city/alpha/session/sess-123/submit":
			sawPost = true
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatalf("decode submit body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"request_id": "req-submit", "event_cursor": "21"}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == "/v0/city/alpha/events/stream":
			if !sawPost {
				t.Fatal("event stream opened before submit POST")
			}
			if got := r.URL.Query().Get("after_seq"); got != "21" {
				t.Fatalf("after_seq = %q, want 21", got)
			}
			writeSSEEnvelope(t, w, events.RequestResultSessionSubmit, SessionSubmitSucceededPayload{
				RequestID: "req-submit",
				SessionID: "sess-123",
				Queued:    true,
				Intent:    string(session.SubmitIntentInterruptNow),
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	resp, err := c.SubmitSession("sess-123", "take this now", session.SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("SubmitSession: %v", err)
	}
	if gotBody.Message != "take this now" {
		t.Fatalf("message = %q, want take this now", gotBody.Message)
	}
	if gotBody.Intent != string(session.SubmitIntentInterruptNow) {
		t.Fatalf("intent = %q, want %q", gotBody.Intent, session.SubmitIntentInterruptNow)
	}
	if resp.Status != "accepted" || resp.ID != "sess-123" || !resp.Queued || resp.Intent != session.SubmitIntentInterruptNow {
		t.Fatalf("response = %#v, want accepted queued interrupt_now for sess-123", resp)
	}
}

func TestClientSubmitSessionReportsAsyncFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/city/alpha/session/sess-123/submit":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"request_id": "req-submit", "event_cursor": "22"}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == "/v0/city/alpha/events/stream":
			writeSSEEnvelope(t, w, events.RequestFailed, RequestFailedPayload{
				RequestID:    "req-submit",
				Operation:    RequestOperationSessionSubmit,
				ErrorCode:    "not_ready",
				ErrorMessage: "session is starting",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.SubmitSession("sess-123", "take this now", session.SubmitIntentInterruptNow)
	if err == nil {
		t.Fatal("SubmitSession succeeded after request.failed")
	}
	if !strings.Contains(err.Error(), "submit failed: not_ready: session is starting") {
		t.Fatalf("error = %q, want async failure detail", err.Error())
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
