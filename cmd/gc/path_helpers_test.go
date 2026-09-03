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

	"github.com/cenkalti/backoff/v4"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltorphan"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

// doltLeakGuardGraceInitialInterval and doltLeakGuardGraceMaxElapsedTime
// bound how long runWith tolerates a candidate leak surviving past
// runTests() returning. A dolt sql-server that a test has already signaled
// to stop still needs a bounded, non-zero amount of wall-clock time to
// actually leave the process table (flush, close listeners, OS reap); under
// host contention that ordinary shutdown tail can still be in flight the
// instant the first final scan fires, which misclassifies a process
// finishing an already-in-progress clean shutdown as a permanent leak
// (ga-szv0ge). The guard's real invariant is "no test leaves a dolt server
// running forever," not "no test leaves a dolt server running for one more
// scheduler tick after Run() returns" — mirrors the hang-budget framing in
// ga-f5clwo: this is a hang detector, not a latency SLO, so per ga-f5clwo's
// own carve-out it gets "one shared generous budget" rather than per-test
// tuning. Round 2's 5s budget still false-positived under host contention
// (ga-d5nmtj gate evidence: PID 2911558 outlived the grace window, a later
// scan found it already gone). Round 3 sets the ceiling to
// config.DefaultDoltStopTimeout rather than a new arbitrary literal: that
// constant is this same codebase's existing answer to "how long may a
// managed dolt server take to actually stop" (its SIGTERM→SIGKILL grace,
// cmd/gc/dolt_stop_managed.go), so the guard now tolerates exactly the
// shutdown tail the system itself is already configured to allow — no
// process the system considers to be stopping normally can trip it. Sized
// well below normal package test timeouts so a genuine leak (one that never
// clears, e.g. ga-vltdpl) still fails within tens of seconds, not minutes.
const (
	doltLeakGuardGraceInitialInterval = 250 * time.Millisecond
	doltLeakGuardGraceMaxElapsedTime  = config.DefaultDoltStopTimeout
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
	requireNoLeakedDoltAfterWithFilterAndReaper(t, discoverDoltProcesses, func(configPath string) bool {
		for _, path := range paths {
			if path != "" && pathutil.PathWithin(path, configPath) {
				return true
			}
		}
		return false
	}, reapDoltLeakPIDs)
}

type doltLeakGuardedTestingM struct {
	m        *testing.M
	tempRoot string
	// sourceRoot is the package directory the test binary runs in. A managed
	// dolt provider handed a city root of "" or "." resolves it to this
	// directory, so the server and its data_dir land in the source checkout
	// at cmd/gc/.gc and cmd/gc/.beads instead of under tempRoot. Those
	// escaped the guard entirely while it watched tempRoot alone, and
	// .gitignore hides the on-disk half, so they accumulated unnoticed.
	//
	// Watching this root is safe for the reaping paths as well as detection:
	// no real city lives inside the checkout, so a dolt sql-server rooted
	// here is a test leak by construction. That is why the fix names a
	// second root rather than watching every dolt process on the machine —
	// an unscoped guard would reap the developer's own city servers, which
	// legitimately start and stop during a long test run.
	sourceRoot   string
	checkoutRoot string
	cleanupPaths []string
}

func newDoltLeakGuardedTestingM(m *testing.M, tempRoot string, cleanupPaths ...string) *doltLeakGuardedTestingM {
	// A failure here must not be fatal: os.Getwd only fails in exotic cases
	// (an unlinked cwd), and losing the second root degrades the guard to its
	// previous tempRoot-only behavior rather than breaking every test run.
	sourceRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: resolving source root: %v\n", err) //nolint:errcheck
		sourceRoot = ""
	}
	return &doltLeakGuardedTestingM{
		m:            m,
		tempRoot:     tempRoot,
		sourceRoot:   sourceRoot,
		checkoutRoot: checkoutRootForTestSource(sourceRoot),
		cleanupPaths: cleanupPaths,
	}
}

// checkoutRootForTestSource returns the nearest repository root above cmd/gc's
// package directory. The go.mod check is a fail-closed safety guard: if the
// test binary ever runs from an unexpected directory, the process reaper must
// narrow its scope rather than treating an arbitrary ancestor as test-owned.
func checkoutRootForTestSource(sourceRoot string) string {
	if sourceRoot == "" {
		return ""
	}
	for root := filepath.Clean(sourceRoot); ; root = filepath.Dir(root) {
		if info, err := os.Stat(filepath.Join(root, "go.mod")); err == nil && !info.IsDir() {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			return ""
		}
	}
}

// leakRoots are the config-path roots the guard treats as test-owned. A dolt
// sql-server whose --config lies under any of them is this run's to detect and
// reap.
func (g *doltLeakGuardedTestingM) leakRoots() []string {
	return []string{g.tempRoot, g.sourceRoot, g.checkoutRoot}
}

// nonEmptyLeakRoots is leakRoots minus unresolved entries, for diagnostics that
// name the roots a leak was found under.
func (g *doltLeakGuardedTestingM) nonEmptyLeakRoots() []string {
	roots := make([]string, 0, 3)
	for _, root := range g.leakRoots() {
		if root != "" {
			roots = append(roots, root)
		}
	}
	return roots
}

func (g *doltLeakGuardedTestingM) Run() int {
	return g.runWith(g.m.Run, discoverDoltProcesses, g.sweepStaleCmdGCTestDoltProcesses, sweepOrphanDoltStoreDirs, reapManagedDoltTestProcesses, reapDoltLeakProcesses, doltLeakGuardGraceInitialInterval, doltLeakGuardGraceMaxElapsedTime)
}

func (g *doltLeakGuardedTestingM) runWith(
	runTests func() int,
	enumerate func() ([]DoltProcInfo, error),
	sweepStale func(string) bool,
	sweepOrphanDirs func(),
	reapRegistered func(),
	reapLeaks func([]DoltProcInfo),
	graceInitialInterval, graceMaxElapsedTime time.Duration,
) int {
	_ = sweepStale("startup")
	sweepOrphanDirs()
	stopSignalHandler := g.installSignalHandler()
	defer stopSignalHandler()

	initial, initialErr := snapshotDoltProcessesForConfigRoots(enumerate, g.leakRoots())
	if initialErr != nil {
		fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: initial scan failed: %v\n", initialErr) //nolint:errcheck
	}

	code := runTests()

	guardFailed := initialErr != nil
	if initialErr == nil {
		leaked, finalErr := g.waitForFinalScanToClear(enumerate, initial, graceInitialInterval, graceMaxElapsedTime)
		if finalErr != nil {
			fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: final scan failed: %v\n", finalErr) //nolint:errcheck
			guardFailed = true
		} else if len(leaked) > 0 {
			fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: leaked %d dolt sql-server process(es) under %s\n", len(leaked), strings.Join(g.nonEmptyLeakRoots(), ", ")) //nolint:errcheck
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

// waitForFinalScanToClear polls the final process-table scan, diffing
// against initial each time, until it shows no candidates or
// maxElapsedTime is exhausted. It tolerates the ordinary tail latency of a
// dolt sql-server's graceful shutdown (already signaled to stop, not yet
// reaped from the process table) without weakening detection of a process
// still present when the grace window closes: the last observed diff is
// what gets reported and reaped in that case, identical to a single
// immediate scan finding the same result.
func (g *doltLeakGuardedTestingM) waitForFinalScanToClear(
	enumerate func() ([]DoltProcInfo, error),
	initial map[int]DoltProcInfo,
	initialInterval, maxElapsedTime time.Duration,
) ([]DoltProcInfo, error) {
	var leaked []DoltProcInfo
	var finalScanErr error

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = initialInterval
	bo.MaxElapsedTime = maxElapsedTime

	// backoff.Retry (v4.3.0) already unwraps a *backoff.PermanentError it
	// returns internally and hands back the inner error directly, so
	// type-asserting on Retry's own return value can never see a
	// *backoff.PermanentError. Capture the scan error directly via this
	// closure variable instead, mirroring leaked above: set at the same
	// point backoff.Permanent(finalErr) is returned, read after Retry
	// returns.
	_ = backoff.Retry(func() error {
		final, err := snapshotDoltProcessesForConfigRoots(enumerate, g.leakRoots())
		if err != nil {
			finalScanErr = err
			return backoff.Permanent(err)
		}
		leaked = diffDoltProcessSnapshots(initial, final)
		if len(leaked) == 0 {
			return nil
		}
		return fmt.Errorf("%d dolt sql-server process(es) still present", len(leaked))
	}, bo)
	if finalScanErr != nil {
		return nil, finalScanErr
	}
	// Retries exhausted with a non-empty diff on the last poll falls
	// through here too: leaked already holds that observation, and the
	// sentinel error returned by Retry in that case carries no
	// information beyond "still non-empty".
	return leaked, nil
}

func (g *doltLeakGuardedTestingM) installSignalHandler() func() {
	signals := make(chan os.Signal, 2)
	done := make(chan struct{})
	// SIGQUIT is what `go test -timeout` raises on a hung shard (see
	// dolttest.Guard, which handles it for the same reason): without it the
	// binary dies before reaping and every managed dolt server leaks.
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
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
	procs, err := snapshotDoltProcessesForConfigRoots(discoverDoltProcesses, g.leakRoots())
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
	fmt.Fprintf(os.Stderr, "cmd/gc test dolt leak guard: %s sweep reaping %d dolt sql-server process(es) under %s\n", label, len(leaked), strings.Join(g.nonEmptyLeakRoots(), ", ")) //nolint:errcheck
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
		return isAbandonedGoTempDirConfigPath(configPath, tempParent)
	}
	return !pidAliveFn(ownerPID)
}

// isAbandonedGoTempDirConfigPath classifies configs under a Go t.TempDir()
// root (Test<Name><rand>/...) that carry no gct<pid>-/gcx<pid>- owner
// component. Those appear when the config is removed from disk while the
// dolt server lives on: TestMain's temp-root cleanup on an uncleanly-ended
// run (timeout or panic), or an external temp wipe (harness/CI/
// systemd-tmpfiles) after a SIGKILL that TestMain cannot trap. The missing
// config file is the stale signal — a concurrent live run keeps its config
// on disk until its servers stop.
func isAbandonedGoTempDirConfigPath(configPath, tempParent string) bool {
	if _, ok := activeTestRootUnder(filepath.Clean(configPath), filepath.Clean(tempParent), []string{"Test"}); !ok {
		return false
	}
	// Bounded like statConfigPathState: configPath comes from an arbitrary
	// host process's argv and may sit on a hung NFS/FUSE mount. A timeout
	// returns ctx.Err() rather than ErrNotExist, so a stuck mount degrades
	// to "not stale" (protect) instead of wedging the startup sweep.
	_, err := statWithTimeout(configPath)
	return errors.Is(err, os.ErrNotExist)
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

// snapshotDoltProcessesForConfigRoots returns the dolt sql-servers whose
// --config path lies under any of roots. Empty roots are ignored, so a caller
// that could not resolve one still gets the others rather than matching
// everything.
func snapshotDoltProcessesForConfigRoots(enumerate func() ([]DoltProcInfo, error), roots []string) (map[int]DoltProcInfo, error) {
	procs, err := enumerate()
	if err != nil {
		return nil, err
	}
	out := make(map[int]DoltProcInfo, len(procs))
	for _, p := range procs {
		configPath := extractConfigPath(p.Argv)
		for _, root := range roots {
			if root == "" || !pathutil.PathWithin(root, configPath) {
				continue
			}
			out[p.PID] = p
			break
		}
	}
	return out, nil
}

func snapshotDoltProcessesForConfigRoot(enumerate func() ([]DoltProcInfo, error), root string) (map[int]DoltProcInfo, error) {
	return snapshotDoltProcessesForConfigRoots(enumerate, []string{root})
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
	pids := make([]int, 0, len(leaked))
	for _, proc := range leaked {
		pids = append(pids, proc.PID)
	}
	_ = reapDoltLeakPIDs(pids)
}

// doltLeakReapPollInterval and doltLeakReapDeadline bound how long
// reapDoltLeakPIDs waits for a signaled pid to actually leave the
// process table after SIGKILL, instead of returning as soon as the signal
// call itself succeeds. A signal delivered successfully only means the
// kernel accepted it, not that the process has exited -- callers racing a
// TempDir RemoveAll (or any cleanup) right behind this reap need the pid to
// actually be gone (ga-62mu45).
const (
	doltLeakReapPollInterval = 20 * time.Millisecond
	doltLeakReapDeadline     = 5 * time.Second
)

// reapDoltLeakPIDs is the real-process reaper: it signals the real process
// table and probes the real process table. Signaling and probing are two
// halves of one reaper and must agree about which process table they act
// on, so they are paired here rather than injected separately -- a no-op
// killer wired to the real prober signals nothing yet still polls the real
// table, which is exactly how ga-ay6x1 went red.
func reapDoltLeakPIDs(pids []int) []error {
	return reapDoltLeakPIDsWithKillerAndWaiter(pids, killProcess, processStillAlive, doltLeakReapPollInterval, doltLeakReapDeadline)
}

// processStillAlive reports whether pid is still present in the process
// table, probing via signal 0 (delivers no actual signal; ESRCH means the
// pid is already gone). Mirrors the ESRCH handling killFn callers already
// use elsewhere in this file.
func processStillAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

// reapDoltLeakPIDsWithKillerAndWaiter is the fully injectable form of the
// reaper, used directly by unit tests for the reaper itself. Callers go
// through one of the two pairings above: reapDoltLeakPIDs (real killer +
// real prober) in production, scriptedDoltLeakReaper (fake killer +
// processNeverAlive) for fabricated pids. After signaling, it polls aliveFn
// for each pid until it reports exited or the shared deadline elapses, so
// the caller gets a confirmed exit rather than a fire-and-forget signal
// send.
func reapDoltLeakPIDsWithKillerAndWaiter(pids []int, killFn func(int, syscall.Signal) error, aliveFn func(int) bool, pollInterval, deadline time.Duration) []error {
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

	// Shared deadline across all pids so the total wait stays bounded by
	// deadline, not deadline multiplied by len(pids). Polling goes through
	// backoff.Retry (mirroring waitForFinalScanToClear above) instead of a
	// bare time.Sleep loop, so the interval sleep lives inside the already-
	// imported backoff library rather than in this file's own source.
	deadlineAt := time.Now().Add(deadline)
	for _, pid := range pids {
		remaining := time.Until(deadlineAt)
		if remaining <= 0 {
			if aliveFn(pid) {
				errs = append(errs, fmt.Errorf("pid %d still alive after SIGKILL and %s wait (leak-guard reap timed out)", pid, deadline))
			}
			continue
		}

		bo := backoff.NewExponentialBackOff()
		bo.InitialInterval = pollInterval
		bo.MaxElapsedTime = remaining

		// The operation's own error carries no information beyond "still
		// alive" (mirrors waitForFinalScanToClear's finalScanErr pattern
		// above) -- the informative error is reconstructed below from
		// pid+deadline once Retry gives up.
		if err := backoff.Retry(func() error {
			if aliveFn(pid) {
				return fmt.Errorf("pid %d still alive", pid)
			}
			return nil
		}, bo); err != nil {
			errs = append(errs, fmt.Errorf("pid %d still alive after SIGKILL and %s wait (leak-guard reap timed out)", pid, deadline))
		}
	}
	return errs
}

func ignoreProcessSignal(int, syscall.Signal) error {
	return nil
}

// processNeverAlive is the scripted-pid counterpart to processStillAlive: it
// reports every pid as already exited without consulting the real process
// table. Any guard driven by a fake killer must probe with this, because a
// no-op killer never signals anything -- probing the real table for a
// fabricated pid that happens to name a live process on the host makes the
// reap spin its whole deadline and emit a spurious cleanup failure
// (ga-ay6x1).
func processNeverAlive(int) bool {
	return false
}

// scriptedDoltLeakReapPollInterval and scriptedDoltLeakReapDeadline bound
// reaps over fabricated pids. Nothing real is ever signaled on that path, so
// the bounds only need to be long enough to prove the poll happened; the
// production bounds above stay untouched.
const (
	scriptedDoltLeakReapPollInterval = 5 * time.Millisecond
	scriptedDoltLeakReapDeadline     = 200 * time.Millisecond
)

// scriptedDoltLeakReaper builds a reaper over fabricated pids, pairing the
// given fake killer with processNeverAlive so the reap never touches the
// real process table.
func scriptedDoltLeakReaper(killFn func(int, syscall.Signal) error) func([]int) []error {
	return func(pids []int) []error {
		return reapDoltLeakPIDsWithKillerAndWaiter(pids, killFn, processNeverAlive, scriptedDoltLeakReapPollInterval, scriptedDoltLeakReapDeadline)
	}
}

// TestReapDoltLeakPIDsWithKillerAndWaiter_WaitsForConfirmedExit proves the
// reaper polls for actual exit instead of returning the instant SIGKILL is
// sent. A killFn call succeeding only means the kernel accepted the signal,
// not that the process has left the process table -- the caller's
// t.Cleanup (and any TempDir RemoveAll racing right behind it) needs the
// pid to actually be gone (ga-62mu45).
func TestReapDoltLeakPIDsWithKillerAndWaiter_WaitsForConfirmedExit(t *testing.T) {
	const pid = 4242
	var aliveCalls int
	aliveFn := func(gotPID int) bool {
		if gotPID != pid {
			t.Fatalf("aliveFn called with unexpected pid %d, want %d", gotPID, pid)
		}
		aliveCalls++
		// Reports alive for the first two polls, then exited -- simulates a
		// process that takes a couple of poll ticks to actually leave the
		// process table after SIGKILL is delivered.
		return aliveCalls <= 2
	}

	errs := reapDoltLeakPIDsWithKillerAndWaiter([]int{pid}, ignoreProcessSignal, aliveFn, 5*time.Millisecond, 200*time.Millisecond)

	if len(errs) != 0 {
		t.Fatalf("reapDoltLeakPIDsWithKillerAndWaiter returned unexpected errors: %v", errs)
	}
	if aliveCalls < 2 {
		t.Fatalf("aliveFn called only %d time(s); reaper must poll for confirmed exit, not return after a single check", aliveCalls)
	}
}

// TestReapDoltLeakPIDsWithKillerAndWaiter_TimesOutWithClearPIDError proves
// a pid that never confirms exit produces a clear, pid-naming error within
// the bounded deadline -- rather than either hanging forever or silently
// returning success for a process that is still alive.
func TestReapDoltLeakPIDsWithKillerAndWaiter_TimesOutWithClearPIDError(t *testing.T) {
	const pid = 4343
	aliveFn := func(int) bool { return true } // never exits

	const deadline = 40 * time.Millisecond
	start := time.Now()
	errs := reapDoltLeakPIDsWithKillerAndWaiter([]int{pid}, ignoreProcessSignal, aliveFn, 5*time.Millisecond, deadline)
	elapsed := time.Since(start)

	if len(errs) == 0 {
		t.Fatal("reapDoltLeakPIDsWithKillerAndWaiter returned no error for a pid that never confirmed exit")
	}
	found := false
	for _, err := range errs {
		if strings.Contains(err.Error(), fmt.Sprintf("%d", pid)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an error naming pid %d, got: %v", pid, errs)
	}
	// Generous upper bound so host contention can't flake this: the reaper
	// must not hang well past its own configured deadline.
	if elapsed > deadline+2*time.Second {
		t.Fatalf("reapDoltLeakPIDsWithKillerAndWaiter took %s, well beyond its %s deadline -- it must not hang past the bound", elapsed, deadline)
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
	requireNoLeakedDoltAfterWithFilterAndReaper(t, enumerate, func(configPath string) bool {
		return isTestConfigPath(configPath, homeDir, tempDir)
	}, scriptedDoltLeakReaper(ignoreProcessSignal))
}

func requireNoLeakedDoltAfterWithFilter(t testReporter, enumerate func() ([]DoltProcInfo, error), includeConfigPath func(string) bool) {
	requireNoLeakedDoltAfterWithFilterAndReaper(t, enumerate, includeConfigPath, scriptedDoltLeakReaper(ignoreProcessSignal))
}

// requireNoLeakedDoltAfterWithFilterAndReaper is the injectable form of the
// leak guard. It takes the whole reaper rather than just its killer: the
// killer and the liveness prober must agree about which process table they
// act on, and injecting only the killer let a scripted guard signal nothing
// while still polling the real table for its fabricated pids (ga-ay6x1).
// Production passes reapDoltLeakPIDs; scripted-pid tests pass
// scriptedDoltLeakReaper.
func requireNoLeakedDoltAfterWithFilterAndReaper(t testReporter, enumerate func() ([]DoltProcInfo, error), includeConfigPath func(string) bool, reap func([]int) []error) {
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
		for _, err := range reap(pids) {
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
