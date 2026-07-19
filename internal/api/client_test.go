package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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
	if !ShouldFallback(nil, err) {
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
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for connection error: %v", err)
	}
}

func TestClientCacheNotLiveFallback(t *testing.T) {
	// Server returns 503 Problem Details with a `cache_not_live:` prefix.
	// Read-path routing must classify this as fallbackable so the CLI lands
	// on raw bd while the supervisor cache is priming or reconciling.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "cache_not_live: supervisor cache is priming or reconciling; retry via fallback",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListServices()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for cache-not-live rejection: %v", err)
	}
	if IsConnError(err) {
		t.Errorf("IsConnError = true for cache-not-live (should be false): %v", err)
	}
}

func TestClientGenericFiveHundredNoFallbackByDefault(t *testing.T) {
	// A 500 without a known detail prefix is NOT fallbackable by the
	// client classifier on its own — the CLI per-command layer handles
	// transport/5xx-style fallback via IsConnError semantics. This test
	// documents the boundary: apiErrorFromResponse only classifies
	// specific prefixes; other 5xx surface as generic errors.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Internal Server Error",
			"status": http.StatusInternalServerError,
			"detail": "internal: something exploded",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	err := c.SuspendCity()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = true for generic 500: %v", err)
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
	if ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = true for business error: %v", err)
	}
}

// TestClientEnumeratedErrorResponseCarriesProblemDetail covers the P12 pilot
// wire shape: bead ops enumerate their error statuses, so oapi-codegen decodes
// the problem body into ApplicationproblemJSON<code> instead of
// ApplicationproblemJSONDefault. pdOf must find the per-status field or the CLI
// would lose the detail and surface a bare status. GetBead (404) and ListBeads
// (503) exercise two different per-status fields.
func TestClientEnumeratedErrorResponseCarriesProblemDetail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		if r.URL.Path == "/v0/city/alpha/beads" {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"type":   "urn:gascity:error:store-unavailable",
				"code":   "store-unavailable",
				"title":  "Store Unavailable",
				"status": http.StatusServiceUnavailable,
				"detail": "cache_not_live: supervisor cache is priming or reconciling; retry via fallback",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"type":   "urn:gascity:error:bead-not-found",
			"code":   "bead-not-found",
			"title":  "Bead Not Found",
			"status": http.StatusNotFound,
			"detail": "bead bd-x not found",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")

	if _, err := c.GetBead("bd-x"); err == nil {
		t.Fatal("GetBead: expected error, got nil")
	} else if !strings.Contains(err.Error(), "bead bd-x not found") {
		t.Fatalf("GetBead error dropped the problem detail (pdOf per-status extraction): %v", err)
	}

	// ListBeads returns 503 with a cache-not-live prefix, which the classifier
	// turns into a fallbackable error — only reachable if pdOf recovered the
	// detail from the per-status field.
	if _, err := c.ListBeads(ListBeadsOpts{}); err == nil {
		t.Fatal("ListBeads: expected error, got nil")
	} else if !ShouldFallback(nil, err) {
		t.Fatalf("ListBeads 503 cache-not-live should be fallbackable (pdOf per-status extraction): %v", err)
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

func TestClientListRigs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/rigs" {
			t.Fatalf("path = %q, want /v0/city/alpha/rigs", r.URL.Path)
		}
		w.Header().Set("X-GC-Cache-Age-S", "1.5")
		w.Header().Set("Content-Type", "application/json")
		prefix := "fe"
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{
				{"name": "frontend", "path": "/abs/frontend", "prefix": prefix, "suspended": false, "agent_count": 0, "running_count": 0},
				{"name": "backend", "path": "/abs/backend", "suspended": true, "agent_count": 0, "running_count": 0},
			},
			"total": 2,
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.ListRigs()
	if err != nil {
		t.Fatalf("ListRigs: %v", err)
	}
	if len(got.Body) != 2 {
		t.Fatalf("items = %d, want 2", len(got.Body))
	}
	if got.Body[0].Name != "frontend" || got.Body[0].Prefix != "fe" {
		t.Errorf("got[0] = %+v, want frontend/fe", got.Body[0])
	}
	if got.Body[1].Name != "backend" || !got.Body[1].Suspended {
		t.Errorf("got[1] = %+v, want backend/suspended", got.Body[1])
	}
	if got.AgeSeconds != 1.5 {
		t.Errorf("AgeSeconds = %v, want 1.5", got.AgeSeconds)
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

func TestClientListRigs_CacheNotLiveFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "cache_not_live: supervisor cache is priming",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListRigs()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for cache-not-live: %v", err)
	}
}

func TestClientListRigs_ConnErrorFallback(t *testing.T) {
	// Pointing at a closed listener produces a transport-level error
	// classified as fallbackable by ShouldFallback.
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListRigs()
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for conn error: %v", err)
	}
}

func TestClientListSessions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/sessions" {
			t.Fatalf("path = %q, want /v0/city/alpha/sessions", r.URL.Path)
		}
		// Every page propagates the filters and asks for the 1000-row server
		// cap explicitly; a refactor that dropped either would trip here.
		if got, want := r.URL.Query().Get("state"), "active"; got != want {
			t.Errorf("state query = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("template"), "mayor"; got != want {
			t.Errorf("template query = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("limit"), "1000"; got != want {
			t.Errorf("limit query = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		// Two pages walked via next_cursor prove ListSessions follows the
		// keyset walk to completion instead of truncating at the first
		// server-cap page.
		switch cursor := r.URL.Query().Get("cursor"); cursor {
		case "":
			w.Header().Set("X-GC-Cache-Age-S", "2.5")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{
						"id":           "gc-abc",
						"template":     "mayor",
						"state":        "active",
						"title":        "Overseer",
						"session_name": "mayor",
						"provider":     "claude",
						"created_at":   "2026-04-23T10:00:00Z",
						"attached":     true,
						"running":      true,
					},
				},
				"next_cursor": "page2",
				"total":       2,
			})
		case "page2":
			// A different cache-age header on the later page proves the merged
			// result reports the first page's age, not the last.
			w.Header().Set("X-GC-Cache-Age-S", "9.9")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{
						"id":           "gc-def",
						"template":     "polecat",
						"state":        "active",
						"title":        "Worker",
						"session_name": "polecat",
						"provider":     "claude",
						"created_at":   "2026-04-23T09:00:00Z",
						"attached":     false,
						"running":      true,
					},
				},
				"total": 2,
			})
		default:
			// Guard against an over-walk: emit an empty terminal page so the
			// loop stops, and fail the test.
			t.Errorf("unexpected extra page request, cursor = %q", cursor)
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}, "total": 2}) //nolint:errcheck
		}
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.ListSessions("active", "mayor", false)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got.Body) != 2 {
		t.Fatalf("items = %d, want 2 (both pages merged)", len(got.Body))
	}
	if got.Body[0].ID != "gc-abc" || got.Body[1].ID != "gc-def" {
		t.Errorf("merged ids = %q,%q; want gc-abc,gc-def", got.Body[0].ID, got.Body[1].ID)
	}
	if got.AgeSeconds != 2.5 {
		t.Errorf("AgeSeconds = %v, want 2.5 (first page's age)", got.AgeSeconds)
	}
}

func TestClientListSessions_CacheNotLiveFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "cache_not_live: supervisor cache is priming",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListSessions("", "", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for cache-not-live: %v", err)
	}
}

func TestClientGetSession(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/session/mayor" {
			t.Fatalf("path = %q, want /v0/city/alpha/session/mayor", r.URL.Path)
		}
		if got := r.URL.Query().Get("peek"); got != "true" {
			t.Errorf("peek query = %q, want true", got)
		}
		if got := r.URL.Query().Get("peek_lines"); got != "25" {
			t.Errorf("peek_lines query = %q, want 25", got)
		}
		w.Header().Set("X-GC-Cache-Age-S", "0.5")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"id":           "gc-xyz",
			"template":     "mayor",
			"state":        "active",
			"title":        "",
			"session_name": "mayor",
			"provider":     "claude",
			"created_at":   "2026-04-23T10:00:00Z",
			"attached":     true,
			"running":      true,
			"last_output":  "hello\n",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.GetSession("mayor", true, 25)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Body.ID != "gc-xyz" || got.Body.LastOutput != "hello\n" {
		t.Errorf("body = %+v", got.Body)
	}
	if got.AgeSeconds != 0.5 {
		t.Errorf("AgeSeconds = %v, want 0.5", got.AgeSeconds)
	}
}

func TestClientListConvoys(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/convoys" {
			t.Fatalf("path = %q, want /v0/city/alpha/convoys", r.URL.Path)
		}
		w.Header().Set("X-GC-Cache-Age-S", "1.25")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{
				{"id": "gc-1", "title": "deploy", "issue_type": "convoy", "status": "open", "created_at": "2026-04-23T10:00:00Z"},
			},
			"total": 1,
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.ListConvoys()
	if err != nil {
		t.Fatalf("ListConvoys: %v", err)
	}
	if len(got.Body) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Body))
	}
	if got.Body[0].ID != "gc-1" || got.Body[0].Title != "deploy" || got.Body[0].Type != "convoy" {
		t.Errorf("got[0] = %+v", got.Body[0])
	}
	if got.AgeSeconds != 1.25 {
		t.Errorf("AgeSeconds = %v, want 1.25", got.AgeSeconds)
	}
}

func TestClientListConvoys_CacheNotLiveFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "cache_not_live: supervisor cache is priming",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListConvoys()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for cache-not-live: %v", err)
	}
}

func TestClientListConvoys_ConnErrorFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListConvoys()
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for conn error: %v", err)
	}
}

func TestClientGetConvoy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/convoy/gc-1" {
			t.Fatalf("path = %q, want /v0/city/alpha/convoy/gc-1", r.URL.Path)
		}
		w.Header().Set("X-GC-Cache-Age-S", "3")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"convoy":   map[string]any{"id": "gc-1", "title": "deploy", "issue_type": "convoy", "status": "open", "created_at": "2026-04-23T10:00:00Z"},
			"children": []map[string]any{{"id": "gc-2", "title": "task a", "issue_type": "task", "status": "closed", "created_at": "2026-04-23T10:00:00Z"}},
			"progress": map[string]any{"total": 1, "closed": 1},
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.GetConvoy("gc-1")
	if err != nil {
		t.Fatalf("GetConvoy: %v", err)
	}
	if got.Body.Convoy.ID != "gc-1" || got.Body.Convoy.Title != "deploy" {
		t.Errorf("Convoy = %+v", got.Body.Convoy)
	}
	if len(got.Body.Children) != 1 || got.Body.Children[0].ID != "gc-2" {
		t.Errorf("Children = %+v", got.Body.Children)
	}
	if got.Body.Progress.Total != 1 || got.Body.Progress.Closed != 1 {
		t.Errorf("Progress = %+v", got.Body.Progress)
	}
	if got.AgeSeconds != 3 {
		t.Errorf("AgeSeconds = %v, want 3", got.AgeSeconds)
	}
}

func TestClientCheckConvoy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/convoy/gc-1/check" {
			t.Fatalf("path = %q, want /v0/city/alpha/convoy/gc-1/check", r.URL.Path)
		}
		w.Header().Set("X-GC-Cache-Age-S", "0")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"convoy_id": "gc-1",
			"total":     2,
			"closed":    2,
			"complete":  true,
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.CheckConvoy("gc-1")
	if err != nil {
		t.Fatalf("CheckConvoy: %v", err)
	}
	if got.Body.ConvoyID != "gc-1" || got.Body.Total != 2 || got.Body.Closed != 2 || !got.Body.Complete {
		t.Errorf("Body = %+v", got.Body)
	}
}

func TestCacheAgeFromResponse(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   float64
	}{
		{"absent", "", 0},
		{"zero", "0", 0},
		{"positive", "42.5", 42.5},
		{"negative clamped to zero", "-1", 0},
		{"invalid", "not-a-number", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Response{Header: http.Header{}}
			if tc.header != "" {
				r.Header.Set("X-GC-Cache-Age-S", tc.header)
			}
			if got := cacheAgeFromResponse(r); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	if got := cacheAgeFromResponse(nil); got != 0 {
		t.Errorf("nil response: got %v, want 0", got)
	}
}

func TestClientListMailInbox(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/mail" {
			t.Fatalf("path = %q, want /v0/city/alpha/mail", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{
				{"id": "msg-1", "from": "alice", "to": "mayor", "subject": "hi", "body": "hello", "created_at": "2026-04-23T10:00:00Z", "read": false},
			},
			"total":          1,
			"partial":        true,
			"partial_errors": []string{"mail provider slow: store_slow: mail read timed out after 8s"},
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.ListMailInbox("mayor", "")
	if err != nil {
		t.Fatalf("ListMailInbox: %v", err)
	}
	if len(got.Body.Items) != 1 || got.Body.Items[0].ID != "msg-1" || got.Body.Items[0].From != "alice" {
		t.Errorf("got.Body = %+v", got.Body)
	}
	if got.Body.Total != 1 || !got.Body.Partial {
		t.Errorf("list metadata = total:%d partial:%v, want total:1 partial:true", got.Body.Total, got.Body.Partial)
	}
	if len(got.Body.PartialErrors) != 1 || !strings.Contains(got.Body.PartialErrors[0], "store_slow:") {
		t.Errorf("PartialErrors = %v, want store_slow entry", got.Body.PartialErrors)
	}
	if got.AgeSeconds != 2 {
		t.Errorf("AgeSeconds = %v, want 2", got.AgeSeconds)
	}
	if !strings.Contains(gotQuery, "agent=mayor") {
		t.Errorf("query = %q, missing agent=mayor", gotQuery)
	}
}

func TestClientListMailInbox_CacheNotLiveFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "cache_not_live: supervisor cache is priming",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListMailInbox("mayor", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for cache-not-live: %v", err)
	}
}

func TestClientListMailInbox_StoreSlowDoesNotFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "store_slow: mail read timed out after 8s",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListMailInbox("mayor", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsStoreSlowError(err) {
		t.Fatalf("IsStoreSlowError = false for store_slow response: %v", err)
	}
	if ShouldFallbackForRead(nil, err) {
		t.Errorf("ShouldFallbackForRead = true for store_slow: %v", err)
	}
	if ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = true for store_slow: %v", err)
	}
}

func TestClientListMailInbox_ConnErrorFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListMailInbox("mayor", "")
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for conn error: %v", err)
	}
}

func TestClientGetMail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/mail/msg-1" {
			t.Fatalf("path = %q, want /v0/city/alpha/mail/msg-1", r.URL.Path)
		}
		w.Header().Set("X-GC-Cache-Age-S", "5")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"id":         "msg-1",
			"from":       "alice",
			"to":         "mayor",
			"subject":    "hi",
			"body":       "hello",
			"created_at": "2026-04-23T10:00:00Z",
			"read":       true,
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.GetMail("msg-1", "")
	if err != nil {
		t.Fatalf("GetMail: %v", err)
	}
	if got.Body.ID != "msg-1" || got.Body.From != "alice" || !got.Body.Read {
		t.Errorf("got.Body = %+v", got.Body)
	}
	if got.AgeSeconds != 5 {
		t.Errorf("AgeSeconds = %v, want 5", got.AgeSeconds)
	}
}

func TestClientGetMail_CacheNotLiveFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "cache_not_live: supervisor cache is priming",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.GetMail("msg-1", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for cache-not-live: %v", err)
	}
}

func TestClientGetMail_StoreSlowDoesNotFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "store_slow: mail read timed out after 8s",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.GetMail("msg-1", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsStoreSlowError(err) {
		t.Fatalf("IsStoreSlowError = false for store_slow response: %v", err)
	}
	if ShouldFallbackForRead(nil, err) {
		t.Errorf("ShouldFallbackForRead = true for store_slow: %v", err)
	}
	if ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = true for store_slow: %v", err)
	}
}

func TestClientGetMail_ConnErrorFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.GetMail("msg-1", "")
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for conn error: %v", err)
	}
}

func TestClientCountMail(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/mail/count" {
			t.Fatalf("path = %q, want /v0/city/alpha/mail/count", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-GC-Cache-Age-S", "1")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"total": 5, "unread": 2}) //nolint:errcheck
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.CountMail("mayor", "myrig")
	if err != nil {
		t.Fatalf("CountMail: %v", err)
	}
	if got.Body.Total != 5 || got.Body.Unread != 2 {
		t.Errorf("got.Body = %+v", got.Body)
	}
	if got.AgeSeconds != 1 {
		t.Errorf("AgeSeconds = %v, want 1", got.AgeSeconds)
	}
	if !strings.Contains(gotQuery, "agent=mayor") || !strings.Contains(gotQuery, "rig=myrig") {
		t.Errorf("query = %q, missing agent=mayor / rig=myrig", gotQuery)
	}
}

func TestClientCountMail_CacheNotLiveFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "cache_not_live: supervisor cache is priming",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.CountMail("mayor", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for cache-not-live: %v", err)
	}
}

func TestClientCountMail_StoreSlowDoesNotFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": "store_slow: mail read timed out after 8s",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.CountMail("mayor", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsStoreSlowError(err) {
		t.Fatalf("IsStoreSlowError = false for store_slow response: %v", err)
	}
	if ShouldFallbackForRead(nil, err) {
		t.Errorf("ShouldFallbackForRead = true for store_slow: %v", err)
	}
	if ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = true for store_slow: %v", err)
	}
}

func TestClientCountMail_ConnErrorFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.CountMail("mayor", "")
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
	if !ShouldFallback(nil, err) {
		t.Errorf("ShouldFallback = false for conn error: %v", err)
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

// TestListBeadsFollowsNextCursor proves ListBeads drains every page by
// following next_cursor. Previously it made one request and the server's
// page-default (50) silently truncated a larger city to the first page.
func TestListBeadsFollowsNextCursor(t *testing.T) {
	var reqs int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		if got := r.URL.Query().Get("limit"); got != "1000" {
			t.Errorf("page limit = %q, want 1000", got)
		}
		id, next := "", ""
		switch r.URL.Query().Get("cursor") {
		case "":
			id, next = "ga-1", "c1"
		case "c1":
			id, next = "ga-2", "c2"
		case "c2":
			id, next = "ga-3", ""
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
		body := map[string]any{"items": []map[string]any{{"id": id, "title": "t", "issue_type": "task", "status": "open"}}}
		if next != "" {
			body["next_cursor"] = next
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	res, err := c.ListBeads(ListBeadsOpts{})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if len(res.Body) != 3 {
		t.Fatalf("got %d beads, want 3 (pagination not followed)", len(res.Body))
	}
	if res.Body[0].ID != "ga-1" || res.Body[1].ID != "ga-2" || res.Body[2].ID != "ga-3" {
		t.Fatalf("ids = %q, want [ga-1 ga-2 ga-3]", []string{res.Body[0].ID, res.Body[1].ID, res.Body[2].ID})
	}
	if n := atomic.LoadInt32(&reqs); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
}

// TestListBeadsHonorsLimitBound proves a positive Limit is a total bound: the
// loop stops once reached without fetching the advertised next page.
func TestListBeadsHonorsLimitBound(t *testing.T) {
	var reqs int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		if got := r.URL.Query().Get("limit"); got != "2" {
			t.Errorf("limit = %q, want 2", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"id": "ga-1", "title": "t", "issue_type": "task", "status": "open"},
				{"id": "ga-2", "title": "t", "issue_type": "task", "status": "open"},
			},
			"next_cursor": "c1",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	res, err := c.ListBeads(ListBeadsOpts{Limit: 2})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if len(res.Body) != 2 {
		t.Fatalf("got %d, want 2", len(res.Body))
	}
	if n := atomic.LoadInt32(&reqs); n != 1 {
		t.Fatalf("requests = %d, want 1 (bound reached, no second page)", n)
	}
}

// TestListBeadsNonAdvancingCursorErrors proves a buggy server that echoes the
// same next_cursor forever makes ListBeads fail loudly instead of hanging.
func TestListBeadsNonAdvancingCursorErrors(t *testing.T) {
	var reqs int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":       []map[string]any{{"id": "ga-1", "title": "t", "issue_type": "task", "status": "open"}},
			"next_cursor": "stuck",
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListBeads(ListBeadsOpts{})
	if err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("err = %v, want 'repeated'", err)
	}
	if n := atomic.LoadInt32(&reqs); n != 2 {
		t.Fatalf("requests = %d, want 2 (bounded)", n)
	}
}

// TestListBeadsCursorCycleErrors proves the drain also catches a multi-cursor
// cycle (A,B,A,B,…), not just an immediately-repeated cursor: a server that
// alternates two cursors forever must still be bounded, not spun indefinitely.
func TestListBeadsCursorCycleErrors(t *testing.T) {
	var reqs int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		next := "a"
		switch r.URL.Query().Get("cursor") {
		case "a":
			next = "b" // a -> b
		case "b":
			next = "a" // b -> a: closes the cycle, revisiting a
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":       []map[string]any{{"id": "ga-1", "title": "t", "issue_type": "task", "status": "open"}},
			"next_cursor": next,
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.ListBeads(ListBeadsOpts{})
	if err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("err = %v, want 'repeated' (cycle not caught)", err)
	}
	// page0 cursor="" -> a (mark a); page1 cursor=a -> b (mark b);
	// page2 cursor=b -> a, already seen -> abort. Exactly 3 requests.
	if n := atomic.LoadInt32(&reqs); n != 3 {
		t.Fatalf("requests = %d, want 3 (cycle bounded)", n)
	}
}

// TestListBeadsEmptyResultIsNonNilSlice pins the empty-result contract: an empty
// city must yield an empty, non-nil slice so `--json` emits `beads: []`, not
// `beads: null`. A nil []beads.Bead marshals to JSON null, which breaks
// `jq '.beads[]'` and diverges from the direct-bd lane.
func TestListBeadsEmptyResultIsNonNilSlice(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	res, err := c.ListBeads(ListBeadsOpts{})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if res.Body == nil {
		t.Fatal("res.Body is nil; empty result must be a non-nil empty slice")
	}
	if len(res.Body) != 0 {
		t.Fatalf("len(res.Body) = %d, want 0", len(res.Body))
	}
	j, err := json.Marshal(res.Body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(j) != "[]" {
		t.Fatalf("json(res.Body) = %s, want []", j)
	}
}

// TestListBeadsDedupesAcrossPages proves a bead re-appearing at a page boundary
// (a write shifted the non-snapshot offset window) is returned once, not twice.
func TestListBeadsDedupesAcrossPages(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var items []map[string]any
		var next string
		switch r.URL.Query().Get("cursor") {
		case "":
			items = []map[string]any{
				{"id": "ga-1", "title": "t", "issue_type": "task", "status": "open"},
				{"id": "ga-2", "title": "t", "issue_type": "task", "status": "open"},
			}
			next = "c1"
		case "c1":
			items = []map[string]any{
				{"id": "ga-2", "title": "t", "issue_type": "task", "status": "open"}, // boundary dup
				{"id": "ga-3", "title": "t", "issue_type": "task", "status": "open"},
			}
		}
		body := map[string]any{"items": items}
		if next != "" {
			body["next_cursor"] = next
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	res, err := c.ListBeads(ListBeadsOpts{})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if len(res.Body) != 3 {
		t.Fatalf("got %d beads, want 3 (ga-2 deduped)", len(res.Body))
	}
	counts := map[string]int{}
	for _, b := range res.Body {
		counts[b.ID]++
	}
	if counts["ga-2"] != 1 {
		t.Fatalf("ga-2 appeared %d times, want 1", counts["ga-2"])
	}
}

// TestListBeadsParams proves the extracted filter mapping sets exactly the
// requested query parameters and leaves the rest nil, and that All maps to a
// pointer-to-true only when requested.
func TestListBeadsParams(t *testing.T) {
	got := listBeadsParams(ListBeadsOpts{Status: "open", Type: "task", Label: "urgent", Assignee: "me", Rig: "core", All: true})
	if got.Status == nil || *got.Status != "open" {
		t.Errorf("Status = %v, want open", got.Status)
	}
	if got.Type == nil || *got.Type != "task" {
		t.Errorf("Type = %v, want task", got.Type)
	}
	if got.Label == nil || *got.Label != "urgent" {
		t.Errorf("Label = %v, want urgent", got.Label)
	}
	if got.Assignee == nil || *got.Assignee != "me" {
		t.Errorf("Assignee = %v, want me", got.Assignee)
	}
	if got.Rig == nil || *got.Rig != "core" {
		t.Errorf("Rig = %v, want core", got.Rig)
	}
	if got.All == nil || !*got.All {
		t.Errorf("All = %v, want *true", got.All)
	}

	empty := listBeadsParams(ListBeadsOpts{})
	if empty.Status != nil || empty.Type != nil || empty.Label != nil || empty.Assignee != nil || empty.Rig != nil || empty.All != nil {
		t.Errorf("empty opts set a non-nil filter: %+v", empty)
	}
}

// TestDrainBeadPagesHardPageCap proves the hard page cap bounds a server that
// returns infinitely many DISTINCT, strictly-advancing cursors with empty
// pages — the one hostile shape the seenCursors repeat-guard cannot catch
// (cursors never repeat) and opts.Limit cannot break (dedup keeps `all` empty).
func TestDrainBeadPagesHardPageCap(t *testing.T) {
	var reqs int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&reqs, 1)
		w.Header().Set("Content-Type", "application/json")
		// A distinct, strictly-longer cursor each page, and never any items.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":       []map[string]any{},
			"next_cursor": strings.Repeat("a", int(n)),
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	_, err := c.drainBeadPages(listBeadsParams(ListBeadsOpts{}), 0, 3)
	if err == nil || !strings.Contains(err.Error(), "exceeded 3 pages") {
		t.Fatalf("err = %v, want 'exceeded 3 pages'", err)
	}
	if n := atomic.LoadInt32(&reqs); n != 3 {
		t.Fatalf("requests = %d, want 3 (page cap bounds the loop)", n)
	}
}
