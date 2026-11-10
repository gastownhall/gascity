//go:build !windows

package livefile

import "os"

// stat is os.Stat: one stat(2) returns the size and dev/inode together.
func stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// open is os.Open: a rename or unlink never waits on an open descriptor.
func open(path string) (*os.File, error) {
	return os.Open(path)
}

// openFile is os.OpenFile, for the same reason as open.
func openFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag, perm)
}
