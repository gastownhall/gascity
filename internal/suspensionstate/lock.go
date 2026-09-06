package suspensionstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// updateFlockTimeout caps cross-process flock acquisition in Update.
// Mirrors internal/events/recorder.go's recordFlockTimeout: local-FS
// flock release latency is sub-millisecond on darwin/linux, so 250 ms
// is well above any reasonable single load-mutate-save critical
// section yet far below a user-perceptible CLI stall. A dead writer
// that held the lock is reaped by the kernel asynchronously —
// blocking on it indefinitely could hang `gc rig suspend`/`resume`.
const updateFlockTimeout = 250 * time.Millisecond

// updateFlockRetryInterval is the fixed cadence between non-blocking
// flock attempts. Fixed over exponential because contention is short
// and uniform timing simplifies test assertions.
const updateFlockRetryInterval = 5 * time.Millisecond

// lockPath returns the path to the sibling advisory-lock file for the
// city's suspension-state.json. A stable sibling file is used instead
// of flocking the data file itself because Save's atomic
// temp-file-plus-rename dance replaces the data file's inode on every
// write, which would make locking it directly meaningless across
// writes — the lock file's inode never changes.
func lockPath(cityPath string) string {
	return citylayout.SuspensionStateFile(cityPath) + ".lock"
}

// Update performs a locked read-modify-write of the runtime
// suspension state. It acquires a cross-process advisory file lock
// (flock on a sibling lock file), loads the current state, applies
// mutate to a pointer to it, and saves the result — closing the gap
// where two concurrent load-mutate-save callers (e.g. concurrent `gc
// rig suspend`/`resume` invocations) could otherwise silently lose
// one another's update (last-write-wins), since Save's atomic write
// prevents a torn file but does nothing to serialize the
// read-modify-write sequence around it.
//
// If mutate returns an error, Update returns it unchanged and does
// not save. The lock is always released before Update returns.
//
// The flock is real-OS-file-backed (see lockUpdateFile) and is only
// meaningful when fs is backed by the real filesystem: with
// fsys.OSFS, Update opens (and, if needed, creates) the sibling lock
// file directly via os.OpenFile, bypassing the fs abstraction exactly
// as internal/events/recorder.go's FileRecorder does for its own
// locked file. Callers that inject a non-OSFS fs (an in-memory fake
// used in unit tests, which has no real sibling file to lock and no
// concurrent OS processes to serialize against) skip the OS lock
// entirely and fall back to a plain Load/mutate/Save sequence.
func Update(fs fsys.FS, cityPath string, mutate func(*State) error) error {
	if _, isRealFS := fs.(fsys.OSFS); isRealFS {
		p := lockPath(cityPath)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fmt.Errorf("creating suspension state directory: %w", err)
		}
		lockFile, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return fmt.Errorf("opening suspension state lock: %w", err)
		}
		defer lockFile.Close() //nolint:errcheck // best-effort close after unlock

		fd := int(lockFile.Fd())
		if err := lockUpdateFile(fd, p); err != nil {
			return fmt.Errorf("locking suspension state: %w", err)
		}
		defer func() {
			_ = syscall.Flock(fd, syscall.LOCK_UN) //nolint:errcheck // best-effort unlock
		}()
	}

	st, err := Load(fs, cityPath)
	if err != nil {
		return err
	}
	if err := mutate(&st); err != nil {
		return err
	}
	return Save(fs, cityPath, st)
}

// lockUpdateFile acquires an exclusive advisory lock on fd, retrying
// on a fixed cadence until it succeeds or updateFlockTimeout elapses.
// Mirrors internal/events/recorder.go's lockRecorderFile.
func lockUpdateFile(fd int, path string) error {
	deadline := time.Now().Add(updateFlockTimeout)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %dms waiting on flock at %s", updateFlockTimeout.Milliseconds(), path)
		}
		time.Sleep(updateFlockRetryInterval)
	}
}
