package sling

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// slingTestStalePID starts a process, waits for it to exit, and returns
// the now-dead PID.
func slingTestStalePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("start subprocess for stale PID: %v", err)
	}
	return cmd.ProcessState.Pid()
}

func TestSweepOrphanSlingSkipsNonDirectories(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pfx123")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("sweepOrphanSlingPIDPrefixedDirs removed a non-directory file")
	}
}

func TestSweepOrphanSlingSkipsNonMatchingPrefix(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "other12345")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanSlingPIDPrefixedDirs removed directory with non-matching prefix")
	}
}

func TestSweepOrphanSlingSkipsNonNumericPIDSuffix(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pfxabc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanSlingPIDPrefixedDirs removed directory with non-numeric PID suffix")
	}
}

func TestSweepOrphanSlingSkipsZeroPID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pfx0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanSlingPIDPrefixedDirs removed directory with zero PID")
	}
}

func TestSweepOrphanSlingSkipsNegativePID(t *testing.T) {
	root := t.TempDir()
	// TrimPrefix("pfx-1", "pfx") → "-1"; Atoi → -1 → pid <= 0 → skip.
	dir := filepath.Join(root, "pfx-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanSlingPIDPrefixedDirs removed directory with negative PID suffix")
	}
}

func TestSweepOrphanSlingSkipsCurrentPID(t *testing.T) {
	root := t.TempDir()
	self := os.Getpid()
	dir := filepath.Join(root, "pfx"+strconv.Itoa(self))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanSlingPIDPrefixedDirs removed the current process PID directory")
	}
}

func TestSweepOrphanSlingPreservesLivePID(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	dir := filepath.Join(root, "pfx"+strconv.Itoa(cmd.Process.Pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Errorf("sweepOrphanSlingPIDPrefixedDirs removed directory for live PID %d", cmd.Process.Pid)
	}
}

func TestSweepOrphanSlingRemovesStalePIDDirectory(t *testing.T) {
	root := t.TempDir()
	pid := slingTestStalePID(t)
	dir := filepath.Join(root, "pfx"+strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("sweepOrphanSlingPIDPrefixedDirs did not remove stale PID %d directory", pid)
	}
}

func TestSweepOrphanSlingToleratesMissingRoot(t *testing.T) {
	sweepOrphanSlingPIDPrefixedDirs(filepath.Join(t.TempDir(), "no-such-dir"), "pfx")
}

func TestSweepOrphanSlingIsIdempotent(t *testing.T) {
	root := t.TempDir()

	selfDir := filepath.Join(root, "pfx"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := slingTestStalePID(t)
	staleDir := filepath.Join(root, "pfx"+strconv.Itoa(pid))
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")

	if _, err := os.Stat(selfDir); os.IsNotExist(err) {
		t.Error("self dir removed by idempotent sweep")
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Error("stale dir still present after idempotent sweep")
	}
}

// TestSweepOrphanSlingBothPrefixesStabilize exercises sweepOrphanSlingPIDPrefixedDirs
// across both sling-specific prefixes, verifying stale dirs are removed and
// current-PID dirs are preserved across repeated calls.
func TestSweepOrphanSlingBothPrefixesStabilize(t *testing.T) {
	prefixes := []string{
		slingTestFormulaDirPrefix,
		slingTestCityDirPrefix,
	}
	root := t.TempDir()
	self := os.Getpid()
	pid := slingTestStalePID(t)

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
		sweepOrphanSlingPIDPrefixedDirs(root, pfx)
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

	// Second sweep must not disturb the current-PID dirs.
	for _, pfx := range prefixes {
		sweepOrphanSlingPIDPrefixedDirs(root, pfx)
	}
	for _, pfx := range prefixes {
		selfDir := filepath.Join(root, pfx+strconv.Itoa(self))
		if _, err := os.Stat(selfDir); os.IsNotExist(err) {
			t.Errorf("prefix %q: current-PID dir removed on second sweep", pfx)
		}
	}
}
