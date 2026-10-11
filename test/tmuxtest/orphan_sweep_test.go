package tmuxtest

import (
	"bytes"
	"fmt"
	"io"
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

func TestSweepOrphanPIDPrefixedDirsRemovesStaleDeadPIDWithNilDiagnostics(t *testing.T) {
	root := t.TempDir()
	dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))
	backdatePastSweepAge(t, dir)

	SweepOrphanPIDPrefixedDirs(root, "pfx-", nil)

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

	SweepOrphanPIDPrefixedDirs(root, "pfx-", io.Discard)

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

	var diagnostics bytes.Buffer
	SweepOrphanPIDPrefixedDirs(root, "pfx-", &diagnostics)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir with free sentinel survived sweep: %s", dir)
	}
	wantDiagnostics := fmt.Sprintf("tmuxtest: removing orphaned socket parent %s (free sentinel)\n", dir)
	if got := diagnostics.String(); got != wantDiagnostics {
		t.Errorf("diagnostics = %q, want %q", got, wantDiagnostics)
	}
}

func TestSweepOrphanPIDPrefixedDirsSkipsYoungDir(t *testing.T) {
	root := t.TempDir()
	dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))
	// No backdate: dir is fresh, inside the min-age window.

	SweepOrphanPIDPrefixedDirs(root, "pfx-", io.Discard)

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("young dir was removed by sweep despite age guard: %v", err)
	}
}

func TestSweepOrphanPIDPrefixedDirsSkipsSelfPID(t *testing.T) {
	root := t.TempDir()
	dir := pidPrefixedTestDir(t, root, "pfx-", os.Getpid())
	backdatePastSweepAge(t, dir)

	SweepOrphanPIDPrefixedDirs(root, "pfx-", io.Discard)

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
	SweepOrphanPIDPrefixedDirs(root, "pfx-", io.Discard)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("SweepOrphanPIDPrefixedDirs removed a non-directory file")
	}
}

func TestNewSocketParentDirCreatesSentinelHeldDir(t *testing.T) {
	root := t.TempDir()

	dir, sentinel, err := NewSocketParentDir(root, io.Discard)
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

	var diagnostics bytes.Buffer
	dir, sentinel, err := NewSocketParentDir(root, &diagnostics)
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
	wantDiagnostics := fmt.Sprintf("tmuxtest: removing orphaned socket parent %s (pid dead, no sentinel)\n", orphan)
	if got := diagnostics.String(); got != wantDiagnostics {
		t.Errorf("diagnostics = %q, want %q", got, wantDiagnostics)
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

	SweepOrphanPIDPrefixedDirs(root, "pfx-", io.Discard)

	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy no-separator dir was removed by sweep: %v", err)
	}
}

// TestSweepOrphanPIDPrefixedDirsAgeFenceBySentinelState verifies the
// sentinel probe gates removal before any age fence applies, with each
// sentinel state carrying its own age threshold (ga-lygcyb): a held
// sentinel is never removed regardless of age; a free (unlocked) sentinel
// proves the creator is gone and only needs a short grace window, not the
// full legacy fence; a dir with no sentinel at all keeps the existing
// hour-long fence. Today the age check runs before the sentinel probe, so
// the "free sentinel, 5 minutes old" case wrongly survives — this must fail
// until the sentinel probe is checked first.
func TestSweepOrphanPIDPrefixedDirsAgeFenceBySentinelState(t *testing.T) {
	tests := []struct {
		name       string
		age        time.Duration
		setup      func(t *testing.T, dir string) (cleanup func())
		wantRemove bool
	}{
		{
			name: "held sentinel, 5 minutes old",
			age:  5 * time.Minute,
			setup: func(t *testing.T, dir string) func() {
				sentinel, err := HoldAliveSentinel(dir)
				if err != nil {
					t.Fatalf("HoldAliveSentinel: %v", err)
				}
				return func() { _ = sentinel.Close() }
			},
			wantRemove: false,
		},
		{
			name: "free sentinel, 90 seconds old",
			age:  90 * time.Second,
			setup: func(t *testing.T, dir string) func() {
				sentinel, err := HoldAliveSentinel(dir)
				if err != nil {
					t.Fatalf("HoldAliveSentinel: %v", err)
				}
				_ = sentinel.Close()
				return func() {}
			},
			wantRemove: false,
		},
		{
			name: "free sentinel, 5 minutes old",
			age:  5 * time.Minute,
			setup: func(t *testing.T, dir string) func() {
				sentinel, err := HoldAliveSentinel(dir)
				if err != nil {
					t.Fatalf("HoldAliveSentinel: %v", err)
				}
				_ = sentinel.Close()
				return func() {}
			},
			wantRemove: true,
		},
		{
			name:       "no sentinel (legacy), over an hour old",
			age:        2 * socketParentSweepMinAge,
			setup:      func(_ *testing.T, _ string) func() { return func() {} },
			wantRemove: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))
			cleanup := tt.setup(t, dir)
			defer cleanup()
			old := time.Now().Add(-tt.age)
			if err := os.Chtimes(dir, old, old); err != nil {
				t.Fatalf("Chtimes(%s): %v", dir, err)
			}

			SweepOrphanPIDPrefixedDirs(root, "pfx-", io.Discard)

			_, err := os.Stat(dir)
			removed := os.IsNotExist(err)
			if removed != tt.wantRemove {
				t.Errorf("dir removed = %v, want %v", removed, tt.wantRemove)
			}
		})
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
