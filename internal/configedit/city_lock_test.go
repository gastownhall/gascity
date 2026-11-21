package configedit_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/testutil"
)

// LockCity on a filesystem other processes cannot share takes no lock and
// creates nothing, on disk or in the fake, even while the OS lock is held.
func TestLockCityOnFakeFSTakesNoLock(t *testing.T) {
	cityDir := t.TempDir()
	fake := fsys.NewFake()

	release, err := configedit.LockCity(fake, cityDir, func() { t.Error("LockCity on fsys.Fake reported a wait") })
	if err != nil {
		t.Fatalf("LockCity(fake): %v", err)
	}
	release()
	release()
	if _, err := os.Stat(filepath.Dir(configedit.CityLockFile(cityDir))); !os.IsNotExist(err) {
		t.Fatalf("lock dir on disk: stat err = %v, want not exist", err)
	}
	if len(fake.Files) != 0 || len(fake.Dirs) != 0 {
		t.Fatalf("LockCity(fake) touched the fake: files %v, dirs %v", fake.Files, fake.Dirs)
	}
	for _, call := range fake.Calls {
		t.Errorf("LockCity(fake) called the fake: %+v", call)
	}

	releaseOS, err := configedit.LockCity(fsys.OSFS{}, cityDir, nil)
	if err != nil {
		t.Fatalf("LockCity(OSFS): %v", err)
	}
	t.Cleanup(releaseOS)
	done := make(chan error, 1)
	go func() {
		release, err := configedit.LockCity(fake, cityDir, nil)
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("LockCity(fake) with the OS lock held: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("LockCity(fake) waited on the OS lock")
	}
}

// TryLockCity takes the city config lock when it is free. While another holder
// has it, TryLockCity takes nothing and fails at once with ErrCityLocked,
// naming the lock file; on a filesystem other processes cannot share it takes
// no lock even then.
func TestTryLockCityFailsAtOnceWhileTheLockIsHeld(t *testing.T) {
	cityDir := t.TempDir()
	releaseHeld, err := configedit.LockCity(fsys.OSFS{}, cityDir, nil)
	if err != nil {
		t.Fatalf("LockCity: %v", err)
	}
	t.Cleanup(releaseHeld)

	done := make(chan error, 1)
	go func() {
		release, err := configedit.TryLockCity(fsys.OSFS{}, cityDir)
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, configedit.ErrCityLocked) {
			t.Fatalf("TryLockCity with the lock held = %v, want ErrCityLocked", err)
		}
		if !strings.Contains(err.Error(), configedit.CityLockFile(cityDir)) {
			t.Fatalf("TryLockCity error = %q, want it to name %s", err, configedit.CityLockFile(cityDir))
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("TryLockCity waited for the held lock")
	}
	releaseFake, err := configedit.TryLockCity(fsys.NewFake(), cityDir)
	if err != nil {
		t.Fatalf("TryLockCity(fake) with the OS lock held: %v", err)
	}
	releaseFake()

	releaseHeld()
	release, err := configedit.TryLockCity(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("TryLockCity with the lock free: %v", err)
	}
	if _, err := configedit.TryLockCity(fsys.OSFS{}, cityDir); !errors.Is(err, configedit.ErrCityLocked) {
		t.Fatalf("TryLockCity while an earlier TryLockCity holds the lock = %v, want ErrCityLocked", err)
	}
	release()
	release()
	again, err := configedit.TryLockCity(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("TryLockCity after the release: %v", err)
	}
	again()
}
