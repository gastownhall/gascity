package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestCityStatusEmptyCity(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, "/home/user/bright-lights", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "bright-lights") {
		t.Errorf("stdout missing city name, got:\n%s", out)
	}
	if !strings.Contains(out, "/home/user/bright-lights") {
		t.Errorf("stdout missing city path, got:\n%s", out)
	}
	if !strings.Contains(out, "Controller: stopped") {
		t.Errorf("stdout missing controller status, got:\n%s", out)
	}
	if !strings.Contains(out, "Suspended:  no") {
		t.Errorf("stdout missing 'Suspended:  no', got:\n%s", out)
	}
	// No agents section when there are no agents.
	if strings.Contains(out, "Agents:") {
		t.Errorf("stdout should not have Agents section for empty city, got:\n%s", out)
	}
}

func TestCityStatusWithAgents(t *testing.T) {
	sp := runtime.NewFake()
	// Start one agent session.
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	dops := newFakeDrainOps()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker", MaxActiveSessions: intPtr(1)},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, "/home/user/city", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	if !strings.Contains(out, "/home/user/city") {
		t.Errorf("stdout missing city path, got:\n%s", out)
	}
	if !strings.Contains(out, "Agents:") {
		t.Errorf("stdout missing 'Agents:', got:\n%s", out)
	}
	if !strings.Contains(out, "mayor") {
		t.Errorf("stdout missing 'mayor', got:\n%s", out)
	}
	if !strings.Contains(out, "worker") {
		t.Errorf("stdout missing 'worker', got:\n%s", out)
	}
	if !strings.Contains(out, "1/2 agents running") {
		t.Errorf("stdout missing '1/2 agents running', got:\n%s", out)
	}
}

func TestCityStatusReportsObservationErrors(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	dops := newFakeDrainOps()
	oldObserve := observeSessionTargetForStatus
	observeSessionTargetForStatus = func(string, beads.Store, runtime.Provider, *config.City, string) (worker.LiveObservation, error) {
		return worker.LiveObservation{}, errors.New("status observation unavailable")
	}
	t.Cleanup(func() { observeSessionTargetForStatus = oldObserve })
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, "/home/user/city", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc status: observing") {
		t.Fatalf("stderr = %q, want observation warning", stderr.String())
	}
}

func TestCityStatusObservationTimesOut(t *testing.T) {
	oldTimeout := statusObservationTimeout
	statusObservationTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		statusObservationTimeout = oldTimeout
	})

	release := make(chan struct{})
	defer close(release)
	oldObserve := observeSessionTargetForStatus
	observeSessionTargetForStatus = func(string, beads.Store, runtime.Provider, *config.City, string) (worker.LiveObservation, error) {
		<-release
		return worker.LiveObservation{Running: true}, nil
	}
	t.Cleanup(func() { observeSessionTargetForStatus = oldObserve })

	var stderr bytes.Buffer
	start := time.Now()
	obs := observeSessionTargetWithWarning(
		"gc status",
		"/city",
		nil,
		runtime.NewFake(),
		&config.City{},
		statusObservationTarget{runtimeSessionName: "slow-session"},
		&stderr,
	)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("observeSessionTargetWithWarning elapsed %s, want bounded timeout", elapsed)
	}
	if obs.Running {
		t.Fatal("observation should not report running after timeout")
	}
	if !strings.Contains(stderr.String(), "observing \"slow-session\" timed out") {
		t.Fatalf("stderr = %q, want timeout warning", stderr.String())
	}
}

func TestCityStatusSuspended(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city", Suspended: true, MaxActiveSessions: intPtr(1)},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Suspended:  yes") {
		t.Errorf("stdout missing 'Suspended:  yes', got:\n%s", out)
	}
}

func TestCityStatusPoolExpansion(t *testing.T) {
	sp := runtime.NewFake()
	// Start 2 of 3 pool instances.
	if err := sp.Start(context.Background(), "hw--polecat-1", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "hw--polecat-2", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	dops := newFakeDrainOps()
	dops.draining["hw--polecat-2"] = true

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "polecat", Dir: "hw", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 1"},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// Pool header line.
	if !strings.Contains(out, "scaled (min=1, max=3)") {
		t.Errorf("stdout missing scaled header, got:\n%s", out)
	}
	// Instance lines.
	if !strings.Contains(out, "polecat-1") {
		t.Errorf("stdout missing polecat-1, got:\n%s", out)
	}
	if !strings.Contains(out, "polecat-2") {
		t.Errorf("stdout missing polecat-2, got:\n%s", out)
	}
	if !strings.Contains(out, "polecat-3") {
		t.Errorf("stdout missing polecat-3, got:\n%s", out)
	}
	// polecat-2 draining.
	if !strings.Contains(out, "running  (draining)") {
		t.Errorf("stdout missing 'running  (draining)', got:\n%s", out)
	}
	// Summary: 2/3 running.
	if !strings.Contains(out, "2/3 agents running") {
		t.Errorf("stdout missing '2/3 agents running', got:\n%s", out)
	}
}

func TestCityStatusCanonicalSingletonPoolUsesCanonicalName(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "hw--refinery", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	dops := newFakeDrainOps()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "hw", MaxActiveSessions: intPtr(1), ScaleCheck: "echo 1"},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "hw/refinery") || !strings.Contains(out, "running") {
		t.Fatalf("stdout missing canonical running singleton status, got:\n%s", out)
	}
	if strings.Contains(out, "hw/refinery-1") {
		t.Fatalf("stdout contains phantom singleton instance, got:\n%s", out)
	}
	if !strings.Contains(out, "1/1 agents running") {
		t.Fatalf("stdout missing canonical singleton running summary, got:\n%s", out)
	}
}

func TestCityStatusRigs(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
		Rigs: []config.Rig{
			{Name: "hello-world", Path: "/home/user/hello-world"},
			{Name: "frontend", Path: "/home/user/frontend", Suspended: true},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Rigs:") {
		t.Errorf("stdout missing 'Rigs:', got:\n%s", out)
	}
	if !strings.Contains(out, "hello-world") {
		t.Errorf("stdout missing 'hello-world', got:\n%s", out)
	}
	if !strings.Contains(out, "/home/user/hello-world") {
		t.Errorf("stdout missing hello-world path, got:\n%s", out)
	}
	if !strings.Contains(out, "frontend") {
		t.Errorf("stdout missing 'frontend', got:\n%s", out)
	}
	if !strings.Contains(out, "(suspended)") {
		t.Errorf("stdout missing '(suspended)' for frontend, got:\n%s", out)
	}
}

func TestCityStatusJSONEmpty(t *testing.T) {
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatusJSON(sp, cfg, "/home/user/bright-lights", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	var status StatusJSON
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}
	if status.CityName != "bright-lights" {
		t.Errorf("city_name = %q, want %q", status.CityName, "bright-lights")
	}
	if status.CityPath != "/home/user/bright-lights" {
		t.Errorf("city_path = %q, want %q", status.CityPath, "/home/user/bright-lights")
	}
	if status.Controller.Running {
		t.Error("controller should not be running")
	}
	if status.Suspended {
		t.Error("suspended should be false")
	}
	if status.Summary.TotalAgents != 0 {
		t.Errorf("total_agents = %d, want 0", status.Summary.TotalAgents)
	}
}

func TestCityStatusFormatJSONIncludesBeadsDiagnostic(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "bright-lights"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{
			Store: beads.NewMemStore(),
			Diagnostic: beads.BeadsDiagnostic{
				Store:               "BdStore",
				NativeStoreEligible: false,
				PreflightGate:       "metadata_backend",
				PreflightReason:     "metadata backend=file; native store requires dolt",
			},
		}, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })

	var stdout, stderr bytes.Buffer
	cmd := newStatusCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--format", "json", cityPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v; stderr: %s", err, stderr.String())
	}

	var status StatusJSON
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}
	if status.Beads == nil {
		t.Fatalf("status.Beads = nil, want diagnostic")
	}
	if status.Beads.Store != "BdStore" {
		t.Fatalf("beads_store = %q, want BdStore", status.Beads.Store)
	}
	if status.Beads.NativeStoreEligible {
		t.Fatalf("native_store_eligible = true, want false")
	}
	if status.Beads.PreflightGate != "metadata_backend" {
		t.Fatalf("preflight_gate = %q, want metadata_backend", status.Beads.PreflightGate)
	}
	if status.Beads.PreflightReason == "" {
		t.Fatalf("preflight_reason = empty, want fallback reason")
	}
}

func TestSnapshotFromStatusViewIncludesBeadsDiagnostic(t *testing.T) {
	view := api.StatusView{
		CityName: "bright-lights",
		CityPath: "/home/user/bright-lights",
		Beads: &beads.BeadsDiagnostic{
			Store:               "BdStore",
			NativeStoreEligible: false,
			PreflightGate:       "metadata_backend",
			PreflightReason:     "metadata backend=file; native store requires dolt",
		},
	}

	snapshot := snapshotFromStatusView(view.CityPath, view)
	if snapshot.Beads == nil {
		t.Fatal("snapshot.Beads = nil, want diagnostic from API status view")
	}
	if snapshot.Beads.Store != "BdStore" {
		t.Fatalf("beads_store = %q, want BdStore", snapshot.Beads.Store)
	}
	if snapshot.Beads.PreflightReason == "" {
		t.Fatal("preflight_reason = empty, want API fallback reason")
	}
}

func TestCityStatusJSONWithAgents(t *testing.T) {
	sp := runtime.NewFake()
	// Start one agent session (default session name = agent name, no city prefix).
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
		},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/home/user/myrig"},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatusJSON(sp, cfg, "/home/user/city", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	var status StatusJSON
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}

	// Mayor singleton + 3 pool instances = 4 agents.
	if status.Summary.TotalAgents != 4 {
		t.Errorf("total_agents = %d, want 4", status.Summary.TotalAgents)
	}
	if status.Summary.RunningAgents != 1 {
		t.Errorf("running_agents = %d, want 1", status.Summary.RunningAgents)
	}
	if len(status.Agents) != 4 {
		t.Fatalf("got %d agents, want 4", len(status.Agents))
	}

	// First agent: mayor (singleton, running).
	if status.Agents[0].Name != "mayor" {
		t.Errorf("agents[0].name = %q, want %q", status.Agents[0].Name, "mayor")
	}
	if status.Agents[0].Scope != "city" {
		t.Errorf("agents[0].scope = %q, want %q", status.Agents[0].Scope, "city")
	}
	if !status.Agents[0].Running {
		t.Error("agents[0] should be running")
	}
	if status.Agents[0].Pool != nil {
		t.Error("agents[0].pool should be nil for singleton")
	}

	// Second agent: polecat-1 (pool, not running).
	if status.Agents[1].QualifiedName != "myrig/polecat-1" {
		t.Errorf("agents[1].qualified_name = %q, want %q", status.Agents[1].QualifiedName, "myrig/polecat-1")
	}
	if status.Agents[1].Scope != "rig" {
		t.Errorf("agents[1].scope = %q, want %q", status.Agents[1].Scope, "rig")
	}
	// Rigs.
	if len(status.Rigs) != 1 {
		t.Fatalf("got %d rigs, want 1", len(status.Rigs))
	}
	if status.Rigs[0].Name != "myrig" {
		t.Errorf("rigs[0].name = %q, want %q", status.Rigs[0].Name, "myrig")
	}
}

func TestCityStatusJSONReportsObservationErrors(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	oldObserve := observeSessionTargetForStatus
	observeSessionTargetForStatus = func(string, beads.Store, runtime.Provider, *config.City, string) (worker.LiveObservation, error) {
		return worker.LiveObservation{}, errors.New("status observation unavailable")
	}
	t.Cleanup(func() { observeSessionTargetForStatus = oldObserve })
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatusJSON(sp, cfg, t.TempDir(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc status: observing") {
		t.Fatalf("stderr = %q, want observation warning", stderr.String())
	}

	var status StatusJSON
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}
	if len(status.Agents) != 1 {
		t.Fatalf("agents len = %d, want 1", len(status.Agents))
	}
	if status.Agents[0].Running {
		t.Fatal("agent should not report running when observation fails")
	}
}

func TestCityStatusJSONReportsStoreOpenError(t *testing.T) {
	sp := runtime.NewFake()
	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{}, errors.New("bead store unavailable")
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
	}
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatusJSON(sp, cfg, cityPath, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gc status: opening bead store") {
		t.Fatalf("stderr = %q, want bead store open error", stderr.String())
	}
}

// TestCityStatusJSONReportsCatalogListError asserts the pre-#2005 contract:
// when the bead store fails to list session beads, `gc status --json` still
// emits the JSON payload (so callers can parse partial status) but exits
// rc=1 so monitoring scripts using `$?` can detect the degraded state. See
// #2147 for the regression history — PR #2005 inadvertently flipped this
// from rc=1 to rc=0 along with renaming the test.
func TestCityStatusJSONReportsCatalogListError(t *testing.T) {
	sp := runtime.NewFake()
	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: &listErrorStore{Store: beads.NewMemStore()}}, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
	}
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatusJSON(sp, cfg, cityPath, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (degraded session snapshot); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc status: loading session snapshot") || !strings.Contains(stderr.String(), "catalog unavailable") {
		t.Fatalf("stderr = %q, want session snapshot warning", stderr.String())
	}
	// JSON payload must still be emitted so callers can parse the partial
	// status — only the exit code signals the degraded state.
	var status StatusJSON
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}
}

func TestCmdCityStatusJSONConfigErrorIsStructured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdCityStatus([]string{filepath.Join(t.TempDir(), "missing-city")}, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}

	var payload cliJSONErrorOutput
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON error: %v\n%s", err, stdout.String())
	}
	if payload.OK {
		t.Fatalf("ok = true, want false; payload=%+v", payload)
	}
	if payload.Error.Code != "city_resolve_failed" || payload.Error.ExitCode != code {
		t.Fatalf("error = %+v, want city_resolve_failed with exit code %d", payload.Error, code)
	}
	if strings.Contains(stderr.String(), "gc status: loading") {
		t.Fatalf("stderr contains human diagnostic: %q", stderr.String())
	}
	var diagnostic cliJSONDiagnostic
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &diagnostic); err != nil {
		t.Fatalf("stderr is not JSON diagnostic: %v\n%s", err, stderr.String())
	}
	if diagnostic.ExitCode != code {
		t.Fatalf("stderr exit_code = %d, want %d", diagnostic.ExitCode, code)
	}
}

func TestCityStatusReportsCatalogListError(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: &listErrorStore{Store: beads.NewMemStore()}}, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "status-checker", MaxActiveSessions: intPtr(1)},
		},
	}
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, cityPath, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (degraded session snapshot); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc status: loading session snapshot") || !strings.Contains(stderr.String(), "catalog unavailable") {
		t.Fatalf("stderr = %q, want session snapshot warning", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Agents:") || !strings.Contains(out, "status-checker") {
		t.Fatalf("stdout = %q, want partial text status report", out)
	}
}

func TestCityStatusSkipsStoreOpenWhenNoPersistedStoreExists(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	oldOpen := openCityStoreAtForStatus
	called := false
	openCityStoreAtForStatus = func(string) (beads.StoreOpenResult, error) {
		called = true
		return beads.StoreOpenResult{}, errors.New("unexpected store open")
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if called {
		t.Fatal("status opened bead store without any persisted store state")
	}
}

func TestCityStatusAgentSuspendedByRig(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)},
		},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/tmp/myrig", Suspended: true},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	out := stdout.String()
	// Agent in suspended rig should show "stopped  (suspended)".
	if !strings.Contains(out, "stopped  (suspended)") {
		t.Errorf("stdout missing 'stopped  (suspended)' for rig-suspended agent, got:\n%s", out)
	}
}

func TestControllerStatusLine(t *testing.T) {
	tests := []struct {
		name string
		ctrl ControllerJSON
		want string
	}{
		{
			name: "standalone running",
			ctrl: ControllerJSON{Mode: "standalone", PID: 1234, Running: true},
			want: "standalone-managed (PID 1234)",
		},
		{
			name: "supervisor not running",
			ctrl: ControllerJSON{Mode: "supervisor"},
			want: "supervisor-managed (supervisor not running)",
		},
		{
			name: "supervisor city stopped",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321},
			want: "supervisor-managed (PID 4321, city stopped)",
		},
		{
			name: "supervisor city starting bead store",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Status: "starting_bead_store"},
			want: "supervisor-managed (PID 4321, starting bead store)",
		},
		{
			name: "supervisor city init failed",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Status: "init_failed"},
			want: "supervisor-managed (PID 4321, init failed)",
		},
		{
			name: "supervisor running",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Running: true},
			want: "supervisor-managed (PID 4321)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := controllerStatusLine(tt.ctrl); got != tt.want {
				t.Fatalf("controllerStatusLine(%+v) = %q, want %q", tt.ctrl, got, tt.want)
			}
		})
	}
}

func startFakeControllerSocket(t *testing.T, cityPath, response string) <-chan struct{} {
	t.Helper()
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = lis.Close()
		_ = os.Remove(sockPath)
	})

	accepted := make(chan struct{}, 1)
	go func() {
		for {
			conn, acceptErr := lis.Accept()
			if acceptErr != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck // test cleanup
				_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				_, _ = conn.Read(make([]byte, 64))
				_ = conn.SetReadDeadline(time.Time{})
				_, _ = conn.Write([]byte(response))
			}(conn)
		}
	}()
	return accepted
}

func TestControllerStatusForCityPrefersRegisteredSupervisorState(t *testing.T) {
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))

	root := shortSocketTempDir(t, "gc-status-")
	cityPath := filepath.Join(root, "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("register city: %v", err)
	}

	accepted := startFakeControllerSocket(t, cityPath, "1234\n")

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4321 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return true, "", true }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	got := controllerStatusForCity(cityPath)
	if got.Mode != "supervisor" || !got.Running || got.PID != 4321 {
		t.Fatalf("controllerStatusForCity = %+v, want running supervisor PID 4321", got)
	}
	select {
	case <-accepted:
		t.Fatal("controllerStatusForCity consulted standalone socket for supervisor-managed city")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestControllerStatusForCityFallsBackToStandaloneWhenRegisteredSupervisorDown(t *testing.T) {
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))

	root := shortSocketTempDir(t, "gc-status-")
	cityPath := filepath.Join(root, "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("register city: %v", err)
	}

	startFakeControllerSocket(t, cityPath, "2468\n")

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 0 }
	supervisorCityRunningHook = func(string) (bool, string, bool) {
		t.Fatal("supervisorCityRunningHook should not be called when supervisor is down")
		return false, "", false
	}
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	got := controllerStatusForCity(cityPath)
	if got.Mode != "standalone" || !got.Running || got.PID != 2468 {
		t.Fatalf("controllerStatusForCity = %+v, want running standalone PID 2468", got)
	}
}

func TestControllerStatusForCityReusesSupervisorPIDWhenCityStateUnknown(t *testing.T) {
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("register city: %v", err)
	}

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	calls := 0
	supervisorAliveHook = func() int {
		calls++
		if calls <= 2 {
			return 4321
		}
		return 0
	}
	supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "", false }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	got := controllerStatusForCity(cityPath)
	if got.Mode != "supervisor" || got.Running || got.PID != 4321 || got.Status != "unknown" {
		t.Fatalf("controllerStatusForCity = %+v, want unknown supervisor PID 4321", got)
	}
	if line := controllerStatusLine(got); line != "supervisor-managed (PID 4321, unknown)" {
		t.Fatalf("controllerStatusLine(%+v) = %q, want unknown supervisor status", got, line)
	}
	if calls != 2 {
		t.Fatalf("supervisorAliveHook calls = %d, want 2", calls)
	}
}

func TestControllerStatusForCityReturnsSupervisorModeWhenProbeSucceedsAfterUnknownRetry(t *testing.T) {
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))

	root := shortSocketTempDir(t, "gc-status-")
	cityPath := filepath.Join(root, "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("register city: %v", err)
	}

	startFakeControllerSocket(t, cityPath, "2468\n")

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	calls := 0
	supervisorAliveHook = func() int {
		calls++
		if calls == 1 {
			return 4321
		}
		return 0
	}
	supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "", false }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	got := controllerStatusForCity(cityPath)
	if got.Mode != "supervisor" || !got.Running || got.PID != 2468 {
		t.Fatalf("controllerStatusForCity = %+v, want running supervisor-mode PID 2468", got)
	}
	if calls != 2 {
		t.Fatalf("supervisorAliveHook calls = %d, want 2", calls)
	}
}

type listErrorStore struct {
	beads.Store
}

func (s *listErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("catalog unavailable")
}

// ---------------------------------------------------------------------------
// Six-row read-path routing matrix for `gc status` (ADR 0001, ga-h6w).
// ---------------------------------------------------------------------------
//
// Each row exercises one branch of routeCityStatus:
//
//   api-happy-path       API returns 200 with items         route=api, exit 0
//   api-cache-not-live   API returns 503 cache_not_live     fallback, exit 0
//   api-500-fallback     API returns generic 500            fallback (conn-refused), exit 0
//   api-404-error        API returns 404                    no fallback, exit 1
//   controller-down      apiClient returns nil (no env)     fallback (controller-down), exit 0
//   escape-hatch         GC_NO_API truthy                   fallback (escape-hatch), exit 0

type cityStatusMatrixHandler func(t *testing.T) http.Handler

func okCityStatusHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":        "test-city",
			"path":        "/tmp/test-city",
			"uptime_sec":  1,
			"suspended":   false,
			"agent_count": 1,
			"rig_count":   0,
			"running":     1,
			"agents":      map[string]any{"total": 1, "running": 1},
			"rigs":        map[string]any{"total": 0},
			"work":        map[string]any{},
			"mail":        map[string]any{},
			"agent_details": []map[string]any{
				{
					"name":           "mayor",
					"qualified_name": "mayor",
					"scope":          "city",
					"running":        true,
					"suspended":      false,
					"session_name":   "test-city--mayor",
					"group_name":     "mayor",
				},
			},
		})
	})
}

func cityStatusProblemHandler(status int, detail string) cityStatusMatrixHandler {
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

func writeCityStatusTestCity(t *testing.T) string {
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
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	return cityPath
}

func TestRouteCityStatus_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      cityStatusMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
	}{
		{
			name:      "api-happy-path",
			handler:   okCityStatusHandler,
			wantExit:  0,
			wantRoute: "api",
		},
		{
			name:       "api-cache-not-live",
			handler:    cityStatusProblemHandler(http.StatusServiceUnavailable, "cache_not_live: priming"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    cityStatusProblemHandler(http.StatusInternalServerError, "explode"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
		},
		{
			name:       "api-404-error",
			handler:    cityStatusProblemHandler(http.StatusNotFound, "not_found: city missing"),
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
			cityPath := writeCityStatusTestCity(t)
			cfg, err := loadCityConfig(cityPath, new(bytes.Buffer))
			if err != nil {
				t.Fatalf("loadCityConfig: %v", err)
			}

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			sp := runtime.NewFake()
			dops := newFakeDrainOps()
			var stdout, stderr bytes.Buffer
			code := routeCityStatus(cityPath, cfg, sp, dops, c, tc.nilReason, false, &stdout, &stderr)

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
		})
	}
}

// TestRouteCityStatus_APIJSONIncludesCacheAge verifies the API-path JSON
// output carries the _cache_age_s envelope field while the fallback path
// omits it. Enforces D5 from the gc-read-path design doc.
func TestRouteCityStatus_APIJSONIncludesCacheAge(t *testing.T) {
	cityPath := writeCityStatusTestCity(t)
	cfg, err := loadCityConfig(cityPath, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	srv := httptest.NewServer(okCityStatusHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	if code := routeCityStatus(cityPath, cfg, sp, dops, c, "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}
	age, ok := envelope["_cache_age_s"].(float64)
	if !ok {
		t.Fatalf("_cache_age_s missing or wrong type; envelope=%v", envelope)
	}
	if age != 2.0 {
		t.Errorf("_cache_age_s = %v, want 2.0", age)
	}
}

func TestRouteCityStatus_FallbackJSONOmitsCacheAge(t *testing.T) {
	cityPath := writeCityStatusTestCity(t)
	cfg, err := loadCityConfig(cityPath, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	if code := routeCityStatus(cityPath, cfg, sp, dops, nil, "controller-down", true, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}
	if _, ok := envelope["_cache_age_s"]; ok {
		t.Errorf("fallback JSON should omit _cache_age_s, got: %s", stdout.String())
	}
}

// TestRouteCityStatus_APIStaleBanner verifies the human-output staleness
// banner appears when the supervisor reports a cache age > 30 s.
func TestRouteCityStatus_APIStaleBanner(t *testing.T) {
	cityPath := writeCityStatusTestCity(t)
	cfg, err := loadCityConfig(cityPath, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	staleHandler := func(_ *testing.T) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Cache-Age-S", "123")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "test-city", "path": "/tmp/test-city",
				"uptime_sec": 1, "suspended": false,
				"agent_count": 0, "rig_count": 0, "running": 0,
				"agents": map[string]any{"total": 0},
				"rigs":   map[string]any{"total": 0},
				"work":   map[string]any{}, "mail": map[string]any{},
			})
		})
	}
	srv := httptest.NewServer(staleHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	if code := routeCityStatus(cityPath, cfg, sp, dops, c, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cache age:") {
		t.Errorf("human output should include stale banner, got:\n%s", stdout.String())
	}
}

func TestControllerStatusGuidance(t *testing.T) {
	tests := []struct {
		name string
		ctrl ControllerJSON
		want []string
	}{
		{
			name: "standalone running",
			ctrl: ControllerJSON{Mode: "standalone", PID: 1234, Running: true},
			want: []string{
				"Authority: standalone controller PID 1234",
				"Next: gc stop /tmp/city && gc start /tmp/city to hand ownership to the supervisor",
			},
		},
		{
			name: "supervisor registered but down",
			ctrl: ControllerJSON{Mode: "supervisor"},
			want: []string{
				"Authority: supervisor registry; no supervisor process is running",
				"Next: gc start /tmp/city to start the supervisor and reconcile this city",
			},
		},
		{
			name: "supervisor city stopped",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321},
			want: []string{
				"Authority: supervisor process PID 4321",
				"Next: gc start /tmp/city to ask the supervisor to start this city",
			},
		},
		{
			name: "supervisor city unknown",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Status: "unknown"},
			want: []string{
				"Authority: supervisor process PID 4321",
				"Next: gc start /tmp/city to ask the supervisor to start this city",
			},
		},
		{
			name: "supervisor starting",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Status: "starting_bead_store"},
			want: []string{
				"Authority: supervisor process PID 4321",
				"Next: gc supervisor logs to inspect startup progress",
			},
		},
		{
			name: "supervisor init failed",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Status: "init_failed"},
			want: []string{
				"Authority: supervisor process PID 4321",
				"Next: gc supervisor logs to see the init failure",
			},
		},
		{
			name: "supervisor running",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Running: true},
			want: []string{
				"Authority: supervisor process PID 4321",
			},
		},
		{
			name: "unmanaged stopped",
			ctrl: ControllerJSON{},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := controllerStatusGuidance(tt.ctrl, "/tmp/city")
			if len(got) != len(tt.want) {
				t.Fatalf("controllerStatusGuidance length = %d, want %d; got %#v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("controllerStatusGuidance[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
