package tmuxtest

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// testNonLivePID is a PID value that will not correspond to a live process
// on any reasonable system (max PID on Linux is well below this).
const testNonLivePID = 2147483647

func nonLivePID(t *testing.T) int {
	t.Helper()
	if pidutil.Alive(testNonLivePID) {
		t.Skipf("test PID %d is unexpectedly alive", testNonLivePID)
	}
	return testNonLivePID
}

func backdatePastSweepAge(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * socketParentSweepMinAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes(%s): %v", path, err)
	}
}

func pidPrefixedTestDir(t *testing.T, root, prefix string, pid int) string {
	t.Helper()
	dir := filepath.Join(root, prefix+strconv.Itoa(pid)+"-fixture")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir(%s): %v", dir, err)
	}
	return dir
}

func TestSweepOrphanPIDPrefixedDirsRemovesStaleDeadPID(t *testing.T) {
	root := t.TempDir()
	dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))
	backdatePastSweepAge(t, dir)

	SweepOrphanPIDPrefixedDirs(root, "pfx-")

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("stale dead-PID dir survived sweep: %s", dir)
	}
}

func TestSweepOrphanPIDPrefixedDirsPreservesHeldSentinel(t *testing.T) {
	root := t.TempDir()
	dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))
	backdatePastSweepAge(t, dir)

	sentinel, err := HoldAliveSentinel(dir)
	if err != nil {
		t.Fatalf("HoldAliveSentinel: %v", err)
	}
	defer func() { _ = sentinel.Close() }()

	SweepOrphanPIDPrefixedDirs(root, "pfx-")

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir with held sentinel was removed by sweep: %v", err)
	}
}

func TestSweepOrphanPIDPrefixedDirsRemovesFreeSentinel(t *testing.T) {
	root := t.TempDir()
	dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))

	sentinel, err := HoldAliveSentinel(dir)
	if err != nil {
		t.Fatalf("HoldAliveSentinel: %v", err)
	}
	_ = sentinel.Close() // release the flock, simulating a crashed creator

	backdatePastSweepAge(t, dir)

	SweepOrphanPIDPrefixedDirs(root, "pfx-")

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir with free sentinel survived sweep: %s", dir)
	}
}

func TestSweepOrphanPIDPrefixedDirsSkipsYoungDir(t *testing.T) {
	root := t.TempDir()
	dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))
	// No backdate: dir is fresh, inside the min-age window.

	SweepOrphanPIDPrefixedDirs(root, "pfx-")

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("young dir was removed by sweep despite age guard: %v", err)
	}
}

func TestSweepOrphanPIDPrefixedDirsSkipsSelfPID(t *testing.T) {
	root := t.TempDir()
	dir := pidPrefixedTestDir(t, root, "pfx-", os.Getpid())
	backdatePastSweepAge(t, dir)

	SweepOrphanPIDPrefixedDirs(root, "pfx-")

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("sweep removed a dir carrying its own PID: %v", err)
	}
}

func TestSweepOrphanPIDPrefixedDirsSkipsNonDirectories(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pfx-123")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	SweepOrphanPIDPrefixedDirs(root, "pfx-")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("SweepOrphanPIDPrefixedDirs removed a non-directory file")
	}
}

func TestNewSocketParentDirCreatesSentinelHeldDir(t *testing.T) {
	root := t.TempDir()

	dir, sentinel, err := NewSocketParentDir(root)
	if err != nil {
		t.Fatalf("NewSocketParentDir: %v", err)
	}
	defer func() { _ = sentinel.Close() }()
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("created dir does not exist: %v", err)
	}
	exists, held := aliveSentinelHeld(dir)
	if !exists || !held {
		t.Errorf("aliveSentinelHeld(%s) = (%v, %v), want (true, true)", dir, exists, held)
	}
	pid, ok := pidFromPrefixedDirName(filepath.Base(dir), SocketParentDirPrefix)
	if !ok || pid != os.Getpid() {
		t.Errorf("created dir %q does not embed this process's PID", dir)
	}
}

func TestNewSocketParentDirReapsOrphanedSibling(t *testing.T) {
	root := t.TempDir()
	orphan := pidPrefixedTestDir(t, root, SocketParentDirPrefix, nonLivePID(t))
	backdatePastSweepAge(t, orphan)

	dir, sentinel, err := NewSocketParentDir(root)
	if err != nil {
		t.Fatalf("NewSocketParentDir: %v", err)
	}
	defer func() { _ = sentinel.Close() }()
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphaned sibling survived NewSocketParentDir: %s", orphan)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("freshly created dir missing: %v", err)
	}
}
