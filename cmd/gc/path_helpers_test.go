package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/testutil"
)

func canonicalTestPath(path string) string {
	return testutil.CanonicalPath(path)
}

func assertSameTestPath(t *testing.T, got, want string) {
	t.Helper()
	testutil.AssertSamePath(t, got, want)
}

func shortSocketTempDir(t *testing.T, prefix string) string {
	t.Helper()
	return testutil.ShortTempDir(t, prefix)
}

// clearInheritedBeadsEnv prevents tests that explicitly write
// [beads]\nprovider = "file" from being silently overridden by an agent
// session's inherited GC_BEADS=bd, which would trigger gc-beads-bd.sh and
// leak an orphan dolt sql-server because test cleanup paths do not call
// shutdownBeadsProvider.
func clearInheritedBeadsEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GC_BEADS",
		"GC_BIN",
		"GC_DOLT",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
		"GC_BEADS_SCOPE_ROOT",
	} {
		t.Setenv(key, "")
	}
}

// requireNoLeakedDoltAfter snapshots the live test-owned dolt sql-server PIDs
// at registration time and re-scans in t.Cleanup. Any matching PID present at
// cleanup that wasn't there at registration is reported via t.Errorf with PID
// and argv so operators can trace the spawn site.
//
// Pair with clearInheritedBeadsEnv: that helper prevents the leak by
// stripping inherited GC_BEADS=bd before the test writes its city.toml;
// this helper catches any leak that slips through (forgotten env scrub,
// child path that spawns dolt despite [beads] provider = "file", etc.).
//
// The scan walks /proc and is a no-op on hosts where /proc is unavailable
// (discoverDoltProcesses returns nil there). The test-config allowlist keeps
// unrelated city/runtime dolt servers out of the diff so background activity
// does not false-positive the cleanup check.
func requireNoLeakedDoltAfterForPaths(t *testing.T, paths ...string) {
	t.Helper()
	requireNoLeakedDoltAfterWithFilter(t, discoverDoltProcesses, func(configPath string) bool {
		for _, path := range paths {
			if path != "" && pathutil.PathWithin(path, configPath) {
				return true
			}
		}
		return false
	})
}

type doltLeakGuardedTestingM struct {
	m            *testing.M
	tempRoot     string
	cleanupPaths []string
}

func newDoltLeakGuardedTestingM(m *testing.M, tempRoot string, cleanupPaths ...string) *doltLeakGuardedTestingM {
	return &doltLeakGuardedTestingM{
		m:            m,
		tempRoot:     tempRoot,
		cleanupPaths: cleanupPaths,
	}
}

func (g *doltLeakGuardedTestingM) Run() int {
	initial, initialErr := snapshotDoltProcessesForConfigRoot(discoverDoltProcesses, g.tempRoot)
	if initialErr != nil {
		fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: initial scan failed: %v\n", initialErr) //nolint:errcheck
	}

	code := g.m.Run()

	guardFailed := initialErr != nil
	if initialErr == nil {
		final, finalErr := snapshotDoltProcessesForConfigRoot(discoverDoltProcesses, g.tempRoot)
		if finalErr != nil {
			fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: final scan failed: %v\n", finalErr) //nolint:errcheck
			guardFailed = true
		} else if leaked := diffDoltProcessSnapshots(initial, final); len(leaked) > 0 {
			fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: leaked %d dolt sql-server process(es) under %s\n", len(leaked), g.tempRoot) //nolint:errcheck
			writeDoltLeakReport(os.Stderr, leaked)
			reapDoltLeakProcesses(leaked)
			guardFailed = true
		}
	}

	for _, path := range g.cleanupPaths {
		if path != "" {
			_ = os.RemoveAll(path)
		}
	}
	if guardFailed && code == 0 {
		return 1
	}
	return code
}

func snapshotDoltProcessesForConfigRoot(enumerate func() ([]DoltProcInfo, error), root string) (map[int]DoltProcInfo, error) {
	procs, err := enumerate()
	if err != nil {
		return nil, err
	}
	out := make(map[int]DoltProcInfo, len(procs))
	for _, p := range procs {
		configPath := extractConfigPath(p.Argv)
		if root == "" || !pathutil.PathWithin(root, configPath) {
			continue
		}
		out[p.PID] = p
	}
	return out, nil
}

func diffDoltProcessSnapshots(initial, final map[int]DoltProcInfo) []DoltProcInfo {
	leaked := make([]DoltProcInfo, 0, len(final))
	for pid, proc := range final {
		if _, ok := initial[pid]; ok {
			continue
		}
		leaked = append(leaked, proc)
	}
	sort.Slice(leaked, func(i, j int) bool {
		return leaked[i].PID < leaked[j].PID
	})
	return leaked
}

func writeDoltLeakReport(w io.Writer, leaked []DoltProcInfo) {
	for _, proc := range leaked {
		fmt.Fprintf(w, "  pid=%d argv=%q\n", proc.PID, strings.Join(proc.Argv, " ")) //nolint:errcheck
	}
}

func reapDoltLeakProcesses(leaked []DoltProcInfo) {
	for _, proc := range leaked {
		_ = killProcess(proc.PID, syscall.SIGTERM)
	}
	time.Sleep(250 * time.Millisecond)
	for _, proc := range leaked {
		_ = killProcess(proc.PID, syscall.SIGKILL)
	}
}

// requireNoLeakedDoltAfterWith is the testReporter+injectable-enumerator
// form of requireNoLeakedDoltAfter. Production callers go through the
// thin wrapper above; unit tests for the leak-detector itself pass a
// recordingTB and a scripted enumerator so the report can be captured
// without spawning real dolt children.
func requireNoLeakedDoltAfterWith(t testReporter, enumerate func() ([]DoltProcInfo, error)) {
	t.Helper()
	homeDir, _ := os.UserHomeDir()
	tempDir := os.TempDir()
	requireNoLeakedDoltAfterWithFilter(t, enumerate, func(configPath string) bool {
		return isTestConfigPath(configPath, homeDir, tempDir)
	})
}

func requireNoLeakedDoltAfterWithFilter(t testReporter, enumerate func() ([]DoltProcInfo, error), includeConfigPath func(string) bool) {
	t.Helper()
	initial := snapshotDoltProcessPIDsWithFilter(t, enumerate, includeConfigPath)
	t.Cleanup(func() {
		leaked := snapshotDoltProcessPIDsWithFilter(t, enumerate, includeConfigPath)
		for pid := range initial {
			delete(leaked, pid)
		}
		if len(leaked) == 0 {
			return
		}
		pids := make([]int, 0, len(leaked))
		for pid := range leaked {
			pids = append(pids, pid)
		}
		sort.Ints(pids)
		var rep []string
		for _, pid := range pids {
			rep = append(rep, fmt.Sprintf("  pid=%d argv=%q", pid, leaked[pid]))
		}
		t.Errorf("test leaked %d dolt sql-server process(es); ensure cleanup paths reach shutdownBeadsProvider, or call clearInheritedBeadsEnv to prevent inherited GC_BEADS=bd from triggering gc-beads-bd.sh:\n%s",
			len(leaked), strings.Join(rep, "\n"))
	})
}

// snapshotDoltProcessPIDsWith returns a map from PID to space-joined argv for
// every live test-owned dolt sql-server returned by enumerate. The production
// caller passes discoverDoltProcesses (which walks /proc and degrades to no-op
// on hosts where /proc is unavailable); unit tests for the leak-detector itself
// pass a scripted enumerator. Enumeration errors are surfaced via Fatalf so a
// swallowed discovery failure can never silently mask a real leak.
func snapshotDoltProcessPIDsWith(t testReporter, enumerate func() ([]DoltProcInfo, error)) map[int]string {
	t.Helper()
	homeDir, _ := os.UserHomeDir()
	tempDir := os.TempDir()
	return snapshotDoltProcessPIDsWithFilter(t, enumerate, func(configPath string) bool {
		return isTestConfigPath(configPath, homeDir, tempDir)
	})
}

func snapshotDoltProcessPIDsWithFilter(t testReporter, enumerate func() ([]DoltProcInfo, error), includeConfigPath func(string) bool) map[int]string {
	t.Helper()
	procs, err := enumerate()
	if err != nil {
		t.Fatalf("discoverDoltProcesses: %v", err)
	}
	out := make(map[int]string, len(procs))
	for _, p := range procs {
		if !includeConfigPath(extractConfigPath(p.Argv)) {
			continue
		}
		out[p.PID] = strings.Join(p.Argv, " ")
	}
	return out
}

func cleanupManagedDoltTestCity(t *testing.T, cityPath string) {
	t.Helper()
	requireNoLeakedDoltAfterForPaths(t, cityPath)
	t.Cleanup(func() {
		tryStopController(cityPath, io.Discard)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if controllerAlive(cityPath) == 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if port := currentManagedDoltPort(cityPath); port != "" {
			if _, err := stopManagedDoltProcess(cityPath, port); err != nil {
				t.Logf("stopManagedDoltProcess(%s, %s): %v", cityPath, port, err)
			}
		}
		if err := shutdownBeadsProvider(cityPath); err != nil {
			t.Logf("shutdownBeadsProvider(%s): %v", cityPath, err)
		}
		stopManagedDoltProcessesUnderTestCity(t, cityPath)
	})
}

func stopManagedDoltProcessesUnderTestCity(t *testing.T, cityPath string) {
	t.Helper()
	procs, err := discoverDoltProcesses()
	if err != nil {
		t.Fatalf("discoverDoltProcesses: %v", err)
	}
	for _, p := range procs {
		configPath := extractConfigPath(p.Argv)
		if !pathutil.PathWithin(cityPath, configPath) {
			continue
		}
		stopManagedDoltTestPID(t, p.PID)
	}
}

func stopManagedDoltTestPID(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 || !managedStopPIDAlive(pid) {
		return
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		t.Fatalf("signal dolt test pid %d with SIGTERM: %v", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for managedStopPIDAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !managedStopPIDAlive(pid) {
		return
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatalf("signal dolt test pid %d with SIGKILL: %v", pid, err)
	}
	deadline = time.Now().Add(time.Second)
	for managedStopPIDAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if managedStopPIDAlive(pid) {
		t.Fatalf("dolt test pid %d still alive after SIGKILL", pid)
	}
}
