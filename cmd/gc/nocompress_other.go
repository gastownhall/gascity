//go:build !linux

package main

// setNoCompressAttr is a no-op on non-Linux platforms. The FS_NOCOMP_FL inode
// flag is a Linux-specific concept (primarily used by btrfs) and has no
// analogue on other operating systems.
func setNoCompressAttr(_ string) error {
	return nil
}
