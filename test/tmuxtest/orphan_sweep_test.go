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

func TestSweepOrphanPIDPrefixedDirsPreservesLegacyNoDashDir(t *testing.T) {
	root := t.TempDir()
	// The pre-sweep harness created its socket parent with
	// os.MkdirTemp(root, "pfx-"), yielding an all-digit "pfx-<random>" name
	// with no "-" separator and no alive sentinel. Those trailing digits are a
	// MkdirTemp random suffix, not an owner PID -- parsing them as a (dead) PID
	// would let the sweep reap a still-live legacy sibling. Even backdated past
	// the age guard and with digits that look like a dead PID, the missing
	// separator must keep the dir out of the sweep.
	legacy := filepath.Join(root, "pfx-"+strconv.Itoa(nonLivePID(t)))
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatalf("Mkdir(%s): %v", legacy, err)
	}
	backdatePastSweepAge(t, legacy)

	SweepOrphanPIDPrefixedDirs(root, "pfx-")

	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy no-separator dir was removed by sweep: %v", err)
	}
}

func TestPIDFromPrefixedDirName(t *testing.T) {
	const prefix = "gct-"
	cases := []struct {
		name    string
		wantPID int
		wantOK  bool
	}{
		{"gct-1234-0007", 1234, true}, // canonical <prefix><PID>-<random>
		{"gct-1234-", 1234, true},     // separator present, empty random suffix
		{"gct-1234", 0, false},        // legacy no-separator name: rejected
		{"gct-", 0, false},            // no digits
		{"gct-abc", 0, false},         // non-digit suffix
		{"gct-12ab-3", 0, false},      // digits not terminated by "-"
		{"other-1234-5", 0, false},    // wrong prefix
	}
	for _, tc := range cases {
		gotPID, gotOK := pidFromPrefixedDirName(tc.name, prefix)
		if gotPID != tc.wantPID || gotOK != tc.wantOK {
			t.Errorf("pidFromPrefixedDirName(%q, %q) = (%d, %v), want (%d, %v)",
				tc.name, prefix, gotPID, gotOK, tc.wantPID, tc.wantOK)
		}
	}
}
