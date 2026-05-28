package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

func TestDoBeadsHealth_FileProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(false, false, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Beads provider: healthy") {
		t.Errorf("should show healthy message: %s", stdout.String())
	}
}

func TestDoBeadsHealth_FileProviderQuiet(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(true, false, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("quiet mode should produce no stdout, got: %s", stdout.String())
	}
}

func TestBeadsHealthJSONFileProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "beads", "health", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc beads health --json = %d; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1: %q", len(lines), stdout.String())
	}

	var payload struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		CityPath      string `json:"city_path"`
		Provider      string `json:"provider"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" || !payload.OK || payload.CityPath != dir || payload.Provider != "file" || payload.Status != "healthy" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestDoBeadsHealth_ExecProviderHealthy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := writeTestScript(t, "", 0, "")
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "exec:"+script)

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(false, false, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Beads provider: healthy") {
		t.Errorf("should show healthy message: %s", stdout.String())
	}
}

func TestDoBeadsHealth_ExecProviderUnhealthy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Script always fails → health and recover both fail.
	script := writeTestScript(t, "", 1, "server down")
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "exec:"+script)

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(false, false, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "recovery failed") {
		t.Errorf("stderr should mention recovery failure: %s", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Six-row read-path routing matrix for `gc beads list` and `gc beads show`
// (ADR 0001, ga-h6w).
// ---------------------------------------------------------------------------
//
// Each row exercises one branch of routeBeadsList / routeBeadsShow:
//
//   api-happy-path       API returns 200 with items         route=api, exit 0
//   api-cache-not-live   API returns 503 cache_not_live     fallback, exit 0
//   api-500-fallback     API returns generic 500            fallback (conn-refused), exit 0
//   api-404-error        API returns 404                    no fallback, exit 1
//   controller-down      apiClient returns nil (no env)     fallback (controller-down), exit 0
//   escape-hatch         GC_NO_API truthy                   fallback (escape-hatch), exit 0

type beadsMatrixHandler func(t *testing.T) http.Handler

func okBeadsListHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/beads") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"id": "ga-abc", "title": "from api", "issue_type": "task", "status": "open"},
			},
			"total": 1,
		})
	})
}

func okBeadsShowHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/bead/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "3")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ga-abc", "title": "detail", "issue_type": "task", "status": "open",
		})
	})
}

func beadsProblemHandler(status int, detail string) beadsMatrixHandler {
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

func writeBeadsTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	// File provider so fallback can open stores without bd.
	t.Setenv("GC_BEADS", "file")
	return cityPath
}

func TestRouteBeadsList_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      beadsMatrixHandler
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
			handler:    okBeadsListHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "ga-abc",
		},
		{
			name:       "api-cache-not-live",
			handler:    beadsProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    beadsProblemHandler(http.StatusInternalServerError, "internal: explode"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
		},
		{
			name:       "api-404-error",
			handler:    beadsProblemHandler(http.StatusNotFound, "not_found: city missing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")
			cityPath := writeBeadsTestCity(t)

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeBeadsList(cityPath, c, tc.nilReason, "text", beadFilters{}, &stdout, &stderr)

			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			if tc.wantRoute != "" {
				want := "route=" + tc.wantRoute
				if tc.wantReason != "" {
					want += " reason=" + tc.wantReason
				}
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr.String())
				}
				if n := strings.Count(stderr.String(), "route="); n != 1 {
					t.Errorf("route=... lines = %d, want 1:\n%s", n, stderr.String())
				}
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

func TestRouteBeadsShow_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      beadsMatrixHandler
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
			handler:    okBeadsShowHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "ga-abc",
		},
		{
			name:       "api-cache-not-live",
			handler:    beadsProblemHandler(http.StatusServiceUnavailable, "cache_not_live: priming"),
			wantExit:   1,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
			wantStderr: "not found",
		},
		{
			name:       "api-500-fallback",
			handler:    beadsProblemHandler(http.StatusInternalServerError, "explode"),
			wantExit:   1,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
			wantStderr: "not found",
		},
		{
			name:       "api-404-error",
			handler:    beadsProblemHandler(http.StatusNotFound, "not_found: bead missing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     1,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
			wantStderr:   "not found",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     1,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
			wantStderr:   "not found",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")
			cityPath := writeBeadsTestCity(t)

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeBeadsShow(cityPath, c, tc.nilReason, "ga-missing", "text", &stdout, &stderr)

			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			if tc.wantRoute != "" {
				want := "route=" + tc.wantRoute
				if tc.wantReason != "" {
					want += " reason=" + tc.wantReason
				}
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr.String())
				}
				if n := strings.Count(stderr.String(), "route="); n != 1 {
					t.Errorf("route=... lines = %d, want 1:\n%s", n, stderr.String())
				}
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

func TestRouteBeadsList_APIJSONIncludesCacheAge(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeBeadsTestCity(t)
	srv := httptest.NewServer(okBeadsListHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeBeadsList(cityPath, c, "", "json", beadFilters{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal stdout: %v\n%s", err, stdout.String())
	}
	if _, ok := out["_cache_age_s"]; !ok {
		t.Errorf("_cache_age_s missing from API --json:\n%s", stdout.String())
	}

	// Fallback path must omit the envelope field.
	stdout.Reset()
	stderr.Reset()
	if code := routeBeadsList(cityPath, nil, "controller-down", "json", beadFilters{}, &stdout, &stderr); code != 0 {
		t.Fatalf("fallback exit = %d, stderr=%q", code, stderr.String())
	}
	// Fallback path writes a bare JSON array (writeBeadsJSON) — no envelope.
	trimmed := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(trimmed, "[") {
		t.Errorf("fallback JSON must be a bare array, got:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "_cache_age_s") {
		t.Errorf("_cache_age_s must be absent on fallback:\n%s", stdout.String())
	}
}

func TestRouteBeadsList_StaleBannerOver30s(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeBeadsTestCity(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "45")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}, "total": 0})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeBeadsList(cityPath, c, "", "text", beadFilters{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cache age: 45s") {
		t.Errorf("stale banner missing from human output:\n%s", stdout.String())
	}
}

// TestRouteBeadsList_AllFlag_Fallback verifies that `--all` on the fallback
// path succeeds without a 'bead query requires scan' error and returns closed
// beads alongside open ones. Guards the B1 regression (inverted AllowScan
// logic) and the C1 parity concern (filters.all must plumb through).
func TestRouteBeadsList_AllFlag_Fallback(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeBeadsTestCity(t)

	// Without any filter and without --all, default CLI should still list
	// (AllowScan permitted so the user sees active beads).
	var stdout, stderr bytes.Buffer
	code := doBeadsListFallback(cityPath, "text", beadFilters{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("default list fallback: exit = %d, stderr=%q", code, stderr.String())
	}

	// With --all and no other filter, the fallback must not return
	// 'bead query requires scan'. This is the B1 regression.
	stdout.Reset()
	stderr.Reset()
	code = doBeadsListFallback(cityPath, "text", beadFilters{all: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--all fallback: exit = %d, stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "requires scan") {
		t.Errorf("--all must not trigger 'requires scan' error; stderr=%q", stderr.String())
	}
}

// TestRouteBeadsList_AllFlag_APIQuery verifies that `--all` forwards as the
// `all=true` query parameter on the API path, so the server can set
// IncludeClosed. Without this, API path silently diverges from fallback.
func TestRouteBeadsList_AllFlag_APIQuery(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeBeadsTestCity(t)

	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("X-GC-Cache-Age-S", "1")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{},
			"total": 0,
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeBeadsList(cityPath, c, "", "text", beadFilters{all: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if got := gotQuery.Get("all"); got != "true" {
		t.Errorf("API query param all = %q, want %q; full query=%v", got, "true", gotQuery)
	}

	// Sanity: without --all, no 'all' param is sent.
	stdout.Reset()
	stderr.Reset()
	gotQuery = nil
	if code := routeBeadsList(cityPath, c, "", "text", beadFilters{}, &stdout, &stderr); code != 0 {
		t.Fatalf("no-all exit = %d, stderr=%q", code, stderr.String())
	}
	if got := gotQuery.Get("all"); got == "true" {
		t.Errorf("API query param all sent without --all flag: got=%q", got)
	}
}

func TestDoBeadsHealth_BdSkip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	MaterializeBuiltinPacks(dir) //nolint:errcheck
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(false, false, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Beads provider: healthy") {
		t.Errorf("GC_DOLT=skip should pass: %s", stdout.String())
	}
}
