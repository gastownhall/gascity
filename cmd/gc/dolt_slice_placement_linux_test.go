package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/systemdscope"
	"github.com/gastownhall/gascity/internal/testutil"
)

// managedDoltPlacementTestSlice is the slice the placement integration tests
// spawn into. Flat by construction: systemd reads "a-b.slice" as a child of
// "a.slice", and a nested test slice would not exercise the property under
// test (escaping the parent memcg).
const managedDoltPlacementTestSlice = "gcdolttest.slice"

// requireTransientUserScopes skips when this host cannot create transient
// systemd user scopes — no systemd-run, no user bus, a container. Placement is
// hardening layered on top of a graceful fallback, so its absence is a skip,
// never a failure.
func requireTransientUserScopes(t *testing.T) {
	t.Helper()
	if err := systemdscope.Probe(managedDoltPlacementTestSlice); err != nil {
		t.Skipf("transient user scopes unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "stop", managedDoltPlacementTestSlice).Run()
	})
}

func readProcCgroup(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		t.Fatalf("read cgroup for pid %d: %v", pid, err)
	}
	return strings.TrimSpace(string(data))
}

// waitForCgroupPlacement polls until pid is inside slice, or fails.
//
// Placement is not observable the instant Cmd.Start returns. `systemd-run
// --scope` registers the transient unit over D-Bus and only then execs the
// payload, so for a short window the child is still systemd-run sitting in the
// caller's cgroup. Reading /proc/<pid>/cgroup once, immediately, is therefore a
// race that reports the pre-placement cgroup. Production does not care — the
// move completes in milliseconds, long before memory pressure is a question —
// but a test must wait for it rather than sample it.
func waitForCgroupPlacement(t *testing.T, pid int, slice string) {
	t.Helper()
	deadline := time.Now().Add(testutil.ExecRaceTimeout)
	var last string
	for {
		last = readProcCgroup(t, pid)
		if strings.Contains(last, slice) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("pid %d cgroup = %q, want it inside %s", pid, last, slice)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readProcOOMScoreAdj(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "oom_score_adj"))
	if err != nil {
		t.Fatalf("read oom_score_adj for pid %d: %v", pid, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse oom_score_adj for pid %d: %v", pid, err)
	}
	return value
}

// TestManagedDoltScopeWatchdogPlacesServerInSlice is the acceptance test for
// this fix. It spawns a fake dolt server through the production scope-watchdog
// path from a parent that carries the inherited oom_score_adj=200, and asserts
// the two properties that failed in production on 2026-07-21:
//
//   - both the watchdog and the server land in the managed-dolt slice,
//     regardless of the spawning process's own cgroup, so placement no longer
//     depends on which process happened to trigger auto-start;
//   - the server does not inherit the badness bonus that made the kernel's
//     memcg OOM killer rank a ~1 GiB process ahead of genuinely large ones.
//
// It also pins the property that made wrapping safe at all: `systemd-run
// --scope` execs in place, so the PIDs reported through the watchdog handshake
// are the real watchdog and dolt PIDs and remain signalable.
func TestManagedDoltScopeWatchdogPlacesServerInSlice(t *testing.T) {
	requireTransientUserScopes(t)

	dir := t.TempDir()
	fakeDoltDir := writeFakeDoltSQLServer(t)
	statePath := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestManagedDoltSlicePlacementHelper", "-test.v")
	cmd.Env = sanitizedBaseEnv(
		"GC_TEST_MANAGED_DOLT_HELPER=slice-placement",
		"GC_TEST_MANAGED_DOLT_HELPER_STATE="+statePath,
		"GC_TEST_MANAGED_DOLT_HELPER_CONFIG="+configPath,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG="+logPath,
		"GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR="+fakeDoltDir,
		"GC_TEST_MANAGED_DOLT_HELPER_SLICE="+managedDoltPlacementTestSlice,
		"GC_TEST_MANAGED_DOLT_HELPER_SCOPE_WD_INTERVAL_MS=50",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	doltPID, watchdogPID := readManagedDoltTestState(t, statePath)
	t.Cleanup(func() {
		cleanupManagedDoltTestPID(t, doltPID)
		cleanupManagedDoltTestPID(t, watchdogPID)
	})

	// The handshake PIDs must be live processes. If systemd-run had forked
	// instead of exec'ing, these would name a systemd-run process that exited
	// the moment it handed off, and every downstream signal would miss.
	if !pidAlive(doltPID) {
		logData, _ := os.ReadFile(logPath)
		t.Fatalf("fake dolt pid %d not alive; helper output:\n%s\nwatchdog log:\n%s", doltPID, output, logData)
	}
	if !pidAlive(watchdogPID) {
		t.Fatalf("watchdog pid %d not alive; helper output:\n%s", watchdogPID, output)
	}

	waitForCgroupPlacement(t, watchdogPID, managedDoltPlacementTestSlice)
	waitForCgroupPlacement(t, doltPID, managedDoltPlacementTestSlice)

	// The helper set its own oom_score_adj to 200 before spawning; the
	// watchdog must have cleared it so the server inherits a neutral value.
	if got := readProcOOMScoreAdj(t, doltPID); got != managedDoltOOMScoreAdj {
		logData, _ := os.ReadFile(logPath)
		t.Errorf("dolt pid %d oom_score_adj = %d, want %d; watchdog log:\n%s",
			doltPID, got, managedDoltOOMScoreAdj, logData)
	}
}

// TestManagedDoltDirectSpawnPlacesServerInSlice covers the watchdog-free
// branch of startManagedDoltSQLServer — the branch an operator reaches by
// setting GC_DOLT_SCOPE_WATCHDOG=0, which is exactly what they would do if the
// watchdog itself were misbehaving. The wrapping helpers are unit-tested in
// isolation elsewhere; what is asserted here is the wiring at this specific
// call site: that argv[0] is swapped for systemd-run without losing the
// command, that the returned PID is the server's own and lands in the slice,
// and that the caller's oom_score_adj is restored rather than left rewritten.
func TestManagedDoltDirectSpawnPlacesServerInSlice(t *testing.T) {
	requireTransientUserScopes(t)

	// Force the direct branch: the scope watchdog is already off inside the
	// test binary, so disabling the test watchdog leaves the plain spawn.
	t.Setenv("GC_MANAGED_DOLT_TEST_WATCHDOG", "0")
	t.Setenv(managedDoltSliceEnv, managedDoltPlacementTestSlice)
	t.Setenv("PATH", writeFakeDoltSQLServer(t)+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer logFile.Close() //nolint:errcheck

	before := readProcOOMScoreAdj(t, os.Getpid())
	started, err := startManagedDoltSQLServer("", configPath, logPath, logFile)
	if err != nil {
		t.Fatalf("startManagedDoltSQLServer: %v", err)
	}
	t.Cleanup(func() { cleanupManagedDoltTestPID(t, started.PID) })

	if !pidAlive(started.PID) {
		logData, _ := os.ReadFile(logPath)
		t.Fatalf("dolt pid %d not alive; log:\n%s", started.PID, logData)
	}
	waitForCgroupPlacement(t, started.PID, managedDoltPlacementTestSlice)
	// The caller is a general-purpose gc process here, so its own badness must
	// be exactly what it was before it started a server.
	if after := readProcOOMScoreAdj(t, os.Getpid()); after != before {
		t.Errorf("caller oom_score_adj = %d after spawning, want it restored to %d", after, before)
	}
}

// TestManagedDoltSlicePlacementHelper runs in a child process: it starts a fake
// dolt server under the production scope watchdog with slice placement armed,
// and records both PIDs.
//
// It duplicates a little of TestManagedDoltScopeWatchdogHelper rather than
// extending it, deliberately. The extra setup is process-environment mutation
// specific to placement, and cmd/gc holds a standing ratchet against growing
// that in untagged test source (TESTING.md, "Small debt ratchet"). Keeping it
// in this platform-constrained file leaves the untagged totals untouched, and
// the setup is Linux-only anyway — it writes /proc/self/oom_score_adj.
func TestManagedDoltSlicePlacementHelper(t *testing.T) {
	if os.Getenv("GC_TEST_MANAGED_DOLT_HELPER") != "slice-placement" {
		t.Skip("helper process only")
	}
	fakeDoltDir := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR"))
	statePath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_STATE"))
	configPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_CONFIG"))
	logPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_LOG"))
	slice := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_SLICE"))
	if fakeDoltDir == "" || statePath == "" || configPath == "" || logPath == "" || slice == "" {
		t.Fatal("missing helper inputs")
	}
	t.Setenv("PATH", fakeDoltDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// TestMain scrubs non-GC_TEST_ GC_* keys, so placement rides a GC_TEST_
	// control var and is re-exported here. Setting it explicitly is what opts
	// this helper into real placement: managedDoltSliceFor declines the
	// implicit default under test mode.
	t.Setenv(managedDoltSliceEnv, slice)
	if interval := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_SCOPE_WD_INTERVAL_MS")); interval != "" {
		t.Setenv(managedDoltScopeWatchdogIntervalEnv, interval)
	}
	// Reproduce the production inheritance chain this fix exists to break:
	// systemd's user manager hands every descendant oom_score_adj=200, and dolt
	// used to inherit it all the way down. Raising needs no privilege.
	if err := os.WriteFile(procSelfOOMScoreAdj, []byte("200"), 0o644); err != nil {
		t.Fatalf("seed inherited oom_score_adj: %v", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer logFile.Close() //nolint:errcheck

	started, err := startManagedDoltSQLServerWithScopeWatchdog("", configPath, logPath, logFile)
	if err != nil {
		t.Fatalf("start managed dolt with scope watchdog: %v", err)
	}
	if err := os.WriteFile(statePath, []byte(fmt.Sprintf("%d %d\n", started.PID, started.WatchdogPID)), 0o644); err != nil {
		t.Fatalf("write helper state: %v", err)
	}
}
