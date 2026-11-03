package main

import (
	"errors"
	"os"
	"testing"
	"time"
)

// TestStartManagedDoltProcessRefusedWithoutNativeIdentity pins that the three
// managed-Dolt lifecycle verbs agree about whether a platform can run one.
//
// Stop and inspect have always been fenced by the native-identity gate. Start
// was not, so where the gate fails a start could still succeed and leave a
// server holding the data directory's exclusive lock and a runtime-state record
// claiming it is running — while the only two operations that could stop it or
// even report on it refused to touch it.
//
// The gate must also be checked before anything is created. A refusal that
// happens after the config file, PID file or state file have been written
// leaves the same debris the refusal exists to avoid.
func TestStartManagedDoltProcessRefusedWithoutNativeIdentity(t *testing.T) {
	sentinel := errors.New("no native identity on this platform")

	origIdentity := managedDoltNativeIdentitySupportedFn
	origStart := managedDoltStartSQLServerFn
	origPreflight := managedDoltPreflightCleanupFn
	t.Cleanup(func() {
		managedDoltNativeIdentitySupportedFn = origIdentity
		managedDoltStartSQLServerFn = origStart
		managedDoltPreflightCleanupFn = origPreflight
	})

	managedDoltNativeIdentitySupportedFn = func() error { return sentinel }
	spawned := false
	managedDoltStartSQLServerFn = func(_, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
		spawned = true
		return managedDoltStartedProcess{}, nil
	}
	preflighted := false
	managedDoltPreflightCleanupFn = func(string) error {
		preflighted = true
		return nil
	}

	cityPath := t.TempDir()
	t.Setenv("GC_PACK_STATE_DIR", cityPath)

	report, err := startManagedDoltProcessWithOptions(cityPath, "127.0.0.1", "17795", "root", "warning", -1, time.Second, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("start with an unsupported platform gate = %v, want the gate's own error", err)
	}
	if spawned {
		t.Error("start spawned a dolt server the platform cannot stop")
	}
	if preflighted {
		t.Error("start ran preflight cleanup before consulting the platform gate; the refusal must come first")
	}
	if report.PID != 0 || report.Port != 0 {
		t.Errorf("refused start still reported a running server: %+v", report)
	}
}
