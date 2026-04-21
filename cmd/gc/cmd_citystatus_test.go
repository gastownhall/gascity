package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestCityStatusSuspended(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city", Suspended: true, MaxActiveSessions: intPtr(1)},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, "/tmp/city", &stdout, &stderr)
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
	code := doCityStatus(sp, dops, cfg, "/tmp/city", &stdout, &stderr)
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
	code := doCityStatus(sp, dops, cfg, "/tmp/city", &stdout, &stderr)
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
	openCityStoreAtForStatus = func(string) (beads.Store, error) {
		return nil, errors.New("bead store unavailable")
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatusJSON(sp, cfg, t.TempDir(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gc status: opening bead store") {
		t.Fatalf("stderr = %q, want bead store open error", stderr.String())
	}
}

func TestCityStatusJSONReportsCatalogListError(t *testing.T) {
	sp := runtime.NewFake()
	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.Store, error) {
		return &listErrorStore{Store: beads.NewMemStore()}, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
	}

	var stdout, stderr bytes.Buffer
	code := doCityStatusJSON(sp, cfg, t.TempDir(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "gc status: building session catalog") || !strings.Contains(stderr.String(), "catalog unavailable") {
		t.Fatalf("stderr = %q, want catalog list error", stderr.String())
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
	code := doCityStatus(sp, dops, cfg, "/tmp/city", &stdout, &stderr)
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
