//go:build unix

package session

import (
	"fmt"
	"syscall"
	"testing"
)

// lockFileIdentity is the identity l's acquire recorded: the boot ID, the
// lock file's device and inode as stat reports them, and l's token, which a
// record write rotated into the lock file.
func lockFileIdentity(t *testing.T, l *RuntimeLease) string {
	t.Helper()
	fi, err := l.lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%s/%d/%d/%s", runtimeLeaseBootID(), st.Dev, st.Ino, l.token)
}
