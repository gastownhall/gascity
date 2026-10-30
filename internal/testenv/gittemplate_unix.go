//go:build unix

package testenv

import (
	"fmt"
	"os"
	"syscall"
)

// checkTemplateEntry reports why the template entry at path, whose Lstat
// result is fi, cannot be trusted by the user uid: someone else owns it, or
// group or others can write it. Either lets another user change what git
// copies into every test repo.
func checkTemplateEntry(path string, fi os.FileInfo, uid int) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot tell who owns it", path)
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d, not uid %d", path, st.Uid, uid)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %04o)", path, perm)
	}
	return nil
}
