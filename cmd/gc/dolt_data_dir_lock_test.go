package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	testDoltFlockHelperEnv      = "GC_TEST_DOLT_FLOCK_HELPER"
	testDoltFlockHelperLockPath = "GC_TEST_DOLT_FLOCK_HELPER_LOCK_PATH"
)

type testDoltFlockHelper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	output *bytes.Buffer
}

func requireProcLocks(t *testing.T) {
	t.Helper()
	f, err := os.Open("/proc/locks")
	if err != nil {
		t.Skipf("/proc/locks unavailable: %v", err)
	}
	_ = f.Close()
}

func startTestDoltFlockHelper(t *testing.T, dir, lockPath string) *testDoltFlockHelper {
	t.Helper()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create helper readiness pipe: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDoltFlockHelperProcess$", "-test.v")
	cmd.Dir = dir
	cmd.Env = sanitizedBaseEnv(
		testDoltFlockHelperEnv+"=1",
		testDoltFlockHelperLockPath+"="+lockPath,
	)
	cmd.ExtraFiles = []*os.File{readyW}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = readyR.Close()
		_ = readyW.Close()
		t.Fatalf("create helper control pipe: %v", err)
	}
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = readyR.Close()
		_ = readyW.Close()
		t.Fatalf("start flock helper: %v", err)
	}
	_ = readyW.Close()
	helper := &testDoltFlockHelper{cmd: cmd, stdin: stdin, output: output}
	t.Cleanup(func() {
		_ = helper.stdin.Close()
		if helper.cmd.ProcessState == nil {
			_ = helper.cmd.Process.Kill()
		}
		_ = helper.cmd.Wait()
	})
	ready := make([]byte, 1)
	if _, err := io.ReadFull(readyR, ready); err != nil {
		_ = readyR.Close()
		t.Fatalf("wait for flock helper readiness: %v\n%s", err, output)
	}
	_ = readyR.Close()
	return helper
}

func TestDoltFlockHelperProcess(t *testing.T) {
	if os.Getenv(testDoltFlockHelperEnv) != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	lockPath := os.Getenv(testDoltFlockHelperLockPath)
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open helper lock file: %v", err)
	}
	defer f.Close() //nolint:errcheck
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("take helper flock: %v", err)
	}
	ready := os.NewFile(3, "flock-ready")
	if ready == nil {
		t.Fatal("open helper readiness descriptor")
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatalf("signal helper readiness: %v", err)
	}
	_ = ready.Close()
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("wait for helper release: %v", err)
	}
}

func TestManagedDoltProcLocksParserUsesHexDeviceAndDecimalInode(t *testing.T) {
	lock, flock, err := parseManagedDoltProcFlock("8: FLOCK ADVISORY WRITE 3675383 103:07:2153249400 0 EOF")
	if err != nil {
		t.Fatalf("parse /proc/locks FLOCK row: %v", err)
	}
	if !flock {
		t.Fatal("FLOCK row was not recognized")
	}
	want := managedDoltProcLock{pid: 3675383, major: 259, minor: 7, inode: 2153249400}
	if lock != want {
		t.Fatalf("parsed lock = %+v, want %+v", lock, want)
	}
}

// makeDoltDataDirWithLock creates a managed-dolt-shaped data dir with one
// database whose noms LOCK file exists, returning the data dir and lock path.
func makeDoltDataDirWithLock(t *testing.T) (string, string) {
	t.Helper()
	dataDir := t.TempDir()
	nomsDir := filepath.Join(dataDir, "dolt", ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("mkdir noms dir: %v", err)
	}
	lockPath := filepath.Join(nomsDir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	return dataDir, lockPath
}

// holdFlock takes an exclusive flock on path and returns a release func.
// flock conflicts are per open-file-description, so a second open in this
// same process observes the lock as held — exactly how a separate dolt
// process would.
func holdFlock(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatalf("flock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	t.Cleanup(release)
	return release
}

func TestManagedDoltDataDirLockHolderFreeWhenNoLockFiles(t *testing.T) {
	if holder := managedDoltDataDirLockHolder(t.TempDir()); holder != "" {
		t.Fatalf("expected no holder for empty data dir, got %q", holder)
	}
}

func TestManagedDoltDataDirLockHolderFreeWhenMissingDataDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if holder := managedDoltDataDirLockHolder(missing); holder != "" {
		t.Fatalf("expected no holder for missing data dir, got %q", holder)
	}
}

func TestManagedDoltDataDirLockHolderFreeWhenUnheld(t *testing.T) {
	dataDir, _ := makeDoltDataDirWithLock(t)
	if holder := managedDoltDataDirLockHolder(dataDir); holder != "" {
		t.Fatalf("expected no holder for unheld lock, got %q", holder)
	}
}

func TestManagedDoltDataDirLockHolderDetectsHeldLock(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	release := holdFlock(t, lockPath)
	if holder := managedDoltDataDirLockHolder(dataDir); holder != lockPath {
		t.Fatalf("expected holder %q, got %q", lockPath, holder)
	}
	release()
	if holder := managedDoltDataDirLockHolder(dataDir); holder != "" {
		t.Fatalf("expected no holder after release, got %q", holder)
	}
}

func TestStopManagedDoltSIGKILLsWedgedSoleLockHolder(t *testing.T) {
	requireProcLocks(t)
	city, lockPath := raceTestCity(t, "[workspace]\nname = \"race-test\"\n\n[daemon]\ndolt_stop_timeout = \"0s\"\n\n[dolt]\ndolt_lock_release_timeout = \"0s\"\n")
	dataDir := filepath.Join(city, ".beads", "dolt")
	helper := startTestDoltFlockHelper(t, dataDir, lockPath)
	pid := helper.cmd.Process.Pid
	if err := os.WriteFile(os.Getenv("GC_DOLT_PID_FILE"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	report, err := stopManagedDoltProcessWithOptions(city, "", false)
	if err != nil {
		t.Fatalf("stop-managed refused SIGKILL of its sole lock holder pid %d: %v\n%s", pid, err, helper.output)
	}
	if !report.Forced {
		t.Fatalf("stop-managed did not report forced termination: %+v", report)
	}
	if pidAlive(pid) {
		t.Fatalf("sole lock holder pid %d still alive after stop-managed", pid)
	}
}

func TestManagedDoltLockHolderPIDsSeesKnownHeldLock(t *testing.T) {
	requireProcLocks(t)
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	helper := startTestDoltFlockHelper(t, dataDir, lockPath)

	pids, err := managedDoltLockHolderPIDs(lockPath, "/proc/locks")
	if err != nil {
		t.Fatalf("resolve known lock holder: %v", err)
	}
	want := helper.cmd.Process.Pid
	if len(pids) != 1 || pids[0] != want {
		t.Fatalf("holders for known-held lock = %v, want [%d]", pids, want)
	}
}

func TestManagedDoltSIGKILLLockGateRefusesOtherHolder(t *testing.T) {
	requireProcLocks(t)
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	helper := startTestDoltFlockHelper(t, dataDir, lockPath)
	targetPID := os.Getpid()

	err := waitManagedDoltSIGKILLLockGate(targetPID, dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond)
	if err == nil {
		t.Fatal("expected SIGKILL gate to refuse a lock held by another process")
	}
	if !strings.Contains(err.Error(), "other holder pid(s)") || !strings.Contains(err.Error(), strconv.Itoa(helper.cmd.Process.Pid)) {
		t.Fatalf("expected other-holder reason naming pid %d, got %v", helper.cmd.Process.Pid, err)
	}
	if strings.Contains(err.Error(), "could not measure lock ownership") {
		t.Fatalf("other-holder refusal was misreported as measurement failure: %v", err)
	}
}

func TestManagedDoltSIGKILLLockGateRefusesWhenOwnershipCannotBeMeasured(t *testing.T) {
	requireProcLocks(t)
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	startTestDoltFlockHelper(t, dataDir, lockPath)
	missingProcLocks := filepath.Join(t.TempDir(), "missing-proc-locks")

	err := waitManagedDoltSIGKILLLockGateWithProcLocks(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond, missingProcLocks)
	if err == nil {
		t.Fatal("expected SIGKILL gate to fail closed when lock ownership cannot be measured")
	}
	if !strings.Contains(err.Error(), "could not measure lock ownership") || !strings.Contains(err.Error(), "read "+missingProcLocks) {
		t.Fatalf("expected could-not-measure reason naming %q, got %v", missingProcLocks, err)
	}
	if strings.Contains(err.Error(), "other holder pid(s)") {
		t.Fatalf("measurement failure was misreported as another holder: %v", err)
	}
}

func TestManagedDoltSIGKILLLockGateRefusesWhenFlockAndProcLocksDisagree(t *testing.T) {
	requireProcLocks(t)
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	startTestDoltFlockHelper(t, dataDir, lockPath)
	emptyProcLocks := filepath.Join(t.TempDir(), "empty-proc-locks")
	if err := os.WriteFile(emptyProcLocks, nil, 0o644); err != nil {
		t.Fatalf("write empty proc locks fixture: %v", err)
	}

	err := waitManagedDoltSIGKILLLockGateWithProcLocks(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond, emptyProcLocks)
	if err == nil {
		t.Fatal("expected SIGKILL gate to fail closed when flock and proc lock probes disagree")
	}
	if !strings.Contains(err.Error(), "could not measure lock ownership") || !strings.Contains(err.Error(), "no matching FLOCK row") {
		t.Fatalf("expected empty-holder measurement reason, got %v", err)
	}
}

func TestManagedDoltDataDirLockHolderDetectsHeldLockUnderGlobMetacharPath(t *testing.T) {
	// A literal data-dir path containing glob metacharacters must not be
	// treated as a pattern: an unmatched `[` makes filepath.Glob error out
	// (silently dropping the probe) and `?`/`*` match the wrong paths —
	// either way the guard would miss a held LOCK and re-open the #3174
	// race for any city at such a path.
	dataDir := filepath.Join(t.TempDir(), "city [prod ?*")
	nomsDir := filepath.Join(dataDir, "dolt", ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("mkdir noms dir: %v", err)
	}
	lockPath := filepath.Join(nomsDir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	holdFlock(t, lockPath)
	if holder := managedDoltDataDirLockHolder(dataDir); holder != lockPath {
		t.Fatalf("expected holder %q, got %q", lockPath, holder)
	}
}

func TestManagedDoltDataDirLockHolderDetectsRootLevelLock(t *testing.T) {
	dataDir := t.TempDir()
	nomsDir := filepath.Join(dataDir, ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("mkdir noms dir: %v", err)
	}
	lockPath := filepath.Join(nomsDir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	holdFlock(t, lockPath)
	if holder := managedDoltDataDirLockHolder(dataDir); holder != lockPath {
		t.Fatalf("expected holder %q, got %q", lockPath, holder)
	}
}

func TestWaitForManagedDoltDataDirLockFreeImmediateWhenUnheld(t *testing.T) {
	dataDir, _ := makeDoltDataDirWithLock(t)
	start := time.Now()
	if err := waitForManagedDoltDataDirLockFree(dataDir, 5*time.Second); err != nil {
		t.Fatalf("expected nil for unheld lock, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("expected immediate return for unheld lock, took %s", elapsed)
	}
}

func TestWaitForManagedDoltDataDirLockFreeFailsClosedOnTimeout(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	holdFlock(t, lockPath)
	err := waitForManagedDoltDataDirLockFree(dataDir, 150*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when lock is held past the timeout")
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("expected error to name the held lock %q, got %v", lockPath, err)
	}
}

func TestWaitForManagedDoltDataDirLockFreeZeroTimeoutProbesOnce(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	holdFlock(t, lockPath)
	if err := waitForManagedDoltDataDirLockFree(dataDir, 0); err == nil {
		t.Fatal("expected fail-closed error for held lock with zero timeout")
	}
}

func TestResolveManagedDoltLockReleaseTimeoutEmptyCityPathReturnsDefault(t *testing.T) {
	got := resolveManagedDoltLockReleaseTimeout("")
	if got != config.DefaultDoltLockReleaseTimeout {
		t.Fatalf("empty cityPath: got %s, want %s", got, config.DefaultDoltLockReleaseTimeout)
	}
}

func TestResolveManagedDoltLockReleaseTimeoutMissingCityTomlReturnsDefault(t *testing.T) {
	got := resolveManagedDoltLockReleaseTimeout(t.TempDir())
	if got != config.DefaultDoltLockReleaseTimeout {
		t.Fatalf("missing city.toml: got %s, want %s", got, config.DefaultDoltLockReleaseTimeout)
	}
}

func TestResolveManagedDoltLockReleaseTimeoutFromCityToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test-city"

[dolt]
dolt_lock_release_timeout = "9s"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltLockReleaseTimeout(dir)
	if got != 9*time.Second {
		t.Fatalf("from city.toml: got %s, want 9s", got)
	}
}

func TestWaitForManagedDoltDataDirLockFreeRecoversOnRelease(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	release := holdFlock(t, lockPath)
	done := make(chan error, 1)
	go func() {
		done <- waitForManagedDoltDataDirLockFree(dataDir, 10*time.Second)
	}()
	time.Sleep(300 * time.Millisecond)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected wait to succeed after release, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not observe lock release")
	}
}
