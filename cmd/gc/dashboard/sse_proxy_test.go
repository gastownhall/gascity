package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSSEProxyForwardsUpstreamError verifies that handleSSEProxy forwards
// non-200 upstream responses as-is instead of wrapping them in SSE headers.
// Without this check, a 404 from the supervisor would be masked as a
// successful SSE connection, causing the browser to reconnect in a tight
// loop (resetting backoff each time via the "connected" event).
func TestSSEProxyForwardsUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"city not found or not running: gc-work"}`))
	}))
	defer upstream.Close()

	h := NewAPIHandler("/tmp/city", "gc-work", upstream.URL, "gc-work",
		10*time.Second, 30*time.Second, "test-token")

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	rec := httptest.NewRecorder()
	h.handleSSEProxy(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if strings.Contains(rec.Body.String(), "event: connected") {
		t.Error("response must NOT contain SSE connected event on upstream error")
	}
}

// TestSSEProxySuccessWritesConnectedEvent verifies that a 200 upstream
// response produces the expected SSE "connected" event and headers.
func TestSSEProxySuccessWritesConnectedEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		// Close immediately to let the proxy return.
	}))
	defer upstream.Close()

	h := NewAPIHandler("/tmp/city", "gc-work", upstream.URL, "gc-work",
		10*time.Second, 30*time.Second, "test-token")

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	rec := httptest.NewRecorder()
	h.handleSSEProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "event: connected") {
		t.Error("response should contain SSE connected event")
	}
}
