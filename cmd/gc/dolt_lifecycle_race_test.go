package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Regression coverage for gastownhall/gascity#3174: a new `dolt sql-server`
// must never bind a data_dir whose exclusive store lock is still held by a
// prior instance, and lifecycle stops must never SIGKILL a process that is
// mid-journal-write (still holding the store lock).

// raceTestCity builds a minimal city whose managed-dolt layout is pinned to
// test-local paths via the GC_DOLT_* env overrides, with one database whose
// noms LOCK file exists. Returns the city path and the lock path.
func raceTestCity(t *testing.T, cityToml string) (string, string) {
	t.Helper()
	city := t.TempDir()
	if cityToml != "" {
		if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
			t.Fatalf("write city.toml: %v", err)
		}
	}
	dataDir := filepath.Join(city, ".beads", "dolt")
	stateDir := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
	for key, value := range map[string]string{
		"GC_PACK_STATE_DIR":   stateDir,
		"GC_CITY_RUNTIME_DIR": "",
		"GC_DOLT_DATA_DIR":    dataDir,
		"GC_DOLT_LOG_FILE":    filepath.Join(stateDir, "dolt.log"),
		"GC_DOLT_STATE_FILE":  filepath.Join(stateDir, "dolt-provider-state.json"),
		"GC_DOLT_PID_FILE":    filepath.Join(stateDir, "dolt.pid"),
		"GC_DOLT_LOCK_FILE":   filepath.Join(stateDir, "dolt.lock"),
		"GC_DOLT_CONFIG_FILE": filepath.Join(stateDir, "dolt-config.yaml"),
	} {
		t.Setenv(key, value)
	}
	nomsDir := filepath.Join(dataDir, "dolt", ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("mkdir noms dir: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	lockPath := filepath.Join(nomsDir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	return city, lockPath
}

// shimLockReleaseTimeout pins the lock-release window so held-lock tests fail
// closed quickly instead of waiting the production default.
func shimLockReleaseTimeout(t *testing.T, window time.Duration) {
	t.Helper()
	orig := managedDoltLockReleaseTimeoutFn
	t.Cleanup(func() { managedDoltLockReleaseTimeoutFn = orig })
	managedDoltLockReleaseTimeoutFn = func(string) time.Duration { return window }
}

// startSigtermIgnoringProcess spawns a shell that ignores SIGTERM, simulating
// a dolt server stuck in a long flush. cwd is set to dir so the managed-dolt
// ownership check (processCWDMatches) attributes it to the data dir.
func startSigtermIgnoringProcess(t *testing.T, dir string) int {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "trap-ready")
	cmd := exec.Command("sh", "-c", "trap '' TERM; : > \"$1\"; while :; do sleep 0.05; done", "sh", ready)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sigterm-ignoring process: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	go func() { _ = cmd.Wait() }()
	// Wait until the trap is installed; signaling earlier would kill the
	// child before it starts ignoring SIGTERM.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("sigterm-ignoring process never installed its trap")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartManagedDoltFailsClosedWhenDataDirLockHeld(t *testing.T) {
	city, lockPath := raceTestCity(t, "")
	holdFlock(t, lockPath)
	shimLockReleaseTimeout(t, 150*time.Millisecond)

	origStart := managedDoltStartSQLServerFn
	t.Cleanup(func() { managedDoltStartSQLServerFn = origStart })
	spawned := false
	managedDoltStartSQLServerFn = func(string, string, string, *os.File) (managedDoltStartedProcess, error) {
		spawned = true
		return managedDoltStartedProcess{}, errors.New("must not spawn while the store lock is held")
	}

	_, err := startManagedDoltProcessWithOptions(city, "127.0.0.1", "13317", "root", "warning", -1, 2*time.Second, false)
	if err == nil {
		t.Fatal("expected start to fail closed while the dolt store lock is held")
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("expected error to name the held lock %q, got %v", lockPath, err)
	}
	if spawned {
		t.Fatal("start spawned dolt sql-server despite a held store lock")
	}
}

func TestStartManagedDoltGuardPassesWhenLockFree(t *testing.T) {
	city, _ := raceTestCity(t, "")
	shimLockReleaseTimeout(t, 150*time.Millisecond)

	origStart := managedDoltStartSQLServerFn
	t.Cleanup(func() { managedDoltStartSQLServerFn = origStart })
	spawned := false
	sentinel := errors.New("spawn reached")
	managedDoltStartSQLServerFn = func(string, string, string, *os.File) (managedDoltStartedProcess, error) {
		spawned = true
		return managedDoltStartedProcess{}, sentinel
	}

	_, err := startManagedDoltProcessWithOptions(city, "127.0.0.1", "13317", "root", "warning", -1, 2*time.Second, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the stubbed spawn error, got %v", err)
	}
	if !spawned {
		t.Fatal("guard blocked the start path even though the store lock was free")
	}
}

func TestTerminateManagedDoltPIDRefusesSIGKILLWhileLockHeld(t *testing.T) {
	skipSlowCmdGCTest(t, "spawns real processes; run via make test-cmd-gc-process")
	city, lockPath := raceTestCity(t, "[workspace]\nname = \"race-test\"\n\n[daemon]\ndolt_stop_timeout = \"100ms\"\n")
	release := holdFlock(t, lockPath)
	shimLockReleaseTimeout(t, 200*time.Millisecond)

	dataDir := filepath.Join(city, ".beads", "dolt")
	pid := startSigtermIgnoringProcess(t, dataDir)

	err := terminateManagedDoltPID(city, pid)
	if err == nil {
		t.Fatal("expected terminate to refuse SIGKILL while the store lock is held")
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("expected error to name the held lock %q, got %v", lockPath, err)
	}
	if !pidAlive(pid) {
		t.Fatal("process was killed despite the held store lock")
	}

	release()
	if err := terminateManagedDoltPID(city, pid); err != nil {
		t.Fatalf("expected terminate to force-kill once the lock is free, got %v", err)
	}
	if pidAlive(pid) {
		t.Fatal("process still alive after lock-free terminate")
	}
}

func TestStopManagedDoltRefusesSIGKILLWhileLockHeld(t *testing.T) {
	skipSlowCmdGCTest(t, "spawns real processes; run via make test-cmd-gc-process")
	city, lockPath := raceTestCity(t, "[workspace]\nname = \"race-test\"\n\n[daemon]\ndolt_stop_timeout = \"100ms\"\n")
	release := holdFlock(t, lockPath)
	shimLockReleaseTimeout(t, 200*time.Millisecond)

	dataDir := filepath.Join(city, ".beads", "dolt")
	pid := startSigtermIgnoringProcess(t, dataDir)
	pidFile := os.Getenv("GC_DOLT_PID_FILE")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	report, err := stopManagedDoltProcessWithOptions(city, "", false)
	if err == nil {
		t.Fatal("expected stop to refuse SIGKILL while the store lock is held")
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("expected error to name the held lock %q, got %v", lockPath, err)
	}
	if report.Forced {
		t.Fatal("stop reported a forced kill despite refusing SIGKILL")
	}
	if !pidAlive(pid) {
		t.Fatal("process was killed despite the held store lock")
	}

	// Once the lock is free, the wedged (but non-writing) process is still
	// force-killed — the legacy escape hatch for hung servers stays intact.
	release()
	report, err = stopManagedDoltProcessWithOptions(city, "", false)
	if err != nil {
		t.Fatalf("expected stop to force-kill once the lock is free, got %v", err)
	}
	if !report.Forced {
		t.Fatal("expected a forced kill for the sigterm-ignoring process")
	}
	if pidAlive(pid) {
		t.Fatal("process still alive after lock-free stop")
	}
}

func TestStopManagedDoltNoControllablePIDStillGatesOnLockRelease(t *testing.T) {
	city, lockPath := raceTestCity(t, "")
	release := holdFlock(t, lockPath)
	shimLockReleaseTimeout(t, 150*time.Millisecond)

	// No PID file and no port holder: a crashed server left a flushing
	// descendant behind. The stop contract says success means the data dir
	// is released — stop must fail closed while the store lock is held, or
	// anything keyed on stop's success (backup, move, delete) can act
	// mid-flush (gastownhall/gascity#3174).
	report, err := stopManagedDoltProcessWithOptions(city, "", false)
	if err == nil {
		t.Fatal("expected stop to fail while the store lock is held with no controllable pid")
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("expected error to name the held lock %q, got %v", lockPath, err)
	}
	if report.HadPID {
		t.Fatal("expected no controllable pid in the report")
	}

	release()
	if _, err := stopManagedDoltProcessWithOptions(city, "", false); err != nil {
		t.Fatalf("expected stop to succeed once the lock is free, got %v", err)
	}
}

func TestStopManagedDoltWaitsForLockReleaseAfterExit(t *testing.T) {
	skipSlowCmdGCTest(t, "spawns real processes; run via make test-cmd-gc-process")
	city, lockPath := raceTestCity(t, "[workspace]\nname = \"race-test\"\n\n[daemon]\ndolt_stop_timeout = \"5s\"\n")
	// The test itself holds the lock, simulating a descendant (e.g. a dolt
	// gc worker) that outlives the server process and is still flushing.
	holdFlock(t, lockPath)
	shimLockReleaseTimeout(t, 200*time.Millisecond)

	dataDir := filepath.Join(city, ".beads", "dolt")
	cmd := exec.Command("sh", "-c", "while :; do sleep 0.05; done")
	cmd.Dir = dataDir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	go func() { _ = cmd.Wait() }()
	pidFile := os.Getenv("GC_DOLT_PID_FILE")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	_, err := stopManagedDoltProcessWithOptions(city, "", false)
	if err == nil {
		t.Fatal("expected stop to report the data dir as not yet released")
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("expected error to name the held lock %q, got %v", lockPath, err)
	}
	if pidAlive(pid) {
		t.Fatal("expected the SIGTERM-respecting process to have exited")
	}
}
