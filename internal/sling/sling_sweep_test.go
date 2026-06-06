package sling

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/pidutil"
)

const slingTestNonLivePID = 2147483647

func slingTestNonLivePIDValue(t *testing.T) int {
	t.Helper()
	if pidutil.Alive(slingTestNonLivePID) {
		t.Skipf("test PID %d is unexpectedly alive", slingTestNonLivePID)
	}
	return slingTestNonLivePID
}

func slingPIDPrefixedTestDir(root, prefix string, pid int) string {
	return filepath.Join(root, prefix+strconv.Itoa(pid)+"-fixture")
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

func TestSweepOrphanSlingSkipsNonDelimitedPIDSuffix(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pfx123abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepOrphanSlingPIDPrefixedDirs(root, "pfx")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("sweepOrphanSlingPIDPrefixedDirs removed directory with non-delimited PID suffix")
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
	dir := slingPIDPrefixedTestDir(root, "pfx", self)
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

	dir := slingPIDPrefixedTestDir(root, "pfx", cmd.Process.Pid)
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
	pid := slingTestNonLivePIDValue(t)
	dir := slingPIDPrefixedTestDir(root, "pfx", pid)
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

	selfDir := slingPIDPrefixedTestDir(root, "pfx", os.Getpid())
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := slingTestNonLivePIDValue(t)
	staleDir := slingPIDPrefixedTestDir(root, "pfx", pid)
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
	pid := slingTestNonLivePIDValue(t)

	for _, pfx := range prefixes {
		for _, d := range []string{
			slingPIDPrefixedTestDir(root, pfx, self),
			slingPIDPrefixedTestDir(root, pfx, pid),
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
		selfDir := slingPIDPrefixedTestDir(root, pfx, self)
		staleDir := slingPIDPrefixedTestDir(root, pfx, pid)
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
		selfDir := slingPIDPrefixedTestDir(root, pfx, self)
		if _, err := os.Stat(selfDir); os.IsNotExist(err) {
			t.Errorf("prefix %q: current-PID dir removed on second sweep", pfx)
		}
	}
}
