//go:build integration

package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/userscope"
)

// infraScopeIntegrationSlice is the slice the integration test's throwaway
// scopes are created under. On hosts that define it, gascity-test.slice is
// the test containment slice (entered via gc-test-run) whose limits then
// bound the stand-in; elsewhere systemd creates it implicitly and removes it
// once the test's scope is gone.
const infraScopeIntegrationSlice = "gascity-test.slice"

// TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd proves, against a
// real systemd user manager, the claim the infrastructure scope rests on:
// systemd-run in scope mode execs the command, so the PID gc records for a
// scoped managed Dolt spawn is the server's own post-exec PID, it is the
// only process in its scope, and the PID-identity guard gc applies before
// signaling it accepts that PID (and refuses a mismatched snapshot).
//
// It runs the production spawn path (startManagedDoltSQLServer through
// infraCommand, with the production wrapper and probe) from a session-stamped
// environment. The "dolt" it spawns is a stand-in on PATH that execs a
// long-running sleep, so no Dolt binary, city or store is involved. It skips
// when no systemd user manager is reachable or when scoping would not be
// applied to this caller (neither lingering nor under user@<uid>.service).
//
// Run it on a host with a user manager, keeping the test runner local and
// passing the user bus through:
//
//	bazel test --config=integration --strategy=TestRunner=local \
//	  --test_env=XDG_RUNTIME_DIR --test_env=DBUS_SESSION_BUS_ADDRESS \
//	  --test_filter=TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd \
//	  //cmd/gc:gc_test
func TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd user scopes are Linux-only")
	}
	t.Setenv(infraSessionIdentityEnv, "gc-infra-scope-integration")
	t.Setenv(infraSliceEnv, infraScopeIntegrationSlice)
	if err := infraScopeKeepsLifetime(); err != nil {
		t.Skipf("infrastructure scoping does not apply to this caller: %v", err)
	}
	if err := userscope.Probe(infraScopeIntegrationSlice); err != nil {
		t.Skipf("no systemd user manager reachable: %v", err)
	}
	oldScope := infraScope
	infraScope = newInfraScope()
	t.Cleanup(func() { infraScope = oldScope })

	withManagedDoltTestMode(t, false)
	t.Setenv(managedDoltTestModeEnv, "")
	t.Setenv(managedDoltScopeWatchdogEnv, "0")
	binDir := t.TempDir()
	standIn := "#!/bin/sh\n[ \"$1\" = sql-server ] || exit 2\nexec sleep 300\n"
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(standIn), 0o755); err != nil {
		t.Fatalf("write stand-in dolt: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	logFile, err := os.Create(filepath.Join(dir, "dolt.log"))
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	started, err := startManagedDoltSQLServer("", filepath.Join(dir, "dolt-config.yaml"), logFile.Name(), logFile)
	if err != nil {
		t.Fatalf("startManagedDoltSQLServer: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(started.PID, syscall.SIGKILL)
		if started.InfraScope != "" {
			_ = exec.Command("systemctl", "--user", "stop", started.InfraScope).Run()
		}
	})
	if !infraUnitPattern.MatchString(started.InfraScope) {
		t.Fatalf("recorded infra scope = %q, want a %s unit", started.InfraScope, infraUnitPattern)
	}

	// The recorded PID becomes the stand-in once systemd-run and the shell
	// have exec'd: the same PID, no intermediate process left behind.
	if !infraPollUntil(10*time.Second, func() bool { return procComm(started.PID) == "sleep" }) {
		t.Fatalf("pid %d is %q, want the post-exec stand-in %q", started.PID, procComm(started.PID), "sleep")
	}

	// systemd's own view: the scope's cgroup holds exactly the recorded PID.
	cgroup := procUnifiedCgroup(t, started.PID)
	if want := "/" + infraScopeIntegrationSlice + "/" + started.InfraScope; !strings.HasSuffix(cgroup, want) {
		t.Fatalf("pid %d cgroup = %q, want it to end in %q", started.PID, cgroup, want)
	}
	if procs := cgroupProcs(t, cgroup); !slices.Equal(procs, []int{started.PID}) {
		t.Fatalf("scope %s holds pids %v, want exactly the recorded pid %d", started.InfraScope, procs, started.PID)
	}

	// The PID-identity guard compares the start identity snapshotted at
	// Start (possibly before systemd-run exec'd) with the post-exec process.
	if started.StartTimeTicks == 0 && started.StartIdentity == "" {
		t.Fatal("no start identity was snapshotted for the scoped spawn")
	}
	if !managedDoltStartedPIDIdentityMatches(started) {
		t.Fatalf("PID-identity guard rejected the scoped spawn's recorded identity (pid %d)", started.PID)
	}

	// The guard is what gc consults before signaling: a mismatched snapshot
	// is refused and the process survives; the recorded one is accepted and
	// the process is terminated.
	mismatched := started
	mismatched.StartTimeTicks++
	mismatched.StartIdentity += "-reused"
	terminateManagedDoltStartedProcess(mismatched)
	if !processAlive(started.PID) {
		t.Fatalf("pid %d was signaled through a mismatched identity snapshot", started.PID)
	}
	terminateManagedDoltStartedProcess(started)
	if !infraPollUntil(10*time.Second, func() bool { return !processAlive(started.PID) }) {
		t.Fatalf("pid %d survived termination through its recorded identity", started.PID)
	}
}

// procComm returns /proc/<pid>/comm, or "" when it cannot be read.
func procComm(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// procUnifiedCgroup returns pid's cgroup v2 path.
func procUnifiedCgroup(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		t.Fatalf("read cgroup of pid %d: %v", pid, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path
		}
	}
	t.Skipf("pid %d has no cgroup v2 membership: %q", pid, data)
	return ""
}

// cgroupProcs lists the PIDs in the cgroup v2 group at path.
func cgroupProcs(t *testing.T, path string) []int {
	t.Helper()
	f, err := os.Open(filepath.Join("/sys/fs/cgroup", path, "cgroup.procs"))
	if err != nil {
		t.Fatalf("open cgroup.procs for %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	var pids []int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
		if err != nil {
			t.Fatalf("parse cgroup.procs line %q: %v", sc.Text(), err)
		}
		pids = append(pids, pid)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read cgroup.procs for %s: %v", path, err)
	}
	return pids
}
