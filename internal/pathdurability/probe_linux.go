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
func filesystemType(path string) (uint32, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	// Superblock magics are 32-bit unsigned, but Statfs_t.Type is signed and
	// arch-dependent (int64 on amd64/arm64, int32 on 386/arm). Converting
	// straight to a signed type sign-extends the magics with the high bit set —
	// ramfs (0x858458f6) becomes negative on 386/arm and matches nothing. Going
	// through uint32 truncates to the low 32 bits on every arch, which is
	// exactly the magic.
	return uint32(st.Type), nil //nolint:unconvert // load-bearing on 32-bit arches
}
