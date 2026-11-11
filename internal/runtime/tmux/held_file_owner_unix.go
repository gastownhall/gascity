//go:build !windows

package tmux

import (
	"os"
	"syscall"
)

// ownedRegularFile reports whether path is a regular file the current user
// owns. Anything else, or a path that cannot be stat'ed, is not.
func ownedRegularFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
