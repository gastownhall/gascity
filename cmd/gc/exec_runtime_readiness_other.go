//go:build !unix

package main

import "os"

// fileInode is fi's inode, 0 where the platform does not report one.
func fileInode(os.FileInfo) uint64 { return 0 }
