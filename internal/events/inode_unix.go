//go:build !windows

package events

import (
	"io/fs"
	"os"
	"syscall"
)

// statInode stats path and returns the result with its inodeOf identity.
func statInode(path string) (fs.FileInfo, uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	return info, inodeOf(info), nil
}

// inodeOfFile stats the open file f and returns the result with its inodeOf
// identity.
func inodeOfFile(f *os.File) (fs.FileInfo, uint64, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	return info, inodeOf(info), nil
}

// inodeOf returns a non-zero file identity for use by the file
// watcher's rotation detector. On Unix this is the inode number from
// the syscall.Stat_t. On filesystems where the OS doesn't surface a
// stable inode (very rare), the function returns 0; callers must
// treat that as "rotation undetectable" and fall back to size+offset
// semantics.
func inodeOf(info fs.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ino
}
