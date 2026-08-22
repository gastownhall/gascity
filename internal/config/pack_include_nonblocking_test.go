//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockRepoCacheRoot takes mode on an existing cache root's lock file. Unlike
// holdRepoCacheLock it locks a root the caller already owns, so a fixture's
// real cache root can be contended.
//
// The returned release is idempotent, so a test that has to release early —
// any test that parks a goroutine on the lock and must join it before
// returning — can call it and still let the t.Cleanup backstop cover the
// paths that don't.
func lockRepoCacheRoot(t *testing.T, root string, mode int) (release func()) {
	t.Helper()
	lockFile, err := os.OpenFile(filepath.Join(root, repoCacheLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), mode); err != nil {
		lockFile.Close() //nolint:errcheck
		t.Fatalf("Flock(%d): %v", mode, err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
			lockFile.Close()                                   //nolint:errcheck
		})
	}
	t.Cleanup(release)
	return release
}

// The non-blocking flag has to reach the lock, not just the signature: an
// unused parameter compiles fine and silently leaves every advisory load
// waiting behind whoever is cloning.
func TestResolvePackRefNonBlockingRefusesBusyCache(t *testing.T) {
	const ref = "https://github.com/example/packs/tree/main/gastown"
	cityDir, cacheDir := setupLockedPackRefTest(t, ref)
	lockRepoCacheRoot(t, filepath.Dir(cacheDir), syscall.LOCK_EX)

	done := make(chan error, 1)
	go func() {
		_, err := resolvePackRef(ref, cityDir, cityDir, true)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrRepoCacheBusy) {
			t.Fatalf("err = %v, want ErrRepoCacheBusy", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("non-blocking resolvePackRef waited on a held repo-cache lock")
	}
}

// The control: blocking stays blocking. Without this, dropping the flag and
// making every caller non-blocking would pass the test above while breaking
// installers, prune and bootstrap, which must see the cache's real contents.
func TestResolvePackRefBlockingWaitsOnBusyCache(t *testing.T) {
	const ref = "https://github.com/example/packs/tree/main/gastown"
	cityDir, cacheDir := setupLockedPackRefTest(t, ref)
	release := lockRepoCacheRoot(t, filepath.Dir(cacheDir), syscall.LOCK_EX)

	done := make(chan error, 1)
	go func() {
		_, err := resolvePackRef(ref, cityDir, cityDir, false)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("blocking resolvePackRef returned %v while the cache lock was held", err)
	case <-time.After(500 * time.Millisecond):
	}

	// Release and join here rather than leaving the goroutine parked for
	// t.Cleanup. Cleanups run LIFO, so the flock release would fire first and
	// wake the goroutine into the rest of this test's teardown: it goes on to
	// read runRepoCacheGit, which setupLockedPackRefTest's own cleanup is
	// restoring, and to walk a t.TempDir that is being removed. That is a real
	// ~7% failure under -race, not a theoretical one.
	release()
	<-done
}
