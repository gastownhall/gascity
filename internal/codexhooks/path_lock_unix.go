//go:build !windows

package codexhooks

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/fsys"
	"golang.org/x/sys/unix"
)

var atomicTempNonce uint64

func withExclusiveOSPathLock(parent, lockName string, fn func() error) error {
	dir, dirInfo, err := openPinnedDirectory(parent)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck

	fd, err := unix.Openat(int(dir.Fd()), lockName, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("opening Codex hooks lock %s: %w", filepath.Join(parent, lockName), err)
	}
	lockFile := os.NewFile(uintptr(fd), filepath.Join(parent, lockName))
	if lockFile == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("opening Codex hooks lock %s: %w", filepath.Join(parent, lockName), os.ErrInvalid)
	}
	defer lockFile.Close() //nolint:errcheck
	lockInfo, err := lockFile.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() {
		if err == nil {
			err = os.ErrInvalid
		}
		return fmt.Errorf("inspecting Codex hooks lock %s: %w", lockFile.Name(), err)
	}
	if err := lockFile.Chmod(0o600); err != nil {
		return fmt.Errorf("securing Codex hooks lock %s: %w", lockFile.Name(), err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return fmt.Errorf("locking Codex hooks path %s: %w", lockFile.Name(), err)
	}
	defer unix.Flock(fd, unix.LOCK_UN) //nolint:errcheck
	if err := verifyPinnedDirectory(parent, dirInfo); err != nil {
		return err
	}
	return fn()
}

func writeFileAtomicNoFollowOS(target string, data []byte, perm os.FileMode) (returnErr error) {
	parent := filepath.Dir(target)
	dir, dirInfo, err := openPinnedDirectory(parent)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck

	base := filepath.Base(target)
	nonce := atomic.AddUint64(&atomicTempNonce, 1)
	tempName := fmt.Sprintf(".%s.gascity-tmp-%d-%d", base, os.Getpid(), nonce)
	fd, err := unix.Openat(int(dir.Fd()), tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		return fmt.Errorf("creating Codex hooks temp file: %w", err)
	}
	temp := os.NewFile(uintptr(fd), filepath.Join(parent, tempName))
	if temp == nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(int(dir.Fd()), tempName, 0)
		return fmt.Errorf("creating Codex hooks temp file: %w", os.ErrInvalid)
	}
	defer func() {
		_ = temp.Close()
		if returnErr != nil {
			_ = unix.Unlinkat(int(dir.Fd()), tempName, 0)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("writing Codex hooks temp file: %w", err)
	}
	if err := temp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod Codex hooks temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("closing Codex hooks temp file: %w", err)
	}
	if err := verifyPinnedDirectory(parent, dirInfo); err != nil {
		return err
	}
	if err := unix.Renameat(int(dir.Fd()), tempName, int(dir.Fd()), base); err != nil {
		return fmt.Errorf("publishing Codex hooks file: %w", err)
	}
	if err := verifyPinnedDirectory(parent, dirInfo); err != nil {
		return err
	}
	return nil
}

func openPinnedDirectory(parent string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening Codex hooks parent %s without following symlinks: %w", parent, err)
	}
	dir := os.NewFile(uintptr(fd), parent)
	if dir == nil {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("opening Codex hooks parent %s: %w", parent, os.ErrInvalid)
	}
	info, err := dir.Stat()
	if err != nil || !info.IsDir() {
		_ = dir.Close()
		if err == nil {
			err = os.ErrInvalid
		}
		return nil, nil, fmt.Errorf("inspecting Codex hooks parent %s: %w", parent, err)
	}
	if err := verifyPinnedDirectory(parent, info); err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	return dir, info, nil
}

func verifyPinnedDirectory(parent string, pinned os.FileInfo) error {
	current, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("rechecking Codex hooks parent %s: %w", parent, err)
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !fsys.SameFileIdentity(pinned, current) {
		return fmt.Errorf("rechecking Codex hooks parent %s: path changed identity or became non-directory", parent)
	}
	return nil
}
