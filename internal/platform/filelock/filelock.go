package filelock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Mode selects the lock sharing mode.
type Mode int

const (
	Shared Mode = iota
	Exclusive
)

// ErrBusy identifies a contended nonblocking lock acquisition.
var ErrBusy = errors.New("file lock busy")

// Options configures file lock acquisition.
type Options struct {
	Mode        Mode
	NonBlocking bool
	Timeout     time.Duration
	RetryEvery  time.Duration
	Label       string
}

func (o Options) retryEvery() time.Duration {
	if o.RetryEvery > 0 {
		return o.RetryEvery
	}
	return 10 * time.Millisecond
}

func (o Options) label() string {
	if o.Label != "" {
		return o.Label
	}
	return "file"
}

// Lock is a kernel-held file lock. Release must be called when the protected
// critical section ends.
type Lock struct {
	f        *os.File
	release  func() error
	released bool
}

// Acquire takes a kernel file lock on f.
func Acquire(f *os.File, opts Options) (*Lock, error) {
	if f == nil {
		return nil, fmt.Errorf("locking %s: nil file", opts.label())
	}
	if opts.Timeout > 0 {
		deadline := time.Now().Add(opts.Timeout)
		for {
			lock, err := tryAcquire(f, opts, true)
			if err == nil {
				return lock, nil
			}
			if !errors.Is(err, ErrBusy) {
				return nil, err
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("timed out after %dms waiting on lock at %s", opts.Timeout.Milliseconds(), opts.label())
			}
			time.Sleep(opts.retryEvery())
		}
	}
	return tryAcquire(f, opts, opts.NonBlocking)
}

// AcquireAdvisoryFile serializes writers of the file at path without
// preventing unlocked readers from reading it. The lock is held on a sidecar,
// path + ".lock", that nothing renames or replaces, so it identifies the path
// rather than the file currently there: writers holding different handles,
// one opened before the file at path was renamed away and replaced and one
// opened after, still exclude each other. Unix also locks f itself, so a
// process that locks only the data file stays excluded; Windows does not,
// because byte-range locks there are mandatory and would block readers.
func AcquireAdvisoryFile(f *os.File, path string, opts Options) (*Lock, error) {
	if f == nil {
		return nil, fmt.Errorf("locking %s: nil file", opts.label())
	}
	return acquireAdvisoryFile(f, path, opts)
}

// AdvisoryLockPath returns the sidecar that [AcquireAdvisoryFile] locks for
// path.
func AdvisoryLockPath(path string) string {
	return path + ".lock"
}

// pathErrorCause drops the path an [fs.PathError] adds, which repeats the lock
// file's path or names the probe file instead of it.
func pathErrorCause(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

// acquireSidecar takes the lock on the sidecar for path, creating it if
// needed, and closes it again on release.
func acquireSidecar(path string, opts Options) (*Lock, error) {
	lockPath := AdvisoryLockPath(path)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening advisory lock file %q: %w", lockPath, err)
	}
	lock, err := Acquire(f, opts)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &Lock{
		f: f,
		release: func() error {
			return errors.Join(lock.Release(), f.Close())
		},
	}, nil
}

// OpenDirectoryLock opens the stable lock identity for a directory. Unix uses
// the directory itself; Windows maps its canonical path to a per-user lock file
// because LockFileEx rejects directory handles.
func OpenDirectoryLock(path string) (*os.File, string, error) {
	return openDirectoryLock(path)
}

// OpenDirectoryForSync opens a directory handle suitable for [os.File.Sync].
// Windows requires a native directory handle with write access; os.Open does
// not request it.
func OpenDirectoryForSync(path string) (*os.File, error) {
	return openDirectoryForSync(path)
}

// Release releases the lock. A second Release is a no-op.
func (l *Lock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	return l.release()
}

// With acquires a lock for fn and releases it before returning.
func With(f *os.File, opts Options, fn func() error) (returnErr error) {
	lock, err := Acquire(f, opts)
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	return fn()
}
