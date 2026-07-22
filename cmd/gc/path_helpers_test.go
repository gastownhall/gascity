package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doltorphan"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/test/tmuxtest"
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

// cmdGCTmuxSocketRoot returns a tmux socket root under socketParentRoot.
// TestMain normally supplies /tmp rather than testTempRoot, which can be an
// arbitrarily long macOS $TMPDIR path that blows Unix socket path limits. It
// also returns the parent dir to remove at teardown and the *os.File holding
// its alive sentinel. The sentinel must stay referenced by the caller for the
// process lifetime so a concurrent sibling run's orphan sweep
// (tmuxtest.SweepOrphanPIDPrefixedDirs, invoked inside NewSocketParentDir)
// does not reclaim this still-active directory.
func cmdGCTmuxSocketRoot(testTempRoot, socketParentRoot string) (string, string, *os.File, error) {
	parent, sentinel, err := tmuxtest.NewSocketParentDir(socketParentRoot, io.Discard)
	if err != nil {
		root := filepath.Join(testTempRoot, "tmux")
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", "", nil, fmt.Errorf("creating fallback cmd/gc tmux socket root: %w", err)
		}
		return root, "", nil, nil
	}
	root := filepath.Join(parent, "tmux")
	if err := os.MkdirAll(root, 0o700); err != nil {
		_ = sentinel.Close()
		_ = os.RemoveAll(parent)
		return "", "", nil, fmt.Errorf("creating cmd/gc tmux socket root: %w", err)
	}
	return root, parent, sentinel, nil
}

// clearInheritedBeadsEnv prevents tests that explicitly write
// [beads]\nprovider = "file" from being silently overridden by an agent
// session's inherited GC_BEADS=bd, which would trigger gc-beads-bd.sh and
// leak an orphan dolt sql-server because test cleanup paths do not call
// shutdownBeadsProvider.
func clearInheritedBeadsEnv(t *testing.T) {
	t.Helper()
	for _, key := range liveEnvKeysForTests() {
		if key == "GC_HOME" {
			continue
		}
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
	requireNoLeakedDoltAfterWithFilterAndKiller(t, discoverDoltProcesses, func(configPath string) bool {
		for _, path := range paths {
			if path != "" && pathutil.PathWithin(path, configPath) {
				return true
			}
		}
		return false
	}, killProcess)
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
	return g.runWith(g.m.Run, discoverDoltProcesses, g.sweepStaleCmdGCTestDoltProcesses, sweepOrphanDoltStoreDirs, reapManagedDoltTestProcesses, reapDoltLeakProcesses)
}

func (g *doltLeakGuardedTestingM) runWith(
	runTests func() int,
	enumerate func() ([]DoltProcInfo, error),
	sweepStale func(string) bool,
	sweepOrphanDirs func(),
	reapRegistered func(),
	reapLeaks func([]DoltProcInfo),
) int {
	_ = sweepStale("startup")
	sweepOrphanDirs()
	stopSignalHandler := g.installSignalHandler()
	defer stopSignalHandler()

	initial, initialErr := snapshotDoltProcessesForConfigRoot(enumerate, g.tempRoot)
	if initialErr != nil {
		fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: initial scan failed: %v\n", initialErr) //nolint:errcheck
	}

	code := runTests()

	guardFailed := initialErr != nil
	if initialErr == nil {
		final, finalErr := snapshotDoltProcessesForConfigRoot(enumerate, g.tempRoot)
		if finalErr != nil {
			fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: final scan failed: %v\n", finalErr) //nolint:errcheck
			guardFailed = true
		} else if leaked := diffDoltProcessSnapshots(initial, final); len(leaked) > 0 {
			fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: leaked %d dolt sql-server process(es) under %s\n", len(leaked), g.tempRoot) //nolint:errcheck
			writeDoltLeakReport(os.Stderr, leaked)
			reapLeaks(leaked)
			guardFailed = true
		}
	}

	g.cleanupTemporaryPaths()
	reapRegistered()

	if guardFailed && code == 0 {
		return 1
	}
	return code
}

func (g *doltLeakGuardedTestingM) installSignalHandler() func() {
	signals := make(chan os.Signal, 2)
	done := make(chan struct{})
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-signals:
			fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: received %s; sweeping test dolt processes before exit\n", sig) //nolint:errcheck
			_ = g.reapDoltProcessesUnderRoot("signal")
			g.cleanupTemporaryPaths()
			signal.Stop(signals)
			if s, ok := sig.(syscall.Signal); ok {
				signal.Reset(s)
				_ = syscall.Kill(os.Getpid(), s)
			}
		case <-done:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}

func (g *doltLeakGuardedTestingM) cleanupTemporaryPaths() {
	for _, path := range g.cleanupPaths {
		if path != "" {
			_ = os.RemoveAll(path)
		}
	}
}

func (g *doltLeakGuardedTestingM) reapDoltProcessesUnderRoot(label string) bool {
	procs, err := snapshotDoltProcessesForConfigRoot(discoverDoltProcesses, g.tempRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: %s scan failed: %v\n", label, err) //nolint:errcheck
		return true
	}
	if len(procs) == 0 {
		return false
	}
	leaked := make([]DoltProcInfo, 0, len(procs))
	for _, proc := range procs {
		leaked = append(leaked, proc)
	}
	sort.Slice(leaked, func(i, j int) bool {
		return leaked[i].PID < leaked[j].PID
	})
	fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: %s sweep reaping %d dolt sql-server process(es) under %s\n", label, len(leaked), g.tempRoot) //nolint:errcheck
	writeDoltLeakReport(os.Stderr, leaked)
	reapDoltLeakProcesses(leaked)
	return true
}

func (g *doltLeakGuardedTestingM) sweepStaleCmdGCTestDoltProcesses(label string) bool {
	procs, err := discoverDoltProcesses()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: %s stale scan failed: %v\n", label, err) //nolint:errcheck
		return true
	}
	activeRoots := cmdGCTestActiveRoots(g.tempRoot)
	tempParent := filepath.Dir(filepath.Clean(g.tempRoot))
	var leaked []DoltProcInfo
	for _, proc := range procs {
		if !isStaleCmdGCTestConfigPath(extractConfigPath(proc.Argv), activeRoots, tempParent) {
			continue
		}
		leaked = append(leaked, proc)
	}
	if len(leaked) == 0 {
		return false
	}
	sort.Slice(leaked, func(i, j int) bool {
		return leaked[i].PID < leaked[j].PID
	})
	fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: %s sweep reaping %d stale cmd/gc test dolt sql-server process(es)\n", label, len(leaked)) //nolint:errcheck
	writeDoltLeakReport(os.Stderr, leaked)
	reapDoltLeakProcesses(leaked)
	return true
}

// sweepOrphanDoltStoreDirs runs the symptom-based fallback sweep
// (internal/doltorphan.Sweep) over os.TempDir(), removing stray dolt store
// directories regardless of what created them (ga-ntbpyb.2 acceptance
// criterion 2). It composes with, but does not replace,
// sweepStaleCmdGCTestDoltProcesses above: that reaps stale *processes* by
// config-path heuristics; this catches the *directory* left behind when a
// process is already gone by the time any process-level sweep runs (e.g. a
// SIGKILLed test binary whose pid was later reused).
func sweepOrphanDoltStoreDirs() {
	result := doltorphan.Sweep(doltorphan.SweepConfig{Root: os.TempDir()})
	for _, dir := range result.Removed {
		fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: startup sweep removed orphaned dolt store dir %s\n", dir) //nolint:errcheck
	}
	for _, err := range result.Errors {
		fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: startup sweep error: %v\n", err) //nolint:errcheck
	}
}

func cmdGCTestActiveRoots(currentRoot string) []string {
	roots := discoverActiveTestRoots("", os.TempDir())
	if currentRoot != "" {
		roots = append(roots, currentRoot)
	}
	cleaned := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		clean := filepath.Clean(root)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		cleaned = append(cleaned, clean)
	}
	return cleaned
}

func isStaleCmdGCTestConfigPath(configPath string, activeRoots []string, tempParent string) bool {
	return isStaleCmdGCTestConfigPathWithPIDCheck(configPath, activeRoots, tempParent, pidAlive)
}

func isStaleCmdGCTestConfigPathWithPIDCheck(configPath string, activeRoots []string, tempParent string, pidAliveFn func(int) bool) bool {
	if configPath == "" || tempParent == "" {
		return false
	}
	if configUnderActiveTestRoot(configPath, activeRoots) {
		return false
	}
	ownerPID, ok := cmdGCTestConfigOwnerPID(configPath, tempParent)
	if !ok {
		return false
	}
	return !pidAliveFn(ownerPID)
}

func cmdGCTestConfigOwnerPID(configPath string, tempParent string) (int, bool) {
	for _, prefix := range []string{testCmdGCTempRootPrefix, testCmdGCShardTempRootPrefix} {
		root, ok := activeTestRootUnder(filepath.Clean(configPath), filepath.Clean(tempParent), []string{prefix})
		if !ok {
			continue
		}
		return pidFromPrefixedDirName(filepath.Base(root), prefix)
	}
	return 0, false
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
	_ = reapDoltLeakProcessesWithKiller(leaked, killProcess)
}

func reapDoltLeakProcessesWithKiller(leaked []DoltProcInfo, killFn func(int, syscall.Signal) error) []error {
	pids := make([]int, 0, len(leaked))
	for _, proc := range leaked {
		pids = append(pids, proc.PID)
	}
	return reapDoltLeakPIDsWithKiller(pids, killFn)
}

func reapDoltLeakPIDsWithKiller(pids []int, killFn func(int, syscall.Signal) error) []error {
	var errs []error
	for _, pid := range pids {
		if err := killFn(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("SIGTERM pid %d: %w", pid, err))
		}
	}
	time.Sleep(250 * time.Millisecond)
	for _, pid := range pids {
		if err := killFn(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("SIGKILL pid %d: %w", pid, err))
		}
	}
	return errs
}

func ignoreProcessSignal(int, syscall.Signal) error {
	return nil
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
	requireNoLeakedDoltAfterWithFilterAndKiller(t, enumerate, func(configPath string) bool {
		return isTestConfigPath(configPath, homeDir, tempDir)
	}, ignoreProcessSignal)
}

func requireNoLeakedDoltAfterWithFilter(t testReporter, enumerate func() ([]DoltProcInfo, error), includeConfigPath func(string) bool) {
	requireNoLeakedDoltAfterWithFilterAndKiller(t, enumerate, includeConfigPath, ignoreProcessSignal)
}

func requireNoLeakedDoltAfterWithFilterAndKiller(t testReporter, enumerate func() ([]DoltProcInfo, error), includeConfigPath func(string) bool, killFn func(int, syscall.Signal) error) {
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
		for _, err := range reapDoltLeakPIDsWithKiller(pids, killFn) {
			t.Errorf("test leaked dolt cleanup failed: %v", err)
		}
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
