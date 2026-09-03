package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testReporter is the subset of *testing.T methods that
// requireNoLeakedDoltAfterWith and snapshotDoltProcessPIDsWith touch.
// Splitting these out lets unit tests pass a recording stand-in
// (recordingTB) instead of a real *testing.T, so the helper's reports
// can be inspected without failing the outer test.
type testReporter interface {
	Helper()
	Cleanup(fn func())
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// recordingTB is a testReporter that records Errorf/Fatalf calls and
// queues Cleanup callbacks for explicit invocation. It does NOT call
// runtime.Goexit on Fatalf — the call is captured so the test can
// assert on the message instead of terminating.
type recordingTB struct {
	cleanups []func()
	errors   []string
	fatals   []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Cleanup(fn func()) {
	r.cleanups = append(r.cleanups, fn)
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recordingTB) failed() bool {
	return len(r.errors) > 0 || len(r.fatals) > 0
}

// runCleanups invokes registered cleanups in LIFO order to mirror the
// ordering that *testing.T.Cleanup guarantees.
func (r *recordingTB) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

// linuxPIDMaxLimit is PID_MAX_LIMIT on 64-bit Linux (1<<22), the hard
// ceiling /proc/sys/kernel/pid_max can be raised to. Used only as the
// fallback when that file is unreadable.
const linuxPIDMaxLimit = 4194304

// unallocatablePIDs returns n ascending pids the kernel cannot have assigned
// to any process on this host, because they sit above /proc/sys/kernel/pid_max.
//
// The leak guard's liveness probe is injected in these tests, so a colliding
// pid no longer changes any outcome; picking unallocatable pids is defense in
// depth against a future call site that pairs a fake killer with the real
// prober again. That pairing is what made ga-ay6x1 deterministically red: the
// fabricated pids 50001/50002 named two live dolt children on this host, so
// the reap polled the real process table until its 5s deadline and appended a
// cleanup-failure Errorf per pid.
func unallocatablePIDs(t *testing.T, n int) []int {
	t.Helper()
	ceiling := linuxPIDMaxLimit
	if raw, err := os.ReadFile("/proc/sys/kernel/pid_max"); err == nil {
		if parsed, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && parsed > 0 {
			ceiling = parsed
		}
	}
	pids := make([]int, n)
	for i := range pids {
		pids[i] = ceiling + 1 + i
	}
	return pids
}

func doltTestProc(pid int, args ...string) DoltProcInfo {
	configPath := filepath.Join(
		"/tmp",
		"TestDoltLeakHelper",
		fmt.Sprintf("%d", pid),
		".gc",
		"runtime",
		"dolt.yaml",
	)
	argv := append([]string{"dolt", "sql-server", "--config=" + configPath}, args...)
	return DoltProcInfo{PID: pid, Argv: argv}
}

// scriptedDoltEnumerator returns a stub func() ([]DoltProcInfo, error)
// that yields successive snapshots from the given slice on each call.
// After all snapshots are exhausted further calls fail the outer test
// — a wrong call count is a test bug, not a behavior we want to assert.
func scriptedDoltEnumerator(t *testing.T, snapshots ...[]DoltProcInfo) func() ([]DoltProcInfo, error) {
	t.Helper()
	var idx int
	return func() ([]DoltProcInfo, error) {
		if idx >= len(snapshots) {
			t.Fatalf("scriptedDoltEnumerator: enumerator called %d times, only %d snapshots scripted", idx+1, len(snapshots))
			return nil, nil
		}
		out := snapshots[idx]
		idx++
		return out, nil
	}
}

// TestRequireNoLeakedDoltAfter_NoChangeNoError pins that when the
// pre-registration and cleanup snapshots are identical (both empty),
// no error is reported. This is the dominant happy path — most tests
// don't spawn any dolt and shouldn't see false-positive leak reports.
func TestRequireNoLeakedDoltAfter_NoChangeNoError(t *testing.T) {
	enumerate := scriptedDoltEnumerator(t, nil, nil)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if inner.failed() {
		t.Fatalf("unexpected reports: errors=%v fatals=%v", inner.errors, inner.fatals)
	}
}

// TestRequireNoLeakedDoltAfter_NewPIDReportedWithArgv pins the core
// behavior: a PID present at cleanup but absent at registration is
// reported via Errorf, and the message embeds both the PID and the
// argv string so operators can trace the spawn site from the test
// log. This is the regression that originally motivated the helper
// (3.3 GiB OOM from un-reaped dolt children — see ga-de27g).
func TestRequireNoLeakedDoltAfter_NewPIDReportedWithArgv(t *testing.T) {
	leakedPID := unallocatablePIDs(t, 1)[0]
	leaked := DoltProcInfo{
		PID:  leakedPID,
		Argv: []string{"dolt", "sql-server", "--config=/tmp/Test123/.gc/runtime/dolt.yaml"},
	}
	enumerate := scriptedDoltEnumerator(t,
		nil,                    // initial: no procs
		[]DoltProcInfo{leaked}, // cleanup: one new proc
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if !inner.failed() {
		t.Fatalf("expected leak Errorf; nothing recorded")
	}
	if len(inner.errors) != 1 {
		t.Fatalf("expected exactly 1 Errorf, got %d: %v", len(inner.errors), inner.errors)
	}
	msg := inner.errors[0]
	if !strings.Contains(msg, strconv.Itoa(leakedPID)) {
		t.Errorf("error message missing leaked PID %d; got %q", leakedPID, msg)
	}
	for _, arg := range leaked.Argv {
		if !strings.Contains(msg, arg) {
			t.Errorf("error message missing argv token %q; got %q", arg, msg)
		}
	}
}

// TestRequireNoLeakedDoltAfter_PreExistingPIDsNotReported pins the
// diff math when pre-existing dolt processes are running on the host:
// PIDs present at registration MUST NOT be reported as leaks at
// cleanup, even though they appear in the cleanup snapshot. Without
// this subtraction the helper would false-positive on every host
// running an unrelated dolt server.
func TestRequireNoLeakedDoltAfter_PreExistingPIDsNotReported(t *testing.T) {
	preexisting := doltTestProc(1000)
	enumerate := scriptedDoltEnumerator(t,
		[]DoltProcInfo{preexisting}, // initial
		[]DoltProcInfo{preexisting}, // cleanup: same set, no leak
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if inner.failed() {
		t.Fatalf("pre-existing PID reported as leaked: errors=%v fatals=%v",
			inner.errors, inner.fatals)
	}
}

// TestRequireNoLeakedDoltAfter_OnlyNewPIDsInDiff pins that when the
// cleanup snapshot contains BOTH a pre-existing PID and a new PID,
// only the new one appears in the error message. This proves the diff
// is computed (cleanup minus initial), not re-reported in full.
func TestRequireNoLeakedDoltAfter_OnlyNewPIDsInDiff(t *testing.T) {
	leakedPID := unallocatablePIDs(t, 1)[0]
	preexisting := doltTestProc(1000)
	leaked := doltTestProc(leakedPID, "--leaked")
	enumerate := scriptedDoltEnumerator(t,
		[]DoltProcInfo{preexisting},
		[]DoltProcInfo{preexisting, leaked},
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if !inner.failed() {
		t.Fatalf("expected leak Errorf for PID %d; nothing recorded", leakedPID)
	}
	msg := strings.Join(inner.errors, "\n")
	if !strings.Contains(msg, strconv.Itoa(leakedPID)) {
		t.Errorf("error missing leaked PID %d; got %q", leakedPID, msg)
	}
	if strings.Contains(msg, "pid=1000") {
		t.Errorf("error must not include pre-existing PID 1000; got %q", msg)
	}
}

// TestRequireNoLeakedDoltAfter_MultipleLeaksReportedSorted pins two
// guarantees needed for stable test logs across runs:
//
//  1. Multiple leaked PIDs are aggregated into a single Errorf call
//     (operators get one report per test, not N).
//  2. PIDs are listed in ascending numerical order regardless of how
//     the enumerator returns them.
func TestRequireNoLeakedDoltAfter_MultipleLeaksReportedSorted(t *testing.T) {
	pids := unallocatablePIDs(t, 2)
	lo, hi := pids[0], pids[1]
	leakedHi := doltTestProc(hi, "--port=3308")
	leakedLo := doltTestProc(lo, "--port=3307")
	enumerate := scriptedDoltEnumerator(t,
		nil,
		// Order in slice deliberately unsorted to verify the helper sorts.
		[]DoltProcInfo{leakedHi, leakedLo},
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if !inner.failed() {
		t.Fatalf("expected leak Errorf for two leaked PIDs; nothing recorded")
	}
	if len(inner.errors) != 1 {
		t.Fatalf("multiple leaks must be aggregated into one Errorf, got %d: %v",
			len(inner.errors), inner.errors)
	}
	msg := inner.errors[0]
	iLo := strings.Index(msg, strconv.Itoa(lo))
	iHi := strings.Index(msg, strconv.Itoa(hi))
	if iLo == -1 {
		t.Errorf("error missing PID %d; got %q", lo, msg)
	}
	if iHi == -1 {
		t.Errorf("error missing PID %d; got %q", hi, msg)
	}
	if iLo != -1 && iHi != -1 && iLo > iHi {
		t.Errorf("PIDs not in ascending order; got %q", msg)
	}
}

// TestRequireNoLeakedDoltAfter_NewNonTestPIDIgnored pins that the leak helper
// ignores unrelated dolt servers whose config path is outside the test-temp
// allowlist. City or pack runtimes can start their own managed dolt process
// while this test package is running; those are not leaks from the test under
// inspection.
func TestRequireNoLeakedDoltAfter_NewNonTestPIDIgnored(t *testing.T) {
	unrelated := DoltProcInfo{
		PID: 2041535,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			"/data/projects/maintainer-city/.gc/runtime/packs/dolt/dolt-config.yaml",
		},
	}
	enumerate := scriptedDoltEnumerator(t,
		nil,
		[]DoltProcInfo{unrelated},
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWith(inner, enumerate)
	inner.runCleanups()
	if inner.failed() {
		t.Fatalf("unrelated dolt server reported as leaked: errors=%v fatals=%v",
			inner.errors, inner.fatals)
	}
}

func TestRequireNoLeakedDoltAfterWithFilterIgnoresUnownedTempPID(t *testing.T) {
	pids := unallocatablePIDs(t, 2)
	ownedPID, unownedPID := pids[0], pids[1]
	ownedRoot := filepath.Join("/tmp", "TestDoltLeakHelper", "owned-city")
	unownedRoot := filepath.Join("/tmp", "TestDoltLeakHelper", "other-city")
	owned := DoltProcInfo{
		PID: ownedPID,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(ownedRoot, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	unowned := DoltProcInfo{
		PID: unownedPID,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(unownedRoot, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	enumerate := scriptedDoltEnumerator(t,
		nil,
		[]DoltProcInfo{owned, unowned},
	)
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWithFilter(inner, enumerate, func(configPath string) bool {
		return samePath(configPath, ownedRoot) || strings.HasPrefix(configPath, ownedRoot+string(filepath.Separator))
	})
	inner.runCleanups()

	if !inner.failed() {
		t.Fatalf("expected scoped leak Errorf for owned PID; nothing recorded")
	}
	msg := strings.Join(inner.errors, "\n")
	if !strings.Contains(msg, strconv.Itoa(ownedPID)) {
		t.Fatalf("error missing owned leaked PID %d; got %q", ownedPID, msg)
	}
	if strings.Contains(msg, strconv.Itoa(unownedPID)) {
		t.Fatalf("error included unowned leaked PID %d; got %q", unownedPID, msg)
	}
}

func TestRequireNoLeakedDoltAfterWithFilterReportsAndKillsOwnedPID(t *testing.T) {
	pids := unallocatablePIDs(t, 2)
	ownedPID, unownedPID := pids[0], pids[1]
	ownedRoot := filepath.Join("/tmp", "TestDoltLeakHelper", "owned-city")
	owned := DoltProcInfo{
		PID: ownedPID,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(ownedRoot, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	unowned := DoltProcInfo{
		PID: unownedPID,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join("/tmp", "TestDoltLeakHelper", "other-city", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	enumerate := scriptedDoltEnumerator(t,
		nil,
		[]DoltProcInfo{owned, unowned},
	)
	type killCall struct {
		pid int
		sig syscall.Signal
	}
	var killed []killCall
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWithFilterAndReaper(inner, enumerate, func(configPath string) bool {
		return samePath(configPath, ownedRoot) || strings.HasPrefix(configPath, ownedRoot+string(filepath.Separator))
	}, scriptedDoltLeakReaper(func(pid int, sig syscall.Signal) error {
		killed = append(killed, killCall{pid: pid, sig: sig})
		return nil
	}))
	inner.runCleanups()

	if !inner.failed() {
		t.Fatalf("expected leak Errorf for owned PID; nothing recorded")
	}
	wantKilled := []killCall{
		{pid: ownedPID, sig: syscall.SIGTERM},
		{pid: ownedPID, sig: syscall.SIGKILL},
	}
	if fmt.Sprint(killed) != fmt.Sprint(wantKilled) {
		t.Fatalf("killed = %v, want %v", killed, wantKilled)
	}
	msg := strings.Join(inner.errors, "\n")
	if !strings.Contains(msg, strconv.Itoa(ownedPID)) {
		t.Fatalf("error missing owned leaked PID %d; got %q", ownedPID, msg)
	}
	if strings.Contains(msg, strconv.Itoa(unownedPID)) {
		t.Fatalf("error included unowned leaked PID %d; got %q", unownedPID, msg)
	}
}

func TestRequireNoLeakedDoltAfterWithFilterReportsKillErrors(t *testing.T) {
	ownedPID := unallocatablePIDs(t, 1)[0]
	ownedRoot := filepath.Join("/tmp", "TestDoltLeakHelper", "owned-city")
	owned := DoltProcInfo{
		PID: ownedPID,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(ownedRoot, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	enumerate := scriptedDoltEnumerator(t, nil, []DoltProcInfo{owned})
	inner := &recordingTB{}
	requireNoLeakedDoltAfterWithFilterAndReaper(inner, enumerate, func(configPath string) bool {
		return samePath(configPath, ownedRoot) || strings.HasPrefix(configPath, ownedRoot+string(filepath.Separator))
	}, scriptedDoltLeakReaper(func(_ int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			return errors.New("synthetic kill failure")
		}
		return nil
	}))
	inner.runCleanups()

	msg := strings.Join(inner.errors, "\n")
	if !strings.Contains(msg, "test leaked 1 dolt sql-server") {
		t.Fatalf("error missing leak report; got %q", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("SIGTERM pid %d", ownedPID)) || !strings.Contains(msg, "synthetic kill failure") {
		t.Fatalf("error missing kill failure; got %q", msg)
	}
}

// TestRequireNoLeakedDoltAfterReportsStillAlivePIDAsCleanupFailure is the
// control for the injected-liveness fix. Injecting processNeverAlive
// everywhere would be worthless if it also silenced genuine reap failures,
// so this case drives the guard with a probe that keeps reporting the pid
// alive after SIGKILL and pins both halves of the expected report: the leak
// itself still aggregates into exactly one Errorf, and the unreaped pid
// still produces its own separate cleanup-failure Errorf naming it.
func TestRequireNoLeakedDoltAfterReportsStillAlivePIDAsCleanupFailure(t *testing.T) {
	ownedPID := unallocatablePIDs(t, 1)[0]
	ownedRoot := filepath.Join("/tmp", "TestDoltLeakHelper", "owned-city")
	owned := DoltProcInfo{
		PID: ownedPID,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(ownedRoot, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	enumerate := scriptedDoltEnumerator(t, nil, []DoltProcInfo{owned})
	inner := &recordingTB{}
	neverDies := func(int) bool { return true }
	requireNoLeakedDoltAfterWithFilterAndReaper(inner, enumerate, func(configPath string) bool {
		return samePath(configPath, ownedRoot) || strings.HasPrefix(configPath, ownedRoot+string(filepath.Separator))
	}, func(pids []int) []error {
		return reapDoltLeakPIDsWithKillerAndWaiter(pids, ignoreProcessSignal, neverDies, time.Millisecond, 30*time.Millisecond)
	})
	inner.runCleanups()

	if len(inner.errors) != 2 {
		t.Fatalf("expected one aggregated leak Errorf plus one cleanup-failure Errorf, got %d: %v",
			len(inner.errors), inner.errors)
	}
	if !strings.Contains(inner.errors[0], "test leaked 1 dolt sql-server") {
		t.Fatalf("first Errorf must be the aggregated leak report; got %q", inner.errors[0])
	}
	if !strings.Contains(inner.errors[1], "test leaked dolt cleanup failed") ||
		!strings.Contains(inner.errors[1], strconv.Itoa(ownedPID)) {
		t.Fatalf("second Errorf must name the unreaped pid %d as a cleanup failure; got %q", ownedPID, inner.errors[1])
	}
}

func TestIsStaleCmdGCTestConfigPathSkipsActiveRoot(t *testing.T) {
	activeRoot := filepath.Join("/tmp", "gctest-active")
	activeConfig := filepath.Join(activeRoot, "TestCase", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	if isStaleCmdGCTestConfigPath(activeConfig, []string{activeRoot}, "/tmp") {
		t.Fatalf("active config path %q classified as stale", activeConfig)
	}

	staleRoot := filepath.Join("/tmp", fmt.Sprintf("%s%d-stale", testCmdGCTempRootPrefix, nonLivePID(t)))
	staleConfig := filepath.Join(staleRoot, "TestCase", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	if !isStaleCmdGCTestConfigPath(staleConfig, []string{activeRoot}, "/tmp") {
		t.Fatalf("stale config path %q not classified as stale", staleConfig)
	}
}

func TestIsStaleCmdGCTestConfigPathSkipsActiveSiblingRoot(t *testing.T) {
	activeRoot := filepath.Join("/tmp", "gctest-sibling")
	activeConfig := filepath.Join(activeRoot, "TestCase", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")

	if isStaleCmdGCTestConfigPath(activeConfig, []string{activeRoot}, "/tmp") {
		t.Fatalf("active sibling config path %q classified as stale", activeConfig)
	}
}

func TestIsStaleCmdGCTestConfigPathSkipsLivePeerOwnerPIDRoot(t *testing.T) {
	peerRoot := filepath.Join("/tmp", fmt.Sprintf("%s%d-peer", testCmdGCTempRootPrefix, os.Getpid()))
	peerConfig := filepath.Join(peerRoot, "TestCase", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")

	if isStaleCmdGCTestConfigPath(peerConfig, nil, "/tmp") {
		t.Fatalf("live peer config path %q classified as stale", peerConfig)
	}
}

func TestIsStaleCmdGCTestConfigPathUsesCurrentGCTOwnerPID(t *testing.T) {
	ownerPID := 12345
	root := filepath.Join("/tmp", fmt.Sprintf("%s%d-current", testCmdGCTempRootPrefix, ownerPID))
	configPath := filepath.Join(root, "TestCase", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")

	if isStaleCmdGCTestConfigPathWithPIDCheck(configPath, nil, "/tmp", func(pid int) bool {
		if pid != ownerPID {
			t.Fatalf("pidAlive called with pid %d, want %d", pid, ownerPID)
		}
		return true
	}) {
		t.Fatalf("live current gct owner config path %q classified as stale", configPath)
	}
	if !isStaleCmdGCTestConfigPathWithPIDCheck(configPath, nil, "/tmp", func(pid int) bool {
		if pid != ownerPID {
			t.Fatalf("pidAlive called with pid %d, want %d", pid, ownerPID)
		}
		return false
	}) {
		t.Fatalf("dead current gct owner config path %q not classified as stale", configPath)
	}
}

func TestIsStaleCmdGCTestConfigPathUsesShardOwnerPID(t *testing.T) {
	ownerPID := 12345
	root := filepath.Join("/tmp", fmt.Sprintf("%s%d-current", testCmdGCShardTempRootPrefix, ownerPID))
	configPath := filepath.Join(root, "TestCase", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")

	if isStaleCmdGCTestConfigPathWithPIDCheck(configPath, nil, "/tmp", func(pid int) bool {
		if pid != ownerPID {
			t.Fatalf("pidAlive called with pid %d, want %d", pid, ownerPID)
		}
		return true
	}) {
		t.Fatalf("live shard cmd/gc owner config path %q classified as stale", configPath)
	}
	if !isStaleCmdGCTestConfigPathWithPIDCheck(configPath, nil, "/tmp", func(pid int) bool {
		if pid != ownerPID {
			t.Fatalf("pidAlive called with pid %d, want %d", pid, ownerPID)
		}
		return false
	}) {
		t.Fatalf("dead shard cmd/gc owner config path %q not classified as stale", configPath)
	}
}

func TestIsTestConfigPathRetainsLegacyGCTestAllowlistOnly(t *testing.T) {
	legacyConfig := filepath.Join("/tmp", "gctest-legacy", "TestCase", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	currentConfig := filepath.Join("/tmp", fmt.Sprintf("%s%d-current", testCmdGCTempRootPrefix, os.Getpid()), "TestCase", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")

	if !isTestConfigPath(legacyConfig, "/home/u", "/tmp") {
		t.Fatalf("legacy gctest config path %q should stay in the test allowlist", legacyConfig)
	}
	if isTestConfigPath(currentConfig, "/home/u", "/tmp") {
		t.Fatalf("current gct config path %q must use owner-PID stale-root logic, not the legacy allowlist", currentConfig)
	}
}

func TestIsStaleCmdGCTestConfigPathSkipsLegacyUnownedRoot(t *testing.T) {
	legacyConfig := filepath.Join("/tmp", "gctest-legacy", "TestCase", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")

	if isStaleCmdGCTestConfigPath(legacyConfig, nil, "/tmp") {
		t.Fatalf("legacy unowned config path %q classified as stale", legacyConfig)
	}
}

// A dolt server whose config sits under a Go t.TempDir() root
// (Test<Name><rand>/...) with the config file gone from disk is the
// signature of a run killed before its reap ran (timeout, panic, SIGKILL):
// TestMain's cleanup removed the temp root while the server lived on
// (observed 2026-08-21: 242 such servers accumulated over five weeks and
// exhausted a host's gascity-test.slice pids quota). The gone config marks
// it stale; a config still on disk marks a live concurrent run.
func TestIsStaleCmdGCTestConfigPathReapsAbandonedGoTempDirRoot(t *testing.T) {
	tempParent := t.TempDir()

	goneConfig := filepath.Join(tempParent, "TestAbandoned1234", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	if !isStaleCmdGCTestConfigPath(goneConfig, nil, tempParent) {
		t.Fatalf("abandoned go tempdir config path %q (file gone) not classified as stale", goneConfig)
	}

	liveConfig := filepath.Join(tempParent, "TestLive1234", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	if err := os.MkdirAll(filepath.Dir(liveConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveConfig, []byte("listener:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isStaleCmdGCTestConfigPath(liveConfig, nil, tempParent) {
		t.Fatalf("live go tempdir config path %q (file on disk) classified as stale", liveConfig)
	}
}

// TestSnapshotDoltProcessPIDs_EnumeratorErrorIsFatal pins that a
// discovery error is reported via Fatalf so test runs surface
// enumeration failures directly rather than silently treating them
// as "no procs". A swallowed error here would mask real leaks.
func TestSnapshotDoltProcessPIDs_EnumeratorErrorIsFatal(t *testing.T) {
	boom := errors.New("synthetic enumeration failure")
	enumerate := func() ([]DoltProcInfo, error) {
		return nil, boom
	}
	inner := &recordingTB{}
	snapshotDoltProcessPIDsWith(inner, enumerate)
	if !inner.failed() {
		t.Fatalf("expected Fatalf when enumerator errors; nothing recorded")
	}
	if len(inner.fatals) == 0 {
		t.Fatalf("expected Fatalf, got Errorf only: %v", inner.errors)
	}
	if !strings.Contains(inner.fatals[0], boom.Error()) {
		t.Errorf("Fatalf message missing original error %q; got %q",
			boom.Error(), inner.fatals[0])
	}
}

func TestSnapshotDoltProcessesForConfigRootFiltersToPrivateTempRoot(t *testing.T) {
	root := filepath.Join("/tmp", "gc-cmd-test-root")
	owned := DoltProcInfo{
		PID: 1001,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(root, "TestOwned", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	unowned := DoltProcInfo{
		PID: 1002,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join("/tmp", "TestOther", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	got, err := snapshotDoltProcessesForConfigRoot(func() ([]DoltProcInfo, error) {
		return []DoltProcInfo{owned, unowned}, nil
	}, root)
	if err != nil {
		t.Fatalf("snapshotDoltProcessesForConfigRoot: %v", err)
	}
	if len(got) != 1 || got[1001].PID != 1001 {
		t.Fatalf("snapshot = %#v, want only owned PID 1001", got)
	}
}

func TestDiffDoltProcessSnapshotsReportsOnlyNewPIDsSorted(t *testing.T) {
	initial := map[int]DoltProcInfo{
		1000: {PID: 1000},
	}
	final := map[int]DoltProcInfo{
		1000: {PID: 1000},
		1003: {PID: 1003},
		1001: {PID: 1001},
	}

	got := diffDoltProcessSnapshots(initial, final)

	if len(got) != 2 {
		t.Fatalf("diff length = %d, want 2: %#v", len(got), got)
	}
	if got[0].PID != 1001 || got[1].PID != 1003 {
		t.Fatalf("diff PIDs = [%d %d], want [1001 1003]", got[0].PID, got[1].PID)
	}
}

func TestDoltLeakGuardedTestingMFinalSnapshotRunsBeforeRegistryReap(t *testing.T) {
	tempRoot := filepath.Join(t.TempDir(), "gct12345-current")
	leaked := DoltProcInfo{
		PID: 1001,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(tempRoot, "TestCase", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	var scan int
	registeredReaped := false
	var reapedLeaks []DoltProcInfo
	enumerate := func() ([]DoltProcInfo, error) {
		scan++
		if scan == 1 {
			return nil, nil
		}
		if registeredReaped {
			return nil, nil
		}
		return []DoltProcInfo{leaked}, nil
	}
	g := newDoltLeakGuardedTestingM(nil, tempRoot)

	code := g.runWith(
		func() int { return 0 },
		enumerate,
		func(string) bool { return false },
		func() {},
		func() { registeredReaped = true },
		func(leaked []DoltProcInfo) { reapedLeaks = append(reapedLeaks, leaked...) },
		time.Millisecond,
		5*time.Millisecond,
	)

	if code != 1 {
		t.Fatalf("guard returned code %d, want 1 for leaked registered process", code)
	}
	if len(reapedLeaks) != 1 || reapedLeaks[0].PID != leaked.PID {
		t.Fatalf("reaped leaks = %#v, want only PID %d through injected reaper", reapedLeaks, leaked.PID)
	}
	if !registeredReaped {
		t.Fatal("registered process reaper was not called after leak detection")
	}
}

func TestDoltLeakGuardedTestingMRunWithSweepsOrphanDirsAtStartup(t *testing.T) {
	// ga-ntbpyb.2 acceptance criterion 2: the symptom-based directory sweep
	// runs at test-suite startup, alongside the existing stale-process
	// sweep, before the tests themselves run.
	tempRoot := filepath.Join(t.TempDir(), "gct-current")
	g := newDoltLeakGuardedTestingM(nil, tempRoot)

	var order []string
	code := g.runWith(
		func() int {
			order = append(order, "runTests")
			return 0
		},
		func() ([]DoltProcInfo, error) { return nil, nil },
		func(string) bool {
			order = append(order, "sweepStale")
			return false
		},
		func() { order = append(order, "sweepOrphanDirs") },
		func() {},
		func([]DoltProcInfo) {},
		time.Millisecond,
		5*time.Millisecond,
	)

	if code != 0 {
		t.Fatalf("guard returned code %d, want 0 for a clean run", code)
	}
	if len(order) < 3 || order[0] != "sweepStale" || order[1] != "sweepOrphanDirs" || order[2] != "runTests" {
		t.Fatalf("call order = %v, want [sweepStale sweepOrphanDirs runTests ...]", order)
	}
}

// A dolt sql-server that a test already told to stop can still be mid
// graceful-shutdown the instant runTests() returns; ga-szv0ge found this
// misclassified as a permanent leak under host contention. This simulates
// the process clearing partway through the grace window: the final scan
// reports it present on the first two polls and gone from the third poll
// onward, well within the injected grace window, so the guard must not
// fail the run or reap anything for it.
func TestDoltLeakGuardedTestingMToleratesLeakClearingWithinGraceWindow(t *testing.T) {
	tempRoot := filepath.Join(t.TempDir(), "gct-current")
	clearing := DoltProcInfo{
		PID: 2001,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config",
			filepath.Join(tempRoot, "TestCase", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
		},
	}
	var scan int
	enumerate := func() ([]DoltProcInfo, error) {
		scan++
		if scan == 1 {
			return nil, nil // initial scan: nothing running yet
		}
		if scan <= 3 {
			return []DoltProcInfo{clearing}, nil // first two final-scan polls: still mid shutdown
		}
		return nil, nil // cleared by the next poll, well within the grace window
	}
	g := newDoltLeakGuardedTestingM(nil, tempRoot)

	var reapedLeaks []DoltProcInfo
	code := g.runWith(
		func() int { return 0 },
		enumerate,
		func(string) bool { return false },
		func() {},
		func() {},
		func(leaked []DoltProcInfo) { reapedLeaks = append(reapedLeaks, leaked...) },
		time.Millisecond,
		200*time.Millisecond,
	)

	if code != 0 {
		t.Fatalf("guard returned code %d, want 0: a leak that clears within the grace window must not fail the run", code)
	}
	if len(reapedLeaks) != 0 {
		t.Fatalf("reaped leaks = %#v, want none: the process cleared on its own within the grace window", reapedLeaks)
	}
}

// waitForFinalScanToClear must surface a persistent final-scan enumeration
// failure as its own error rather than dropping it. backoff.Retry (v4.3.0)
// already unwraps the *backoff.PermanentError it returns internally and
// hands back the inner error directly, so a later
// errors.As(err, &permErr) against Retry's return value can never match --
// ga-7r2h5i found this let a real enumeration failure fall through and get
// reported as a clean run.
func TestDoltLeakGuardedTestingMWaitForFinalScanToClearReturnsEnumerationError(t *testing.T) {
	g := newDoltLeakGuardedTestingM(nil, t.TempDir())
	scanErr := errors.New("reading /proc: permission denied")
	enumerate := func() ([]DoltProcInfo, error) { return nil, scanErr }

	leaked, err := g.waitForFinalScanToClear(enumerate, map[int]DoltProcInfo{}, time.Millisecond, 5*time.Millisecond)

	if !errors.Is(err, scanErr) {
		t.Fatalf("waitForFinalScanToClear err = %v, want it to be the enumeration failure %v", err, scanErr)
	}
	if leaked != nil {
		t.Fatalf("leaked = %#v, want nil alongside a scan error", leaked)
	}
}

// Same failure, exercised through runWith: a final-scan enumeration error
// must fail the guard (code 1) and must not reap anything, since an
// enumeration failure carries no process list to act on. Pre-fix, this
// returned code 0 -- the swallowed error read as "no leak found".
func TestDoltLeakGuardedTestingMRunWithFailsOnFinalScanError(t *testing.T) {
	tempRoot := filepath.Join(t.TempDir(), "gct-current")
	scanErr := errors.New("reading /proc: permission denied")
	var scan int
	enumerate := func() ([]DoltProcInfo, error) {
		scan++
		if scan == 1 {
			return nil, nil // initial scan succeeds
		}
		return nil, scanErr // every final-scan poll fails
	}
	g := newDoltLeakGuardedTestingM(nil, tempRoot)

	var reapedLeaks []DoltProcInfo
	code := g.runWith(
		func() int { return 0 },
		enumerate,
		func(string) bool { return false },
		func() {},
		func() {},
		func(leaked []DoltProcInfo) { reapedLeaks = append(reapedLeaks, leaked...) },
		time.Millisecond,
		5*time.Millisecond,
	)

	if code != 1 {
		t.Fatalf("guard returned code %d, want 1: a final-scan enumeration failure must fail the run, not be swallowed as a clean result", code)
	}
	if len(reapedLeaks) != 0 {
		t.Fatalf("reaped leaks = %#v, want none: an enumeration failure carries no process list to reap", reapedLeaks)
	}
}

// A dolt sql-server rooted in the source checkout rather than under the run's
// temp root is the leak shape the guard used to miss entirely: cmd/gc's own
// package dir is structurally outside tempRoot, so PathWithin(tempRoot, cfg)
// was false and the process was skipped in silence. .gitignore hides the
// matching cmd/gc/.gc and cmd/gc/.beads dirs, so nothing surfaced it.
func TestSnapshotDoltProcessesForConfigRootsCatchesSourceTreeLeak(t *testing.T) {
	tempRoot := filepath.Join("/tmp", "gct12345-678")
	sourceRoot := filepath.Join("/home", "dev", "gascity", "cmd", "gc")

	underTemp := DoltProcInfo{
		PID:  1001,
		Argv: []string{"dolt", "sql-server", "--config", filepath.Join(tempRoot, "TestX", "001", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")},
	}
	underSource := DoltProcInfo{
		PID:  1002,
		Argv: []string{"dolt", "sql-server", "--config", filepath.Join(sourceRoot, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")},
	}
	// A real city server on the same machine must stay out of the snapshot: the
	// reaping paths kill everything the snapshot returns, so widening the guard
	// must not put a developer's own city at risk.
	realCity := DoltProcInfo{
		PID:  1003,
		Argv: []string{"dolt", "sql-server", "--config", filepath.Join("/home", "dev", "city", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")},
	}

	enumerate := func() ([]DoltProcInfo, error) {
		return []DoltProcInfo{underTemp, underSource, realCity}, nil
	}

	got, err := snapshotDoltProcessesForConfigRoots(enumerate, []string{tempRoot, sourceRoot})
	if err != nil {
		t.Fatalf("snapshotDoltProcessesForConfigRoots: %v", err)
	}
	if _, ok := got[1002]; !ok {
		t.Errorf("source-tree leak PID 1002 missing from snapshot: %#v", got)
	}
	if _, ok := got[1001]; !ok {
		t.Errorf("temp-root leak PID 1001 missing from snapshot: %#v", got)
	}
	if _, ok := got[1003]; ok {
		t.Errorf("real city server PID 1003 must not be snapshotted (the reaper kills these): %#v", got)
	}

	// The pre-fix behavior, pinned so a regression to a single root is caught.
	tempOnly, err := snapshotDoltProcessesForConfigRoots(enumerate, []string{tempRoot})
	if err != nil {
		t.Fatalf("snapshotDoltProcessesForConfigRoots(tempRoot only): %v", err)
	}
	if _, ok := tempOnly[1002]; ok {
		t.Errorf("tempRoot-only snapshot should not see the source-tree leak: %#v", tempOnly)
	}
}

func TestDoltLeakGuardedTestingMLeakRootsIncludeCheckoutRoot(t *testing.T) {
	tempRoot := filepath.Join("/tmp", "gct12345-678")
	checkoutRoot := filepath.Join(t.TempDir(), "gascity")
	sourceRoot := filepath.Join(checkoutRoot, "cmd", "gc")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkoutRoot, "go.mod"), []byte("module example.com/gascity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &doltLeakGuardedTestingM{
		tempRoot:     tempRoot,
		sourceRoot:   sourceRoot,
		checkoutRoot: checkoutRootForTestSource(sourceRoot),
	}

	if roots := g.leakRoots(); len(roots) != 3 || roots[2] != checkoutRoot {
		t.Fatalf("leakRoots() = %q, want checkout root %q", roots, checkoutRoot)
	}
}

func TestCheckoutRootForTestSourceRejectsUnexpectedLayout(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "cmd", "gc")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := checkoutRootForTestSource(sourceRoot); got != "" {
		t.Fatalf("checkoutRootForTestSource() = %q, want empty without go.mod", got)
	}
}

// An unresolved root must narrow the snapshot, never widen it to match every
// dolt server on the host — the reaping paths would then kill real cities.
func TestSnapshotDoltProcessesForConfigRootsIgnoresEmptyRoots(t *testing.T) {
	proc := DoltProcInfo{
		PID:  2001,
		Argv: []string{"dolt", "sql-server", "--config", filepath.Join("/home", "dev", "city", ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")},
	}
	got, err := snapshotDoltProcessesForConfigRoots(func() ([]DoltProcInfo, error) {
		return []DoltProcInfo{proc}, nil
	}, []string{"", ""})
	if err != nil {
		t.Fatalf("snapshotDoltProcessesForConfigRoots: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty roots matched %d process(es), want 0: %#v", len(got), got)
	}
}

// The grace ceiling must stay a generous hang budget (ga-f5clwo), not drift
// back toward round 2's 5s window, which still false-positived under host
// contention (ga-d5nmtj gate evidence). The floor is picked independent of
// config.DefaultDoltStopTimeout's exact value, so a future change to that
// constant can't silently retighten this guard without a test noticing.
func TestDoltLeakGuardGraceMaxElapsedTimeBudget(t *testing.T) {
	const floor = 15 * time.Second
	if doltLeakGuardGraceMaxElapsedTime < floor {
		t.Fatalf("doltLeakGuardGraceMaxElapsedTime = %s, want >= %s", doltLeakGuardGraceMaxElapsedTime, floor)
	}
}
