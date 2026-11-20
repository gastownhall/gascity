package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouting404(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", "/nonexistent", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestCORSHeaders(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("OPTIONS", "/v0/status", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("CORS origin = %q, want %q", got, "http://localhost:3000")
	}
}

func TestCORSOnRegularRequest(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", "/v0/status", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:8080" {
		t.Errorf("CORS origin = %q, want %q", got, "http://127.0.0.1:8080")
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "X-GC-Index, X-GC-Request-Id, Retry-After" {
		t.Errorf("CORS expose = %q, want %q", got, "X-GC-Index, X-GC-Request-Id, Retry-After")
	}
}

func TestRequestIDHeader(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", "/v0/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	rid := rec.Header().Get("X-GC-Request-Id")
	if rid == "" {
		t.Fatal("X-GC-Request-Id header missing")
	}
	if len(rid) != 16 {
		t.Errorf("X-GC-Request-Id length = %d, want 16 hex chars", len(rid))
	}

	// Each request gets a unique ID.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v0/status", nil))
	if rec2.Header().Get("X-GC-Request-Id") == rid {
		t.Error("two requests should have different request IDs")
	}
}

func TestCORSRejectsNonLocalhost(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	// Reject obvious non-localhost.
	req := httptest.NewRequest("GET", "/v0/status", nil)
	req.Header.Set("Origin", "http://evil.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("CORS origin = %q for spoof %q, want empty", got, spoof)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	// POST to a GET-only endpoint. Go 1.22+ mux returns 405 when a
	// path has handlers for other methods but not the requested one.
	req := newPostRequest(cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
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

// TestPanicRecoveryPreservesHTTPAbortSemantics exercises the recovery boundary
// used by service proxies when a response stream cannot continue. Only the
// exact net/http sentinel aborts; application panics still receive Problem Details.
func TestPanicRecoveryPreservesHTTPAbortSemantics(t *testing.T) {
	for _, tc := range []struct {
		name          string
		panicValue    any
		streamStarted bool
		wantAbort     bool
	}{
		{name: "before response", panicValue: http.ErrAbortHandler, wantAbort: true},
		{name: "after streamed response", panicValue: http.ErrAbortHandler, streamStarted: true, wantAbort: true},
		{name: "wrapped sentinel is application panic", panicValue: fmt.Errorf("proxy failed: %w", http.ErrAbortHandler)},
		{name: "same message is application panic", panicValue: errors.New(http.ErrAbortHandler.Error())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs lockedBuffer
			previousLog := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previousLog) })

			req := httptest.NewRequest(http.MethodGet, "/svc/test/events", nil)
			rec := httptest.NewRecorder()
			const prefix = "data: ready\\n\\n"
			handler := withRecovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.streamStarted {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(prefix))
				}
				panic(tc.panicValue)
			}))
			var gotPanic any
			func() {
				defer func() { gotPanic = recover() }()
				handler.ServeHTTP(rec, req)
			}()

			if tc.wantAbort {
				switch gotPanic {
				case http.ErrAbortHandler:
				default:
					t.Fatalf("panic = %v, want exact http.ErrAbortHandler", gotPanic)
				}
				wantBody, wantContentType := "", ""
				if tc.streamStarted {
					wantBody, wantContentType = prefix, "text/event-stream"
				}
				if rec.Code != http.StatusOK || rec.Body.String() != wantBody || rec.Header().Get("Content-Type") != wantContentType {
					t.Fatalf("abort changed response: status=%d body=%q content-type=%q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
				}
				if got := logs.String(); got != "" {
					t.Fatalf("abort logged as application panic: %s", got)
				}
				return
			}
			if gotPanic != nil {
				t.Fatalf("application panic escaped recovery: %v", gotPanic)
			}
			if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("application panic response: status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
			}
			var problem struct {
				Status int `json:"status"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil || problem.Status != http.StatusInternalServerError {
				t.Fatalf("application panic problem body = %q, error = %v", rec.Body.String(), err)
			}
			if !strings.Contains(logs.String(), "api: panic: ") {
				t.Fatal("application panic was not logged")
			}
		})
	}
}
