//go:build unix

package acceptancehelpers

import "syscall"

// dirWritable reports whether files can be created in dir. It asks the kernel
// with access(2) rather than creating a probe file: NewEnv calls it, and NewEnv
// must leave the operator's HOME untouched.
func dirWritable(dir string) bool {
	const wOK = 0x2 // W_OK, which syscall does not export
	return syscall.Access(dir, wOK) == nil
}
