//go:build !windows

package filelock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func tryAcquire(f *os.File, opts Options, nonblocking bool) (*Lock, error) {
	mode := syscall.LOCK_SH
	if opts.Mode == Exclusive {
		mode = syscall.LOCK_EX
	}
	if nonblocking {
		mode |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, fmt.Errorf("%w: %s", ErrBusy, opts.label())
		}
		return nil, err
	}
	return &Lock{
		f: f,
		release: func() error {
			return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		},
	}, nil
}

// acquireAdvisoryFile takes the sidecar first, so every writer queues on the
// one lock that survives replacement of the file at path, then f itself. The
// two waits share the caller's timeout.
func acquireAdvisoryFile(f *os.File, path string, opts Options) (*Lock, error) {
	deadline := time.Now().Add(opts.Timeout)
	sidecar, err := acquireSidecar(path, opts)
	if err != nil {
		return nil, err
	}
	if opts.Timeout > 0 {
		opts.Timeout = max(time.Until(deadline), time.Nanosecond)
	}
	data, err := Acquire(f, opts)
	if err != nil {
		return nil, errors.Join(err, sidecar.Release())
	}
	return &Lock{
		f: f,
		release: func() error {
			return errors.Join(data.Release(), sidecar.Release())
		},
	}, nil
}

func openDirectoryLock(path string) (*os.File, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

func openDirectoryForSync(path string) (*os.File, error) {
	return os.Open(path)
}
