package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoverManagedDoltExistingObserveTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "zero defaults to 5s", timeout: 0, want: 5 * time.Second},
		{name: "negative defaults to 5s", timeout: -1, want: 5 * time.Second},
		{name: "below 5s returns input", timeout: 2 * time.Second, want: 2 * time.Second},
		{name: "exactly 5s returns 5s", timeout: 5 * time.Second, want: 5 * time.Second},
		{name: "above 5s capped at 5s", timeout: 30 * time.Second, want: 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recoverManagedDoltExistingObserveTimeout(tt.timeout); got != tt.want {
				t.Errorf("recoverManagedDoltExistingObserveTimeout(%v) = %v, want %v", tt.timeout, got, tt.want)
			}
		})
	}
}

func TestRecoverManagedDoltShouldReuseExisting(t *testing.T) {
	tests := []struct {
		name          string
		existingPort  int
		requestedPort string
		want          bool
	}{
		{name: "zero port never reuses", existingPort: 0, requestedPort: "3306", want: false},
		{name: "negative port never reuses", existingPort: -1, requestedPort: "3306", want: false},
		{name: "empty requested always reuses", existingPort: 3306, requestedPort: "", want: true},
		{name: "whitespace requested always reuses", existingPort: 3306, requestedPort: "  ", want: true},
		{name: "different port reuses", existingPort: 3307, requestedPort: "3306", want: true},
		{name: "same port does not reuse", existingPort: 3306, requestedPort: "3306", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recoverManagedDoltShouldReuseExisting(tt.existingPort, tt.requestedPort); got != tt.want {
				t.Errorf("recoverManagedDoltShouldReuseExisting(%d, %q) = %v, want %v",
					tt.existingPort, tt.requestedPort, got, tt.want)
			}
		})
	}
}

func TestManagedDoltRecoverFields(t *testing.T) {
	report := managedDoltRecoverReport{
		DiagnosedReadOnly: true,
		HadPID:            true,
		Forced:            false,
		Ready:             true,
		PID:               9876,
		Port:              3311,
		Healthy:           true,
		Restarted:         true,
	}
	fields := managedDoltRecoverFields(report)
	want := []string{
		"diagnosed_read_only\ttrue",
		"had_pid\ttrue",
		"forced\tfalse",
		"ready\ttrue",
		"pid\t9876",
		"port\t3311",
		"healthy\ttrue",
		"restarted\ttrue",
	}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d", len(fields), len(want))
	}
	for i, w := range want {
		if fields[i] != w {
			t.Errorf("fields[%d] = %q, want %q", i, fields[i], w)
		}
	}
}

func TestCleanupFailedManagedDoltRecovery_NilCause(t *testing.T) {
	if err := cleanupFailedManagedDoltRecovery("/nonexistent", 0, 0, nil); err != nil {
		t.Errorf("cleanupFailedManagedDoltRecovery(nil cause) = %v, want nil", err)
	}
}

func TestRecoverManagedDoltObservedRebindPossible(t *testing.T) {
	t.Run("empty port always possible", func(t *testing.T) {
		if !recoverManagedDoltObservedRebindPossible(t.TempDir(), "") {
			t.Error("empty requestedPort should return true")
		}
	})

	t.Run("no state files returns false", func(t *testing.T) {
		if recoverManagedDoltObservedRebindPossible(t.TempDir(), "3306") {
			t.Error("missing state files should return false")
		}
	})

	t.Run("state with different port returns true", func(t *testing.T) {
		cityPath := t.TempDir()
		statePath := providerManagedDoltStatePath(cityPath)
		if err := writeDoltRuntimeStateFile(statePath, doltRuntimeState{
			Running: true,
			PID:     1234,
			Port:    3307,
		}); err != nil {
			t.Fatalf("writeDoltRuntimeStateFile: %v", err)
		}
		if !recoverManagedDoltObservedRebindPossible(cityPath, "3306") {
			t.Error("different port should return true")
		}
	})

	t.Run("state with same port returns false", func(t *testing.T) {
		cityPath := t.TempDir()
		statePath := providerManagedDoltStatePath(cityPath)
		if err := writeDoltRuntimeStateFile(statePath, doltRuntimeState{
			Running: true,
			PID:     1234,
			Port:    3306,
		}); err != nil {
			t.Fatalf("writeDoltRuntimeStateFile: %v", err)
		}
		if recoverManagedDoltObservedRebindPossible(cityPath, "3306") {
			t.Error("same port should return false")
		}
	})
}

func setupRecoveryTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	packStateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Setenv("GC_DOLT_PASSWORD", "test")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	return cityPath
}

func writeRecoveryRuntimeState(t *testing.T, cityPath string, pid, port int) {
	t.Helper()
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), doltRuntimeState{
		Running:   true,
		PID:       pid,
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}
}

func TestRecoverManagedDolt_SkipsRestartWhenProbeHealthy(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)
	writeRecoveryRuntimeState(t, cityPath, 4321, 3306)

	oldProbe := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) { return "false", nil }
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "5", nil }

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !report.Ready {
		t.Error("expected Ready=true when probe succeeds")
	}
	if !report.Healthy {
		t.Error("expected Healthy=true when probe succeeds")
	}
	if report.DiagnosedReadOnly {
		t.Error("expected DiagnosedReadOnly=false for healthy server")
	}
	if !report.HadPID {
		t.Error("expected HadPID=true from runtime state")
	}
	if report.PID != 4321 {
		t.Errorf("expected PID=4321 from runtime state, got %d", report.PID)
	}
	if report.Port != 3306 {
		t.Errorf("expected Port=3306 from runtime state, got %d", report.Port)
	}
	if report.Restarted {
		t.Error("expected Restarted=false when healthy server is reused")
	}
}

func TestRecoverManagedDolt_ProceedsWhenReadOnly(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)

	oldProbe := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	oldPreflight := managedDoltPreflightCleanupFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
		managedDoltPreflightCleanupFn = oldPreflight
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) { return "true", nil }
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "5", nil }
	managedDoltPreflightCleanupFn = func(_ string) error {
		return fmt.Errorf("stop: expected — no real dolt process")
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if err == nil {
		t.Fatal("expected error when read-only server recovery proceeds to stop/start")
	}
	if !report.DiagnosedReadOnly {
		t.Error("expected DiagnosedReadOnly=true for read-only server")
	}
	if report.Ready {
		t.Error("expected Ready=false when recovery proceeds past probe")
	}
}

func TestRecoverManagedDolt_ProceedsWhenProbeUnreachable(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)

	oldProbe := managedDoltQueryProbeDirectFn
	oldPreflight := managedDoltPreflightCleanupFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltPreflightCleanupFn = oldPreflight
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error {
		return fmt.Errorf("connection refused")
	}
	managedDoltPreflightCleanupFn = func(_ string) error {
		return fmt.Errorf("stop: expected — no real dolt process")
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if err == nil {
		t.Fatal("expected error when unreachable server recovery proceeds to stop/start")
	}
	if report.Ready {
		t.Error("expected Ready=false when probe fails")
	}
}

func TestRecoverManagedDolt_ProceedsWhenReadOnlyUnknown(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)

	oldProbe := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnCount := managedDoltConnectionCountDirectFn
	oldPreflight := managedDoltPreflightCleanupFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnCount
		managedDoltPreflightCleanupFn = oldPreflight
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) {
		return "unknown", errManagedDoltNoUserDatabase
	}
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "5", nil }
	managedDoltPreflightCleanupFn = func(_ string) error {
		return fmt.Errorf("stop: expected - no real dolt process")
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if err == nil {
		t.Fatal("expected error when read-only state is unknown and recovery proceeds to stop/start")
	}
	if report.DiagnosedReadOnly {
		t.Error("expected DiagnosedReadOnly=false for unknown read-only state")
	}
	if report.Ready {
		t.Error("expected Ready=false when recovery proceeds past unknown read-only health")
	}
}

func TestRecoverManagedDolt_ProceedsWhenHealthCheckErrors(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)

	oldProbe := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldPreflight := managedDoltPreflightCleanupFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldProbe
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltPreflightCleanupFn = oldPreflight
	})

	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) {
		return "", fmt.Errorf("broken pipe")
	}
	managedDoltPreflightCleanupFn = func(_ string) error {
		return fmt.Errorf("stop: expected — no real dolt process")
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", "3306", "root", "warning", 10*time.Second)
	if err == nil {
		t.Fatal("expected error when health check fails and recovery proceeds to stop/start")
	}
	if report.Ready {
		t.Error("expected Ready=false when health check errors")
	}
}
