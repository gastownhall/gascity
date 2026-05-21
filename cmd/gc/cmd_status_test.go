package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

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
