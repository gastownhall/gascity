//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FS_NOCOW_FL is the "no copy-on-write" inode attribute. This is the flag
// that `chattr +C` sets. On btrfs it disables both copy-on-write AND
// transparent compression for the inode — new files created inside a
// directory with this flag inherit it automatically.
//
// We apply this to .gc/runtime/ to avoid pathological kworker thrashing on
// the hot append-only trace and event files: with btrfs zstd compression
// enabled, each small append forces a read-modify-write-recompress of the
// existing compressed extent. Setting NOCOW/+C disables both behaviors for
// new files and eliminates the thrashing.
//
// Note: the kernel also defines a separate FS_NOCOMP_FL (0x400) flag, but
// in practice FS_IOC_SETFLAGS returns EOPNOTSUPP for it on btrfs.
// FS_NOCOW_FL (0x00800000) is the flag the chattr(1) tool actually uses
// and the one btrfs honors at the inode level for this workload.
const fsNoCowFL = 0x00800000

// getInodeFlags wraps the FS_IOC_GETFLAGS ioctl. The kernel interface uses
// `long` (platform word size) — on amd64 that's 8 bytes — so we must pass
// an `int` (Go's platform-sized signed integer) pointer rather than int32.
// The IoctlSetPointerInt helper in golang.org/x/sys/unix narrows to int32
// which is incorrect on 64-bit platforms for this ioctl.
func getInodeFlags(fd int) (int, error) {
	var flags int
	// nolint:gosec // G103: unsafe.Pointer required for ioctl syscall argument
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.FS_IOC_GETFLAGS),
		uintptr(unsafe.Pointer(&flags)),
	)
	if errno != 0 {
		return 0, errno
	}
	return flags, nil
}

// setInodeFlags wraps the FS_IOC_SETFLAGS ioctl. Same word-size caveat as
// getInodeFlags applies — we pass a pointer to a platform-sized int.
func setInodeFlags(fd int, flags int) error {
	// nolint:gosec // G103: unsafe.Pointer required for ioctl syscall argument
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.FS_IOC_SETFLAGS),
		uintptr(unsafe.Pointer(&flags)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// setNoCompressAttr sets FS_NOCOW_FL on the given directory. It is a no-op
// (returning nil) on filesystems that do not support FS_IOC_SETFLAGS, which
// includes tmpfs, ext4 without the feature, overlayfs, and similar. A missing
// path is reported as an error; other unexpected errors are returned as-is.
//
// This targets btrfs specifically, where zstd compression on hot append-only
// files in .gc/runtime/ causes kworker thrashing via read-modify-write extent
// recompression. Suppressing the flag elsewhere is harmless.
func setNoCompressAttr(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	fd := int(f.Fd())
	flags, err := getInodeFlags(fd)
	if err != nil {
		if isUnsupportedIoctlErr(err) {
			return nil
		}
		return err
	}

	if flags&fsNoCowFL != 0 {
		return nil
	}

	if err := setInodeFlags(fd, flags|fsNoCowFL); err != nil {
		if isUnsupportedIoctlErr(err) {
			return nil
		}
		return err
	}
	return nil
}

// isUnsupportedIoctlErr reports whether err indicates the filesystem does not
// support FS_IOC_GETFLAGS/FS_IOC_SETFLAGS. Different filesystems return
// different errnos; we treat all of them as "not supported, skip silently".
func isUnsupportedIoctlErr(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ENOTTY, syscall.ENOSYS, syscall.EOPNOTSUPP, syscall.EINVAL, syscall.EPERM, syscall.EACCES:
		return true
	}
	return false
}
