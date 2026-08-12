package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/molecule"
)

// newSlingDashboardTestServer is newSlingTestServer plus a dashboard base
// injected on the per-city Server, mirroring what SupervisorMux.
// WithDashboardBase does on the production path. base == "" leaves the
// provider nil (the standalone-controller shape: no dashboard mounted).
func newSlingDashboardTestServer(t *testing.T, base string) (http.Handler, *fakeMutatorState) {
	t.Helper()
	state := newFakeMutatorState(t)
	state.cfg.Rigs[0].Prefix = "gc" // match MemStore's auto-generated prefix
	srv := New(state)
	srv.SlingRunnerFunc = func(_ string, _ string, _ map[string]string) (string, error) {
		return "", nil // no-op runner
	}
	if base != "" {
		srv.dashboardBase = func() string { return base }
	}
	return newTestCityHandlerWith(t, state, srv), state
}

func TestSlingDashboardURLResolver(t *testing.T) {
	tests := []struct {
		name       string
		base       string // "" means nil provider
		cityName   string
		workflowID string
		want       string
	}{
		{
			name:     "nil provider omits link",
			base:     "",
			cityName: "test-city",
			want:     "",
		},
		{
			name:       "workflow id links to run detail",
			base:       "http://127.0.0.1:8372",
			cityName:   "test-city",
			workflowID: "gcg-run-1",
			want:       "http://127.0.0.1:8372/city/test-city/runs/gcg-run-1",
		},
		{
			name:     "no workflow id links to runs list",
			base:     "http://127.0.0.1:8372",
			cityName: "test-city",
			want:     "http://127.0.0.1:8372/city/test-city/runs",
		},
		{
			name:       "trailing slash on base is trimmed",
			base:       "http://127.0.0.1:8372/",
			cityName:   "test-city",
			workflowID: "gcg-run-1",
			want:       "http://127.0.0.1:8372/city/test-city/runs/gcg-run-1",
		},
		{
			name:       "city name outside BFF grammar omits link",
			base:       "http://127.0.0.1:8372",
			cityName:   "bright.lights",
			workflowID: "gcg-run-1",
			want:       "",
		},
		{
			name:       "workflow id is path escaped",
			base:       "http://127.0.0.1:8372",
			cityName:   "test-city",
			workflowID: "gcg/run 1",
			want:       "http://127.0.0.1:8372/city/test-city/runs/gcg%2Frun%201",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(newFakeMutatorState(t))
			if tt.base != "" {
				srv.dashboardBase = func() string { return tt.base }
			}
			if got := srv.slingDashboardURL(tt.cityName, tt.workflowID); got != tt.want {
				t.Fatalf("slingDashboardURL(%q, %q) = %q, want %q", tt.cityName, tt.workflowID, got, tt.want)
			}
		})
	}
}

func TestSlingDashboardURLResolverEmptyBase(t *testing.T) {
	srv := New(newFakeMutatorState(t))
	srv.dashboardBase = func() string { return "" }
	if got := srv.slingDashboardURL("test-city", "gcg-run-1"); got != "" {
		t.Fatalf("slingDashboardURL with empty base = %q, want empty", got)
	}
}

func TestWithDashboardBasePropagatesToCityServers(t *testing.T) {
	state := newFakeMutatorState(t)
	sm := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", time.Now())
	sm.WithDashboardBase(func() string { return "http://127.0.0.1:8372" })

	srv := sm.getCityServer(state.CityName(), state)
	if srv.dashboardBase == nil {
		t.Fatal("dashboardBase = nil, want provider propagated from WithDashboardBase")
	}
	if got := srv.dashboardBase(); got != "http://127.0.0.1:8372" {
		t.Fatalf("dashboardBase() = %q, want http://127.0.0.1:8372", got)
	}
}

func TestCityServersDefaultToNoDashboardBase(t *testing.T) {
	state := newFakeMutatorState(t)
	sm := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", time.Now())

	srv := sm.getCityServer(state.CityName(), state)
	if srv.dashboardBase != nil {
		t.Fatal("dashboardBase != nil, want unset on a mux without WithDashboardBase")
	}
}

func TestWithDashboardBaseNilIsNoOp(t *testing.T) {
	state := newFakeMutatorState(t)
	sm := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", time.Now())
	sm.WithDashboardBase(nil)

	srv := sm.getCityServer(state.CityName(), state)
	if srv.dashboardBase != nil {
		t.Fatal("dashboardBase != nil, want nil provider ignored")
	}
}

func TestSlingResponseDashboardURLRunsListForDirectRoute(t *testing.T) {
	h, state := newSlingDashboardTestServer(t, "http://127.0.0.1:8372/")
	store := state.stores["myrig"]
	b, err := store.Create(beads.Bead{Title: "test task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","bead":"` + b.ID + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status       string `json:"status"`
		DashboardURL string `json:"dashboard_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "slung" {
		t.Fatalf("status = %q, want slung", resp.Status)
	}
	if want := "http://127.0.0.1:8372/city/test-city/runs"; resp.DashboardURL != want {
		t.Fatalf("dashboard_url = %q, want %q (runs list for a non-workflow sling)", resp.DashboardURL, want)
	}
}

func TestSlingResponseDashboardURLRunDetailForGraphLaunch(t *testing.T) {
	// Same compile-time flag choreography as
	// TestSlingGraphV2RejectsLegacySourceWorkflowConflict: flip the shared
	// FormulaV2 + graph-apply flags only after New() has run so
	// syncFeatureFlags cannot stomp them back.
	setFormulaV2 := formulatest.LockV2ForTest(t)
	prevGraphApply := molecule.IsGraphApplyEnabled()
	t.Cleanup(func() {
		molecule.SetGraphApplyEnabled(prevGraphApply)
	})

	h, state := newSlingDashboardTestServer(t, "http://127.0.0.1:8372")
	setFormulaV2(true)
	molecule.SetGraphApplyEnabled(true)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	state.cfg.Agents = append(state.cfg.Agents,
		config.Agent{Name: config.ControlDispatcherAgentName, MaxActiveSessions: intPtr(1)},
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "myrig", MaxActiveSessions: intPtr(1)},
	)
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.stores["myrig"]
	source, err := store.Create(beads.Bead{ID: "BL-42", Title: "test task", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","formula":"graph-work","attached_bead_id":"` + source.ID + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		WorkflowID   string `json:"workflow_id"`
		DashboardURL string `json:"dashboard_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WorkflowID == "" {
		t.Fatal("workflow_id empty, want graph.v2 launch to mint a run root")
	}
	if want := "http://127.0.0.1:8372/city/test-city/runs/" + resp.WorkflowID; resp.DashboardURL != want {
		t.Fatalf("dashboard_url = %q, want %q (run detail for a workflow launch)", resp.DashboardURL, want)
	}
}

func TestSlingResponseOmitsDashboardURLWhenUnmounted(t *testing.T) {
	h, state := newSlingDashboardTestServer(t, "")
	store := state.stores["myrig"]
	b, err := store.Create(beads.Bead{Title: "test task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","bead":"` + b.ID + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "dashboard_url") {
		t.Fatalf("body = %s, want dashboard_url omitted when no dashboard is mounted", rec.Body.String())
	}
}

func TestSlingResponseOmitsDashboardURLForUnservableCityName(t *testing.T) {
	// The supervisor registry grammar accepts names (e.g. with dots) that
	// the dashboard BFF grammar rejects; such cities are
	// dashboard-unreachable so the link must be omitted, not minted dead.
	state := newFakeMutatorState(t)
	state.cityName = "bright.lights"
	state.cfg.Rigs[0].Prefix = "gc"
	srv := New(state)
	srv.SlingRunnerFunc = func(_ string, _ string, _ map[string]string) (string, error) {
		return "", nil
	}
	srv.dashboardBase = func() string { return "http://127.0.0.1:8372" }
	h := newTestCityHandlerWith(t, state, srv)

	store := state.stores["myrig"]
	b, err := store.Create(beads.Bead{Title: "test task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","bead":"` + b.ID + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "dashboard_url") {
		t.Fatalf("body = %s, want dashboard_url omitted for a BFF-unservable city name", rec.Body.String())
	}
}

func TestSlingFailureResponseHasNoDashboardURL(t *testing.T) {
	h, state := newSlingDashboardTestServer(t, "http://127.0.0.1:8372")

	body := `{"target":"myrig/worker","bead":"gc-does-not-exist"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "dashboard_url") {
		t.Fatalf("body = %s, want no dashboard_url on a failed sling", rec.Body.String())
	}
}
