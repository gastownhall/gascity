//go:build linux

package pathdurability

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// deviceID returns the filesystem device the path lives on. Two paths with the
// same device ID are on the same mounted filesystem.
func deviceID(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, fmt.Errorf("stat %q: %w", path, err)
	}
	return st.Dev, nil
}

// filesystemType returns the superblock magic of the filesystem containing
// path, which is what distinguishes tmpfs and an overlay container rootfs from
// a real block device.
func filesystemType(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	// The conversion is redundant on 64-bit Linux, where Statfs_t.Type is already
	// int64, but required on 386/arm where it is int32.
	return int64(st.Type), nil //nolint:unconvert // load-bearing on 32-bit arches
}
