package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testNonLivePID = 2147483647

func nonLivePID(t *testing.T) int {
	t.Helper()
	if pidAlive(testNonLivePID) {
		t.Skipf("test PID %d is unexpectedly alive", testNonLivePID)
	}
	return testNonLivePID
}

func pidPrefixedTestDir(root, prefix string, pid int) string {
	return filepath.Join(root, prefix+strconv.Itoa(pid)+"-fixture")
}

// backdatePastSweepAge ages a fixture dir past the sweep's minimum-age guard
// so tests exercise the liveness branches rather than the age guard.
func backdatePastSweepAge(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * testOrphanSweepMinAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes(%s): %v", path, err)
	}
}

func TestCmdGCTempRootPrefixKeepsControllerSocketLegacy(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", pidPrefixedTempPattern(testCmdGCTempRootPrefix))
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	sockPath := filepath.Join(root, "gc-testscript-1234567890", "script-controller", "ctrl-city", ".gc", "controller.sock")
	if len(sockPath) > controllerSocketPathLimit {
		t.Fatalf("controller test socket path length = %d, want <= %d: %s", len(sockPath), controllerSocketPathLimit, sockPath)
	}
}

func TestCmdGCTestTempRootPrefixDefaultsToLegacy(t *testing.T) {
	t.Setenv(testShardIndexEnv, "")
	t.Setenv(testShardTotalEnv, "")

	if got := cmdGCTestTempRootPrefix(); got != testCmdGCTempRootPrefix {
		t.Fatalf("cmdGCTestTempRootPrefix() = %q, want %q", got, testCmdGCTempRootPrefix)
	}
}

func TestCmdGCTestTempRootPrefixUsesShardPrefix(t *testing.T) {
	t.Setenv(testShardIndexEnv, "2")
	t.Setenv(testShardTotalEnv, "6")

	if got := cmdGCTestTempRootPrefix(); got != testCmdGCShardTempRootPrefix {
		t.Fatalf("cmdGCTestTempRootPrefix() = %q, want %q", got, testCmdGCShardTempRootPrefix)
	}
}

func TestCmdGCTmuxSocketRootUsesShortPath(t *testing.T) {
	longMacRoot := filepath.Join("/private/var/folders/pm/cmklcsfj60nd7nfc79g8xmbc0000gn/T", "gcx12345-1234567890")

	root, cleanupRoot, err := cmdGCTmuxSocketRoot(longMacRoot)
	if err != nil {
		t.Fatalf("cmdGCTmuxSocketRoot: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cleanupRoot) })

	if !strings.HasPrefix(root, "/tmp/gct-") {
		t.Fatalf("tmux socket root = %q, want short /tmp/gct-* root", root)
	}
	socketPath := filepath.Join(root, "tmux-"+strconv.Itoa(os.Getuid()), "gctest-12345678")
	if len(socketPath) > 104 {
		t.Fatalf("tmux socket path length = %d, want <= 104: %s", len(socketPath), socketPath)
	}
}

func TestSweepOrphanSkipsNonDirectories(t *testing.T) {
	root := t.TempDir()
	// A regular file whose name matches the prefix+PID pattern must not be removed.
	path := filepath.Join(root, "pfx123")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("sweepOrphanPIDPrefixedDirs removed a non-directory file")
	}
}

func TestSweepOrphanSkipsNonMatchingPrefix(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "other12345")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanPIDPrefixedDirs removed directory with non-matching prefix")
	}
}

func TestSweepOrphanSkipsNonNumericPIDSuffix(t *testing.T) {
	root := t.TempDir()
	// No leading PID digits means skip.
	dir := filepath.Join(root, "pfxabc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanPIDPrefixedDirs removed directory with non-numeric PID suffix")
	}
}

func TestSweepOrphanSkipsNonDelimitedPIDSuffix(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pfx123abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanPIDPrefixedDirs removed directory with non-delimited PID suffix")
	}
}

func TestSweepOrphanSkipsZeroPID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pfx0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanPIDPrefixedDirs removed directory with zero PID")
	}
}

func TestSweepOrphanSkipsNegativePID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pfx-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanPIDPrefixedDirs removed directory with negative PID suffix")
	}
}

func TestSweepOrphanSkipsCurrentPID(t *testing.T) {
	root := t.TempDir()
	self := os.Getpid()
	dir := pidPrefixedTestDir(root, "pfx", self)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	backdatePastSweepAge(t, dir)
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanPIDPrefixedDirs removed the current process PID directory")
	}
}

func TestSweepOrphanPreservesLivePID(t *testing.T) {
	root := t.TempDir()
	// Start a long-lived subprocess; its PID is alive.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	dir := pidPrefixedTestDir(root, "pfx", cmd.Process.Pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	backdatePastSweepAge(t, dir)
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Errorf("sweepOrphanPIDPrefixedDirs removed directory for live PID %d", cmd.Process.Pid)
	}
}

func TestSweepOrphanRemovesStalePIDDirectory(t *testing.T) {
	root := t.TempDir()
	pid := nonLivePID(t)
	dir := pidPrefixedTestDir(root, "pfx", pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	backdatePastSweepAge(t, dir)
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("sweepOrphanPIDPrefixedDirs did not remove stale PID %d directory", pid)
	}
}

func TestSweepOrphanSkipsMarkedActiveRoot(t *testing.T) {
	root := t.TempDir()
	pid := nonLivePID(t)
	dir := pidPrefixedTestDir(root, "pfx", pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testActiveTempRootMarker), []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backdatePastSweepAge(t, dir)
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Errorf("sweepOrphanPIDPrefixedDirs removed marked active root for stale PID %d", pid)
	}
}

func TestSweepOrphanToleratesMissingRoot(t *testing.T) {
	// ReadDir on a non-existent root must not panic.
	sweepOrphanPIDPrefixedDirs(filepath.Join(t.TempDir(), "no-such-dir"), "pfx")
}

func TestSweepOrphanIsIdempotent(t *testing.T) {
	root := t.TempDir()

	selfDir := pidPrefixedTestDir(root, "pfx", os.Getpid())
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := nonLivePID(t)
	staleDir := pidPrefixedTestDir(root, "pfx", pid)
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	backdatePastSweepAge(t, selfDir)
	backdatePastSweepAge(t, staleDir)

	sweepOrphanPIDPrefixedDirs(root, "pfx")
	sweepOrphanPIDPrefixedDirs(root, "pfx") // second call must be safe

	if _, err := os.Stat(selfDir); os.IsNotExist(err) {
		t.Error("self dir removed by idempotent sweep")
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Error("stale dir still present after idempotent sweep")
	}
}

// TestSweepOrphanAllPrefixesStabilize verifies that sweepOrphanPIDPrefixedDirs
// removes stale dirs and preserves current-PID dirs for all test-fixture
// prefixes used by cmd/gc's shared fixtures.
func TestSweepOrphanAllPrefixesStabilize(t *testing.T) {
	prefixes := []string{
		testGCBinaryDirPrefix,
		testCmdGCTempRootPrefix,
		testCmdGCShardTempRootPrefix,
		testSharedFixtureDirPrefix,
		testSlingFormulaDirPrefix,
		testSlingCityDirPrefix,
		testGCHomeDirPrefix,
		testRuntimeDirPrefix,
		testProviderStubDirPrefix,
	}
	root := t.TempDir()
	self := os.Getpid()
	pid := nonLivePID(t)

	for _, pfx := range prefixes {
		for _, d := range []string{
			pidPrefixedTestDir(root, pfx, self),
			pidPrefixedTestDir(root, pfx, pid),
		} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatalf("MkdirAll %s: %v", d, err)
			}
			backdatePastSweepAge(t, d)
		}
	}

	for _, pfx := range prefixes {
		sweepOrphanPIDPrefixedDirs(root, pfx)
	}

	for _, pfx := range prefixes {
		selfDir := pidPrefixedTestDir(root, pfx, self)
		staleDir := pidPrefixedTestDir(root, pfx, pid)
		if _, err := os.Stat(selfDir); os.IsNotExist(err) {
			t.Errorf("prefix %q: current-PID dir removed", pfx)
		}
		if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
			t.Errorf("prefix %q: stale dir not removed", pfx)
		}
	}

	// Running a second sweep must leave the current-PID dirs intact (count stable).
	for _, pfx := range prefixes {
		sweepOrphanPIDPrefixedDirs(root, pfx)
	}
	for _, pfx := range prefixes {
		selfDir := pidPrefixedTestDir(root, pfx, self)
		if _, err := os.Stat(selfDir); os.IsNotExist(err) {
			t.Errorf("prefix %q: current-PID dir removed on second sweep", pfx)
		}
	}
}

// TestTestscriptCommandInvocationDoesNotLeakTempRoot verifies that re-executing
// the test binary as "gc" or "bd" (the testscript path) does not create a
// new /tmp/gct<PID>-* directory — the leak root cause (ga-lh1k9).
func TestTestscriptCommandInvocationDoesNotLeakTempRoot(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	dir := t.TempDir()

	tests := []struct {
		name    string
		command string
		args    []string
		wantErr bool
	}{
		{name: "gc", command: "gc", args: []string{"version"}},
		{name: "bd", command: "bd", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commandPath := filepath.Join(dir, tt.command)
			if err := os.Symlink(self, commandPath); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
			t.Cleanup(func() { _ = os.Remove(commandPath) })

			cmd := exec.Command(commandPath, tt.args...)
			cmd.Env = append(os.Environ(), "GC_DOLT=skip")
			err := cmd.Run()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("%s unexpectedly succeeded", tt.command)
				}
			} else if err != nil {
				t.Fatalf("%s %v: %v", tt.command, tt.args, err)
			}

			pid := cmd.ProcessState.Pid()
			for _, prefix := range []string{testCmdGCTempRootPrefix, testCmdGCShardTempRootPrefix} {
				// A leaked root would be created under the inherited
				// TMPDIR (os.TempDir()), not hardcoded /tmp.
				matches, err := filepath.Glob(filepath.Join(os.TempDir(), fmt.Sprintf("%s%d-*", prefix, pid)))
				if err != nil {
					t.Fatalf("Glob: %v", err)
				}
				for _, match := range matches {
					t.Cleanup(func() { _ = os.RemoveAll(match) })
				}
				if len(matches) > 0 {
					t.Fatalf("leaked temp root(s) for pid %d with prefix %q: %v", pid, prefix, matches)
				}
			}
		})
	}
}

// TestSweepOrphanSkipsDirYoungerThanMinAge verifies the minimum-age guard:
// a freshly created dir must survive the sweep even when its PID looks dead
// and it has no sentinel, closing the window between MkdirTemp and sentinel
// acquisition (ga-djbcqt).
func TestSweepOrphanSkipsDirYoungerThanMinAge(t *testing.T) {
	root := t.TempDir()
	pid := nonLivePID(t)
	dir := pidPrefixedTestDir(root, "pfx", pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Errorf("sweepOrphanPIDPrefixedDirs removed dir younger than %v for stale PID %d", testOrphanSweepMinAge, pid)
	}
}

// TestSweepOrphanSkipsHeldSentinelEvenWhenPIDLooksDead simulates the
// cross-PID-namespace incident (ga-djbcqt): from inside a bwrap
// --unshare-pid sandbox every host PID looks dead, but the host creator
// still holds the flock on the alive sentinel. The sweep must trust the
// lock, not PID visibility.
func TestSweepOrphanSkipsHeldSentinelEvenWhenPIDLooksDead(t *testing.T) {
	root := t.TempDir()
	pid := nonLivePID(t) // creator's PID "looks dead", as across namespaces
	dir := pidPrefixedTestDir(root, "pfx", pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel, err := holdAliveSentinel(dir)
	if err != nil {
		t.Fatalf("holdAliveSentinel: %v", err)
	}
	t.Cleanup(func() { _ = sentinel.Close() })
	backdatePastSweepAge(t, dir)

	sweepOrphanPIDPrefixedDirs(root, "pfx")

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Errorf("sweepOrphanPIDPrefixedDirs removed dir with held alive sentinel for dead-looking PID %d", pid)
	}
}

// TestSweepOrphanRemovesDirWithFreeSentinel verifies the reclaim path: when
// the sentinel exists but nobody holds its lock, the creator is gone and the
// dir is removable — even though the active-root marker is still present
// (crashed runs never remove their marker).
func TestSweepOrphanRemovesDirWithFreeSentinel(t *testing.T) {
	root := t.TempDir()
	pid := nonLivePID(t)
	dir := pidPrefixedTestDir(root, "pfx", pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testActiveTempRootMarker), []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testAliveSentinelName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	backdatePastSweepAge(t, dir)

	sweepOrphanPIDPrefixedDirs(root, "pfx")

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("sweepOrphanPIDPrefixedDirs did not remove dir with free alive sentinel for stale PID %d", pid)
	}
}

// TestCreateActiveTestTempRootHonorsTMPDIR verifies the TestMain fixture root
// is created under the inherited TMPDIR instead of hardcoded /tmp, so gate
// runners can isolate concurrent runs (ga-djbcqt).
func TestCreateActiveTestTempRootHonorsTMPDIR(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)

	root, sentinel, err := createActiveTestTempRoot("pfx")
	if err != nil {
		t.Fatalf("createActiveTestTempRoot: %v", err)
	}
	t.Cleanup(func() {
		_ = sentinel.Close()
		_ = os.RemoveAll(root)
	})

	if filepath.Dir(root) != parent {
		t.Errorf("createActiveTestTempRoot created %s; want a child of TMPDIR %s", root, parent)
	}
	if _, err := os.Stat(filepath.Join(root, testActiveTempRootMarker)); err != nil {
		t.Errorf("active-root marker missing: %v", err)
	}
	exists, held := aliveSentinelHeld(root)
	if !exists || !held {
		t.Errorf("aliveSentinelHeld(%s) = (exists=%v, held=%v); want (true, true)", root, exists, held)
	}
}

// TestCreateActiveTestTempRootSentinelFreeAfterClose verifies the sentinel
// lock dies with its holder: once the creator's handle closes (as on process
// death), the probe reports the root reclaimable.
func TestCreateActiveTestTempRootSentinelFreeAfterClose(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	root, sentinel, err := createActiveTestTempRoot("pfx")
	if err != nil {
		t.Fatalf("createActiveTestTempRoot: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	if err := sentinel.Close(); err != nil {
		t.Fatalf("close sentinel: %v", err)
	}
	exists, held := aliveSentinelHeld(root)
	if !exists || held {
		t.Errorf("aliveSentinelHeld(%s) after close = (exists=%v, held=%v); want (true, false)", root, exists, held)
	}
}

// captureSweepStderr runs fn with os.Stderr redirected to a pipe and returns
// what fn wrote. Safe here because this test does not call t.Parallel, so no
// parallel test can write to the swapped stderr.
func captureSweepStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatalf("closing stderr pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	return string(out)
}

// TestSweepOrphanLogsRemovalReason verifies every removal emits one stderr
// line naming the removed path and the branch that justified it, so a future
// recurrence of ga-djbcqt is attributable without gate-log forensics.
func TestSweepOrphanLogsRemovalReason(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, dir string)
		wantReason string
	}{
		{
			name: "free sentinel",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, testAliveSentinelName), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: "free sentinel",
		},
		{
			name:       "legacy dir without sentinel",
			setup:      func(*testing.T, string) {},
			wantReason: "legacy: pid dead, no active marker",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			pid := nonLivePID(t)
			dir := pidPrefixedTestDir(root, "pfx", pid)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, dir)
			backdatePastSweepAge(t, dir)

			got := captureSweepStderr(t, func() { sweepOrphanPIDPrefixedDirs(root, "pfx") })

			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("sweepOrphanPIDPrefixedDirs did not remove %s: %v", dir, err)
			}
			if !strings.Contains(got, dir) || !strings.Contains(got, tt.wantReason) {
				t.Errorf("sweep stderr = %q; want it to name %q with reason %q", got, dir, tt.wantReason)
			}
		})
	}
}

func TestSweepOrphanRemovesStaleCmdGCTempRootInSystemTmp(t *testing.T) {
	prefix := fmt.Sprintf("%s%d-test-", testCmdGCTempRootPrefix, os.Getpid())
	pid := nonLivePID(t)
	root := filepath.Join("/tmp", fmt.Sprintf("%s%d-stale-backstop", prefix, pid))
	_ = os.RemoveAll(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	backdatePastSweepAge(t, root)

	sweepOrphanPIDPrefixedDirs("/tmp", prefix)

	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("stale cmd/gc temp root still exists after sweep: %v", err)
	}
}
