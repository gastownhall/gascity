package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// stalePID starts a process, waits for it to exit, and returns the now-dead PID.
func stalePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("start subprocess for stale PID: %v", err)
	}
	return cmd.ProcessState.Pid()
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
	// strconv.Atoi("abc") fails → skip.
	dir := filepath.Join(root, "pfxabc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanPIDPrefixedDirs removed directory with non-numeric PID suffix")
	}
}

func TestSweepOrphanSkipsZeroPID(t *testing.T) {
	root := t.TempDir()
	// pid == 0 → pid <= 0 → skip.
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
	// TrimPrefix("pfx-1", "pfx") → "-1"; Atoi("-1") = -1 → pid <= 0 → skip.
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
	dir := filepath.Join(root, "pfx"+strconv.Itoa(self))
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

	dir := filepath.Join(root, "pfx"+strconv.Itoa(cmd.Process.Pid))
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
	pid := stalePID(t)
	dir := filepath.Join(root, "pfx"+strconv.Itoa(pid))
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

	selfDir := filepath.Join(root, "pfx"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := stalePID(t)
	staleDir := filepath.Join(root, "pfx"+strconv.Itoa(pid))
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

// TestSweepOrphanAllFivePrefixesStabilize verifies that sweepOrphanPIDPrefixedDirs
// removes stale dirs and preserves current-PID dirs for all five test-fixture
// prefixes used across cmd/gc and internal/sling. This is the isolated TMPDIR
// stability check described in the bead acceptance criteria.
func TestSweepOrphanAllFivePrefixesStabilize(t *testing.T) {
	prefixes := []string{
		"gc-test-binary-pid",
		"gc-sling-test-formulas-pid",
		"gc-sling-test-city-pid",
		"gascity-gc-home-pid",
		"gascity-runtime-pid",
	}
	root := t.TempDir()
	self := os.Getpid()
	pid := stalePID(t)

	for _, pfx := range prefixes {
		for _, d := range []string{
			filepath.Join(root, pfx+strconv.Itoa(self)),
			filepath.Join(root, pfx+strconv.Itoa(pid)),
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
		selfDir := filepath.Join(root, pfx+strconv.Itoa(self))
		staleDir := filepath.Join(root, pfx+strconv.Itoa(pid))
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
		selfDir := filepath.Join(root, pfx+strconv.Itoa(self))
		if _, err := os.Stat(selfDir); os.IsNotExist(err) {
			t.Errorf("prefix %q: current-PID dir removed on second sweep", pfx)
		}
	}
}
