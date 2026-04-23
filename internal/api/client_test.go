package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
