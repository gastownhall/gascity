//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const advisoryLockRetryInterval = 10 * time.Millisecond

type unixAdvisoryLock struct {
	once sync.Once
	fd   int
	err  error
}

func (directory *unixStorageDirectory) acquireLock(ctx context.Context, name string) (storageLockBackend, error) {
	if !directory.mutable {
		return nil, errors.New("productmetrics: read-only storage cannot acquire a lock")
	}
	if !directory.rootDirectory {
		return nil, errors.New("productmetrics: advisory locks are available only at the storage root")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("productmetrics: acquire lock %q: %w", name, err)
	}
	directoryFD, err := directory.duplicateFD()
	if err != nil {
		return nil, err
	}
	defer closeUnixFD(directoryFD)
	path := filepath.Join(directory.path, name)
	lockFD, created, err := openStableLockFile(directoryFD, name)
	if err != nil {
		return nil, storagePathError("open advisory lock", path, err)
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = unix.Close(lockFD)
		}
	}()
	if created {
		if err := unix.Fchmod(lockFD, 0o600); err != nil {
			return nil, fmt.Errorf("productmetrics: set advisory-lock mode: %w", err)
		}
	}
	if _, err := validateOpenedRegularFile(directoryFD, name, lockFD, path, directory.euid, created, directory.hooks); err != nil {
		return nil, err
	}
	if created {
		if err := syncFileFD(lockFD, directory.hooks); err != nil {
			return nil, fmt.Errorf("productmetrics: sync new advisory lock: %w", err)
		}
		if err := syncDirectoryFD(directoryFD, directory.hooks); err != nil {
			return nil, fmt.Errorf("productmetrics: sync advisory-lock directory: %w", err)
		}
	}

	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("productmetrics: acquire lock %q: %w", name, err)
		}
		if err := directory.hooks.run(storageStepLock); err != nil {
			return nil, fmt.Errorf("productmetrics: injected advisory-lock failure: %w", err)
		}
		err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			directoryMetadata, validationErr := metadataForFD(directoryFD, directory.path, directory.hooks)
			if validationErr == nil {
				validationErr = validatePrivateDirectory(directoryMetadata, directory.path, directory.euid, false)
			}
			if validationErr != nil {
				_ = unix.Flock(lockFD, unix.LOCK_UN)
				return nil, validationErr
			}
			if _, validationErr := validateOpenedRegularFile(directoryFD, name, lockFD, path, directory.euid, false, directory.hooks); validationErr != nil {
				_ = unix.Flock(lockFD, unix.LOCK_UN)
				return nil, validationErr
			}
			closeLock = false
			return &unixAdvisoryLock{fd: lockFD}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return nil, fmt.Errorf("productmetrics: acquire advisory lock: %w", err)
		}
		timer.Reset(advisoryLockRetryInterval)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("productmetrics: acquire lock %q: %w", name, ctx.Err())
		case <-timer.C:
		}
	}
}

func openStableLockFile(directoryFD int, name string) (int, bool, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	for {
		fd, err := openFileAt(directoryFD, name, flags, 0)
		if err == nil {
			return fd, false, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return -1, false, err
		}
		fd, err = openFileAt(directoryFD, name, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
		if err == nil {
			return fd, true, nil
		}
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		return -1, false, err
	}
}

func (lock *unixAdvisoryLock) release() error {
	lock.once.Do(func() {
		unlockErr := unix.Flock(lock.fd, unix.LOCK_UN)
		closeErr := unix.Close(lock.fd)
		lock.fd = -1
		lock.err = errors.Join(unlockErr, closeErr)
	})
	return lock.err
}
