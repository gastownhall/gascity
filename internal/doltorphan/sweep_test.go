package doltorphan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dolthub/fslock"
	"github.com/gastownhall/gascity/internal/clock"
)

func mkStoreDir(t *testing.T, root, name string, markerDepth int, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, name)
	markerParent := dir
	for i := 1; i < markerDepth; i++ {
		markerParent = filepath.Join(markerParent, "level")
	}
	if err := os.MkdirAll(markerParent, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", markerParent, err)
	}
	if markerDepth > 0 {
		if err := os.MkdirAll(filepath.Join(markerParent, ".dolt"), 0o755); err != nil {
			t.Fatalf("MkdirAll(.dolt): %v", err)
		}
	}
	if err := chtimesRecursive(dir, mtime); err != nil {
		t.Fatalf("chtimesRecursive(%s): %v", dir, err)
	}
	return dir
}

func chtimesRecursive(dir string, mtime time.Time) error {
	return filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, mtime, mtime)
	})
}

func noLsofHits(context.Context) ([]byte, error) { return nil, nil }

func TestSweep_RemovesOldMarkedUnheldDir(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "orphan1", 1, old)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if len(result.Removed) != 1 || result.Removed[0] != dir {
		t.Fatalf("Removed = %v, want [%s]", result.Removed, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir %s should have been removed, stat err = %v", dir, err)
	}
}

func TestSweep_SkipsDirYoungerThanMinAge(t *testing.T) {
	root := t.TempDir()
	recent := time.Now().Add(-5 * time.Minute)
	dir := mkStoreDir(t, root, "fresh1", 1, recent)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (too young)", result.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_SkipsDirWithoutDoltMarker(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "nomarker1", 0, old)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (no .dolt marker)", result.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_FindsMarkerAtEachAllowedDepth(t *testing.T) {
	for _, depth := range []int{1, 2, 3} {
		t.Run(string(rune('0'+depth)), func(t *testing.T) {
			root := t.TempDir()
			old := time.Now().Add(-2 * time.Hour)
			dir := mkStoreDir(t, root, "orphan", depth, old)

			result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

			if len(result.Removed) != 1 {
				t.Fatalf("depth %d: Removed = %v, want exactly one removal", depth, result.Removed)
			}
			if result.Removed[0] != dir {
				t.Fatalf("depth %d: Removed = %v, want [%s]", depth, result.Removed, dir)
			}
		})
	}
}

func TestSweep_IgnoresMarkerBeyondMaxDepth(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "toodeep", 4, old)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (.dolt marker beyond depth 3)", result.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_SkipsLsofHeldDir(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "held1", 2, old)

	held := func(context.Context) ([]byte, error) {
		return []byte("dolt    1234 root   12r   REG  8,1  4096 55555 " + dir + "/noms/oldgen/000001.chunk\n"), nil
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: held})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (lsof-held)", result.Removed)
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_LsofErrorFailsClosed(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "orphan1", 1, old)

	boom := errors.New("lsof: command not found")
	failing := func(context.Context) ([]byte, error) { return nil, boom }

	result := Sweep(SweepConfig{Root: root, RunLsof: failing})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none when lsof fails (fail closed)", result.Removed)
	}
	if len(result.Errors) == 0 {
		t.Fatalf("expected an error to be reported when lsof fails")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_ContinuesAfterOneRemovalFails(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dirA := mkStoreDir(t, root, "orphanA", 1, old)
	dirB := mkStoreDir(t, root, "orphanB", 1, old)

	removeAll := func(path string) error {
		if path == dirA {
			return errors.New("permission denied")
		}
		return os.RemoveAll(path)
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits, RemoveAll: removeAll})

	if len(result.Removed) != 1 || result.Removed[0] != dirB {
		t.Fatalf("Removed = %v, want [%s]", result.Removed, dirB)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %v, want exactly one", result.Errors)
	}
	if _, err := os.Stat(dirA); err != nil {
		t.Fatalf("dirA should still exist after failed removal: %v", err)
	}
	if _, err := os.Stat(dirB); !os.IsNotExist(err) {
		t.Fatalf("dirB should have been removed: %v", err)
	}
}

func TestSweep_SkipsNonDirectoryEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "not-a-dir"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "not-a-dir"), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 || len(result.Errors) != 0 {
		t.Fatalf("result = %+v, want no-op for a plain file", result)
	}
}

func TestSweep_RootReadErrorIsReported(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	result := Sweep(SweepConfig{Root: missing, RunLsof: noLsofHits})

	if len(result.Errors) == 0 {
		t.Fatalf("expected an error reading a missing root")
	}
	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none", result.Removed)
	}
}

func TestSweep_DefaultMinAgeAppliesWhenUnset(t *testing.T) {
	root := t.TempDir()
	justUnderDefault := time.Now().Add(-DefaultMinAge + time.Minute)
	dir := mkStoreDir(t, root, "borderline", 1, justUnderDefault)

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (younger than DefaultMinAge)", result.Removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

func TestSweep_UsesInjectedClock(t *testing.T) {
	root := t.TempDir()
	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	dir := mkStoreDir(t, root, "orphan1", 1, fixed.Add(-2*time.Hour))

	fake := &clock.Fake{Time: fixed}
	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits, Clock: fake})

	if len(result.Removed) != 1 || result.Removed[0] != dir {
		t.Fatalf("Removed = %v, want [%s] under fake clock", result.Removed, dir)
	}
}

func TestSweep_MultipleCandidatesMixedOutcomes(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-time.Minute)

	removeMe := mkStoreDir(t, root, "remove-me", 2, old)
	tooYoung := mkStoreDir(t, root, "too-young", 2, recent)
	noMarker := mkStoreDir(t, root, "no-marker", 0, old)

	held := func(context.Context) ([]byte, error) {
		return []byte(filepath.Join(root, "held-dir") + "/noms/x.chunk\n"), nil
	}
	heldDir := mkStoreDir(t, root, "held-dir", 1, old)

	result := Sweep(SweepConfig{Root: root, RunLsof: held})

	if len(result.Removed) != 1 || result.Removed[0] != removeMe {
		t.Fatalf("Removed = %v, want exactly [%s]", result.Removed, removeMe)
	}
	for _, d := range []string{tooYoung, noMarker, heldDir} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("dir %s should still exist: %v", d, err)
		}
	}
}

// TestSweep_ConfirmsUnheldWithSecondLsofScanBeforeRemoving reproduces
// ga-vbyn8v: under heavy host load, a single lsof scan can transiently miss
// a still-open file for a live process without lsof itself erroring. Sweep
// must not trust a lone "unheld" reading before deleting anything.
func TestSweep_ConfirmsUnheldWithSecondLsofScanBeforeRemoving(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "flaky-held", 2, old)

	calls := 0
	flaky := func(context.Context) ([]byte, error) {
		calls++
		if calls == 1 {
			// First scan transiently misses the live holder.
			return nil, nil
		}
		// Confirming second scan sees it.
		return []byte(dir + "/noms/oldgen/000001.chunk\n"), nil
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: flaky})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (confirm scan caught what the first scan missed)", result.Removed)
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
	if calls != 2 {
		t.Fatalf("RunLsof called %d times, want exactly 2 (initial + confirm)", calls)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

// TestSweep_SkipsConfirmScanWhenFirstScanAlreadyHeld proves the confirm
// scan is only spent on candidates about to be deleted, not on every
// candidate — a directory already known held doesn't need reconfirming.
func TestSweep_SkipsConfirmScanWhenFirstScanAlreadyHeld(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "held1", 2, old)

	calls := 0
	held := func(context.Context) ([]byte, error) {
		calls++
		return []byte(dir + "/noms/oldgen/000001.chunk\n"), nil
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: held})

	if len(result.Removed) != 0 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want held dir skipped without removal", result)
	}
	if calls != 1 {
		t.Fatalf("RunLsof called %d times, want exactly 1 (no confirm scan needed)", calls)
	}
}

// TestSweep_RemovesWhenBothScansAgreeUnheld guards against a regression
// where the confirm scan makes removal impossible: a genuinely orphaned
// directory that both scans agree is unheld must still be removed.
func TestSweep_RemovesWhenBothScansAgreeUnheld(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "orphan1", 1, old)

	calls := 0
	unheld := func(context.Context) ([]byte, error) {
		calls++
		return nil, nil
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: unheld})

	if len(result.Removed) != 1 || result.Removed[0] != dir {
		t.Fatalf("Removed = %v, want [%s]", result.Removed, dir)
	}
	if calls != 2 {
		t.Fatalf("RunLsof called %d times, want exactly 2 (initial + confirm)", calls)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir %s should have been removed, stat err = %v", dir, err)
	}
}

// TestSweep_ConfirmScanErrorFailsClosed extends the fail-closed contract to
// the confirm scan: if it errors, treat the candidate as held rather than
// falling back to the (possibly wrong) first-scan reading.
func TestSweep_ConfirmScanErrorFailsClosed(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "orphan1", 1, old)

	calls := 0
	boom := errors.New("lsof: resource temporarily unavailable")
	flaky := func(context.Context) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return nil, boom
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: flaky})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none when the confirm scan fails (fail closed)", result.Removed)
	}
	if len(result.Errors) == 0 {
		t.Fatalf("expected an error to be reported when the confirm lsof scan fails")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

// TestSweep_RespectsRealDoltLockEvenWhenLsofMissesIt reproduces ga-63rfxj:
// lsof is a point-in-time /proc snapshot and can transiently miss a still-
// open file for a live process under heavy host load without erroring, on
// both the initial and the confirm scan alike (the gate-fail evidence for
// ga-vbyn8v showed exactly this — a live dolt sql-server's data dir removed
// under a 40-job parallel run despite the two-scan confirm). Dolt's NBS
// store independently holds a real kernel flock on <dir>/.dolt/noms/LOCK
// for as long as it has the store open; that lock is atomic and race-free
// because it's enforced by the kernel, not reconstructed from a process
// listing. This test holds a real fslock on a real LOCK file and configures
// lsof to report the directory clean on every scan, proving Sweep must
// still refuse to remove it.
func TestSweep_RespectsRealDoltLockEvenWhenLsofMissesIt(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "live-dolt-store", 1, old)

	nomsDir := filepath.Join(dir, ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", nomsDir, err)
	}
	lockPath := filepath.Join(nomsDir, "LOCK")
	lock, err := fslock.New(lockPath)
	if err != nil {
		t.Fatalf("fslock.New(%s): %v", lockPath, err)
	}
	if err := lock.TryLock(); err != nil {
		t.Fatalf("TryLock(%s): %v", lockPath, err)
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			t.Errorf("Unlock(%s): %v", lockPath, err)
		}
	}()

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (real dolt LOCK held, even though lsof missed it on both scans)", result.Removed)
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}

// TestSweep_RemovesWhenDoltLockFileExistsButIsUnheld guards against the
// real-lock check over-blocking: a LOCK file left on disk by a store that
// has since closed (and so released its flock) must not itself prevent a
// genuinely orphaned directory from being removed.
func TestSweep_RemovesWhenDoltLockFileExistsButIsUnheld(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "stale-lock-file", 1, old)

	nomsDir := filepath.Join(dir, ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", nomsDir, err)
	}
	lockPath := filepath.Join(nomsDir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", lockPath, err)
	}

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 1 || result.Removed[0] != dir {
		t.Fatalf("Removed = %v, want [%s] (LOCK file present on disk but not held)", result.Removed, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir %s should have been removed, stat err = %v", dir, err)
	}
}

// TestSweep_LockSearchTraversalErrorFailsClosed covers the gap between the
// two fail-closed checks that already existed: an unopenable LOCK file fails
// closed via probeLockHeld, but a subtree we cannot even list used to look
// identical to "no LOCK here" and permitted removal. ReadDir can fail with
// EMFILE under exactly the heavy parallel load this whole check exists for.
func TestSweep_LockSearchTraversalErrorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; cannot force a ReadDir failure")
	}
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	dir := mkStoreDir(t, root, "unreadable-noms", 1, old)

	nomsDir := filepath.Join(dir, ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", nomsDir, err)
	}
	if err := os.Chmod(nomsDir, 0o000); err != nil {
		t.Fatalf("Chmod(%s): %v", nomsDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(nomsDir, 0o755) })

	result := Sweep(SweepConfig{Root: root, RunLsof: noLsofHits})

	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (lock search unverifiable; fail closed)", result.Removed)
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
	if len(result.Errors) == 0 {
		t.Fatalf("expected an error reported for the unreadable lock search")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir %s should still exist: %v", dir, err)
	}
}
