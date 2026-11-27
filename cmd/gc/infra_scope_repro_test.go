package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScopeWatchdogFromNonSessionUserUnitGetsOwnScope pins issue #5910: a gc
// command run from a transient user unit that is not an agent session (a
// `systemd-run --user --scope` caller, a one-shot user .service) lazily starts
// the managed Dolt through its scope watchdog. The watchdog and its dolt child
// must land in a scope of their own, not in the caller's cgroup, or stopping
// that unit (KillMode=control-group) SIGTERMs the store.
func TestScopeWatchdogFromNonSessionUserUnitGetsOwnScope(t *testing.T) {
	logPath := installInfraScopeHost(t, infraScopeHost{session: false, linger: true, cgroup: userManagerCgroup})
	cityPath := t.TempDir()
	binDir, marker := writeMarkingFakeDolt(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dolt-config.yaml")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	logFilePath := filepath.Join(dir, "dolt.log")
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	parentEnv := removeEnvKey(os.Environ(), "PATH")
	parentEnv = append(parentEnv, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	started, err := startManagedDoltSQLServerWithScopeWatchdogEnv(cityPath, configPath, logFilePath, logFile, parentEnv)
	if err != nil {
		t.Fatalf("start managed dolt with scope watchdog: %v", err)
	}
	killInfraScopeTestPID(t, started.PID)
	killInfraScopeTestPID(t, started.WatchdogPID)
	waitForFile(t, marker)

	if started.InfraScope == "" {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("watchdog started from a non-session user unit ran in the caller's cgroup (no infra scope); systemd-run log:\n%s", data)
	}
	assertScopedInvocation(t, logPath, started.InfraScope, "gascity infra: managed dolt for city "+cityPath)
}
