package doltorphan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
