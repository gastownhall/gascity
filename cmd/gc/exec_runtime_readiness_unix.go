//go:build unix

package main

import (
	"os"
	"syscall"
)

// fileInode is fi's inode, 0 where the platform does not report one.
func fileInode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // Ino is uint32 on some unixes
	}
	return 0
}
