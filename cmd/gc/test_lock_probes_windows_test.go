//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/platform/filelock"
	"golang.org/x/sys/windows"
)

// probeLockHeldForTest reports whether another holder has the lock on the
// file at path, taking and releasing it when free.
func probeLockHeldForTest(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck // test probe
	lock, err := filelock.Acquire(f, filelock.Options{Mode: filelock.Exclusive, NonBlocking: true})
	if errors.Is(err, filelock.ErrBusy) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, lock.Release()
}

func nudgeQueueLockHeldForTest(city string) bool {
	held, _ := probeLockHeldForTest(nudgequeue.LockPath(city))
	return held
}

func sourceWorkflowLockHeld(t *testing.T, cityPath string) bool {
	t.Helper()
	names := sourceWorkflowLockFiles(t, cityPath)
	if len(names) != 1 {
		t.Fatalf("source-workflow lock files under %s = %v, want exactly the one the cook takes", cityPath, names)
	}
	held, err := probeLockHeldForTest(filepath.Join(citylayout.RuntimeDataDir(cityPath), "sling-source-locks", names[0]))
	if err != nil {
		t.Fatalf("probing source-workflow lock %s: %v", names[0], err)
	}
	return held
}

func holdNudgeQueueLockForTest(t *testing.T, city string) func() {
	t.Helper()
	path := nudgequeue.LockPath(city)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := filelock.Acquire(f, filelock.Options{Mode: filelock.Exclusive})
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = lock.Release()
		_ = f.Close()
	}
	t.Cleanup(release)
	return release
}

// statIno returns path's file index, Windows' counterpart of an inode.
func statIno(path string) (uint64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close() //nolint:errcheck // read-only handle
	var d windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &d); err != nil {
		return 0, false
	}
	return uint64(d.FileIndexHigh)<<32 | uint64(d.FileIndexLow), true
}
