package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
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
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("sweepOrphanPIDPrefixedDirs did not remove stale PID %d directory", pid)
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
