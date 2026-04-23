package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if !ShouldFallback(err) {
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
	if ShouldFallback(err) {
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
	if !ShouldFallback(err) {
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
	if !ShouldFallback(err) {
		t.Errorf("ShouldFallback = false for conn error: %v", err)
	}
}

func TestClientListSessions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/sessions" {
			t.Fatalf("path = %q, want /v0/city/alpha/sessions", r.URL.Path)
		}
		// Verify query parameters were propagated by the wrapper.
		if got, want := r.URL.Query().Get("state"), "active"; got != want {
			t.Errorf("state query = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("template"), "mayor"; got != want {
			t.Errorf("template query = %q, want %q", got, want)
		}
		w.Header().Set("X-GC-Cache-Age-S", "2.5")
		w.Header().Set("Content-Type", "application/json")
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
			"total": 1,
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.ListSessions("active", "mayor", false)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got.Body) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Body))
	}
	if got.Body[0].ID != "gc-abc" || got.Body[0].Template != "mayor" {
		t.Errorf("got[0] = %+v", got.Body[0])
	}
	if got.AgeSeconds != 2.5 {
		t.Errorf("AgeSeconds = %v, want 2.5", got.AgeSeconds)
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
	if !ShouldFallback(err) {
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
	if !ShouldFallback(err) {
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
	if !ShouldFallback(err) {
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
			"total": 1,
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	got, err := c.ListMailInbox("mayor", "")
	if err != nil {
		t.Fatalf("ListMailInbox: %v", err)
	}
	if len(got.Body) != 1 || got.Body[0].ID != "msg-1" || got.Body[0].From != "alice" {
		t.Errorf("got.Body = %+v", got.Body)
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
	if !ShouldFallback(err) {
		t.Errorf("ShouldFallback = false for cache-not-live: %v", err)
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
	if !ShouldFallback(err) {
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
	if !ShouldFallback(err) {
		t.Errorf("ShouldFallback = false for cache-not-live: %v", err)
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
	if !ShouldFallback(err) {
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
	if !ShouldFallback(err) {
		t.Errorf("ShouldFallback = false for cache-not-live: %v", err)
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
	if !ShouldFallback(err) {
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
