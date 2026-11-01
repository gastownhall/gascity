package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// okMaintenanceStatusHandler emits a plausible GET /maintenance/status
// body with one prior run and no in-flight cycle. Used as the
// api-happy-path fixture in the six-row matrix.
func okMaintenanceStatusHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled":          true,
			"interval_seconds": int64(604800),
			"in_flight":        false,
			"last_run": map[string]any{
				"started_at":   "2026-04-22T03:00:00Z",
				"finished_at":  "2026-04-22T03:00:05Z",
				"stage":        "done",
				"before_bytes": int64(11000000000),
				"after_bytes":  int64(2000000000),
				"duration_s":   5.0,
			},
			"history": []map[string]any{
				{
					"started_at":   "2026-04-22T03:00:00Z",
					"finished_at":  "2026-04-22T03:00:05Z",
					"stage":        "done",
					"before_bytes": int64(11000000000),
					"after_bytes":  int64(2000000000),
					"duration_s":   5.0,
				},
			},
		})
	})
}

// okMaintenanceTriggerHandler emits a 202 response body carrying a
// synthetic started_at; the handler is idempotent so every request the
// test makes sees the same body regardless of order.
func okMaintenanceTriggerHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":   true,
			"started_at": "2026-04-22T03:00:00Z",
		})
	})
}

// okMaintenanceTriggerWaitHandler emits the full 202 body for a synchronous
// (?wait=true) call. The run has stage=done so the CLI exits 0.
func okMaintenanceTriggerWaitHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":   true,
			"started_at": "2026-04-22T03:00:00Z",
			"run": map[string]any{
				"started_at":   "2026-04-22T03:00:00Z",
				"finished_at":  "2026-04-22T03:00:05Z",
				"stage":        "done",
				"before_bytes": int64(11000000000),
				"after_bytes":  int64(2000000000),
				"duration_s":   5.0,
			},
		})
	})
}

// maintenanceProblemHandler returns a Huma-style Problem Details body at
// the configured status/detail. Matches the test scaffolding used by the
// mail and order read-path matrices so the assertion helpers stay shared.
func maintenanceProblemHandler(status int, detail string) func(*testing.T) http.Handler {
	return func(_ *testing.T) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": status,
				"title":  http.StatusText(status),
				"detail": detail,
			})
		})
	}
}

// assertMaintenanceRouteLog verifies exactly one route=... line with the
// expected shape is present in stderr. Empty wantRoute means "do not assert
// routing" — some rows assert an API error instead.
func assertMaintenanceRouteLog(t *testing.T, stderrStr, wantRoute, wantReason string) {
	t.Helper()
	if wantRoute == "" {
		return
	}
	want := "route=" + wantRoute
	if wantReason != "" {
		want += " reason=" + wantReason
	}
	if !strings.Contains(stderrStr, want) {
		t.Errorf("stderr missing %q:\n%s", want, stderrStr)
	}
	if n := strings.Count(stderrStr, "route="); n != 1 {
		t.Errorf("route=... lines = %d, want 1:\n%s", n, stderrStr)
	}
}

// TestRouteMaintenanceStatus_SixRowMatrix exercises the six mandatory
// rows for `gc maintenance status`. Exit codes diverge from the generic
// mail pattern because no local fallback exists for maintenance reads —
// the in-memory ring buffer lives only in the supervisor process.
// Fallback rows therefore exit 2 (supervisor unreachable) rather than 0.
func TestRouteMaintenanceStatus_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      func(*testing.T) http.Handler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okMaintenanceStatusHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "Maintenance: enabled=yes",
		},
		{
			name:       "api-cache-not-live",
			handler:    maintenanceProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   2, // no local fallback source
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    maintenanceProblemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   2,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
		},
		{
			name:       "api-404-error",
			handler:    maintenanceProblemHandler(http.StatusNotFound, "not_found: no such thing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeMaintenanceStatus(c, tc.nilReason, false, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			assertMaintenanceRouteLog(t, stderr.String(), tc.wantRoute, tc.wantReason)
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

// TestRouteMaintenanceDoltGC_SixRowMatrix exercises the six mandatory
// rows for `gc maintenance dolt-gc`. Like the status subcommand there is
// no local fallback; unlike status, this command is a mutator and uses
// ShouldFallback (not ShouldFallbackForRead) so generic 5xx is a hard
// error (exit 1), not a fallback.
func TestRouteMaintenanceDoltGC_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      func(*testing.T) http.Handler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okMaintenanceTriggerHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "Maintenance accepted",
		},
		{
			name:       "api-cache-not-live",
			handler:    maintenanceProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   2,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    maintenanceProblemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   1, // mutation: generic 5xx is a hard error
			wantStderr: "API error",
		},
		{
			name:       "api-404-error",
			handler:    maintenanceProblemHandler(http.StatusNotFound, "not_found: no such thing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeMaintenanceDoltGC(c, tc.nilReason, false, false, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			assertMaintenanceRouteLog(t, stderr.String(), tc.wantRoute, tc.wantReason)
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

// TestRouteMaintenanceDoltGC_InProgress verifies 409 with
// maintenance-in-progress body maps to exit 3 (bead's documented exit
// code for "already running").
func TestRouteMaintenanceDoltGC_InProgress(t *testing.T) {
	t.Setenv("GC_DEBUG", "1")
	body := `maintenance-in-progress: {"type":"maintenance-in-progress","started_at":"2026-04-22T03:00:00Z"}`
	srv := httptest.NewServer(maintenanceProblemHandler(http.StatusConflict, body)(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", false, false, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (already running); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already in progress") {
		t.Errorf("stderr missing 'already in progress':\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "2026-04-22T03:00:00Z") {
		t.Errorf("stderr missing in-flight started_at:\n%s", stderr.String())
	}
}

// TestRouteMaintenanceDoltGC_WaitFailure verifies that --wait returning a
// run with stage!='done' maps to exit 1.
func TestRouteMaintenanceDoltGC_WaitFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":   true,
			"started_at": "2026-04-22T03:00:00Z",
			"run": map[string]any{
				"started_at":  "2026-04-22T03:00:00Z",
				"finished_at": "2026-04-22T03:00:01Z",
				"stage":       "gc",
				"err":         "CALL DOLT_GC(): lock timeout",
				"duration_s":  1.0,
			},
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", true, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (wait + failure); stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "stage=gc") {
		t.Errorf("stdout missing stage=gc:\n%s", stdout.String())
	}
}

// TestRouteMaintenanceDoltGC_WaitSuccess verifies that --wait returning a
// run with stage='done' maps to exit 0.
func TestRouteMaintenanceDoltGC_WaitSuccess(t *testing.T) {
	srv := httptest.NewServer(okMaintenanceTriggerWaitHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", true, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "stage=done") {
		t.Errorf("stdout missing stage=done:\n%s", stdout.String())
	}
}

// TestRouteMaintenanceStatus_JSONOutput verifies that --json emits a
// stable envelope with a _cache_age_s field mirroring the read-path
// contract.
func TestRouteMaintenanceStatus_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(okMaintenanceStatusHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceStatus(c, "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; want 0; stderr=%q", code, stderr.String())
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, stdout.String())
	}
	if _, ok := env["_cache_age_s"]; !ok {
		t.Errorf("JSON envelope missing _cache_age_s: %v", env)
	}
	if _, ok := env["status"]; !ok {
		t.Errorf("JSON envelope missing status: %v", env)
	}
}

// TestMaintenanceDoltGCAPIClientGrantGatedSupervisor proves `gc maintenance
// dolt-gc` never dials a supervisor-hosted city's supervisor that requires
// city write grants: that client mints no grant, so the trigger would be
// refused with a 401 no caller falls back from. The resolver returns (nil,
// "grant-required") without resolving the supervisor client, and the
// command reports the refusal without an HTTP call. A supervisor that
// requires no grants, or only read grants (its read gate passes the
// trigger's POST through), and a live controller predating identify, still
// route to the supervisor-managed client, as `gc maintenance status` does.
func TestMaintenanceDoltGCAPIClientGrantGatedSupervisor(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	origGrants, origWriteGrants := apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
		apiRouteSupervisorWriteGrantsHook = origWriteGrants
	})
	supervisorHosted := func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}
	// The trigger is a mutation, so only the write-grant requirement decides
	// it; the read-or-write requirement is apiClient's.
	apiRouteSupervisorGrantsHook = func() bool {
		t.Errorf("read-or-write grant requirement consulted for the dolt-gc mutation")
		return true
	}

	t.Run("write-grants-required-skips-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = supervisorHosted
		apiRouteSupervisorClientHook = func(string) *api.Client {
			t.Errorf("supervisor client resolved for a supervisor that requires city write grants")
			return sentinel
		}
		apiRouteSupervisorWriteGrantsHook = func() bool { return true }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceDoltGCAPIClient(dir)
		if c != nil || reason != "grant-required" {
			t.Fatalf("maintenanceDoltGCAPIClient = (%p, %q), want (nil, \"grant-required\")", c, reason)
		}
	})

	t.Run("grants-not-required-routes-to-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = supervisorHosted
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		apiRouteSupervisorWriteGrantsHook = func() bool { return false }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceDoltGCAPIClient(dir)
		if c != sentinel || reason != "" {
			t.Fatalf("maintenanceDoltGCAPIClient = (%p, %q), want (supervisor sentinel %p, \"\")", c, reason, sentinel)
		}
	})

	t.Run("read-grants-only-routes-to-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = supervisorHosted
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		useReadAuthOnlySupervisor(t)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceDoltGCAPIClient(dir)
		if c != sentinel || reason != "" {
			t.Fatalf("maintenanceDoltGCAPIClient = (%p, %q), want (supervisor sentinel %p, \"\") for a supervisor that gates only reads", c, reason, sentinel)
		}
	})

	t.Run("read-grants-only-supervisor-unavailable-is-controller-down", func(t *testing.T) {
		// With no supervisor-managed client, a supervisor that gates only
		// reads is not the reason the trigger cannot run.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = supervisorHosted
		apiRouteSupervisorClientHook = func(string) *api.Client { return nil }
		useReadAuthOnlySupervisor(t)
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceDoltGCAPIClient(dir)
		if c != nil || reason != "controller-down" {
			t.Fatalf("maintenanceDoltGCAPIClient = (%p, %q), want (nil, \"controller-down\")", c, reason)
		}
	})

	t.Run("unknown-hosting-no-api-port-routes-to-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = func(string) controllerIdentityReply { return controllerIdentityReply{PID: 4242} }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		apiRouteSupervisorWriteGrantsHook = func() bool {
			t.Errorf("write-grant requirement consulted for a controller of unknown hosting mode")
			return true
		}
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := maintenanceDoltGCAPIClient(dir)
		if c != sentinel || reason != "" {
			t.Fatalf("maintenanceDoltGCAPIClient = (%p, %q), want (supervisor sentinel %p, \"\")", c, reason, sentinel)
		}
	})

	t.Run("status-still-routes-to-grant-gated-supervisor", func(t *testing.T) {
		// The status read keeps the fall-through: a write-auth-only
		// supervisor serves it without a grant.
		t.Setenv("GC_NO_API", "")
		apiRouteControllerIdentityHook = supervisorHosted
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		apiRouteSupervisorWriteGrantsHook = func() bool { return true }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if c, _ := maintenanceAPIClient(dir); c != sentinel {
			t.Fatalf("maintenanceAPIClient = %p, want supervisor sentinel %p", c, sentinel)
		}
	})
}

// maintenanceDoltGCGrantRefusal is the refusal `gc maintenance dolt-gc` prints
// for a supervisor that requires a city write grant. It names no remote
// --context: the command does not accept one yet.
const maintenanceDoltGCGrantRefusal = "gc maintenance dolt-gc: the supervisor requires a signed city write grant (grant-required), which gc cannot attach to this command yet\n"

// TestRouteMaintenanceDoltGC_GrantRequired verifies the grant-required
// refusal names the cause instead of reporting the supervisor as down.
func TestRouteMaintenanceDoltGC_GrantRequired(t *testing.T) {
	t.Setenv("GC_DEBUG", "1")
	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(nil, "grant-required", false, false, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
	assertMaintenanceRouteLog(t, stderr.String(), "fallback", "grant-required")
	if !strings.Contains(stderr.String(), maintenanceDoltGCGrantRefusal) {
		t.Errorf("stderr missing the grant-required refusal %q:\n%s", maintenanceDoltGCGrantRefusal, stderr.String())
	}
	if strings.Contains(stderr.String(), "not running") {
		t.Errorf("stderr reports the supervisor as not running for a grant refusal:\n%s", stderr.String())
	}
}

// TestCmdMaintenanceDoltGCGrantGatedSupervisor pins `gc maintenance dolt-gc`
// to maintenanceDoltGCAPIClient: for a supervisor-hosted city whose
// supervisor requires city write grants, the command exits 2 with the grant
// refusal without resolving the supervisor client, where maintenanceAPIClient
// would resolve it and send the grantless trigger.
func TestCmdMaintenanceDoltGCGrantGatedSupervisor(t *testing.T) {
	origIdentity, origSup := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook
	origGrants, origWriteGrants := apiRouteSupervisorGrantsHook, apiRouteSupervisorWriteGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
		apiRouteSupervisorWriteGrantsHook = origWriteGrants
	})
	t.Setenv("GC_NO_API", "")
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}
	apiRouteSupervisorClientHook = func(string) *api.Client {
		t.Errorf("supervisor client resolved for a supervisor that requires city write grants")
		return nil
	}
	apiRouteSupervisorGrantsHook = func() bool { return true }
	apiRouteSupervisorWriteGrantsHook = func() bool { return true }
	t.Setenv("GC_CITY", writeCityTOMLForRoute(t, t.TempDir(), "[workspace]\nname = \"t\"\n"))

	var stdout, stderr bytes.Buffer
	if code := cmdMaintenanceDoltGC(false, false, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), maintenanceDoltGCGrantRefusal) {
		t.Errorf("stderr missing the grant-required refusal %q:\n%s", maintenanceDoltGCGrantRefusal, stderr.String())
	}
}
