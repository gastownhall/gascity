package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouting404(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestCORSHeaders(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("OPTIONS", "/v0/status", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("CORS origin = %q, want %q", got, "http://localhost:3000")
	}
}

func TestCORSOnRegularRequest(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/status", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:8080" {
		t.Errorf("CORS origin = %q, want %q", got, "http://127.0.0.1:8080")
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "X-GC-Index, X-GC-Request-Id" {
		t.Errorf("CORS expose = %q, want %q", got, "X-GC-Index, X-GC-Request-Id")
	}
}

func TestRequestIDHeader(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/status", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	rid := rec.Header().Get("X-GC-Request-Id")
	if rid == "" {
		t.Fatal("X-GC-Request-Id header missing")
	}
	if len(rid) != 16 {
		t.Errorf("X-GC-Request-Id length = %d, want 16 hex chars", len(rid))
	}

	// Each request gets a unique ID.
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, httptest.NewRequest("GET", "/v0/status", nil))
	if rec2.Header().Get("X-GC-Request-Id") == rid {
		t.Error("two requests should have different request IDs")
	}
}

func TestCORSRejectsNonLocalhost(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	// Reject obvious non-localhost.
	req := httptest.NewRequest("GET", "/v0/status", nil)
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("CORS origin = %q for non-localhost, want empty", got)
	}

	// Reject localhost spoofing via subdomain (http://localhost.evil.com).
	for _, spoof := range []string{
		"http://localhost.evil.com",
		"http://localhost.evil.com:3000",
		"http://127.0.0.1.evil.com",
	} {
		req = httptest.NewRequest("GET", "/v0/status", nil)
		req.Header.Set("Origin", spoof)
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("CORS origin = %q for spoof %q, want empty", got, spoof)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	// POST to a GET-only endpoint.
	req := newPostRequest(cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Either 404 or 405 is acceptable — Go's mux distinguishes method
	// mismatches from missing paths via 405, but the transitional
	// legacyCityForwarder prefix handler can mask that into 404.
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 or 405", rec.Code)
	}
}

func TestPanicRecovery(t *testing.T) {
	state := newFakeState(t)
	srv := &Server{state: state, mux: http.NewServeMux()}
	srv.mux.HandleFunc("GET /v0/panic", func(_ http.ResponseWriter, _ *http.Request) {
		panic("test panic")
	})

	req := httptest.NewRequest("GET", "/v0/panic", nil)
	rec := httptest.NewRecorder()
	handler := withRecovery(srv.mux)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	// Phase 3 Fix 3d: withRecovery emits RFC 9457 Problem Details.
	var problem struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if problem.Status != http.StatusInternalServerError {
		t.Errorf("problem.status = %d, want %d", problem.Status, http.StatusInternalServerError)
	}
	if problem.Title == "" {
		t.Error("problem.title should be non-empty")
	}
}
