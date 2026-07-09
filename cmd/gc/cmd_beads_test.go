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

// TestCmdBeadsShow_MissingID_DoesNotProbeAPIClient locks the ordering that a
// Fable red-team caught (2026-07-08): the missing-id guard must fire BEFORE the
// local beadsShowAPIClient seam. That seam's apiClient() call has observable
// side effects — a GC_NO_API-unrecognized warning to os.Stderr, a
// controller-liveness probe, and a config.Load — none of which the old
// hand-written cmdBeadsShow performed on the no-id path. Folding the guard into
// routeReadCmd's route closure ran the seam first (routeReadCmd calls localSeam
// before the closure), reintroducing a warning line ahead of the missing-id
// error. This asserts the structural invariant directly: no bead id => the seam
// is never consulted, and stderr is exactly the missing-id line.
func TestCmdBeadsShow_MissingID_DoesNotProbeAPIClient(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeCityToml(t, cityDir, "[workspace]\nname = \"beads-show-order\"\n")

	prev := beadsShowAPIClient
	t.Cleanup(func() { beadsShowAPIClient = prev })
	seamConsulted := false
	beadsShowAPIClient = func(string) (*api.Client, string) {
		seamConsulted = true
		return nil, "seam-should-not-run"
	}

	var stdout, stderr bytes.Buffer
	code := cmdBeadsShow("", "text", &stdout, &stderr) // no bead id

	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	if seamConsulted {
		t.Fatal("beadsShowAPIClient ran before the missing-id guard — ordering regression: " +
			"the guard must precede the local seam so its side effects stay off the no-id path")
	}
	if got := stderr.String(); got != "gc beads show: missing bead id\n" {
		t.Fatalf("stderr = %q, want exactly \"gc beads show: missing bead id\\n\"", got)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
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
	materializeBuiltinPacksForTest(t, dir)
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

// The tests below drive the REAL cobra entry point via run() with argv flags,
// exercising the flag-PARSING path. The earlier remote test
// (TestCmdBeadsList_RemoteRoutesToServerNoFallback) sets contextFlag/cityURLFlag
// package vars directly, so it never proved cobra parses --city-url on a beads
// command — which it did NOT, because `beads list`/`show` set
// DisableFlagParsing: the persistent remote flags were silently dropped and the
// command fell back to a LOCAL read. These lock the fix (real cobra flags): the
// persistent remote flags now reach the resolver AND the bead-specific flags
// still parse.

// TestRun_BeadsListCityURLFlagRoutesRemote proves `gc --city-url <loopback>
// --city-name mc beads list --status open --label X --all` parses the persistent
// --city-url (routing REMOTE, not the silent local fallback of the
// DisableFlagParsing era) AND parses every bead filter flag, each of which must
// land on the request query. Asserting all three (label/status/all) — not just
// one — catches a wiring drop or a label<->status swap in the RunE beadFilters
// literal that a single-flag assertion would miss.
func TestRun_BeadsListCityURLFlagRoutesRemote(t *testing.T) {
	clearGCEnv(t)

	var gotPath, gotStatus, gotLabel, gotAll string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotStatus = r.URL.Query().Get("status")
		gotLabel = r.URL.Query().Get("label")
		gotAll = r.URL.Query().Get("all")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	prev := beadsListAPIClient
	beadsListAPIClient = func(string) (*api.Client, string) {
		t.Fatal("local beadsListAPIClient must not run under --city-url — the flag was unparsed and it fell back to local")
		return nil, ""
	}
	t.Cleanup(func() { beadsListAPIClient = prev })

	var out, errb bytes.Buffer
	code := run([]string{
		"--city-url", srv.URL, "--city-name", "mc", "beads", "list",
		"--status", "open", "--label", "ready-to-build", "--all",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, errb.String())
	}
	if !strings.Contains(gotPath, "/v0/city/mc/beads") {
		t.Fatalf("remote path = %q, want it to include /v0/city/mc/beads", gotPath)
	}
	if gotStatus != "open" {
		t.Fatalf("--status did not reach the request query: status = %q, want open", gotStatus)
	}
	if gotLabel != "ready-to-build" {
		t.Fatalf("--label did not reach the request query: label = %q, want ready-to-build", gotLabel)
	}
	if gotAll != "true" {
		t.Fatalf("--all did not reach the request query: all = %q, want true", gotAll)
	}
}

// TestRun_BeadsShowCityURLFlagRoutesRemote is the show-side sibling: the bead-id
// positional and the persistent --city-url both parse, routing the single-bead
// read to the remote city (never the local seam).
func TestRun_BeadsShowCityURLFlagRoutesRemote(t *testing.T) {
	clearGCEnv(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	prev := beadsShowAPIClient
	beadsShowAPIClient = func(string) (*api.Client, string) {
		t.Fatal("local beadsShowAPIClient must not run under --city-url")
		return nil, ""
	}
	t.Cleanup(func() { beadsShowAPIClient = prev })

	var out, errb bytes.Buffer
	code := run([]string{"--city-url", srv.URL, "--city-name", "mc", "beads", "show", "ga-abc"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, errb.String())
	}
	if !strings.Contains(gotPath, "ga-abc") {
		t.Fatalf("remote path = %q, want it to include the bead id ga-abc", gotPath)
	}
}

// TestRun_BeadsListRejectsUnknownFlag locks the fail-loud upgrade: with real
// cobra parsing an unknown flag is a hard error routed through the root
// FlagErrorFunc (the DisableFlagParsing era silently swallowed it).
func TestRun_BeadsListRejectsUnknownFlag(t *testing.T) {
	clearGCEnv(t)
	var out, errb bytes.Buffer
	code := run([]string{"beads", "list", "--no-such-flag"}, &out, &errb)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for an unknown flag; stderr = %q", errb.String())
	}
	if !strings.Contains(errb.String(), "unknown flag") {
		t.Fatalf("stderr = %q, want it to mention 'unknown flag'", errb.String())
	}
}

// TestRun_BeadsListHelpNotSwallowed locks that `gc beads list --help` now prints
// help. DisableFlagParsing used to swallow --help (and `beads show --help` even
// tried to resolve a bead literally named "--help").
func TestRun_BeadsListHelpNotSwallowed(t *testing.T) {
	clearGCEnv(t)
	var out, errb bytes.Buffer
	code := run([]string{"beads", "list", "--help"}, &out, &errb)
	if code != 0 {
		t.Fatalf("--help exit = %d, want 0; stderr = %q", code, errb.String())
	}
	if combined := out.String() + errb.String(); !strings.Contains(combined, "--status") {
		t.Fatalf("help output missing the --status flag listing; got %q", combined)
	}
}

// TestRun_BeadsShowMissingIDReachesGuard pins Args:MaximumNArgs(1) (not
// ExactArgs(1)) together with the resolve-before-guard ordering: with a RESOLVED
// remote target, a zero-arg `beads show` must reach the internal missing-id guard
// (printing "missing bead id") and NEVER dispatch to the server. ExactArgs(1)
// would make cobra reject the zero-arg case before the resolver, changing the
// message and inverting the documented ordering — this test would then fail.
func TestRun_BeadsShowMissingIDReachesGuard(t *testing.T) {
	clearGCEnv(t)

	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := run([]string{"--city-url", srv.URL, "--city-name", "mc", "beads", "show"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (missing-id guard); stderr = %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "missing bead id") {
		t.Fatalf("stderr = %q, want the missing-id guard message", errb.String())
	}
	if hit {
		t.Fatal("server was dispatched despite a missing bead id — guard did not fire before dispatch")
	}
}

// TestRun_BeadsShowRejectsExtraArgs locks that a second positional is a loud
// error (MaximumNArgs(1)); the DisableFlagParsing era silently ignored extras
// and showed the first.
func TestRun_BeadsShowRejectsExtraArgs(t *testing.T) {
	clearGCEnv(t)
	var out, errb bytes.Buffer
	code := run([]string{"beads", "show", "ga-abc", "ga-def"}, &out, &errb)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for two positionals; stderr = %q", errb.String())
	}
	if !strings.Contains(errb.String(), "accepts at most 1 arg") {
		t.Fatalf("stderr = %q, want an at-most-1-arg error", errb.String())
	}
}
