//go:build !windows

package fsys

import "io/fs"

// IsExecutable reports whether info names a file the platform runs as a
// program: any of its execute permission bits is set. Whether info is a
// regular file is the caller's to check.
func IsExecutable(info fs.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}
