//go:build windows

package filelock

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func tryAcquire(f *os.File, opts Options, nonblocking bool) (*Lock, error) {
	flags := uint32(0)
	if opts.Mode == Exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if nonblocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}

	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("creating lock completion event: %w", err)
	}
	ov := windows.Overlapped{HEvent: event}
	acquired, err := acquireWindowsFileLock(windows.Handle(f.Fd()), flags, nonblocking, &ov, opts.label())
	_ = windows.CloseHandle(event)
	ov.HEvent = 0
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, fmt.Errorf("%w: %s", ErrBusy, opts.label())
	}

	return &Lock{
		f: f,
		release: func() error {
			return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ov)
		},
	}, nil
}

func acquireWindowsFileLock(handle windows.Handle, flags uint32, nonblocking bool, ov *windows.Overlapped, label string) (bool, error) {
	err := windows.LockFileEx(handle, flags, 0, 1, 0, ov)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		if nonblocking && errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return false, nil
		}
		return false, err
	}

	if nonblocking {
		cancelErr := windows.CancelIoEx(handle, ov)
		var transferred uint32
		completionErr := windows.GetOverlappedResult(handle, ov, &transferred, true)
		switch {
		case completionErr == nil:
			return true, nil
		case errors.Is(completionErr, windows.ERROR_OPERATION_ABORTED):
			if cancelErr != nil && !errors.Is(cancelErr, windows.ERROR_NOT_FOUND) {
				return false, errors.Join(
					fmt.Errorf("cancelling pending lock at %s: %w", label, cancelErr),
					fmt.Errorf("draining cancelled lock at %s: %w", label, completionErr),
				)
			}
			return false, nil
		default:
			if cancelErr != nil && !errors.Is(cancelErr, windows.ERROR_NOT_FOUND) {
				return false, errors.Join(cancelErr, completionErr)
			}
			return false, completionErr
		}
	}

	var transferred uint32
	if err := windows.GetOverlappedResult(handle, ov, &transferred, true); err != nil {
		return false, fmt.Errorf("waiting for lock at %s: %w", label, err)
	}
	return true, nil
}

func acquireAdvisoryFile(_ *os.File, path string, opts Options) (*Lock, error) {
	return acquireSidecar(path, opts)
}

func openDirectoryLock(path string) (*os.File, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolving directory lock path %q: %w", path, err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, "", fmt.Errorf("resolving directory lock target %q: %w", path, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, "", fmt.Errorf("inspecting directory lock target %q: %w", path, err)
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("directory lock target %q is not a directory", path)
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, "", fmt.Errorf("locating per-user lock root: %w", err)
	}
	lockRoot := filepath.Join(cacheDir, "gascity", "locks", "directories")
	if err := os.MkdirAll(lockRoot, 0o700); err != nil {
		return nil, "", fmt.Errorf("creating per-user lock root %q: %w", lockRoot, err)
	}

	key := sha256.Sum256([]byte(strings.ToUpper(filepath.Clean(canonical))))
	lockPath := filepath.Join(lockRoot, fmt.Sprintf("%x.lock", key))
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("opening directory lock file %q: %w", lockPath, err)
	}
	return f, lockPath, nil
}

func openDirectoryForSync(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("opening directory %q for sync: invalid file handle", path)
	}
	return f, nil
}
