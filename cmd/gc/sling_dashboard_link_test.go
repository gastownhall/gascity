package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// stubSlingDashboardSupervisor fakes supervisor liveness and base-URL
// discovery for the resolver tests. Restored on cleanup.
func stubSlingDashboardSupervisor(t *testing.T, alivePID int, baseURL string, baseErr error) {
	t.Helper()
	oldAlive := supervisorAliveHook
	oldBase := supervisorAPIBaseURLHook
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorAPIBaseURLHook = oldBase
	})
	supervisorAliveHook = func() int { return alivePID }
	supervisorAPIBaseURLHook = func() (string, error) { return baseURL, baseErr }
}

// registerSlingDashboardCity points GC_HOME at a temp registry and registers
// a city under the given supervisor name, returning the city path.
func registerSlingDashboardCity(t *testing.T, name string) string {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, name); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

// slingDashboardHealthServer serves GET /api/health with the given status.
func slingDashboardHealthServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSlingDashboardURLWorkflowRunDetail(t *testing.T) {
	cityPath := registerSlingDashboardCity(t, "bright-lights")
	srv := slingDashboardHealthServer(t, http.StatusOK)
	stubSlingDashboardSupervisor(t, 4242, srv.URL, nil)

	got := slingDashboardURL(cityPath, sling.SlingResult{WorkflowID: "gcg-run-1", BeadID: "gcg-run-1"})
	want := srv.URL + "/city/bright-lights/runs/gcg-run-1"
	if got != want {
		t.Fatalf("slingDashboardURL = %q, want %q", got, want)
	}
}

func TestSlingDashboardURLRunsListVariants(t *testing.T) {
	cityPath := registerSlingDashboardCity(t, "bright-lights")
	srv := slingDashboardHealthServer(t, http.StatusOK)
	stubSlingDashboardSupervisor(t, 4242, srv.URL, nil)

	want := srv.URL + "/city/bright-lights/runs"
	tests := []struct {
		name   string
		result sling.SlingResult
	}{
		{"wisp", sling.SlingResult{BeadID: "b-1", WispRootID: "w-1"}},
		{"plain bead", sling.SlingResult{BeadID: "b-1"}},
		{"idempotent skip", sling.SlingResult{BeadID: "b-1", Idempotent: true}},
		{"batch", sling.SlingResult{
			BeadID: "convoy-1", ContainerType: "convoy", Total: 2, Routed: 2,
			Children: []sling.SlingChildResult{
				{BeadID: "c-1", Routed: true, WorkflowID: "gcg-c1"},
				{BeadID: "c-2", Routed: true, WorkflowID: "gcg-c2"},
			},
		}},
		{"batch with top-level workflow id", sling.SlingResult{
			BeadID: "convoy-1", WorkflowID: "gcg-c1", ContainerType: "convoy", Total: 1, Routed: 1,
			Children: []sling.SlingChildResult{{BeadID: "c-1", Routed: true, WorkflowID: "gcg-c1"}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slingDashboardURL(cityPath, tt.result); got != want {
				t.Fatalf("slingDashboardURL = %q, want %q", got, want)
			}
		})
	}
}

func TestSlingDashboardURLSuppressed(t *testing.T) {
	workflowResult := sling.SlingResult{WorkflowID: "gcg-run-1", BeadID: "gcg-run-1"}

	t.Run("supervisor down", func(t *testing.T) {
		cityPath := registerSlingDashboardCity(t, "bright-lights")
		srv := slingDashboardHealthServer(t, http.StatusOK)
		stubSlingDashboardSupervisor(t, 0, srv.URL, nil)
		if got := slingDashboardURL(cityPath, workflowResult); got != "" {
			t.Fatalf("slingDashboardURL = %q, want empty when supervisor is down", got)
		}
	})

	t.Run("base url error", func(t *testing.T) {
		cityPath := registerSlingDashboardCity(t, "bright-lights")
		stubSlingDashboardSupervisor(t, 4242, "", fmt.Errorf("no supervisor config"))
		if got := slingDashboardURL(cityPath, workflowResult); got != "" {
			t.Fatalf("slingDashboardURL = %q, want empty on base URL failure", got)
		}
	})

	t.Run("city unregistered", func(t *testing.T) {
		registerSlingDashboardCity(t, "bright-lights")
		srv := slingDashboardHealthServer(t, http.StatusOK)
		stubSlingDashboardSupervisor(t, 4242, srv.URL, nil)
		other := filepath.Join(t.TempDir(), "other-city")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := slingDashboardURL(other, workflowResult); got != "" {
			t.Fatalf("slingDashboardURL = %q, want empty for unregistered city", got)
		}
	})

	t.Run("dashboard-invalid city name", func(t *testing.T) {
		// Valid per the supervisor registry grammar (dots allowed) but
		// invalid per the stricter BFF grammar — dashboard-unreachable.
		cityPath := registerSlingDashboardCity(t, "bright.lights")
		srv := slingDashboardHealthServer(t, http.StatusOK)
		stubSlingDashboardSupervisor(t, 4242, srv.URL, nil)
		if got := slingDashboardURL(cityPath, workflowResult); got != "" {
			t.Fatalf("slingDashboardURL = %q, want empty for BFF-invalid name", got)
		}
	})

	t.Run("health probe non-200", func(t *testing.T) {
		cityPath := registerSlingDashboardCity(t, "bright-lights")
		srv := slingDashboardHealthServer(t, http.StatusNotFound)
		stubSlingDashboardSupervisor(t, 4242, srv.URL, nil)
		if got := slingDashboardURL(cityPath, workflowResult); got != "" {
			t.Fatalf("slingDashboardURL = %q, want empty when dashboard is not mounted", got)
		}
	})

	t.Run("health probe unreachable", func(t *testing.T) {
		cityPath := registerSlingDashboardCity(t, "bright-lights")
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()
		stubSlingDashboardSupervisor(t, 4242, base, nil)
		if got := slingDashboardURL(cityPath, workflowResult); got != "" {
			t.Fatalf("slingDashboardURL = %q, want empty when probe cannot connect", got)
		}
	})
}

func TestDashboardHealthOK(t *testing.T) {
	t.Run("200", func(t *testing.T) {
		srv := slingDashboardHealthServer(t, http.StatusOK)
		if !dashboardHealthOK(srv.URL) {
			t.Fatal("dashboardHealthOK = false, want true for 200")
		}
	})
	t.Run("500", func(t *testing.T) {
		srv := slingDashboardHealthServer(t, http.StatusInternalServerError)
		if dashboardHealthOK(srv.URL) {
			t.Fatal("dashboardHealthOK = true, want false for 500")
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()
		if dashboardHealthOK(base) {
			t.Fatal("dashboardHealthOK = true, want false for closed server")
		}
	})
}

// stubSlingDashboardLink replaces the wiring hook and records the city path
// it was invoked with.
func stubSlingDashboardLink(t *testing.T, url string) *string {
	t.Helper()
	old := slingDashboardURLHook
	t.Cleanup(func() { slingDashboardURLHook = old })
	var gotCityPath string
	slingDashboardURLHook = func(cityPath string, _ sling.SlingResult) string {
		gotCityPath = cityPath
		return url
	}
	return &gotCityPath
}

func TestDoSlingBatchPrintsDashboardLine(t *testing.T) {
	link := "http://127.0.0.1:8372/city/test-city/runs"
	gotCityPath := stubSlingDashboardLink(t, link)

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSlingBatch(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	slungIdx := strings.Index(out, "Slung BL-42")
	dashIdx := strings.Index(out, "Dashboard: "+link)
	if slungIdx == -1 || dashIdx == -1 {
		t.Fatalf("stdout = %q, want sling confirmation followed by dashboard line", out)
	}
	if dashIdx < slungIdx {
		t.Fatalf("stdout = %q, want dashboard line after confirmation", out)
	}
	if *gotCityPath != deps.CityPath {
		t.Fatalf("hook city path = %q, want %q", *gotCityPath, deps.CityPath)
	}
}

func TestDoSlingBatchOmitsDashboardLineWhenUnresolved(t *testing.T) {
	stubSlingDashboardLink(t, "")

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSlingBatch(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "Dashboard:") {
		t.Fatalf("stdout = %q, want no dashboard line when resolution fails", stdout.String())
	}
}

func TestDoSlingBatchSkipsDashboardLinkOnDryRun(t *testing.T) {
	old := slingDashboardURLHook
	t.Cleanup(func() { slingDashboardURLHook = old })
	called := false
	slingDashboardURLHook = func(string, sling.SlingResult) string {
		called = true
		return "http://127.0.0.1:8372/city/test-city/runs"
	}

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	var stdout, stderr bytes.Buffer
	deps, _, _ := testDeps(cfg, sp, runner.run)
	deps.Store = seededStore("BL-42")
	opts := testOpts(a, "BL-42")
	opts.DryRun = true
	code := doSlingBatchWithJSON(opts, deps, nil, true, io.Discard, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if called {
		t.Fatal("slingDashboardURLHook called on dry-run, want skipped")
	}
	if strings.Contains(stdout.String(), "dashboard_url") {
		t.Fatalf("dry-run JSON = %q, want no dashboard_url", stdout.String())
	}
}

func TestDoSlingBatchSkipsDashboardLinkOnError(t *testing.T) {
	old := slingDashboardURLHook
	t.Cleanup(func() { slingDashboardURLHook = old })
	called := false
	slingDashboardURLHook = func(string, sling.SlingResult) string {
		called = true
		return "http://127.0.0.1:8372/city/test-city/runs"
	}

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.getErr = fmt.Errorf("bd not available")

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code == 0 {
		t.Fatalf("doSlingBatch returned 0, want failure; stdout: %s", stdout.String())
	}
	if called {
		t.Fatal("slingDashboardURLHook called on error, want skipped")
	}
	if strings.Contains(stdout.String(), "Dashboard:") {
		t.Fatalf("stdout = %q, want no dashboard line on failure", stdout.String())
	}
}

func TestDoSlingBatchJSONIncludesDashboardURL(t *testing.T) {
	link := "http://127.0.0.1:8372/city/test-city/runs/gcg-run-1"
	stubSlingDashboardLink(t, link)

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	var jsonStdout, stderr bytes.Buffer
	deps, _, _ := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSlingBatchWithJSON(opts, deps, nil, true, io.Discard, &jsonStdout, &stderr)

	if code != 0 {
		t.Fatalf("doSlingBatchWithJSON returned %d, want 0; stderr: %s", code, stderr.String())
	}
	var payload struct {
		DashboardURL string `json:"dashboard_url"`
	}
	if err := json.Unmarshal(jsonStdout.Bytes(), &payload); err != nil {
		t.Fatalf("parsing JSON output: %v\n%s", err, jsonStdout.String())
	}
	if payload.DashboardURL != link {
		t.Fatalf("dashboard_url = %q, want %q", payload.DashboardURL, link)
	}
	validateJSONAgainstResultSchema(t, []string{"sling"}, jsonStdout.Bytes())
}

func TestDoSlingBatchJSONOmitsDashboardURLWhenUnresolved(t *testing.T) {
	stubSlingDashboardLink(t, "")

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	var jsonStdout, stderr bytes.Buffer
	deps, _, _ := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSlingBatchWithJSON(opts, deps, nil, true, io.Discard, &jsonStdout, &stderr)

	if code != 0 {
		t.Fatalf("doSlingBatchWithJSON returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(jsonStdout.String(), "dashboard_url") {
		t.Fatalf("JSON output = %q, want dashboard_url omitted", jsonStdout.String())
	}
	validateJSONAgainstResultSchema(t, []string{"sling"}, jsonStdout.Bytes())
}
