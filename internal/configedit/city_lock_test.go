package configedit_test

import (
	"os"
	"path/filepath"
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
