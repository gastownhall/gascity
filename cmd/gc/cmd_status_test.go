package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// ---------------------------------------------------------------------------
// doRigStatus tests
// ---------------------------------------------------------------------------

func runDoRigStatus(
	sp runtime.Provider,
	dops drainOps,
	rig config.Rig,
	agents []config.Agent,
	cityPath string,
	stdout, stderr io.Writer,
) int {
	var store beads.Store
	if cityPath != "" {
		if opened, err := openCityStoreAt(cityPath); err == nil {
			store = opened
		}
	}
	statusSnapshot := loadStatusSessionSnapshot(store, stderr)
	return doRigStatusWithStoreAndSnapshot(sp, dops, rig, agents, cityPath, "city", "", nil, store, statusSnapshot, false, stdout, stderr)
}

func runDoRigStatusJSON(
	sp runtime.Provider,
	dops drainOps,
	rig config.Rig,
	agents []config.Agent,
	stdout, stderr io.Writer,
) int {
	return doRigStatusWithStoreAndSnapshot(sp, dops, rig, agents, "", "city", "", nil, nil, newSessionBeadSnapshot(nil), true, stdout, stderr)
}

func TestDoRigStatus(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "frontend--polecat", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	// worker is NOT running.

	dops := newFakeDrainOps()
	rig := config.Rig{Name: "frontend", Path: "/home/user/projects/frontend"}
	agents := []config.Agent{
		{Name: "polecat", Dir: "frontend", MaxActiveSessions: intPtr(1)},
		{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(1)},
	}

	var stdout, stderr bytes.Buffer
	code := runDoRigStatus(sp, dops, rig, agents, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// Rig header.
	if !strings.Contains(out, "frontend:") {
		t.Errorf("stdout missing 'frontend:', got:\n%s", out)
	}
	if !strings.Contains(out, "Path:       /home/user/projects/frontend") {
		t.Errorf("stdout missing path, got:\n%s", out)
	}
	if !strings.Contains(out, "Suspended:  no") {
		t.Errorf("stdout missing 'Suspended:  no', got:\n%s", out)
	}

	// Agent status lines.
	if !strings.Contains(out, "polecat") && !strings.Contains(out, "running") {
		t.Errorf("stdout missing polecat running status, got:\n%s", out)
	}
	if !strings.Contains(out, "worker") && !strings.Contains(out, "stopped") {
		t.Errorf("stdout missing worker stopped status, got:\n%s", out)
	}
}

func TestDoRigStatusJSON(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "frontend--worker-1", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	dops := newFakeDrainOps()
	dops.draining["frontend--worker-1"] = true
	rig := config.Rig{Name: "frontend", Path: "/tmp/frontend", Prefix: "fe", DefaultBranch: "main"}
	agents := []config.Agent{
		{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1), ScaleCheck: "echo 1"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoRigStatusJSON(sp, dops, rig, agents, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigStatusWithStoreAndSnapshot --json = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1; stdout=%q", len(lines), stdout.String())
	}
	var result RigStatusJSON
	if err := json.Unmarshal([]byte(lines[0]), &result); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.SchemaVersion != "1" || result.Rig.Name != "frontend" || result.Rig.Prefix != "fe" {
		t.Fatalf("unexpected rig status result: %+v", result)
	}
	if len(result.Agents) != 2 {
		t.Fatalf("agents = %+v, want canonical worker plus stale numbered worker instance", result.Agents)
	}
	byName := map[string]RigStatusAgent{}
	for _, agent := range result.Agents {
		byName[agent.QualifiedName] = agent
	}
	if agent := byName["frontend/worker"]; agent.QualifiedName != "frontend/worker" || agent.Running || agent.Status != "stopped" {
		t.Fatalf("canonical agent = %+v, want stopped frontend/worker", agent)
	}
	if agent := byName["frontend/worker-1"]; agent.QualifiedName != "frontend/worker-1" || !agent.Running || !agent.Draining || agent.Status != "draining" {
		t.Fatalf("numbered agent = %+v, want running draining frontend/worker-1", agent)
	}
}

func TestDoRigStatusSuspendedRig(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	rig := config.Rig{Name: "frontend", Path: "/tmp/frontend", Suspended: true}
	agents := []config.Agent{
		{Name: "polecat", Dir: "frontend", Suspended: true},
	}

	var stdout, stderr bytes.Buffer
	code := runDoRigStatus(sp, dops, rig, agents, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Suspended:  yes") {
		t.Errorf("stdout missing 'Suspended:  yes', got:\n%s", out)
	}
}

func TestDoRigStatusWithDraining(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "frontend--worker-1", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	dops := newFakeDrainOps()
	dops.draining["frontend--worker-1"] = true

	rig := config.Rig{Name: "frontend", Path: "/tmp/frontend"}
	agents := []config.Agent{
		{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2), ScaleCheck: "echo 1"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoRigStatus(sp, dops, rig, agents, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "running  (draining)") {
		t.Errorf("stdout missing 'running  (draining)', got:\n%s", out)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("stdout missing 'stopped' for worker-2, got:\n%s", out)
	}
}

func TestDoRigStatusCanonicalSingletonPoolUsesCanonicalName(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "frontend--refinery", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	dops := newFakeDrainOps()
	rig := config.Rig{Name: "frontend", Path: "/tmp/frontend"}
	agents := []config.Agent{
		{Name: "refinery", Dir: "frontend", MaxActiveSessions: intPtr(1), ScaleCheck: "echo 1"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoRigStatus(sp, dops, rig, agents, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "frontend/refinery") || !strings.Contains(out, "running") {
		t.Fatalf("stdout missing canonical running singleton status, got:\n%s", out)
	}
	if strings.Contains(out, "frontend/refinery-1") {
		t.Fatalf("stdout contains phantom singleton instance, got:\n%s", out)
	}
}

func TestDoRigStatusSuspendedAgent(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	rig := config.Rig{Name: "frontend", Path: "/tmp/frontend"}
	agents := []config.Agent{
		{Name: "worker", Dir: "frontend", Suspended: true, MaxActiveSessions: intPtr(1)},
	}

	var stdout, stderr bytes.Buffer
	code := runDoRigStatus(sp, dops, rig, agents, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "stopped  (suspended)") {
		t.Errorf("stdout missing 'stopped  (suspended)', got:\n%s", out)
	}
}

func TestDoRigStatusReportsObservationErrors(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "frontend--worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	dops := newFakeDrainOps()
	oldObserve := observeSessionTargetForStatus
	observeSessionTargetForStatus = func(string, beads.Store, runtime.Provider, *config.City, string) (worker.LiveObservation, error) {
		return worker.LiveObservation{}, errors.New("status observation unavailable")
	}
	t.Cleanup(func() { observeSessionTargetForStatus = oldObserve })
	rig := config.Rig{Name: "frontend", Path: "/tmp/frontend"}
	agents := []config.Agent{
		{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(1)},
	}

	var stdout, stderr bytes.Buffer
	code := runDoRigStatus(sp, dops, rig, agents, "/tmp/city", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc rig status: observing") {
		t.Fatalf("stderr = %q, want observation warning", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Six-row read-path routing matrix for `gc rig status` (ADR 0001, ga-h6w).
// ---------------------------------------------------------------------------

type rigStatusMatrixHandler func(t *testing.T) http.Handler

func okRigStatusHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "5")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":        "test-city",
			"path":        "/tmp/test-city",
			"uptime_sec":  1,
			"suspended":   false,
			"agent_count": 1,
			"rig_count":   1,
			"running":     1,
			"agents":      map[string]any{"total": 1, "running": 1},
			"rigs":        map[string]any{"total": 1},
			"work":        map[string]any{},
			"mail":        map[string]any{},
			"agent_details": []map[string]any{
				{
					"name":           "worker",
					"qualified_name": "frontend/worker",
					"scope":          "rig",
					"running":        true,
					"suspended":      false,
					"session_name":   "test-city--frontend--worker",
					"group_name":     "frontend/worker",
				},
			},
			"rig_details": []map[string]any{
				{"name": "frontend", "path": "/tmp/frontend", "suspended": false},
			},
		})
	})
}

func rigStatusProblemHandler(status int, detail string) rigStatusMatrixHandler {
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

func writeRigStatusTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[rig]]
name = "frontend"
path = "/tmp/frontend"

[[agent]]
name = "worker"
dir = "frontend"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	return cityPath
}

func TestRouteRigStatus_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      rigStatusMatrixHandler
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
			handler:    okRigStatusHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "frontend/worker",
		},
		{
			name:       "api-cache-not-live",
			handler:    rigStatusProblemHandler(http.StatusServiceUnavailable, "cache_not_live: priming"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    rigStatusProblemHandler(http.StatusInternalServerError, "explode"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
		},
		{
			name:       "api-404-error",
			handler:    rigStatusProblemHandler(http.StatusNotFound, "not_found: city missing"),
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
			cityPath := writeRigStatusTestCity(t)

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			sp := runtime.NewFake()
			dops := newFakeDrainOps()
			rig := config.Rig{Name: "frontend", Path: "/tmp/frontend"}
			rigAgents := []config.Agent{{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(1)}}
			var stdout, stderr bytes.Buffer
			code := routeRigStatus(cityPath, "test-city", rig, rigAgents, "", nil, nil, nil, sp, dops, c, tc.nilReason, false, &stdout, &stderr)

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

// TestRouteRigStatus_APIStaleBanner verifies the human-output staleness
// banner appears when the supervisor reports a cache age > 30 s.
func TestRouteRigStatus_APIStaleBanner(t *testing.T) {
	cityPath := writeRigStatusTestCity(t)
	staleHandler := func(_ *testing.T) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Cache-Age-S", "99")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "test-city", "path": "/tmp",
				"uptime_sec": 1, "suspended": false,
				"agent_count": 0, "rig_count": 0, "running": 0,
				"agents": map[string]any{}, "rigs": map[string]any{},
				"work": map[string]any{}, "mail": map[string]any{},
				"rig_details": []map[string]any{
					{"name": "frontend", "path": "/tmp/frontend", "suspended": false},
				},
			})
		})
	}
	srv := httptest.NewServer(staleHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	rig := config.Rig{Name: "frontend", Path: "/tmp/frontend"}
	var stdout, stderr bytes.Buffer
	if code := routeRigStatus(cityPath, "test-city", rig, nil, "", nil, nil, nil, sp, dops, c, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cache age:") {
		t.Errorf("human output should include stale banner, got:\n%s", stdout.String())
	}
}
