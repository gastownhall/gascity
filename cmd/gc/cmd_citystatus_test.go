package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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

	// Scaled header line.
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

	// Second agent: polecat-1 (scaled, not running).
	if status.Agents[1].QualifiedName != "myrig/polecat-1" {
		t.Errorf("agents[1].qualified_name = %q, want %q", status.Agents[1].QualifiedName, "myrig/polecat-1")
	}
	if status.Agents[1].Scope != "rig" {
		t.Errorf("agents[1].scope = %q, want %q", status.Agents[1].Scope, "rig")
	}
	if status.Agents[1].Pool != nil {
		t.Fatal("agents[1].pool should be nil for scaled session output")
	}

	// Rigs.
	if len(status.Rigs) != 1 {
		t.Fatalf("got %d rigs, want 1", len(status.Rigs))
	}
	if status.Rigs[0].Name != "myrig" {
		t.Errorf("rigs[0].name = %q, want %q", status.Rigs[0].Name, "myrig")
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
			name: "supervisor not running",
			ctrl: ControllerJSON{Mode: "supervisor"},
			want: "supervisor-managed (supervisor not running)",
		},
		{
			name: "supervisor city stopped",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321},
			want: "supervisor (PID 4321, city stopped)",
		},
		{
			name: "supervisor city starting bead store",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Status: "starting_bead_store"},
			want: "supervisor (PID 4321, starting bead store)",
		},
		{
			name: "supervisor city init failed",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Status: "init_failed"},
			want: "supervisor (PID 4321, init failed)",
		},
		{
			name: "supervisor running",
			ctrl: ControllerJSON{Mode: "supervisor", PID: 4321, Running: true},
			want: "supervisor (PID 4321)",
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
