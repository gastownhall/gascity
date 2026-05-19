package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gastownhall/gascity/internal/telemetry"
)

// problemBody is a pre-serialized RFC 9457 Problem Details response emitted
// by mux-level gates that run before Huma takes over (withRecovery, the
// supervisor's service-proxy dispatcher, handler_services' /svc/* gates).
// Pre-serialization satisfies Principle 8: no runtime json.Marshal on
// error paths. Huma handlers do not use these — they return typed
// huma.StatusError values that Huma serializes.
type problemBody struct {
	status int
	body   []byte
}

func (p problemBody) writeTo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(p.status)
	_, _ = w.Write(p.body)
}

var (
	problemInternalServerError = problemBody{
		status: http.StatusInternalServerError,
		body:   []byte(`{"status":500,"title":"Internal Server Error","detail":"internal server error"}`),
	}
	problemCityNameRequired = problemBody{
		status: http.StatusBadRequest,
		body:   []byte(`{"status":400,"title":"Bad Request","detail":"bad_request: city name required in URL"}`),
	}
	problemCityNotFound = problemBody{
		status: http.StatusNotFound,
		body:   []byte(`{"status":404,"title":"Not Found","detail":"not_found: city not found or not running"}`),
	}
	problemServiceRouteNotFound = problemBody{
		status: http.StatusNotFound,
		body:   []byte(`{"status":404,"title":"Not Found","detail":"not_found: service route not found"}`),
	}
	problemServiceReadOnly = problemBody{
		status: http.StatusForbidden,
		body:   []byte(`{"status":403,"title":"Forbidden","detail":"read_only: service mutations are disabled for unpublished services"}`),
	}
	problemServiceCSRFRequired = problemBody{
		status: http.StatusForbidden,
		body:   []byte(`{"status":403,"title":"Forbidden","detail":"csrf: X-GC-Request header required on private service mutation endpoints"}`),
	}
)

type dataSourceKey struct{}

// withLogging wraps a handler with request logging and OTel metrics.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Inject a mutable data source slot into the context so handlers
		// can tag what backend they used (memory, cache, sql, bd_subprocess).
		var source string
		ctx := context.WithValue(r.Context(), dataSourceKey{}, &source)
		r = r.WithContext(ctx)

		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		dur := time.Since(start)
		durMs := float64(dur.Microseconds()) / 1000.0
		if source == "" {
			source = "memory"
		}
		log.Printf("api: %s %s %d %s [%s]", r.Method, r.URL.Path, rw.status, dur.Round(time.Microsecond), source)
		telemetry.RecordHTTPRequest(r.Context(), r.Method, r.URL.Path, rw.status, durMs, source)
	})
}

// withRecovery catches panics and returns an RFC 9457 Problem Details 500.
// Stays outermost at the mux level so it covers non-Huma routes (e.g.
// /svc/* service proxy) that Huma's own recovery wouldn't reach.
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("api: panic: %v\n%s", err, debug.Stack())
				problemInternalServerError.writeTo(w)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withCORS adds restricted CORS headers for localhost dashboard access.
// Only allows localhost origins to prevent browser-origin attacks on mutation endpoints.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if isLocalhostOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID, X-GC-Request")
			w.Header().Set("Access-Control-Expose-Headers", "X-GC-Index, X-GC-Request-Id, Retry-After")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isMutationMethod returns true for HTTP methods that modify state.
func isMutationMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// isLocalhostOrigin checks if an origin is from localhost/127.0.0.1.
// Rejects origins like http://localhost.evil.com by requiring the host
// to be exactly localhost, 127.0.0.1, or [::1] with an optional port.
func isLocalhostOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	// Match http://localhost, http://localhost:PORT
	for _, base := range []string{
		"http://localhost",
		"http://127.0.0.1",
		"http://[::1]",
		"https://localhost",
		"https://127.0.0.1",
		"https://[::1]",
	} {
		if origin == base {
			return true
		}
		// Must be base + ":" + numeric port (no other suffixes like ".evil.com")
		if len(origin) > len(base)+1 && origin[:len(base)] == base && origin[len(base)] == ':' {
			port := origin[len(base)+1:]
			if isNumeric(port) {
				return true
			}
		}
	}
	return false
}

// isNumeric returns true if s is non-empty and contains only ASCII digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// withRequestID adds a unique X-GC-Request-Id header to every response.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf [8]byte
		rand.Read(buf[:]) //nolint:errcheck
		w.Header().Set("X-GC-Request-Id", hex.EncodeToString(buf[:]))
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Unwrap supports http.ResponseController and http.Flusher detection.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
