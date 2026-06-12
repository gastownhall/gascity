package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// makeDoltDataDirWithLock creates a managed-dolt-shaped data dir with one
// database whose noms LOCK file exists, returning the data dir and lock path.
func makeDoltDataDirWithLock(t *testing.T) (string, string) {
	t.Helper()
	dataDir := t.TempDir()
	nomsDir := filepath.Join(dataDir, "dolt", ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("mkdir noms dir: %v", err)
	}
	lockPath := filepath.Join(nomsDir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	return dataDir, lockPath
}

// holdFlock takes an exclusive flock on path and returns a release func.
// flock conflicts are per open-file-description, so a second open in this
// same process observes the lock as held — exactly how a separate dolt
// process would.
func holdFlock(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatalf("flock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	t.Cleanup(release)
	return release
}

func TestManagedDoltDataDirLockHolderFreeWhenNoLockFiles(t *testing.T) {
	if holder := managedDoltDataDirLockHolder(t.TempDir()); holder != "" {
		t.Fatalf("expected no holder for empty data dir, got %q", holder)
	}
}

func TestManagedDoltDataDirLockHolderFreeWhenMissingDataDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if holder := managedDoltDataDirLockHolder(missing); holder != "" {
		t.Fatalf("expected no holder for missing data dir, got %q", holder)
	}
}

func TestManagedDoltDataDirLockHolderFreeWhenUnheld(t *testing.T) {
	dataDir, _ := makeDoltDataDirWithLock(t)
	if holder := managedDoltDataDirLockHolder(dataDir); holder != "" {
		t.Fatalf("expected no holder for unheld lock, got %q", holder)
	}
}

func TestManagedDoltDataDirLockHolderDetectsHeldLock(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	release := holdFlock(t, lockPath)
	if holder := managedDoltDataDirLockHolder(dataDir); holder != lockPath {
		t.Fatalf("expected holder %q, got %q", lockPath, holder)
	}
	release()
	if holder := managedDoltDataDirLockHolder(dataDir); holder != "" {
		t.Fatalf("expected no holder after release, got %q", holder)
	}
}

func TestManagedDoltDataDirLockHolderDetectsHeldLockUnderGlobMetacharPath(t *testing.T) {
	// A literal data-dir path containing glob metacharacters must not be
	// treated as a pattern: an unmatched `[` makes filepath.Glob error out
	// (silently dropping the probe) and `?`/`*` match the wrong paths —
	// either way the guard would miss a held LOCK and re-open the #3174
	// race for any city at such a path.
	dataDir := filepath.Join(t.TempDir(), "city [prod ?*")
	nomsDir := filepath.Join(dataDir, "dolt", ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("mkdir noms dir: %v", err)
	}
	lockPath := filepath.Join(nomsDir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	holdFlock(t, lockPath)
	if holder := managedDoltDataDirLockHolder(dataDir); holder != lockPath {
		t.Fatalf("expected holder %q, got %q", lockPath, holder)
	}
}

func TestManagedDoltDataDirLockHolderDetectsRootLevelLock(t *testing.T) {
	dataDir := t.TempDir()
	nomsDir := filepath.Join(dataDir, ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("mkdir noms dir: %v", err)
	}
	lockPath := filepath.Join(nomsDir, "LOCK")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	holdFlock(t, lockPath)
	if holder := managedDoltDataDirLockHolder(dataDir); holder != lockPath {
		t.Fatalf("expected holder %q, got %q", lockPath, holder)
	}
}

func TestWaitForManagedDoltDataDirLockFreeImmediateWhenUnheld(t *testing.T) {
	dataDir, _ := makeDoltDataDirWithLock(t)
	start := time.Now()
	if err := waitForManagedDoltDataDirLockFree(dataDir, 5*time.Second); err != nil {
		t.Fatalf("expected nil for unheld lock, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("expected immediate return for unheld lock, took %s", elapsed)
	}
}

func TestWaitForManagedDoltDataDirLockFreeFailsClosedOnTimeout(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	holdFlock(t, lockPath)
	err := waitForManagedDoltDataDirLockFree(dataDir, 150*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when lock is held past the timeout")
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("expected error to name the held lock %q, got %v", lockPath, err)
	}
}

func TestWaitForManagedDoltDataDirLockFreeZeroTimeoutProbesOnce(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	holdFlock(t, lockPath)
	if err := waitForManagedDoltDataDirLockFree(dataDir, 0); err == nil {
		t.Fatal("expected fail-closed error for held lock with zero timeout")
	}
}

func TestResolveManagedDoltLockReleaseTimeoutEmptyCityPathReturnsDefault(t *testing.T) {
	got := resolveManagedDoltLockReleaseTimeout("")
	if got != config.DefaultDoltLockReleaseTimeout {
		t.Fatalf("empty cityPath: got %s, want %s", got, config.DefaultDoltLockReleaseTimeout)
	}
}

func TestResolveManagedDoltLockReleaseTimeoutMissingCityTomlReturnsDefault(t *testing.T) {
	got := resolveManagedDoltLockReleaseTimeout(t.TempDir())
	if got != config.DefaultDoltLockReleaseTimeout {
		t.Fatalf("missing city.toml: got %s, want %s", got, config.DefaultDoltLockReleaseTimeout)
	}
}

func TestResolveManagedDoltLockReleaseTimeoutFromCityToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test-city"

[dolt]
dolt_lock_release_timeout = "9s"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltLockReleaseTimeout(dir)
	if got != 9*time.Second {
		t.Fatalf("from city.toml: got %s, want 9s", got)
	}
}

func TestWaitForManagedDoltDataDirLockFreeRecoversOnRelease(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	release := holdFlock(t, lockPath)
	done := make(chan error, 1)
	go func() {
		done <- waitForManagedDoltDataDirLockFree(dataDir, 10*time.Second)
	}()
	time.Sleep(300 * time.Millisecond)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected wait to succeed after release, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not observe lock release")
	}
}
