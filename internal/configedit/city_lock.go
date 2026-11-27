package configedit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/platform/filelock"
)

// CityLockFile returns the path of the lock file that serializes edits of a
// city's config files (city.toml, .gc/site.toml, the root pack.toml,
// packs.lock and agents/*/agent.toml) across processes. The file carries no
// state; only the kernel lock on it does.
func CityLockFile(cityRoot string) string {
	return citylayout.RuntimePath(cityRoot, "runtime", "city-config.lock")
}

// LockCity takes the city's config edit lock, an exclusive advisory lock that
// every process rewriting the city's config files holds from its first read
// of those files through its last write and any rollback, so an edit is never
// computed from a read another process's write has since replaced.
//
// It tries the lock without waiting first; when another holder has it,
// onWait (which may be nil) is called once and LockCity then blocks with no
// timeout until the lock is free. The kernel promises no wakeup order among
// waiters, and releases the lock if the holding process dies.
//
// Only the OS filesystem is shared with other processes: on any other
// [fsys.FS] LockCity takes no lock, creates nothing and returns a no-op
// release.
//
// Contract for holders:
//   - The lock belongs to the open lock file, not to the process, so a holder
//     that calls LockCity again for the same city waits on itself. Only the
//     owner of an edit transaction takes it; helpers that run inside a
//     transaction ([Editor] methods, config writers) never do.
//   - Never call the controller API or wait on the controller while holding
//     it: the controller takes the same lock for its API edits.
//   - Lock order: the city config lock before any in-process lock the edit
//     takes (the controller's config transaction and publication locks, the
//     [Editor] lock), so an edit waiting for another process holds nothing
//     other work in its own process needs; and before the suspension fence
//     (suspensionstate.Fence), never the reverse.
//
// The returned release is safe to call more than once.
func LockCity(fs fsys.FS, cityRoot string, onWait func()) (release func(), err error) {
	return lockCity(fs, cityRoot, true, onWait)
}

// ErrCityLocked is the error TryLockCity returns, in place of waiting, when
// another holder has the city config lock.
var ErrCityLocked = errors.New("another config edit of this city is in progress")

// TryLockCity is LockCity for a caller that must not wait: when another holder
// has the lock it takes nothing and returns an error that wraps
// [ErrCityLocked] and names the lock file. The holder contract is LockCity's.
func TryLockCity(fs fsys.FS, cityRoot string) (release func(), err error) {
	return lockCity(fs, cityRoot, false, nil)
}

// lockCity is LockCity when wait is set and TryLockCity when it is not.
func lockCity(fs fsys.FS, cityRoot string, wait bool, onWait func()) (func(), error) {
	if _, ok := fs.(fsys.OSFS); !ok {
		return func() {}, nil
	}
	path := CityLockFile(cityRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("opening city config lock %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening city config lock %s: %w", path, err)
	}
	opts := filelock.Options{Mode: filelock.Exclusive, NonBlocking: true, Label: path}
	lock, err := filelock.Acquire(f, opts)
	if errors.Is(err, filelock.ErrBusy) {
		if !wait {
			return nil, errors.Join(fmt.Errorf("%w (%s)", ErrCityLocked, path), f.Close())
		}
		if onWait != nil {
			onWait()
		}
		opts.NonBlocking = false
		lock, err = filelock.Acquire(f, opts)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("locking city config lock %s: %w", path, err), f.Close())
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = lock.Release()
			_ = f.Close()
		})
	}, nil
}
